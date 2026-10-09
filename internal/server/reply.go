package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
)

const pingEvery = 15 * time.Second

// timingSink 记录首个内容事件时间，用于 TTFT / TPS。
type timingSink struct {
	turn.Sink
	start time.Time
	first time.Time
}

func (t *timingSink) mark() {
	if t.first.IsZero() {
		t.first = time.Now()
	}
}

func (t *timingSink) Text(s string)             { t.mark(); t.Sink.Text(s) }
func (t *timingSink) Thinking(s string)         { t.mark(); t.Sink.Thinking(s) }
func (t *timingSink) ToolStart(id, name string) { t.mark(); t.Sink.ToolStart(id, name) }

func (t *timingSink) firstMS() int64 {
	if t.first.IsZero() {
		return 0
	}
	return t.first.Sub(t.start).Milliseconds()
}

// reply 是一次回复的结局。ok 与 failure 至多一个为真；都不是表示下游自己走了。
type reply struct {
	// usage 是本地计量。成功时完整；中途失败 / 下游走了时是已消耗的部分：
	// 上游已开始回复就说明整段输入已处理，再加上已收到的输出与已报的 credits。
	usage   turn.Usage
	kiro    kiro.Usage // 上游报的：credits、上下文百分比
	ok      bool
	failure *kiro.Failure // 上游中途报错或断流，应记到号上
}

func failed(text string) reply {
	f := kiro.ClassifyStream(text)
	return reply{failure: &f}
}

const brokeOff = "the reply ended before it was complete"

// toolCallTokens 是每个工具调用在输出侧的包装开销。
const toolCallTokens = 8

// pump 把解码事件写进 sink。first 是已经读出的第一条事件。
// input 是输入侧用量；settle 给完整用量填费用并记账，在 sink.End 之前调用：
// 客户端收到结尾时账已记上，紧接着的下一个请求做预算判断能看到它。输出 token 在本地按实际内容计。
// 上游中途失败时调 sink.Fail 并把失败返回；下游走了则不再写 sink。
// pumpOpts 是上游报了 tokenUsage 时的口径。
type pumpOpts struct {
	mode string // kiro.Reported*
	// reportsSeen 表示上游曾报过 tokenUsage：那时最终输入与本地估算无关，开头的输入只能报 0
	reportsSeen bool
}

// startUsage 是流式开头（message_start）报的 usage：每一项都不超过结尾的最终值。
//   - cache_read / cache_creation 给 0（最终拆分要校准后才知道）；
//   - input 给普通输入的下界：客户端部分的校准比例不低于 0.5，Kiro 基线固定（meter.Calibrate），
//     再留 2 个 token 的舍入余量；上游报过 tokenUsage 时最终输入取上游值，与本地无关，给 0。
func startUsage(in turn.Usage, reportsSeen bool) turn.Usage {
	if reportsSeen {
		return turn.Usage{}
	}
	return turn.Usage{Input: max(0, in.Input/2-2)}
}

// reportedUsage 合并上游已报的记账；未报的部分保留本地计量。
// 首条内容前失败时传入零用量，不能假定本地整段输入已被上游处理。
func reportedUsage(u turn.Usage, k kiro.Usage, po pumpOpts) turn.Usage {
	if k.Reported && po.mode != kiro.ReportedIgnore {
		// 上游不分 TTL：1h 按本地拆分的占比搬到上游的 cacheWrite 上。
		u.CacheWrite1h = meter.Scale1h(u.CacheWrite1h, u.CacheWrite, k.CacheWrite)
		u.Input, u.CacheRead, u.CacheWrite = k.Input, k.CacheRead, k.CacheWrite
		u.KiroInput = 0
	}
	if k.OutputReported && po.mode != kiro.ReportedIgnore {
		u.Output = max(k.Output, u.Reasoning)
	}
	u.Credits = k.Credits
	return u
}

func pump(ctx context.Context, dec *kiro.Decoder, first kiro.Event, sink turn.Sink, input turn.Usage, settle func(turn.Usage, kiro.Usage) turn.Usage, po pumpOpts) reply {
	// 开头只报保守的下界，最终值以结尾为准。客户端若把 message_start 的 usage 也计入，不会比最终值多
	sink.Begin(startUsage(input, po.reportsSeen))

	// 上游在长 thinking 时可能很久不出字，定时 ping 防止客户端 / 代理断开
	events := make(chan kiro.Event)
	readerDone := make(chan struct{})
	// 返回前等读协程退出：调用方随后会 drain/关闭同一个 body，不能并发读
	defer func() { <-readerDone }()
	go func() {
		defer close(readerDone)
		defer close(events)
		ev, err := first, error(nil)
		for err == nil {
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
			ev, err = dec.Next()
		}
	}()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	var out, think strings.Builder
	tools := 0
	// measure 是到目前为止的用量：k 是上游报的记账。
	measure := func(k kiro.Usage) turn.Usage {
		u := input
		u.Reasoning = meter.Count(think.String())
		u.Output = meter.Count(out.String()) + u.Reasoning + tools*toolCallTokens
		return reportedUsage(u, k, po)
	}
	// partial 在没正常结束时取已消耗的量。先等读协程退出：Decoder 不能并发访问。
	partial := func(r reply) reply {
		<-readerDone
		r.usage = measure(dec.Partial())
		return r
	}
	fail := func(text string) reply {
		r := failed(text)
		sink.Fail(r.failure.Status, r.failure.Message)
		return partial(r)
	}
	for {
		var ev kiro.Event
		var more bool
		select {
		case <-ctx.Done():
			return partial(reply{})
		case <-ping.C:
			sink.Ping()
			continue
		case ev, more = <-events:
		}
		if !more {
			if ctx.Err() != nil {
				return partial(reply{})
			}
			return fail(brokeOff)
		}
		switch ev.Kind {
		case kiro.EvText:
			out.WriteString(ev.Text)
			sink.Text(ev.Text)
		case kiro.EvThink:
			think.WriteString(ev.Text)
			sink.Thinking(ev.Text)
		case kiro.EvSig:
			sink.Signature(ev.Text)
		case kiro.EvToolStart:
			tools++
			out.WriteString(ev.Name)
			sink.ToolStart(ev.ID, ev.Name)
		case kiro.EvToolArgs:
			out.WriteString(ev.Text)
			sink.ToolArgs(ev.Text)
		case kiro.EvError:
			return fail(ev.Text)
		case kiro.EvStop:
			u := settle(measure(ev.Usage), ev.Usage)
			sink.End(ev.Stop, u)
			return reply{usage: u, kiro: ev.Usage, ok: true}
		}
	}
}

