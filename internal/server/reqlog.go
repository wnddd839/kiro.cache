package server

import (
	"sync"
	"time"
)

// statusClientClosed 是下游中途断开时记的状态（nginx 的 499 约定）。
const statusClientClosed = 499

// requestRecord 是一次上游尝试的记录，给管理台看。不含消息内容与 token。
type requestRecord struct {
	Time       time.Time `json:"time"`
	Protocol   string    `json:"protocol,omitzero"`
	Key        string    `json:"key,omitzero"`
	Account    string    `json:"account,omitzero"`
	Model      string    `json:"model"`
	Stream     bool      `json:"stream"`
	Thinking   int       `json:"thinking"`
	Pinned     bool      `json:"pinned"`
	Thread     string    `json:"thread,omitzero"`
	Conv       string    `json:"conv,omitzero"`
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cache_read"`
	CacheWrite int       `json:"cache_write"`
	Credits    float64   `json:"credits,omitzero"`
	CostUSD    float64   `json:"cost_usd,omitzero"`
	Status     int       `json:"status"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitzero"`
	MS         int64     `json:"ms"`
	FirstMS    int64     `json:"first_ms,omitzero"`
}

// requestLog 是最近请求的环形缓冲。零值不可用，用 newRequestLog。
type requestLog struct {
	mu   sync.Mutex
	buf  []requestRecord
	next int
	full bool
}

func newRequestLog(n int) *requestLog { return &requestLog{buf: make([]requestRecord, n)} }

func (l *requestLog) add(r requestRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.next] = r
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
}

// recent 返回最近 limit 条，新的在前。
func (l *requestLog) recent(limit int) []requestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.buf)
	}
	n = min(n, max(limit, 0))
	out := make([]requestRecord, 0, n)
	for i := range n {
		out = append(out, l.buf[(l.next-1-i+len(l.buf))%len(l.buf)])
	}
	return out
}
