// Package probe 测 Kiro 上游的真实缓存与计费行为，用来校准本地计量（credit 汇率、auto TTL、cachePoint 开关）。
//
// 只用一个号，直接打 generateAssistantResponse（不经过分发层），每次调用记一行 JSONL：
// 发了什么（前缀长度、conversationId 是否复用、cachePoint 位置、间隔）、上游回了什么（tokenUsage 原样、
// totalTokens、normalizedTokenUsage、meteringEvent、上下文百分比、事件序列）。
//
// 实验：
//
//	usage    第 0 步：上游到底报不报 tokenUsage、报几次、是不是累计值
//	ttl      第 1 步：缓存能活多久；每个间隔一份新前缀（防止中途命中续期），同 / 新 conversationId 各一组
//	cachepoint 第 2 步：显式 cachePoint（首条 user / assistant / tools 末尾）开与不开对比
//	credits  第 3 步：5k / 20k / 50k 前缀冷热各一次（只回 ok），再一组长输出；拟合每 token credits，隐藏 ~4k 为截距
//
// 每个实验前后各拉一次 Get-Usage-Limits（第 4 步），核对 meteringEvent 之和与账户实际扣的差。
package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/kiro"
)

// Target 是被测的号与模型。
type Target struct {
	Client *kiro.Client
	Cred   func(ctx context.Context) (kiro.Cred, error) // 每次调用前取（可能刷新）
	Model  string
	Window int // 模型上下文长度，用于从百分比换算 token；0 = 200000
}

// Record 是一次上游调用的结果。字段名稳定：分析脚本按它读。
type Record struct {
	Experiment string    `json:"experiment"`
	Case       string    `json:"case"`
	Step       string    `json:"step"` // write | read | cold | warm | long | limits-before | limits-after
	Time       time.Time `json:"time"`
	Model      string    `json:"model,omitzero"`

	PrefixTokens   int           `json:"prefix_tokens,omitzero"` // 目标前缀长度（本地估算）
	Gap            time.Duration `json:"gap_ns,omitzero"`        // 距该前缀写入的间隔
	GapText        string        `json:"gap,omitzero"`
	SameConv       bool          `json:"same_conversation,omitzero"`
	ConversationID string        `json:"conversation_id,omitzero"`
	CachePoints    string        `json:"cache_points,omitzero"`
	Continuation   bool          `json:"agent_continuation,omitzero"`
	MaxOutput      string        `json:"output_mode,omitzero"` // ok | long

	Status       int               `json:"status,omitzero"`
	Error        string            `json:"error,omitzero"`
	MS           int64             `json:"ms,omitzero"`
	FirstMS      int64             `json:"first_ms,omitzero"`
	OutputChars  int               `json:"output_chars,omitzero"`
	TokenUsage   json.RawMessage   `json:"token_usage,omitzero"`     // 最后一次 tokenUsage 原样
	TokenUsages  int               `json:"token_usage_events"`       // tokenUsage 出现次数
	AllUsage     []json.RawMessage `json:"token_usage_all,omitzero"` // 每一次（判断增量 / 累计）
	Credits      float64           `json:"credits"`
	Metering     int               `json:"metering_events"`
	MeteringUnit string            `json:"metering_unit,omitzero"`
	ContextPct   float64           `json:"context_pct,omitzero"`
	ContextTok   int               `json:"context_tokens,omitzero"` // 百分比 × 窗口
	Events       []string          `json:"events,omitzero"`         // 事件类型序列（去重相邻）
	// Raw 是非内容事件（metadata / metering / contextUsage 等）的原始 payload，按顺序
	Raw []RawEvent `json:"raw_events,omitzero"`
	// LimitsRaw 是 Get-Usage-Limits 的原始响应（去掉邮箱）
	LimitsRaw json.RawMessage `json:"limits_raw,omitzero"`

	LimitsUsed    float64 `json:"limits_used,omitzero"`
	LimitsOverage float64 `json:"limits_overage,omitzero"`
}

// RawEvent 是一帧非内容事件。
type RawEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Runner 跑实验并写结果。
type Runner struct {
	T    Target
	Out  io.Writer // JSONL
	Log  func(format string, args ...any)
	Salt string // 前缀里的随机串：每次运行都是新前缀，不受上次残留缓存影响

	mu sync.Mutex
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Runner) write(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Gap > 0 {
		rec.GapText = rec.Gap.String()
	}
	b, _ := json.Marshal(rec)
	_, _ = r.Out.Write(append(b, '\n'))
}

