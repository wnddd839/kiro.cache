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

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/normalize"
	"kiro-proxy/internal/openai"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/sessionpin"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

// proto 是下游协议。三者共用同一条分发管线，只在解析请求与写回复时不同。
type proto string

const (
	protoAnthropic proto = "anthropic"
	protoChat      proto = "openai-chat"
	protoResponses proto = "openai-responses"
)

func (p proto) writeError(w http.ResponseWriter, status int, msg string) {
	if p == protoAnthropic {
		writeError(w, status, msg)
		return
	}
	openai.WriteError(w, status, msg)
}

func (p proto) newID() string {
	switch p {
	case protoChat:
		return openai.NewID("chatcmpl-")
	case protoResponses:
		return openai.NewID("resp_")
	}
	return openai.NewID("msg_")
}

// thinkingTagTokens 是 thinking 开启时加在 system 前的标签的 token 数。
const thinkingTagTokens = 24

// call 是一次下游请求在分发层里的状态。
type call struct {
	proto   proto
	req     anthropic.Request
	chat    openai.ChatOptions
	resp    openai.ResponsesOptions
	model   string // Kiro 模型 id
	thread  string // 会话线程键：粘号、钉 conversationId、钉 thinking
	budget  int
	lineage []uint64
	prompt  *meter.Prompt
	schema  *kiro.ModelRequestSchema
	// overflow 是本次尝试只是临时借用（钉的号并发打满）：答复后不改钉
	overflow bool
	key      *keys.Key // nil 是匿名（未配 key 或命中 config.api_keys）
	start    time.Time
	entry    usage.Entry // 最终一次尝试的记账（含请求状态）
	recorded bool        // 成功时在写出结尾前已记账
	// spent 是最终尝试未完成时已消耗的量（中断 / 中途报错）：上游已经扣了 credits，账本要记上。
	// 换号重试了的尝试不在这里，它们已各自记了一行（Retried）。
	spent   turn.Usage
	aborted bool
	hold    *hold // 准入时为 key 预留的额度，记账时释放
}

// generate 是三个生成接口的公共入口。
func (s *Server) generate(p proto) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, k *keys.Key) {
		c, err := s.prepare(r, p, k)
		if err != nil {
			p.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if status, err := s.admit(w, c); err != nil {
			s.log.Info("request refused", "key", k.Name, "model", c.model, "err", err)
			p.writeError(w, status, err.Error())
			return
		}
		defer s.holds.release(c.hold)
		s.ensureCatalog()
		s.dispatch(w, r, c)
	}
}

