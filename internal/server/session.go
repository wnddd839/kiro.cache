package server

import (
	"encoding/json"
	"hash/maphash"
	"slices"
	"sync"
	"time"

	"kiro-go/internal/anthropic"
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

func (t *ttlMap[V]) delete(key string) {
	t.mu.Lock()
	delete(t.m, key)
	t.mu.Unlock()
}

// 一个进程内固定的种子：指纹只在进程内比较。
var lineageSeed = maphash.MakeSeed()

// lineage 是每条消息的指纹。解析后的 Block 已丢掉 cache_control 等易变字段。
func lineage(req *anthropic.Request) []uint64 {
	out := make([]uint64, 0, len(req.Messages))
	for _, m := range req.Messages {
		b, _ := json.Marshal(m)
		out = append(out, maphash.Bytes(lineageSeed, b))
	}
	return out
}

// continues 报告 cur 是否是 prev 的延续：prev 是 cur 的前缀。
// 客户端回退 / 编辑 / 压缩历史时不是延续，上游 conversationId 应轮换。
func continues(prev, cur []uint64) bool {
	return len(prev) <= len(cur) && slices.Equal(prev, cur[:len(prev)])
}