// Prefix 生成约 tokens 个 token 的唯一前缀（英文散文，约 4 字符 / token）。tag 让不同用例互不命中。
func Prefix(tag string, tokens int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reference document %s. Read it; later questions refer to it.\n\n", tag)
	for i := 0; b.Len() < tokens*4; i++ {
		fmt.Fprintf(&b, "Section %d (%s). The quick brown fox jumps over the lazy dog while the committee reviews clause %d of the agreement. ", i, tag, i*7+3)
	}
	return b.String()
}

func salt() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// call 是一次调用的输入。
type call struct {
	prefix       string
	question     string
	history      []anthropic.Message // 在 prefix 之后、question 之前
	convID       string
	points       kiro.CachePoints
	long         bool // 要长输出
	continuation bool // 发 agentContinuationId
	tools        bool // 带一个工具（测 tools 末尾 cachePoint）
}

const okOnly = "Reply with exactly: ok"

func (c call) request(model string) *anthropic.Request {
	q := c.question
	if q == "" {
		q = okOnly
		if c.long {
			q = "Write 600 words summarizing the reference document. No preamble."
		}
	}
	req := &anthropic.Request{Model: model, MaxTokens: 1024,
		System: anthropic.Content{{Type: "text", Text: c.prefix}}}
	req.Messages = append(req.Messages, anthropic.Message{Role: "user", Content: anthropic.Content{{Type: "text", Text: "Acknowledge the document."}}})
	req.Messages = append(req.Messages, anthropic.Message{Role: "assistant", Content: anthropic.Content{{Type: "text", Text: "Acknowledged."}}})
	req.Messages = append(req.Messages, c.history...)
	req.Messages = append(req.Messages, anthropic.Message{Role: "user", Content: anthropic.Content{{Type: "text", Text: q}}})
	if c.tools {
		req.Tools = []anthropic.Tool{{Name: "lookup", Description: "Look up a clause in the reference document.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"clause":{"type":"integer"}}}`)}}
	}
	return req
}

// do 发一次并解码。不重试：探测要的是原样行为。
func (r *Runner) do(ctx context.Context, rec Record, c call) Record {
	rec.Time = time.Now()
	rec.Model = r.T.Model
	rec.ConversationID = c.convID
	rec.CachePoints = pointsName(c.points)
	rec.Continuation = c.continuation
	rec.MaxOutput = "ok"
	if c.long {
		rec.MaxOutput = "long"
	}
	cred, err := r.T.Cred(ctx)
	if err != nil {
		rec.Error = "credentials: " + err.Error()
		r.write(rec)
		return rec
	}
	payload, names, err := kiro.Build(c.request(r.T.Model), kiro.BuildOptions{
		Model: r.T.Model, ProfileArn: cred.ProfileArn, ConversationID: c.convID, CachePoints: c.points,
		AgentContinuation: c.continuation,
	})
	if err != nil {
		rec.Error = "build: " + err.Error()
		r.write(rec)
		return rec
	}
	start := time.Now()
	res, err := r.T.Client.Generate(ctx, cred, payload)
	if err != nil {
		rec.Error = err.Error()
		r.write(rec)
		return rec
	}
	defer res.Body.Close()
	rec.Status = res.StatusCode
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		rec.Error = strings.TrimSpace(string(b))
		rec.MS = time.Since(start).Milliseconds()
		r.write(rec)
		return rec
	}
	window := r.T.Window
	if window <= 0 {
		window = 200_000
	}
	dec := kiro.NewDecoder(res.Body, false, window, names)
	dec.Trace = func(ev string, payload []byte) {
		if n := len(rec.Events); n == 0 || rec.Events[n-1] != ev {
			rec.Events = append(rec.Events, ev)
		}
		switch ev {
		case "assistantResponseEvent", "reasoningContentEvent", "toolUseEvent":
		default:
			if json.Valid(payload) {
				rec.Raw = append(rec.Raw, RawEvent{Type: ev, Payload: append(json.RawMessage(nil), payload...)})
			}
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(payload, &m) == nil {
			if tu, ok := m["tokenUsage"]; ok {
				rec.TokenUsage = tu
				rec.AllUsage = append(rec.AllUsage, tu)
			}
		}
	}
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		switch ev.Kind {
		case kiro.EvText, kiro.EvThink:
			if rec.FirstMS == 0 {
				rec.FirstMS = time.Since(start).Milliseconds()
			}
			rec.OutputChars += len(ev.Text)
		case kiro.EvError:
			rec.Error = ev.Text
		case kiro.EvStop:
			u := ev.Usage
			rec.Credits, rec.Metering, rec.MeteringUnit = u.Credits, u.MeteringEvents, u.MeteringUnit
			rec.TokenUsages, rec.ContextPct = u.UsageEvents, u.ContextPct
			if u.ContextPct > 0 {
				rec.ContextTok = int(u.ContextPct/100*float64(window) + 0.5)
			}
		}
	}
	rec.MS = time.Since(start).Milliseconds()
	r.write(rec)
	return rec
}

// scrubEmail 去掉额度响应里的邮箱，结果文件可以拿去分享。
func scrubEmail(raw []byte) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	delete(m, "userInfo")
	b, _ := json.Marshal(m)
	return b
}