// prepare 解析并整理请求，算出会话线程、thinking 预算与本地计量用的前缀形状。
// k 是下游 key（nil 为匿名）：会话线程按 key 隔离。
func (s *Server) prepare(r *http.Request, p proto, k *keys.Key) (*call, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	c := &call{proto: p, start: time.Now(), key: k}
	switch p {
	case protoChat:
		c.req, c.chat, err = openai.FromChat(raw, s.cfg.OpenAIHostedTools)
		if len(c.chat.Dropped) > 0 {
			s.log.Debug("dropped hosted OpenAI tools", "proto", p, "items", c.chat.Dropped)
		}
	case protoResponses:
		c.req, c.resp, err = openai.FromResponses(raw, s.cfg.OpenAIHostedTools)
		if len(c.resp.Dropped) > 0 {
			s.log.Debug("dropped hosted OpenAI tools", "proto", p, "items", c.resp.Dropped)
		}
	default:
		if err = json.Unmarshal(raw, &c.req); err != nil {
			err = fmt.Errorf("a request that isn't Messages JSON: %w", err)
		} else if len(c.req.Messages) == 0 {
			err = errors.New("messages: at least one message is required")
		}
	}
	if err != nil {
		return nil, err
	}
	normalize.Request(&c.req, s.norm)
	for i := range c.req.Tools {
		t := &c.req.Tools[i]
		t.Description = kiro.ToolDescription(t.Description, t.Name)
	}
	s.loadCatalog(r.Context())
	c.model = kiroModel(c.req.Model, s.cfg.ModelAliases, s.models.known)
	c.schema = s.models.model(c.model).RequestSchema
	fields, err := kiro.ThinkingFields(&c.req, c.schema)
	if err != nil {
		return nil, err
	}
	c.thread = threadKey(r, &c.req, k)
	c.lineage = lineage(&c.req)

	c.budget = kiro.ThinkingBudget(&c.req, c.model)
	if c.schema == nil && s.cfg.PinThinking && c.thread != "" {
		// 同一线程的 thinking 以首次为准：它写在首条消息前面，一变整段前缀就变
		if pinned, ok := s.think.get(c.thread); ok {
			c.budget = pinned
		}
		s.think.set(c.thread, c.budget)
	}

	extra := 0
	// 原生字段不改 prompt，但实测切 effort 仍会变冷，缓存按生效参数隔离。
	native, _ := json.Marshal(fields)
	seed := c.model + "|native|" + string(native)
	if c.schema == nil {
		seed = c.model + "|" + strconv.Itoa(c.budget)
		if c.budget > 0 {
			extra = thinkingTagTokens
		}
	}
	// 前缀指纹包含模型原生参数；旧模型另外计入 prompt 标签的开销。
	// 上游声明不支持 prompt caching 的模型不模拟命中。
	c.prompt = meter.Analyze(&c.req, seed, extra, s.models.cacheMode(c.model, s.cacheMode(p)))
	if s.cfg.CacheTTL == config.CacheTTL5m {
		c.prompt.CapTTL(5 * time.Minute)
	}
	for _, w := range c.prompt.Warnings {
		s.log.Warn("cache_control not as Anthropic accepts; billed the cheaper way", "model", c.model, "why", w)
	}
	// 对下游计费的最小可缓存长度用 Anthropic 官方分模型表（下游按它复核）。
	// Kiro 的 minimumTokensPerCacheCheckpoint 不参与收费（管理台模型列表里展示）。
	c.prompt.MinTokens = meter.AnthropicMinCacheable(c.model)
	return c, nil
}

// cacheMode 是一个下游协议生效的本地缓存计量方式。
func (s *Server) cacheMode(p proto) string {
	if s.cfg.CacheMode != meter.ModeProtocol {
		return s.cfg.CacheMode
	}
	if p == protoAnthropic {
		return meter.ModeExplicit
	}
	return meter.ModeAuto
}

// admit 做 key 的准入判断：模型白名单、周期预算、每分钟请求数。
// 已用量含同一 key 上在途请求的预留；通过后为本请求预留一份估计额度，记账时释放。
// 并行的 subagent 同时发请求时不会全部通过后一起超支。
func (s *Server) admit(w http.ResponseWriter, c *call) (int, error) {
	k := c.key
	if k == nil {
		return 0, nil
	}
	if !k.Allows(c.req.Model, c.model) {
		return http.StatusForbidden, fmt.Errorf("%w: %s", keys.ErrModel, c.req.Model)
	}
	now := time.Now()
	start := k.PeriodStart(now, time.Local)

	// 同一把锁内读已用 + 预留、判断、预留：并发准入串行，彼此看得见对方的预留
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	t := s.usage.Spend(k.ID, start)
	// 换号重试的失败尝试不扣预算：请求数与 CostUSD 已不含它们，credits 单独减掉
	spent := keys.Spent{Requests: t.Requests, Credits: t.BillableCredits(), CostUSD: t.CostUSD}
	r := s.holds.byKey[k.ID]
	spent.Requests += r.Requests
	spent.Credits += r.Credits
	spent.CostUSD += r.CostUSD
	err := k.Check(now, spent, time.Local)
	if err == nil {
		err = s.keys.Allow(k)
	}
	if err != nil {
		return admitStatus(w, err), err
	}
	c.hold = s.holds.addLocked(k.ID, s.estimate(c, t))
	return 0, nil
}

// reserveOutput 是预留时对输出 token 的估计上限。
const reserveOutput = 4096

