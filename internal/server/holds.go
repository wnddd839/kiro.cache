package server

import (
	"sync"

	"kiro-proxy/internal/keys"
)

// hold 是一个在途请求在准入时为 key 预留的额度。
type hold struct {
	key  string
	amt  keys.Spent
	once sync.Once
}

// holdTable 记每把 key 在途请求的预留总量。准入判断看「已入账 + 预留」，
// 并发准入在同一把锁内串行：同时到达的 subagent 不会都按同一份已用量放行。
type holdTable struct {
	mu    sync.Mutex
	byKey map[string]keys.Spent
}

func newHoldTable() *holdTable { return &holdTable{byKey: map[string]keys.Spent{}} }

// addLocked 记一份预留。调用方持有 mu。
func (t *holdTable) addLocked(key string, amt keys.Spent) *hold {
	r := t.byKey[key]
	r.Requests += amt.Requests
	r.Credits += amt.Credits
	r.CostUSD += amt.CostUSD
	t.byKey[key] = r
	return &hold{key: key, amt: amt}
}

// release 释放预留；可重复调用，只生效一次。h 为 nil 时什么也不做。
func (t *holdTable) release(h *hold) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		r := t.byKey[h.key]
		r.Requests -= h.amt.Requests
		r.Credits -= h.amt.Credits
		r.CostUSD -= h.amt.CostUSD
		if r.Requests <= 0 {
			delete(t.byKey, h.key)
			return
		}
		t.byKey[h.key] = r
	})
}

// reserved 是 key 当前的预留总量。
func (t *holdTable) reserved(key string) keys.Spent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byKey[key]
}