func pointsName(p kiro.CachePoints) string {
	var s []string
	if p.FirstUser {
		s = append(s, "first-user")
	}
	if p.Assistant {
		s = append(s, "assistant")
	}
	if p.Tools {
		s = append(s, "tools")
	}
	return strings.Join(s, "+")
}

// limits 记一次 Get-Usage-Limits（第 4 步：前后核对）。
func (r *Runner) limits(ctx context.Context, exp, step string) kiro.Limits {
	rec := Record{Experiment: exp, Step: step, Time: time.Now()}
	cred, err := r.T.Cred(ctx)
	if err != nil {
		rec.Error = err.Error()
		r.write(rec)
		return kiro.Limits{}
	}
	l, raw, err := r.T.Client.UsageLimitsRaw(ctx, cred)
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.LimitsUsed, rec.LimitsOverage = l.Used, l.Overage
		rec.LimitsRaw = scrubEmail(raw)
	}
	r.write(rec)
	return l
}

// Run 跑一个实验，前后各拉一次额度。返回本实验 meteringEvent 之和与额度增量。
func (r *Runner) Run(ctx context.Context, exp string, opts Options) (Summary, error) {
	if r.Salt == "" {
		r.Salt = salt()
	}
	var s Summary
	before := r.limits(ctx, exp, "limits-before")
	var recs []Record
	var err error
	switch exp {
	case "usage":
		recs = r.usage(ctx)
	case "ttl":
		recs, err = r.ttl(ctx, opts)
	case "cachepoint":
		recs = r.cachepoint(ctx, opts)
	case "credits":
		recs = r.credits(ctx, opts)
	case "continuation":
		recs = r.continuationAB(ctx, opts)
	case "longctx":
		recs = r.longctx(ctx, opts)
	default:
		return s, fmt.Errorf("unknown experiment %q (usage | ttl | cachepoint | credits | continuation | longctx)", exp)
	}
	// 额度更新有延迟：等一会再拉
	select {
	case <-ctx.Done():
	case <-time.After(opts.settle()):
	}
	after := r.limits(context.WithoutCancel(ctx), exp, "limits-after")
	for _, rec := range recs {
		s.Calls++
		s.Metered += rec.Credits
		if rec.Error != "" {
			s.Errors++
		}
	}
	s.LimitsDelta = after.Consumed() - before.Consumed()
	s.Unassigned = s.LimitsDelta - s.Metered
	return s, err
}

// Summary 是一个实验的核对结果。
type Summary struct {
	Calls       int     `json:"calls"`
	Errors      int     `json:"errors"`
	Metered     float64 `json:"metered_credits"` // meteringEvent 之和
	LimitsDelta float64 `json:"limits_delta"`    // Get-Usage-Limits 已用增量
	Unassigned  float64 `json:"unassigned"`      // 增量 − meteringEvent 之和
}

// Options 是实验参数。零值用默认。
type Options struct {
	Gaps     []time.Duration // ttl 的间隔
	Sizes    []int           // credits 的前缀长度（token）
	Prefix   int             // ttl / cachepoint 的前缀长度
	Settle   time.Duration   // 实验后等多久再拉额度
	Parallel int             // ttl 同时进行的间隔数
}

// DefaultGaps 是 ttl 实验的间隔：覆盖 5m 边界、1h 边界。
var DefaultGaps = []time.Duration{0, 4 * time.Minute, 6 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, 45 * time.Minute, 65 * time.Minute}