// estimate 是一次请求在准入时预留的量：1 次请求；费用按整段输入未命中缓存、输出取 max_tokens
// （不超过 reserveOutput）估，并且不低于本周期的单次均值；credits 按本周期单次均值估。
func (s *Server) estimate(c *call, period usage.Totals) keys.Spent {
	out := reserveOutput
	if c.req.MaxTokens > 0 {
		out = min(out, c.req.MaxTokens)
	}
	u := turn.Usage{Input: c.prompt.Tokens, Output: out}
	if period.Requests > 0 {
		u.Credits = period.Credits / float64(period.Requests)
	}
	cost := s.pricer.Cost(c.model, u)
	if period.Requests > 0 {
		cost = max(cost, period.CostUSD/float64(period.Requests))
	}
	return keys.Spent{Requests: 1, Credits: u.Credits, CostUSD: cost}
}

func admitStatus(w http.ResponseWriter, err error) int {
	if err == nil {
		return 0
	}
	if le, ok := errors.AsType[*keys.LimitError](err); ok {
		if le.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(le.RetryAfter.Seconds()))))
		}
		if le.What == "rpm" {
			return http.StatusTooManyRequests
		}
		// 预算用完：402 + x-should-retry: false 让客户端别自动重试（429 会被当成短暂限流；
		// Anthropic / OpenAI SDK 认这个头）
		w.Header().Set("X-Should-Retry", "false")
		return http.StatusPaymentRequired
	}
	if errors.Is(err, keys.ErrModel) {
		return http.StatusForbidden
	}
	return http.StatusUnauthorized
}

// threadKey 是会话键加首条前缀。客户端给的 session 在主线程与子 agent / 标题生成间共享，
// 它们的 system 与首条 user 不同，必须是不同的上游 conversation。
// 按下游 key 隔离：不同成员发同样的短提示（如标题生成）或同名 session id 不会撞进同一个会话。
func threadKey(r *http.Request, req *anthropic.Request, k *keys.Key) string {
	msgs := normalize.PinMessages(req)
	clientKey := cmp.Or(req.PromptCacheKey, r.Header.Get("X-Claude-Code-Session-Id"), normalize.MetadataSession(req))
	base := sessionpin.Key(r.Header, clientKey, msgs)
	prefix := sessionpin.Key(nil, "", msgs)
	if strings.HasPrefix(base, "hdr:") && prefix != "" {
		base += "|" + prefix
	}
	if base != "" && k != nil {
		base = "key:" + k.ID + "|" + base
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
	c.entry = usage.Entry{Time: c.start, Model: c.model, Protocol: string(c.proto), Stream: c.req.Stream, Thread: shortKey(c.thread),
		Request: c.proto.newID()}
	if c.key != nil {
		c.entry.Key, c.entry.KeyName = c.key.ID, c.key.Name
	}
	defer func() {
		if !c.recorded {
			// 没有成功回复：记上最后一次尝试已消耗的量（中断 / 中途失败）
			s.fillUsage(&c.entry, c.spent)
			c.entry.Aborted = c.aborted
			c.entry.MS = time.Since(c.start).Milliseconds()
			s.usage.Record(c.entry)
		}
	}()
	fail := func(status int, msg string) {
		c.entry.Status, c.entry.Error = status, msg
		c.proto.writeError(w, status, msg)
	}

	var tried []string
	var last *kiro.Failure
	for range s.cfg.MaxAttempts {
		lease, err := s.pool.AcquireContext(ctx, c.thread, tried)
		if err != nil {
			if last != nil {
				fail(last.Status, last.Message)
				return
			}
			status := http.StatusServiceUnavailable
			if ue, ok := errors.AsType[*pool.UnavailableError](err); ok && !ue.RetryAt.IsZero() {
				status = http.StatusTooManyRequests
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(time.Until(ue.RetryAt).Seconds()))))
			}
			s.reqlog.add(requestRecord{Time: time.Now(), Protocol: string(c.proto), Key: c.entry.KeyName, Model: c.model,
				Stream: c.req.Stream, Thinking: c.budget, Thread: shortKey(c.thread), Status: status, Error: err.Error()})
			fail(status, err.Error())
			return
		}
		c.entry.Attempts++
		c.overflow = lease.Overflow
		c.entry.Account, c.entry.Pinned = lease.ID, lease.Pinned
		out := s.attempt(ctx, w, lease, c)
		lease.Release()
		if out.failure != nil && out.failure.Class == kiro.ClassQuota {
			s.checkQuota(lease.ID)
		}
		if out.done || ctx.Err() != nil {
			if ctx.Err() != nil && c.entry.Status == 0 {
				c.entry.Status, c.entry.Error = statusClientClosed, "client went away"
			}
			return
		}
		tried = append(tried, lease.ID)
		last = out.failure
	}
	if last == nil {
		last = &kiro.Failure{Status: http.StatusBadGateway, Message: "no attempt succeeded"}
	}
	fail(last.Status, last.Message)
}

