// Package server 是本地入口：Anthropic Messages、OpenAI Chat Completions / Responses 共用一条管线：
// 鉴权与预算 → 整理前缀 → 选号钉会话 → 转 Kiro → 本地计量 → 按下游协议写回 → 记账。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/normalize"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/sessionpin"
	"kiro-proxy/internal/usage"
)

const maxBody = 64 << 20 // 带图片的长对话

// Server 是分发层 HTTP 服务。
type Server struct {
	cfg  config.Config
	pool *pool.Pool
	log  *slog.Logger
	norm normalize.Options

	conv   *sessionpin.ConversationTable // 线程+号 → 上游 conversationId
	lines  *ttlMap[[]uint64]             // 线程 → 上次请求的消息指纹
	think  *ttlMap[int]                  // 线程 → 钉住的 thinking 预算
	models catalog

	keys         *keys.Store
	usage        *usage.Journal
	cache        *meter.Cache // 按号隔离的本地 prompt cache 模拟
	pricer       *meter.Pricer
	holds        *holdTable // 在途请求为 key 预留的额度
	hidden       *hiddenWatch
	reportedSeen atomic.Int64 // 上游报了 tokenUsage 的回复数（采样用）
	reportsSeen  atomic.Bool  // 上游是否报过 tokenUsage（影响流式开头的下界）

	catalogBusy sync.Mutex
	priceBusy   sync.Mutex
	quotaBusy   sync.Map        // 正在查额度的号（同一号额度用尽时并发失败只查一次）
	priceErr    atomicString    // 上次在线价格拉取的错误（部分来源失败也记）
	bg          context.Context // 后台任务的生命周期
	bgWG        sync.WaitGroup  // 需要在退出前收尾的后台任务（最后一次落盘）

	reqlog  *requestLog
	login   loginJob
	started time.Time

	// Version 显示在管理台；由 main 设置。
	Version string

	// signIn / openBrowser 可替换，测试用。
	signIn      func(ctx context.Context, open func(string) error) (kiro.Login, error)
	openBrowser func(string) error
}

// Stores 是服务用到的持久化存储。零值字段用只在内存里的实现。
type Stores struct {
	Keys  *keys.Store
	Usage *usage.Journal
}

// New 建服务。bg 结束时后台任务停止。st 的生命周期归调用方。
func New(bg context.Context, cfg config.Config, p *pool.Pool, log *slog.Logger, st Stores) (*Server, error) {
	strip, err := normalize.Compile(cfg.SystemStrip)
	if err != nil {
		return nil, err
	}
	if st.Keys == nil {
		if st.Keys, err = keys.Open("", nil); err != nil {
			return nil, err
		}
	}
	if st.Usage == nil {
		if st.Usage, err = usage.Open("", usage.Options{}); err != nil {
			return nil, err
		}
	}
	if cfg.CacheMode == "" {
		cfg.CacheMode = meter.ModeProtocol
	}
	ttl := time.Duration(cfg.SessionTTL)
	cache := meter.NewCache(nil)
	s := &Server{
		cfg:   cfg,
		pool:  p,
		log:   log,
		norm:  normalize.Options{SortTools: cfg.SortTools, SystemStrip: strip},
		conv:  sessionpin.NewConversationTable(ttl),
		lines: newTTLMap[[]uint64](ttl),
		think: newTTLMap[int](ttl),
		bg:    bg,

		keys:   st.Keys,
		usage:  st.Usage,
		cache:  cache,
		pricer: &meter.Pricer{Overrides: cfg.Prices, CreditUSD: cfg.CreditUSD, Basis: cfg.CostBasis},
		holds:  newHoldTable(),
		hidden: newHiddenWatch(),

		reqlog:      newRequestLog(500),
		started:     time.Now(),
		Version:     "dev",
		openBrowser: kiro.OpenBrowser,
	}
	if cfg.CacheFile != "" {
		// prompt cache 与会话状态一起载入；任一坏掉就都从空开始。只影响命中与计量，不阻止启动
		if n, m, err := s.loadState(); err != nil {
			s.cache = meter.NewCache(nil)
			s.conv = sessionpin.NewConversationTable(ttl)
			log.Warn("cache / session state unreadable, starting empty", "file", cfg.CacheFile, "err", err)
		} else if n+m > 0 {
			log.Info("cache / session state loaded", "file", cfg.CacheFile, "cache_entries", n, "sessions", m)
		}
		s.bgWG.Go(s.persistCache)
	}
	if cfg.PricesFile != "" {
		if n, err := s.pricer.LoadOnline(cfg.PricesFile); err != nil {
			log.Warn("online prices unreadable, using builtin", "file", cfg.PricesFile, "err", err)
		} else if n > 0 {
			s.pricer.KeepOnline(meter.IsBuiltinID)
			online, _ := s.pricer.Online()
			s.log.Info("online prices loaded", "file", cfg.PricesFile, "models", len(online), "was", n)
			if len(online) != n {
				if err := s.pricer.SaveOnline(cfg.PricesFile); err != nil {
					log.Warn("prices prune save", "file", cfg.PricesFile, "err", err)
				}
			}
		}
	}
	if cfg.PriceSync > 0 {
		go s.syncPrices()
	}
	return s, nil
}

