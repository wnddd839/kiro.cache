package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"kiro-proxy/internal/anthropic"
)

const maxSessions = 4096

// ttlMap 是带空闲过期与容量上限的小表。丢了只影响 cache 命中。
type ttlMap[V any] struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	v     V
	until time.Time
}

func newTTLMap[V any](ttl time.Duration) *ttlMap[V] {
	return &ttlMap[V]{ttl: ttl, m: map[string]ttlEntry[V]{}}
}

func (t *ttlMap[V]) get(key string) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[key]
	if !ok || time.Now().After(e.until) {
		delete(t.m, key)
		var zero V
		return zero, false
	}
	return e.v, true
}

func (t *ttlMap[V]) set(key string, v V) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if _, ok := t.m[key]; !ok && len(t.m) >= maxSessions {
		var oldest string
		var at time.Time
		for k, e := range t.m {
			if now.After(e.until) {
				delete(t.m, k)
			} else if oldest == "" || e.until.Before(at) {
				oldest, at = k, e.until
			}
		}
		if len(t.m) >= maxSessions {
			delete(t.m, oldest)
		}
	}
	t.m[key] = ttlEntry[V]{v: v, until: now.Add(t.ttl)}
}

// snapshotTTL 是 ttlMap 的落盘条目。
type snapshotTTL[V any] struct {
	V     V         `json:"v"`
	Until time.Time `json:"until"`
}

// dump 是仍有效的条目。
func (t *ttlMap[V]) dump() map[string]snapshotTTL[V] {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	out := make(map[string]snapshotTTL[V], len(t.m))
	for k, e := range t.m {
		if now.Before(e.until) {
			out[k] = snapshotTTL[V]{V: e.v, Until: e.until}
		}
	}
	return out
}

// restore 载入条目，跳过已过期的；不覆盖已有的。
func (t *ttlMap[V]) restore(in map[string]snapshotTTL[V]) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now, n := time.Now(), 0
	for k, e := range in {
		if _, ok := t.m[k]; ok || !now.Before(e.Until) || len(t.m) >= maxSessions {
			continue
		}
		t.m[k] = ttlEntry[V]{v: e.V, until: e.Until}
		n++
	}
	return n
}

func (t *ttlMap[V]) delete(key string) {
	t.mu.Lock()
	delete(t.m, key)
	t.mu.Unlock()
}

// lineage 是每条消息的指纹。cache_control 断点每轮都会挪位置，不属于内容，剔掉再算。
// 用 sha256 截断（不是进程内随机种子的 maphash）：指纹要落盘，重启后仍能判断历史是否延续。
func lineage(req *anthropic.Request) []uint64 {
	out := make([]uint64, 0, len(req.Messages))
	for _, m := range req.Messages {
		m.Content = withoutCacheControl(m.Content)
		b, _ := json.Marshal(m)
		sum := sha256.Sum256(b)
		out = append(out, binary.LittleEndian.Uint64(sum[:8]))
	}
	return out
}

func withoutCacheControl(c anthropic.Content) anthropic.Content {
	out := make(anthropic.Content, len(c))
	for i, b := range c {
		b.CacheControl = nil
		if len(b.Content) > 0 {
			b.Content = withoutCacheControl(b.Content)
		}
		out[i] = b
	}
	return out
}

// continues 报告 cur 是否是 prev 的延续：prev 是 cur 的前缀。
// 客户端回退 / 编辑 / 压缩历史时不是延续，上游 conversationId 应轮换。
func continues(prev, cur []uint64) bool {
	return len(prev) <= len(cur) && slices.Equal(prev, cur[:len(prev)])
}