// attempt 在一个号上试一次。在向下游写出任何字节之前失败都可以换号重试。
func (s *Server) attempt(ctx context.Context, w http.ResponseWriter, lease *pool.Lease, c *call) outcome {
	id := lease.ID
	rec := requestRecord{Time: time.Now(), Protocol: string(c.proto), Key: c.entry.KeyName, Account: id, Model: c.model,
		Stream: c.req.Stream, Thinking: c.budget, Pinned: lease.Pinned, Thread: shortKey(c.thread)}
	defer func() { rec.MS = time.Since(rec.Time).Milliseconds(); s.reqlog.add(rec) }()
	note := func(f kiro.Failure) {
		rec.Status, rec.Error = f.Status, f.Message
		c.entry.Status, c.entry.Error = f.Status, f.Message
	}
	gone := func() {
		rec.Status, rec.Error = statusClientClosed, "client went away"
		c.entry.Status, c.entry.Error = statusClientClosed, "client went away"
	}
	terminal := func(f kiro.Failure) outcome {
		note(f)
		c.proto.writeError(w, f.Status, f.Message)
		return outcome{done: true}
	}

	cred, err := s.pool.Cred(ctx, id, "")
	if err != nil {
		f := kiro.Failure{Status: http.StatusBadGateway, Message: "credentials: " + err.Error(), Class: kiro.ClassAuth}
		note(f)
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
	rec.Conv = shortKey(convID)
	points, _ := s.cfg.KiroCachePoints() // 已在加载配置时校验
	opts := kiro.BuildOptions{Model: c.model, ProfileArn: cred.ProfileArn, ConversationID: convID, ThinkingBudget: c.budget, RequestSchema: c.schema, CachePoints: points}
	payload, names, err := kiro.Build(&c.req, opts)
	if err != nil {
		return terminal(kiro.Failure{Status: http.StatusBadRequest, Message: "build request: " + err.Error()})
	}

	res, err := s.pool.Client().Generate(ctx, cred, payload)
	refreshErr := false
	if err == nil && res.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if kiro.Banned(res.StatusCode, body) {
			// 封号 / 暂停：刷新 token 救不回来，刷了反而会一直轮回。停用并换号
			drain(res)
			f := kiro.Classify(res.StatusCode, body)
			note(f)
			s.pool.Fail(id, f)
			return outcome{failure: &f}
		}
		if !kiro.TokenRejected(res.StatusCode, body) {
			// 不是明确的 token 失效：刷新也没用。计一次失败并换号
			drain(res)
			f := kiro.Classify(res.StatusCode, body)
			note(f)
			if !s.pool.Fail(id, f) {
				return terminal(f)
			}
			return outcome{failure: &f}
		}
		// token 被拒：强制刷新后在同一号上再试一次
		drain(res)
		if cred, err = s.pool.Cred(ctx, id, cred.AccessToken); err != nil {
			refreshErr = true
		} else {
			if cred.ProfileArn != opts.ProfileArn {
				opts.ProfileArn = cred.ProfileArn
				payload, _, _ = kiro.Build(&c.req, opts)
			}
			res, err = s.pool.Client().Generate(ctx, cred, payload)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			gone()
			return outcome{done: true}
		}
		// 刷新失败已由号池记过（含全局熔断），这里只把 Generate 自己的传输错误算作网络失败
		f := kiro.Failure{Status: http.StatusBadGateway, Message: err.Error(), Class: kiro.ClassTransient, Network: !refreshErr}
		note(f)
		s.pool.Fail(id, f)
		return outcome{failure: &f}
	}
	defer drain(res)

	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		f := kiro.Classify(res.StatusCode, body)
		f.RetryAfter = kiro.RetryAfter(res.Header)
		if !s.pool.Fail(id, f) {
			return terminal(f)
		}
		note(f)
		return outcome{failure: &f}
	}

	dec := kiro.NewDecoder(res.Body, c.budget > 0, s.models.window(c.model), names)
	dec.Mode = s.cfg.ReportedUsageMode()
	first, err := dec.Next()

	// 输入侧按本地 prompt cache 模拟拆分；缓存跟号走，与上游一致
	split := s.cache.Peek(id, c.prompt)
	input := turn.Usage{Input: split.Input, CacheRead: split.CacheRead, CacheWrite: split.CacheWrite, CacheWrite1h: split.CacheWrite1h}
	po := pumpOpts{hidden: kiro.HiddenTokens(c.model), mode: s.cfg.ReportedUsageMode(),
		reportsSeen: s.reportsSeen.Load() && s.cfg.ReportedUsageMode() != kiro.ReportedIgnore}
	if err == nil && first.Kind == kiro.EvError {
		// 首条内容前也可能已报 credits / tokenUsage，只按已报用量记成本。
		k := dec.Partial()
		u := reportedUsage(turn.Usage{}, k, po)
		u.CacheWrite1h = meter.Scale1h(input.CacheWrite1h, input.CacheWrite, u.CacheWrite)
		s.noteReported(c, id, input, k)
		s.charge(id, c, u)
		spent := u.PromptTokens() > 0 || u.Output > 0 || u.Credits > 0
		rec.Input, rec.Output, rec.CacheRead, rec.CacheWrite = u.Input, u.Output, u.CacheRead, u.CacheWrite
		rec.Credits, rec.CostUSD = u.Credits, s.pricer.Cost(c.model, u)
		f := kiro.ClassifyStream(first.Text)
		if !s.pool.Fail(id, f) {
			c.spent, c.aborted = u, spent
			return terminal(f)
		}
		note(f)
		if spent {
			s.retried(c, id, f, u, time.Since(rec.Time))
		}
		return outcome{failure: &f}
	}
	ts := &timingSink{start: c.start}
	settle := func(u turn.Usage, k kiro.Usage) turn.Usage {
		s.watchHidden(c.model, u, k) // 用校准前的本地计量反推 Kiro 隐藏 token
		s.noteReported(c, id, input, k)
		if !k.Reported && k.Input > 0 {
			// 上游的上下文占用是真实 token（与各家分词器逐 token 一致），用它校正本地估算；1h 占比不变
			u = meter.Calibrate(u, k.Input, kiro.HiddenTokens(c.model))
		}
		u.CostUSD = s.pricer.Cost(c.model, u)
		// 先记账再写结尾：下一个请求的预算判断要看到这一笔
		c.entry.Status, c.entry.Error = http.StatusOK, ""
		s.fillUsage(&c.entry, u)
		c.entry.Context = k.Input
		c.entry.MS = time.Since(c.start).Milliseconds()
		c.entry.FirstMS = ts.firstMS()
		s.usage.Record(c.entry)
		c.recorded = true
		s.holds.release(c.hold) // 已入账：预留立刻释放，下一个请求不重复计
		return u
	}
	respID := c.proto.newID()

	var out reply
	if c.req.Stream {
		// 流式：从这里开始向下游输出，不再重试
		s.commit(c, id)
		w.Header().Set("X-Kiro-Account", id)
		ts.Sink = s.streamSink(w, c, respID)
		out = pump(ctx, dec, first, ts, input, settle, po)
	} else {
		// 非流式：整条收完才写下游，中途失败仍可换号
		col := &turn.Collector{}
		ts.Sink = col
		out = pump(ctx, dec, first, ts, input, settle, po)
		if !out.ok {
			// 上游已开始回复：输入已处理、credits 已扣，换号重试或下游走了也要入账
			s.charge(id, c, out.usage)
		}
		switch {
		case out.failure != nil && ctx.Err() == nil:
			f := *out.failure
			if s.pool.Fail(id, f) {
				note(f)
				s.log.Warn("reply broke off, failing over", "account", id, "msg", f.Message)
				s.retried(c, id, f, out.usage, time.Since(rec.Time))
				return outcome{failure: &f}
			}
			c.spent, c.aborted = out.usage, true
			return terminal(f)
		case !out.ok:
			gone() // 下游走了，读失败不是号的错
			c.spent, c.aborted = out.usage, true
			return outcome{done: true}
		}
		s.commit(c, id)
		col.Turn.ID, col.Turn.Model = respID, c.req.Model
		w.Header().Set("X-Kiro-Account", id)
		s.writeTurn(w, c, &col.Turn)
	}

	if c.req.Stream && !out.ok {
		s.charge(id, c, out.usage)
		c.spent, c.aborted = out.usage, true
	}
	u := out.usage
	if !out.ok {
		u = s.priced(c.model, u)
	}
	rec.Input, rec.Output, rec.CacheRead, rec.CacheWrite = u.Input, u.Output, u.CacheRead, u.CacheWrite
	rec.Credits, rec.CostUSD, rec.FirstMS = u.Credits, u.CostUSD, ts.firstMS()
	switch {
	case out.ok:
		s.cache.Commit(id, c.prompt)
		rec.Status, rec.OK = http.StatusOK, true
		su, est := s.estimateCredits(c.model, u)
		s.pool.Success(id, kiro.Usage{Input: su.Input, Output: su.Output, CacheRead: su.CacheRead, CacheWrite: su.CacheWrite, Credits: su.Credits}, u.CostUSD, est)
	case out.failure != nil:
		// 流式中途失败：下游已收到 error 事件，这里记到号上（限流冷却 / 统计）
		note(*out.failure)
		s.pool.Fail(id, *out.failure)
		if out.failure.Class == kiro.ClassQuota {
			s.checkQuota(id)
		}
	default:
		gone()
	}
	s.log.Info("generate",
		"proto", c.proto, "key", c.entry.KeyName, "account", id, "lease_pinned", lease.Pinned,
		"thread", shortKey(c.thread), "conv", shortKey(convID), "model", c.model, "stream", c.req.Stream, "thinking", c.budget,
		"input", u.Input, "output", u.Output, "cache_read", u.CacheRead, "cache_write", u.CacheWrite,
		"credits", u.Credits, "ok", out.ok, "ms", time.Since(c.start).Milliseconds(), "first_ms", rec.FirstMS)
	return outcome{done: true}
}

