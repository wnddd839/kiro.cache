package sessionpin

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"
)

// conversation 是某会话在某个号上的上游 conversationId。
type conversation struct {
	id    string
	until time.Time
}

// ConversationTable 让同一会话在同一个号上复用同一个 Kiro conversationId。
// Magpie / 插件每次 randomUUID()，这里按 会话 → 号 → conversationId 钉住：
// 会话在几个号之间来回（并发、换号后回退）时，每个号各自保住自己的 conversation，
// 不会因为换了一次号就把旧号上的上游 cache 丢掉。
// 条目按 TTL 空闲过期；容量按会话计。丢了只影响上游 cache 命中。
type ConversationTable struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]map[string]conversation // 会话键 → 号 → conversation
}

// NewConversationTable 建表；ttl 非正则回落 24 小时空闲过期。
func NewConversationTable(ttl time.Duration) *ConversationTable {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &ConversationTable{ttl: ttl, m: map[string]map[string]conversation{}}
}

// ID 返回该会话键在该号上的上游 conversationId；没有或已过期就生成新的。
// 生成在锁内完成，同一会话同一号的并发首请求共享同一个 ID。
// 键或账号为空时返回空串，调用方回落逐请求随机 ID。
func (t *ConversationTable) ID(key, accountID string) string {
	key = strings.TrimSpace(key)
	accountID = strings.TrimSpace(accountID)
	if t == nil || key == "" || accountID == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	byAccount, ok := t.m[key]
	if !ok {
		if len(t.m) >= maxEntries {
			t.evictExpiredLocked(now)
			if len(t.m) >= maxEntries {
				t.evictOldestLocked()
			}
		}
		byAccount = map[string]conversation{}
		t.m[key] = byAccount
	}
	entry, ok := byAccount[accountID]
	if !ok || !now.Before(entry.until) {
		entry.id = randomUUIDv4()
	}
	entry.until = now.Add(t.ttl)
	byAccount[accountID] = entry
	return entry.id
}

// Forget 作废该会话在所有号上的 conversationId。历史被改写（回退 / 编辑 / 压缩）时调用。
func (t *ConversationTable) Forget(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.m, strings.TrimSpace(key))
	t.mu.Unlock()
}

// ForgetAccount 只作废该会话在某个号上的 conversationId。该号失败换号时调用，
// 不影响会话在其它号上的 conversation。
func (t *ConversationTable) ForgetAccount(key, accountID string) {
	if t == nil {
		return
	}
	key = strings.TrimSpace(key)
	t.mu.Lock()
	if byAccount, ok := t.m[key]; ok {
		delete(byAccount, strings.TrimSpace(accountID))
		if len(byAccount) == 0 {
			delete(t.m, key)
		}
	}
	t.mu.Unlock()
}

// ConvSnapshot 是一个 conversationId 的落盘条目。
type ConvSnapshot struct {
	ID    string    `json:"id"`
	Until time.Time `json:"until"`
}

// Dump 是仍有效的 会话 → 号 → conversationId。
func (t *ConversationTable) Dump() map[string]map[string]ConvSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	out := make(map[string]map[string]ConvSnapshot, len(t.m))
	for k, byAccount := range t.m {
		for acc, c := range byAccount {
			if !now.Before(c.until) {
				continue
			}
			if out[k] == nil {
				out[k] = map[string]ConvSnapshot{}
			}
			out[k][acc] = ConvSnapshot{ID: c.id, Until: c.until}
		}
	}
	return out
}

// Restore 载入 conversationId，跳过过期的与已有的。返回载入条数。
func (t *ConversationTable) Restore(in map[string]map[string]ConvSnapshot) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now, n := time.Now(), 0
	for k, byAccount := range in {
		for acc, c := range byAccount {
			if c.ID == "" || !now.Before(c.Until) {
				continue
			}
			m := t.m[k]
			if m == nil {
				if len(t.m) >= maxEntries {
					continue
				}
				m = map[string]conversation{}
				t.m[k] = m
			}
			if _, ok := m[acc]; ok {
				continue
			}
			m[acc] = conversation{id: c.ID, until: c.Until}
			n++
		}
	}
	return n
}

// lastUsed 是会话在任一号上最近一次的过期时刻。
func lastUsed(byAccount map[string]conversation) time.Time {
	var latest time.Time
	for _, c := range byAccount {
		if c.until.After(latest) {
			latest = c.until
		}
	}
	return latest
}

func (t *ConversationTable) evictExpiredLocked(now time.Time) {
	for k, byAccount := range t.m {
		if now.After(lastUsed(byAccount)) {
			delete(t.m, k)
		}
	}
}

func (t *ConversationTable) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, byAccount := range t.m {
		if u := lastUsed(byAccount); first || u.Before(oldest) {
			oldestKey, oldest, first = k, u, false
		}
	}
	if oldestKey != "" {
		delete(t.m, oldestKey)
	}
}

// randomUUIDv4 生成 RFC 4122 v4 UUID。随机源失败属于系统级故障：
// 宁可 panic 也不静默发出空/重复会话 ID。
func randomUUIDv4() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("sessionpin: crypto/rand unavailable: %v", err))
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
