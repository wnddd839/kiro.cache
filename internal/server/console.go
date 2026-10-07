package server

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/usage"
)

// consoleHTML 是管理台页面。页面本身不含数据，数据走 /admin/*（受 admin_token 保护）。
//
//go:embed admin.html
var consoleHTML []byte

func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
	_, _ = w.Write(consoleHTML)
}

// overview 是管理台首页的汇总。
func (s *Server) overview(w http.ResponseWriter, _ *http.Request) {
	type counts struct {
		Total    int `json:"total"`
		Enabled  int `json:"enabled"`
		Cooling  int `json:"cooling"`
		Disabled int `json:"disabled"`
		InFlight int `json:"in_flight"`
	}
	var c counts
	now := time.Now()
	for _, v := range s.pool.List() {
		c.Total++
		c.InFlight += v.InFlight
		switch {
		case v.Disabled:
			c.Disabled++
		case v.CooldownUntil.After(now):
			c.Cooling++
		default:
			c.Enabled++
		}
	}
	t := s.pool.Totals()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.Version,
		"listen":     s.cfg.Listen,
		"started_at": s.started,
		"auth":       map[string]bool{"api_keys": len(s.cfg.APIKeys) > 0, "admin_token": s.cfg.AdminToken != ""},
		"config": map[string]any{
			"conversation_mode": s.cfg.ConversationMode,
			"cache_mode":        s.cfg.CacheMode,
			"session_ttl":       time.Duration(s.cfg.SessionTTL).String(),
			"sort_tools":        s.cfg.SortTools,
			"pin_thinking":      s.cfg.PinThinking,
			"max_attempts":      s.cfg.MaxAttempts,
			"limits_interval":   time.Duration(s.cfg.LimitsInterval).String(),
		},
		"accounts":       c,
		"totals":         t,
		"cache_hit_rate": t.HitRate(),
		"models":         len(s.models.list()),
		"keys":           s.keys.Len(),
		"today":          s.usage.Query(usage.Query{From: today, PageSize: 1}).Totals,
		"month":          s.usage.Query(usage.Query{From: month, Bucket: "day", PageSize: 1}).Totals,
		"billing":        map[string]any{"cache_mode": s.cfg.CacheMode, "cache_ttl": s.cfg.CacheTTL, "credit_usd": s.pricer.CreditUSD},
		"hidden":         s.hidden.stats(),
		"breaker":        s.pool.Breaker(),
	})
}

func (s *Server) recentRequests(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.reqlog.recent(limit)})
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		s.refreshCatalog(ctx)
		cancel()
	}
	resp := map[string]any{"models": s.modelRows(), "credit_usd": s.pricer.CreditUSD}
	if at := s.models.updated(); !at.IsZero() {
		resp["updated_at"] = at
	}
	writeJSON(w, http.StatusOK, resp)
}

// loginState 是管理台发起的浏览器登录的进度。
type loginState struct {
	State     string    `json:"state"` // idle | waiting | done | failed
	URL       string    `json:"url,omitzero"`
	StartedAt time.Time `json:"started_at,omitzero"`
	AccountID string    `json:"account_id,omitzero"`
	Email     string    `json:"email,omitzero"`
	Error     string    `json:"error,omitzero"`
}

// loginJob 保证同时只有一次浏览器登录：回调端口是固定的那几个。
type loginJob struct {
	mu     sync.Mutex
	state  loginState
	cancel context.CancelFunc
}

func (j *loginJob) get() loginState {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.State == "" {
		return loginState{State: "idle"}
	}
	return j.state
}

func (j *loginJob) update(f func(*loginState)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f(&j.state)
}

// errLoginCanceled 是管理台取消登录时的原因。
var errLoginCanceled = errors.New("sign-in canceled")

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.login.get())
	case http.MethodDelete:
		s.login.mu.Lock()
		if s.login.cancel != nil {
			s.login.cancel()
		}
		s.login.mu.Unlock()
		writeJSON(w, http.StatusOK, s.login.get())
	case http.MethodPost:
		s.startLogin(w)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "use GET, POST or DELETE")
	}
}

func (s *Server) startLogin(w http.ResponseWriter) {
	j := &s.login
	j.mu.Lock()
	if j.state.State == "waiting" {
		j.mu.Unlock()
		writeError(w, http.StatusConflict, "a sign-in is already in progress")
		return
	}
	ctx, cancel := context.WithCancelCause(s.bg)
	j.cancel = func() { cancel(errLoginCanceled) }
	j.state = loginState{State: "waiting", StartedAt: time.Now()}
	j.mu.Unlock()

	opened := make(chan struct{})
	signIn := s.signIn
	if signIn == nil {
		signIn = func(ctx context.Context, open func(string) error) (kiro.Login, error) {
			return (&kiro.SignIn{Client: s.pool.Client(), Open: open}).Run(ctx)
		}
	}
	go func() {
		defer cancel(nil)
		var once sync.Once
		l, err := signIn(ctx, func(u string) error {
			j.update(func(st *loginState) { st.URL = u })
			once.Do(func() { close(opened) })
			return s.openBrowser(u)
		})
		once.Do(func() { close(opened) })
		if err == nil {
			var a pool.Account
			a, err = s.pool.Add(pool.Account{Label: cmp.Or(l.Email, "kiro"), Cred: l.Cred})
			if err == nil {
				lctx, lcancel := context.WithTimeout(s.bg, time.Minute)
				if _, lerr := s.pool.RefreshLimits(lctx, a.ID); lerr != nil {
					s.log.Warn("sign-in: usage check", "account", a.ID, "err", lerr)
				}
				lcancel()
				j.update(func(st *loginState) { st.State, st.AccountID, st.Email, st.URL = "done", a.ID, l.Email, "" })
				s.log.Info("sign-in: account added", "account", a.ID, "email", l.Email)
				return
			}
		}
		j.update(func(st *loginState) { st.State, st.Error, st.URL = "failed", err.Error(), "" })
		s.log.Warn("sign-in failed", "err", err)
	}()

	// 等到登录页 URL 出来（或立刻失败，如端口全占）再答复，页面拿到就能显示链接
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
	}
	writeJSON(w, http.StatusAccepted, j.get())
}