// charge 把一次没有成功的尝试已消耗的量记到号的统计上。有输入用量才写入 prompt cache；
// 首条内容前只报 credits 不能证明整段前缀已处理。账本由调用方记（retried 或最终一行）。
// credits 与账本行用同一套估算（estimateCredits），号上的统计与账本一致。
func (s *Server) charge(id string, c *call, u turn.Usage) {
	if u.PromptTokens() == 0 && u.Output == 0 && u.Credits == 0 {
		return
	}
	u, est := s.estimateCredits(c.model, u)
	u = s.priced(c.model, u)
	if u.PromptTokens() > 0 {
		s.cache.Commit(id, c.prompt)
	}
	s.pool.Charge(id, kiro.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Credits: u.Credits}, u.CostUSD, est)
}

// estimateCredits 在上游没报 credits（中断时常见）而用量非零时按 token 估一个，返回是否估算。
// 中断、失败、成功三条路径都用它。
func (s *Server) estimateCredits(model string, u turn.Usage) (turn.Usage, bool) {
	if u.Credits != 0 || (u.PromptTokens() == 0 && u.Output == 0) {
		return u, false
	}
	u.Credits = meter.EstimateCredits(meter.CreditRateOf(model, s.cfg.CreditRates), u, kiro.HiddenTokens(model))
	return u, u.Credits > 0
}