// Wait 等后台任务在 bg 结束后收尾（最后一次落盘）。先让 bg 结束再调。
func (s *Server) Wait() { s.bgWG.Wait() }

// cacheSaveEvery 是 prompt cache 状态的落盘间隔。
const cacheSaveEvery = time.Minute

// persistCache 定时把 prompt cache 与会话状态一起落盘，bg 结束时再存一次。
func (s *Server) persistCache() {
	t := time.NewTicker(cacheSaveEvery)
	defer t.Stop()
	save := func() {
		if err := s.saveState(); err != nil {
			s.log.Warn("cache / session state save", "err", err)
		}
	}
	for {
		select {
		case <-s.bg.Done():
			save()
			return
		case <-t.C:
			save()
		}
	}
}

// Handler 是全部路由。
//
// 下游路径按后缀识别：/v1/messages、/messages、/v1/v1/messages、/anthropic/v1/messages
// 都落到同一处。客户端 base URL 带或不带 /v1、带代理前缀都能用。
func (s *Server) Handler() http.Handler {
	// 管理台没设 admin_token 时，挂在浏览器里的任意网页都能向本机 /admin 发 POST；拒绝跨站的写请求。
	admin := http.NewCrossOriginProtection().Handler(s.adminMux())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimRight(path.Clean("/"+r.URL.Path), "/")
		switch {
		case p == "/health":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": s.pool.Len()})
		case p == "" || p == "/ui":
			// 根路径：浏览器给页面，其余（探活脚本）给 health
			if p == "/ui" || strings.Contains(r.Header.Get("Accept"), "text/html") {
				s.serveConsole(w, r)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": s.pool.Len()})
		case strings.HasPrefix(p, "/admin/") || p == "/admin":
			if !s.adminAllowed(r) {
				msg := "admin token required"
				if s.cfg.AdminToken == "" {
					msg = "admin is only reachable from this machine (http://127.0.0.1 or http://localhost) unless admin_token is set"
				}
				writeError(w, http.StatusUnauthorized, msg)
				return
			}
			admin.ServeHTTP(w, r)
		case strings.HasSuffix(p, "/messages/count_tokens"):
			s.guard(w, r, http.MethodPost, protoAnthropic, s.countTokens)
		case strings.HasSuffix(p, "/messages"):
			s.guard(w, r, http.MethodPost, protoAnthropic, s.generate(protoAnthropic))
		case strings.HasSuffix(p, "/chat/completions"):
			s.guard(w, r, http.MethodPost, protoChat, s.generate(protoChat))
		case strings.HasSuffix(p, "/responses"):
			s.guard(w, r, http.MethodPost, protoResponses, s.generate(protoResponses))
		case strings.HasSuffix(p, "/models"):
			s.guard(w, r, http.MethodGet, protoAnthropic, s.listModels)
		default:
			writeError(w, http.StatusNotFound, "no route for "+r.URL.Path)
		}
	})
}