func (o Options) gaps() []time.Duration {
	if len(o.Gaps) > 0 {
		return o.Gaps
	}
	return DefaultGaps
}

func (o Options) sizes() []int {
	if len(o.Sizes) > 0 {
		return o.Sizes
	}
	return []int{5_000, 20_000, 50_000}
}

func (o Options) prefix() int {
	if o.Prefix > 0 {
		return o.Prefix
	}
	return 6_000
}

func (o Options) settle() time.Duration {
	if o.Settle > 0 {
		return o.Settle
	}
	return 20 * time.Second
}

// usage：第 0 步。一次冷请求 + 同前缀的热请求，看 tokenUsage 报不报、报几次、是否累计。
func (r *Runner) usage(ctx context.Context) []Record {
	p := Prefix("usage-"+r.Salt, 3_000)
	conv := kiro.NewUUID()
	a := r.do(ctx, Record{Experiment: "usage", Case: "3k", Step: "cold", PrefixTokens: 3_000}, call{prefix: p, convID: conv})
	b := r.do(ctx, Record{Experiment: "usage", Case: "3k", Step: "warm", PrefixTokens: 3_000, SameConv: true}, call{prefix: p, convID: conv})
	c := r.do(ctx, Record{Experiment: "usage", Case: "3k", Step: "long", PrefixTokens: 3_000, SameConv: true}, call{prefix: p, convID: conv, long: true})
	r.logf("usage: tokenUsage events cold=%d warm=%d long=%d (0 = upstream does not report token counts)", a.TokenUsages, b.TokenUsages, c.TokenUsages)
	return []Record{a, b, c}
}

// ttl：第 1 步。每个间隔一份新前缀：写入 → 等间隔 → 同前缀再发一次（分同 / 新 conversationId 两组）。
// 各间隔并行错开，总耗时约等于最长间隔。
func (r *Runner) ttl(ctx context.Context, o Options) ([]Record, error) {
	var (
		mu   sync.Mutex
		recs []Record
		wg   sync.WaitGroup
	)
	add := func(rs ...Record) {
		mu.Lock()
		recs = append(recs, rs...)
		mu.Unlock()
	}
	sem := make(chan struct{}, max(1, o.Parallel))
	if o.Parallel <= 0 {
		sem = make(chan struct{}, 4) // 同一号并发别太高，免得被限流干扰
	}
	size := o.prefix()
	for i, gap := range o.gaps() {
		for _, same := range []bool{true, false} {
			caseName := fmt.Sprintf("gap=%s same_conv=%v", gap, same)
			p := Prefix(fmt.Sprintf("ttl-%s-%d-%v", r.Salt, i, same), size)
			wg.Go(func() {
				sem <- struct{}{}
				conv := kiro.NewUUID()
				w := r.do(ctx, Record{Experiment: "ttl", Case: caseName, Step: "write", PrefixTokens: size, SameConv: same}, call{prefix: p, convID: conv})
				<-sem
				add(w)
				select {
				case <-ctx.Done():
					return
				case <-time.After(gap):
				}
				if !same {
					conv = kiro.NewUUID()
				}
				sem <- struct{}{}
				rd := r.do(ctx, Record{Experiment: "ttl", Case: caseName, Step: "read", PrefixTokens: size, Gap: gap, SameConv: same},
					call{prefix: p, convID: conv, question: "Reply with exactly: ok2"})
				<-sem
				add(rd)
				r.logf("ttl %s: credits write=%.4f read=%.4f", caseName, w.Credits, rd.Credits)
			})
		}
	}
	wg.Wait()
	return recs, ctx.Err()
}

// cachepoint：第 2 步。同样的多轮对话，分别不发 / 只在一个位置发 / 全发 cachePoint，冷热各一次。
func (r *Runner) cachepoint(ctx context.Context, o Options) []Record {
	size := o.prefix()
	variants := []kiro.CachePoints{{}, {FirstUser: true}, {Assistant: true}, {Tools: true}, {FirstUser: true, Assistant: true, Tools: true}}
	hist := []anthropic.Message{
		{Role: "user", Content: anthropic.Content{{Type: "text", Text: "What is section 3 about?"}}},
		{Role: "assistant", Content: anthropic.Content{{Type: "text", Text: "It reviews clause 24 of the agreement."}}},
	}
	var recs []Record
	for i, v := range variants {
		name := pointsName(v)
		if name == "" {
			name = "none"
		}
		p := Prefix(fmt.Sprintf("cp-%s-%d", r.Salt, i), size)
		conv := kiro.NewUUID()
		c := call{prefix: p, convID: conv, points: v, history: hist, tools: true}
		recs = append(recs, r.do(ctx, Record{Experiment: "cachepoint", Case: name, Step: "cold", PrefixTokens: size}, c))
		c.question = "Reply with exactly: ok2"
		recs = append(recs, r.do(ctx, Record{Experiment: "cachepoint", Case: name, Step: "warm", PrefixTokens: size, SameConv: true}, c))
		r.logf("cachepoint %s: credits cold=%.4f warm=%.4f", name, recs[len(recs)-2].Credits, recs[len(recs)-1].Credits)
	}
	return recs
}