// retried 给一次换号重试了的尝试单独记一行。失败尝试不向下游收费：CostUSD = 0，
// 只记上游成本（全额是重试亏损）。不计请求数、不扣 key 预算（预算按 CostUSD 算）。
// 不标 Aborted：否则中断统计会把重试重复算进去。
func (s *Server) retried(c *call, id string, f kiro.Failure, u turn.Usage, took time.Duration) {
	e := c.entry
	e.ID = ""
	e.Time = time.Now().Add(-took)
	e.Account, e.Retried, e.Aborted = id, true, false
	e.Status, e.Error = f.Status, f.Message
	e.FirstMS, e.Context = 0, 0
	s.fillUsage(&e, u)
	e.CostUSD = 0
	e.MS = took.Milliseconds()
	s.usage.Record(e)
}

// priced 补上费用；上游没来得及报 credits（中断时常见）时按 token 估一个。
func (s *Server) priced(model string, u turn.Usage) turn.Usage {
	u.CostUSD = s.pricer.Cost(model, u)
	return u
}

// fillUsage 把一次尝试的用量写进账本行，算两本账：对下游收费（Anthropic 口径）与上游成本。
// 上游没报 credits 而用量非零时按 token 估 credits，并标 CreditsEstimated。
func (s *Server) fillUsage(e *usage.Entry, u turn.Usage) {
	u, e.CreditsEstimated = s.estimateCredits(e.Model, u)
	u = s.priced(e.Model, u)
	e.Input, e.CacheRead, e.CacheWrite, e.CacheWrite1h = u.Input, u.CacheRead, u.CacheWrite, u.CacheWrite1h
	e.Output, e.Reasoning = u.Output, u.Reasoning
	e.Credits, e.CostUSD = u.Credits, u.CostUSD
	e.UpstreamUSD = s.pricer.CreditsUSD(u.Credits)
}