// apiHandler 是通过鉴权的下游接口。k 为 nil 表示匿名（未配置 key，或命中 config 里的 api_keys）。
type apiHandler func(w http.ResponseWriter, r *http.Request, k *keys.Key)

func (s *Server) guard(w http.ResponseWriter, r *http.Request, method string, p proto, h apiHandler) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		p.writeError(w, http.StatusMethodNotAllowed, "use "+method)
		return
	}
	k, err := s.client(r)
	if err != nil {
		p.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	h(w, r, k)
}

// client 识别下游：key 库里的 key 带预算；config.api_keys 是不记名的全权 key；
// 两者都没配置时不校验。
func (s *Server) client(r *http.Request) (*keys.Key, error) {
	got := requestToken(r, "X-Api-Key")
	if k, ok := s.keys.Lookup(got); ok {
		if k.Disabled {
			return nil, keys.ErrDisabled
		}
		if !k.ExpiresAt.IsZero() && !time.Now().Before(k.ExpiresAt) {
			return nil, keys.ErrExpired
		}
		return &k, nil
	}
	for _, k := range s.cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			return nil, nil
		}
	}
	if len(s.cfg.APIKeys) == 0 && s.keys.Len() == 0 {
		return nil, nil
	}
	return nil, keys.ErrUnknown
}

// adminAllowed 判断 /admin 的访问。没设 admin_token 时只放行本机：连接来自回环地址，
// 且 Host 是 localhost / 回环 IP（挡 DNS rebinding：恶意网页把自己的域名解析到 127.0.0.1 后读 /admin/keys）。
// 监听 0.0.0.0 时局域网里的其他机器必须带 admin_token。
func (s *Server) adminAllowed(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return isLoopback(r)
	}
	got := requestToken(r, "X-Admin-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) == 1
}

// isLoopback 报告请求是否来自本机，且是用本机地址访问的。
// 带了转发头（X-Forwarded-For / Forwarded / X-Real-IP）就不算：反代把外网请求从 127.0.0.1 转进来，
// 连接地址是回环，实际来源不是。放在反代后面时必须设 admin_token。
func isLoopback(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip", "X-Forwarded-Host"} {
		if _, ok := r.Header[h]; ok {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return false
	}
	return loopbackHost(r.Host)
}

func loopbackHost(h string) bool {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func requestToken(r *http.Request, header string) string {
	if v := r.Header.Get(header); v != "" {
		return v
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// checkQuota 在一个号报额度用尽后异步查一次它的额度：拿到重置时间，号池就冷却到重置。
// 这是除加号 / 管理台手动外唯一查额度的地方，不再定时轮询所有号。
func (s *Server) checkQuota(id string) {
	if _, busy := s.quotaBusy.LoadOrStore(id, struct{}{}); busy {
		return
	}
	go func() {
		defer s.quotaBusy.Delete(id)
		ctx, cancel := context.WithTimeout(s.bg, time.Minute)
		defer cancel()
		l, err := s.pool.RefreshLimits(ctx, id)
		if err != nil {
			s.log.Warn("usage limits after quota failure", "account", id, "err", err)
			return
		}
		s.log.Info("usage limits after quota failure", "account", id, "used", l.Used, "limit", l.Limit, "reset_at", l.ResetAt)
	}()
}

// ensureCatalog 在模型列表过期时异步刷新，不阻塞请求。
func (s *Server) ensureCatalog() {
	if !s.models.stale() || !s.catalogBusy.TryLock() {
		return
	}
	go func() {
		defer s.catalogBusy.Unlock()
		ctx, cancel := context.WithTimeout(s.bg, time.Minute)
		defer cancel()
		s.refreshCatalog(ctx)
	}()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, anthropic.ErrorBody{Type: "error", Error: anthropic.ErrorDetail{Type: anthropic.ErrorType(status), Message: msg}})
}
