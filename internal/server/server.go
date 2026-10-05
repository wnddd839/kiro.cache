// Package server 是本地 Anthropic Messages 兼容入口：选号、钉会话、转 Kiro、转回 SSE。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"kiro-go/internal/anthropic"
	"kiro-go/internal/config"
	"kiro-go/internal/normalize"
	"kiro-go/internal/pool"
	"kiro-go/internal/sessionpin"
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

	catalogBusy sync.Mutex
	bg          context.Context // 后台任务的生命周期
}

// New 建服务。bg 结束时后台任务停止。
func New(bg context.Context, cfg config.Config, p *pool.Pool, log *slog.Logger) (*Server, error) {
	strip, err := normalize.Compile(cfg.SystemStrip)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(cfg.SessionTTL)
	return &Server{
		cfg:   cfg,
		pool:  p,
		log:   log,
		norm:  normalize.Options{SortTools: cfg.SortTools, SystemStrip: strip},
		conv:  sessionpin.NewConversationTable(ttl),
		lines: newTTLMap[[]uint64](ttl),
		think: newTTLMap[int](ttl),
		bg:    bg,
	}, nil
}

// Handler 是全部路由。
//
// 下游路径按后缀识别：/v1/messages、/messages、/v1/v1/messages、/anthropic/v1/messages
// 都落到同一处。客户端 base URL 带或不带 /v1、带代理前缀都能用。
func (s *Server) Handler() http.Handler {
	admin := s.adminMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimRight(path.Clean("/"+r.URL.Path), "/")
		switch {
		case p == "" || p == "/health":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": s.pool.Len()})
		case strings.HasPrefix(p, "/admin/") || p == "/admin":
			if !s.adminAllowed(r) {
				writeError(w, http.StatusUnauthorized, "admin token required")
				return
			}
			admin.ServeHTTP(w, r)
		case strings.HasSuffix(p, "/messages/count_tokens"):
			s.guard(w, r, http.MethodPost, s.countTokens)
		case strings.HasSuffix(p, "/messages"):
			s.guard(w, r, http.MethodPost, s.messages)
		case strings.HasSuffix(p, "/models"):
			s.guard(w, r, http.MethodGet, s.listModels)
		case strings.HasSuffix(p, "/chat/completions") || strings.HasSuffix(p, "/responses"):
			writeError(w, http.StatusNotFound, "only the Anthropic Messages API (/v1/messages) is served")
		default:
			writeError(w, http.StatusNotFound, "no route for "+r.URL.Path)
		}
	})
}

func (s *Server) guard(w http.ResponseWriter, r *http.Request, method string, h http.HandlerFunc) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeError(w, http.StatusMethodNotAllowed, "use "+method)
		return
	}
	if !s.clientAllowed(r) {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	h(w, r)
}

func (s *Server) clientAllowed(r *http.Request) bool {
	if len(s.cfg.APIKeys) == 0 {
		return true
	}
	got := requestToken(r, "X-Api-Key")
	for _, k := range s.cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) adminAllowed(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return true
	}
	got := requestToken(r, "X-Admin-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) == 1
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