// anthropicStream 是 Anthropic Messages SSE 的 Sink。
type anthropicStream struct {
	w         http.ResponseWriter
	rc        *http.ResponseController
	id, model string
	index     int
	open      string
}

func newAnthropicStream(w http.ResponseWriter, id, model string) turn.Sink {
	return &anthropicStream{w: w, rc: http.NewResponseController(w), id: id, model: model, index: -1}
}

func (a *anthropicStream) send(typ string, data map[string]any) {
	data["type"] = typ
	b, _ := json.Marshal(data)
	fmt.Fprintf(a.w, "event: %s\ndata: %s\n\n", typ, b)
	_ = a.rc.Flush()
}

func (a *anthropicStream) closeBlock() {
	if a.open != "" {
		a.send("content_block_stop", map[string]any{"index": a.index})
		a.open = ""
	}
}

func (a *anthropicStream) begin(kind string, block map[string]any) {
	a.closeBlock()
	a.index++
	a.open = kind
	a.send("content_block_start", map[string]any{"index": a.index, "content_block": block})
}

func (a *anthropicStream) delta(d map[string]any) {
	a.send("content_block_delta", map[string]any{"index": a.index, "delta": d})
}

func (a *anthropicStream) Begin(u turn.Usage) {
	h := a.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	a.w.WriteHeader(http.StatusOK)
	a.send("message_start", map[string]any{"message": map[string]any{
		"id": a.id, "type": "message", "role": "assistant", "model": a.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": anthropicUsage(u),
	}})
}

func (a *anthropicStream) Text(s string) {
	if a.open != "text" {
		a.begin("text", map[string]any{"type": "text", "text": ""})
	}
	a.delta(map[string]any{"type": "text_delta", "text": s})
}

func (a *anthropicStream) Thinking(s string) {
	if a.open != "thinking" {
		a.begin("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	}
	a.delta(map[string]any{"type": "thinking_delta", "thinking": s})
}

func (a *anthropicStream) Signature(s string) {
	if a.open == "thinking" {
		a.delta(map[string]any{"type": "signature_delta", "signature": s})
	}
}

func (a *anthropicStream) ToolStart(id, name string) {
	a.begin("tool", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
}

func (a *anthropicStream) ToolArgs(s string) {
	if a.open == "tool" {
		a.delta(map[string]any{"type": "input_json_delta", "partial_json": s})
	}
}

func (a *anthropicStream) Ping() { a.send("ping", map[string]any{}) }

func (a *anthropicStream) End(stop string, u turn.Usage) {
	a.closeBlock()
	// message_delta 的 usage 是累计值，客户端以它为准：输入侧此时已按上游上下文校正过，比 message_start 里的估算准
	a.send("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": anthropicUsage(u),
	})
	a.send("message_stop", map[string]any{})
}

func (a *anthropicStream) Fail(status int, message string) {
	a.closeBlock()
	a.send("error", map[string]any{"error": map[string]any{"type": anthropic.ErrorType(status), "message": message}})
}

// anthropicMessage 是非流式 Messages 响应。
func anthropicMessage(t *turn.Turn) anthropic.Response {
	blocks := make([]anthropic.ResponseBlock, 0, len(t.Blocks))
	for _, b := range t.Blocks {
		switch b.Kind {
		case "text":
			blocks = append(blocks, anthropic.ResponseBlock{Type: "text", Text: b.Text})
		case "thinking":
			blocks = append(blocks, anthropic.ResponseBlock{Type: "thinking", Thinking: &b.Text, Signature: &b.Signature})
		case "tool_use":
			blocks = append(blocks, anthropic.ResponseBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return anthropic.Response{
		ID: t.ID, Type: "message", Role: "assistant", Model: t.Model, Content: blocks,
		StopReason: t.Stop, Usage: anthropicUsage(t.Usage),
	}
}

func anthropicUsage(u turn.Usage) anthropic.Usage {
	w5 := u.CacheWrite5m()
	return anthropic.Usage{
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheWrite,
		CacheCreation:            &anthropic.CacheCreation{Ephemeral5m: w5, Ephemeral1h: u.CacheWrite - w5},
		Credits:                  u.Credits,
	}
}
