package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiro-go/internal/anthropic"
	"kiro-go/internal/config"
	"kiro-go/internal/kiro"
	"kiro-go/internal/normalize"
	"kiro-go/internal/pool"
	"kiro-go/internal/sessionpin"
)

// call 是一次下游请求在分发层里的状态。
type call struct {
	req     anthropic.Request
	model   string // Kiro 模型 id
	thread  string // 会话线程键：粘号、钉 conversationId、钉 thinking
	budget  int
	lineage []uint64
	start   time.Time
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	c, err := s.prepare(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.ensureCatalog()
	s.dispatch(w, r, c)
}

// prepare 解析并整理请求，算出会话线程与 thinking 预算。
func (s *Server) prepare(r *http.Request) (*call, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	c := &call{start: time.Now()}
	if err := json.Unmarshal(raw, &c.req); err != nil {
		return nil, fmt.Errorf("a request that isn't Messages JSON: %w", err)
	}
	if len(c.req.Messages) == 0 {
		return nil, errors.New("messages: at least one message is required")
	}
	normalize.Request(&c.req, s.norm)
	c.model = kiroModel(c.req.Model, s.cfg.ModelAliases, s.models.known)
	c.thread = threadKey(r, &c.req)
	c.lineage = lineage(&c.req)

	c.budget = kiro.ThinkingBudget(&c.req, c.model)
	if s.cfg.PinThinking && c.thread != "" {
		// 同一线程的 thinking 以首次为准：它写在首条消息前面，一变整段前缀就变
		if pinned, ok := s.think.get(c.thread); ok {
			c.budget = pinned
		}
		s.think.set(c.thread, c.budget)
	}
	return c, nil
}

// threadKey 是会话键加首条前缀。客户端给的 session 在主线程与子 agent / 标题生成间共享，
// 它们的 system 与首条 user 不同，必须是不同的上游 conversation。
func threadKey(r *http.Request, req *anthropic.Request) string {
	msgs := normalize.PinMessages(req)
	clientKey := cmp.Or(req.PromptCacheKey, r.Header.Get("X-Claude-Code-Session-Id"), normalize.MetadataSession(req))
	base := sessionpin.Key(r.Header, clientKey, msgs)
	prefix := sessionpin.Key(nil, "", msgs)
	if strings.HasPrefix(base, "hdr:") && prefix != "" {
		return base + "|" + prefix
	}
	return base
}

// outcome 是一次上游尝试的结果。
type outcome struct {
	done    bool // 已向下游写出响应（成功或不可重试的失败）
	failure *kiro.Failure
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, c *call) {
	ctx := r.Context()
	var tried []string
	var last *kiro.Failure
	for range s.cfg.MaxAttempts {
		lease, err := s.pool.Acquire(c.thread, tried)
		if err != nil {
			if last != nil {
				writeError(w, last.Status, last.Message)
				return
			}
			if ue, ok := errors.AsType[*pool.UnavailableError](err); ok && !ue.RetryAt.IsZero() {
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(time.Until(ue.RetryAt).Seconds()))))
				writeError(w, http.StatusTooManyRequests, err.Error())
				return
			}
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		out := s.attempt(ctx, w, lease, c)
		lease.Release()
		if out.done || ctx.Err() != nil {
			return
		}
		tried = append(tried, lease.ID)
		last = out.failure
	}
	if last == nil {
		last = &kiro.Failure{Status: http.StatusBadGateway, Message: "no attempt succeeded"}
	}
	writeError(w, last.Status, last.Message)
}