// credits：第 3 步。各前缀长度冷 / 热各一次（只回 ok），再一次长输出。
func (r *Runner) credits(ctx context.Context, o Options) []Record {
	var recs []Record
	for _, n := range o.sizes() {
		p := Prefix(fmt.Sprintf("cr-%s-%d", r.Salt, n), n)
		conv := kiro.NewUUID()
		cs := fmt.Sprintf("%dk", n/1000)
		recs = append(recs, r.do(ctx, Record{Experiment: "credits", Case: cs, Step: "cold", PrefixTokens: n}, call{prefix: p, convID: conv}))
		recs = append(recs, r.do(ctx, Record{Experiment: "credits", Case: cs, Step: "warm", PrefixTokens: n, SameConv: true}, call{prefix: p, convID: conv, question: "Reply with exactly: ok2"}))
		recs = append(recs, r.do(ctx, Record{Experiment: "credits", Case: cs, Step: "long", PrefixTokens: n, SameConv: true}, call{prefix: p, convID: conv, long: true}))
		r.logf("credits %s: cold=%.4f warm=%.4f long=%.4f", cs, recs[len(recs)-3].Credits, recs[len(recs)-2].Credits, recs[len(recs)-1].Credits)
	}
	return recs
}

// continuationAB：同样的两轮对话，发 / 不发 agentContinuationId（由 conversationId 派生的固定值），
// 对比 credits、首字延迟与是否报错。每组一份新前缀。
func (r *Runner) continuationAB(ctx context.Context, o Options) []Record {
	size := o.prefix()
	var recs []Record
	for i, on := range []bool{false, true} {
		name := map[bool]string{false: "off", true: "on"}[on]
		p := Prefix(fmt.Sprintf("ac-%s-%d", r.Salt, i), size)
		c := call{prefix: p, convID: kiro.NewUUID(), continuation: on}
		recs = append(recs, r.do(ctx, Record{Experiment: "continuation", Case: name, Step: "cold", PrefixTokens: size}, c))
		c.question = "Reply with exactly: ok2"
		recs = append(recs, r.do(ctx, Record{Experiment: "continuation", Case: name, Step: "warm", PrefixTokens: size, SameConv: true}, c))
		r.logf("continuation %s: credits cold=%.4f warm=%.4f first_ms=%d/%d err=%q", name,
			recs[len(recs)-2].Credits, recs[len(recs)-1].Credits, recs[len(recs)-2].FirstMS, recs[len(recs)-1].FirstMS, recs[len(recs)-1].Error)
	}
	return recs
}

// longctx：50k 以上的长前缀，同一号、同一 conversationId 连发两次（都只回 ok），
// 对比 contextUsagePercentage：缓存命中时它会不会变小（上游是否只报未缓存部分）。
func (r *Runner) longctx(ctx context.Context, o Options) []Record {
	size := max(o.prefix(), 60_000)
	if o.Prefix > 0 {
		size = o.Prefix
	}
	p := Prefix("lc-"+r.Salt, size)
	c := call{prefix: p, convID: kiro.NewUUID()}
	cs := fmt.Sprintf("%dk", size/1000)
	a := r.do(ctx, Record{Experiment: "longctx", Case: cs, Step: "cold", PrefixTokens: size}, c)
	b := r.do(ctx, Record{Experiment: "longctx", Case: cs, Step: "warm", PrefixTokens: size, SameConv: true}, c)
	r.logf("longctx %s: context_pct cold=%.4f warm=%.4f (tokens %d / %d), credits %.4f / %.4f",
		cs, a.ContextPct, b.ContextPct, a.ContextTok, b.ContextTok, a.Credits, b.Credits)
	return []Record{a, b}
}