func (s *Server) streamSink(w http.ResponseWriter, c *call, id string) turn.Sink {
	switch c.proto {
	case protoChat:
		return openai.NewChatStream(w, id, c.req.Model, c.start.Unix(), c.chat.IncludeUsage)
	case protoResponses:
		return openai.NewResponsesStream(w, id, c.req.Model, c.start.Unix(), c.resp.Tools)
	}
	return newAnthropicStream(w, id, c.req.Model)
}

func (s *Server) writeTurn(w http.ResponseWriter, c *call, t *turn.Turn) {
	switch c.proto {
	case protoChat:
		writeJSON(w, http.StatusOK, openai.ChatCompletion(t, c.start.Unix()))
	case protoResponses:
		writeJSON(w, http.StatusOK, openai.Response(t, c.start.Unix(), c.resp.Tools))
	default:
		writeJSON(w, http.StatusOK, anthropicMessage(t))
	}
}

// commit 在确定由该号答复后记住：会话钉到这个号，并记下该号上的消息指纹。
// 临时借用的号（原号只是并发打满）只记指纹不改钉：会话下一轮回到原号，那里的上游缓存还在。
func (s *Server) commit(c *call, id string) {
	if c.thread == "" {
		return
	}
	if !c.overflow {
		s.pool.Pin(c.thread, id)
	}
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

// countTokens 按本地计量规则估 input tokens（与计费一致）。Kiro 没有对应接口。
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request, _ *keys.Key) {
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
	normalize.Request(&req, s.norm)
	for i := range req.Tools {
		t := &req.Tools[i]
		t.Description = kiro.ToolDescription(t.Description, t.Name)
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": meter.Analyze(&req, "", 0, meter.ModeOff).Tokens})
}

// listModels 同时是 Anthropic 与 OpenAI 的模型列表形状。key 限定了模型时只列允许的。
func (s *Server) listModels(w http.ResponseWriter, r *http.Request, k *keys.Key) {
	if len(s.models.list()) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		s.refreshCatalog(ctx)
		cancel()
	}
	type model struct {
		Type        string `json:"type"`
		Object      string `json:"object"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		Context     int    `json:"context_window,omitzero"`
		MaxOutput   int    `json:"max_output_tokens,omitzero"`
	}
	data := []model{}
	for _, m := range s.models.list() {
		if k != nil && !k.Allows(m.ID) {
			continue
		}
		data = append(data, model{Type: "model", Object: "model", ID: m.ID, DisplayName: m.Name,
			CreatedAt: "2025-01-01T00:00:00Z", Created: 1735689600, OwnedBy: "kiro", Context: m.Context, MaxOutput: m.Output})
	}
	if len(data) == 0 {
		// 不编造模型：客户端会把它当真列出来，然后请求必然失败
		writeError(w, http.StatusServiceUnavailable, "no model list: the pool is empty or no account could list models; add or fix an account (see /admin/accounts)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false, "first_id": data[0].ID, "last_id": data[len(data)-1].ID})
}