// attempt 在一个号上试一次。在向下游写出任何字节之前失败都可以换号重试。
func (s *Server) attempt(ctx context.Context, w http.ResponseWriter, lease *pool.Lease, c *call) outcome {
	id := lease.ID
	cred, err := s.pool.Cred(ctx, id, "")
	if err != nil {
		f := kiro.Failure{Status: http.StatusBadGateway, Message: "credentials: " + err.Error(), Class: kiro.ClassAuth}
		s.pool.Fail(id, f)
		return outcome{failure: &f}
	}

	convID := ""
	if s.cfg.ConversationMode == config.ConversationSession && c.thread != "" {
		// 前缀延续按 线程+号 判断：同一线程并发落在不同号上时互不干扰
		if prev, ok := s.lines.get(lineKey(c.thread, id)); ok && !continues(prev, c.lineage) {
			// 历史被改写（回退 / 压缩 / 编辑）：该号上的旧 conversation 已不是前缀
			s.conv.ForgetAccount(c.thread, id)
		}
		convID = s.conv.ID(c.thread, id)
	}
	opts := kiro.BuildOptions{Model: c.model, ProfileArn: cred.ProfileArn, ConversationID: convID, ThinkingBudget: c.budget}
	payload, names, err := kiro.Build(&c.req, opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "build request: "+err.Error())
		return outcome{done: true}
	}

	res, err := s.pool.Client().Generate(ctx, cred, payload)
	if err == nil && res.StatusCode == http.StatusForbidden {
		// token 被拒：刷新后在同一号上再试一次
		drain(res)
		if cred, err = s.pool.Cred(ctx, id, cred.AccessToken); err == nil {
			if cred.ProfileArn != opts.ProfileArn {
				opts.ProfileArn = cred.ProfileArn
				payload, _, _ = kiro.Build(&c.req, opts)
			}
			res, err = s.pool.Client().Generate(ctx, cred, payload)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return outcome{done: true}
		}
		f := kiro.Failure{Status: http.StatusBadGateway, Message: err.Error(), Class: kiro.ClassTransient}
		s.pool.Fail(id, f)
		return outcome{failure: &f}
	}
	defer drain(res)

	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		f := kiro.Classify(res.StatusCode, body)
		if !s.pool.Fail(id, f) {
			writeError(w, f.Status, f.Message)
			return outcome{done: true}
		}
		return outcome{failure: &f}
	}

	dec := kiro.NewDecoder(res.Body, c.budget > 0, s.models.window(c.model), names)
	first, err := dec.Next()
	if err == nil && first.Kind == kiro.EvError {
		f := kiro.ClassifyStream(first.Text)
		if !s.pool.Fail(id, f) {
			writeError(w, f.Status, f.Message)
			return outcome{done: true}
		}
		return outcome{failure: &f}
	}

	msgID := "msg_" + strings.ReplaceAll(kiro.NewUUID(), "-", "")[:24]
	var out reply
	if c.req.Stream {
		// 流式：从这里开始向下游输出，不再重试
		s.commit(c, id)
		w.Header().Set("X-Kiro-Account", id)
		out = streamSSE(ctx, w, dec, first, msgID, c.req.Model)
	} else {
		// 非流式：整条收完才写下游，中途失败仍可换号
		var msg anthropic.Response
		msg, out = collectMessage(dec, first, msgID, c.req.Model)
		if out.failure != nil {
			if ctx.Err() != nil {
				return outcome{done: true} // 下游走了，读失败不是号的错
			}
			f := *out.failure
			if s.pool.Fail(id, f) {
				s.log.Warn("reply broke off, failing over", "account", id, "msg", f.Message)
				return outcome{failure: &f}
			}
			writeError(w, f.Status, f.Message)
			return outcome{done: true}
		}
		s.commit(c, id)
		w.Header().Set("X-Kiro-Account", id)
		writeJSON(w, http.StatusOK, msg)
	}
	switch {
	case out.ok:
		s.pool.Success(id, out.usage)
	case out.failure != nil:
		// 流式中途失败：下游已收到 error 事件，这里记到号上（限流冷却 / 统计）
		s.pool.Fail(id, *out.failure)
	}
	usage, ok := out.usage, out.ok
	s.log.Info("messages",
		"account", id, "lease_pinned", lease.Pinned, "thread", shortKey(c.thread), "conv", shortKey(convID),
		"model", c.model, "stream", c.req.Stream, "thinking", c.budget,
		"input", usage.Input, "output", usage.Output, "cache_read", usage.CacheRead, "cache_write", usage.CacheWrite,
		"ok", ok, "ms", time.Since(c.start).Milliseconds())
	return outcome{done: true}
}

// commit 在确定由该号答复后记住：会话钉到这个号，并记下该号上的消息指纹。
func (s *Server) commit(c *call, id string) {
	if c.thread == "" {
		return
	}
	s.pool.Pin(c.thread, id)
	s.lines.set(lineKey(c.thread, id), c.lineage)
}

func lineKey(thread, account string) string { return thread + "\x00" + account }

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	res.Body.Close()
}

func shortKey(k string) string {
	if k == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:4])
}

// countTokens 粗估 input tokens（4 字符≈ 1 token）。Kiro 没有对应接口。
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req anthropic.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var chars int
	chars += len(req.System.Text("\n"))
	for _, m := range req.Messages {
		for _, b := range m.Content {
			chars += len(b.Text) + len(b.Input) + len(b.Content.Text("\n")) + len(b.Thinking)
		}
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": (chars + 3) / 4})
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if len(s.models.list()) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		s.refreshCatalog(ctx)
		cancel()
	}
	type model struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
		Context     int    `json:"context_window,omitzero"`
		MaxOutput   int    `json:"max_output_tokens,omitzero"`
	}
	data := []model{}
	for _, m := range s.models.list() {
		data = append(data, model{Type: "model", ID: m.ID, DisplayName: m.Name, CreatedAt: "2025-01-01T00:00:00Z", Context: m.Context, MaxOutput: m.Output})
	}
	if len(data) == 0 {
		// 不编造模型：客户端会把它当真列出来，然后请求必然失败
		writeError(w, http.StatusServiceUnavailable, "no model list: the pool is empty or no account could list models; add or fix an account (see /admin/accounts)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": false, "first_id": data[0].ID, "last_id": data[len(data)-1].ID})
}
