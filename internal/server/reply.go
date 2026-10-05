package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"kiro-go/internal/anthropic"
	"kiro-go/internal/kiro"
)

const pingEvery = 15 * time.Second

// reply 是一次回复的结局。ok 与 failure 至多一个为真；都不是表示下游自己走了。
type reply struct {
	usage   kiro.Usage
	ok      bool
	failure *kiro.Failure // 上游中途报错或断流，应记到号上
}

func failed(text string) reply {
	f := kiro.ClassifyStream(text)
	return reply{failure: &f}
}

const brokeOff = "the reply ended before it was complete"

// streamSSE 把解码事件写成 Anthropic SSE。first 是已经读出的第一条事件。
// 上游中途失败时向下游发 error 事件，并把失败返回给调用方记到号上。
func streamSSE(ctx context.Context, w http.ResponseWriter, dec *kiro.Decoder, first kiro.Event, msgID, model string) reply {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	send := func(typ string, data map[string]any) {
		data["type"] = typ
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
	}
	flush := func() { _ = rc.Flush() }

	send("message_start", map[string]any{"message": map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
	flush()

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

	index, open := -1, ""
	closeBlock := func() {
		if open != "" {
			send("content_block_stop", map[string]any{"index": index})
			open = ""
		}
	}
	begin := func(kind string, block map[string]any) {
		closeBlock()
		index++
		open = kind
		send("content_block_start", map[string]any{"index": index, "content_block": block})
	}
	fail := func(text string) reply {
		r := failed(text)
		closeBlock()
		send("error", map[string]any{"error": map[string]any{"type": anthropic.ErrorType(r.failure.Status), "message": r.failure.Message}})
		flush()
		return r
	}
	for {
		var ev kiro.Event
		var more bool
		select {
		case <-ctx.Done():
			return reply{}
		case <-ping.C:
			send("ping", map[string]any{})
			flush()
			continue
		case ev, more = <-events:
		}
		if !more {
			if ctx.Err() != nil {
				return reply{}
			}
			return fail(brokeOff)
		}
		switch ev.Kind {
		case kiro.EvText:
			if open != "text" {
				begin("text", map[string]any{"type": "text", "text": ""})
			}
			send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
		case kiro.EvThink:
			if open != "thinking" {
				begin("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
			send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text}})
		case kiro.EvSig:
			if open == "thinking" {
				send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "signature_delta", "signature": ev.Text}})
			}
		case kiro.EvToolStart:
			begin("tool", map[string]any{"type": "tool_use", "id": ev.ID, "name": ev.Name, "input": map[string]any{}})
		case kiro.EvToolArgs:
			if open == "tool" {
				send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Text}})
			}
		case kiro.EvError:
			return fail(ev.Text)
		case kiro.EvStop:
			closeBlock()
			send("message_delta", map[string]any{
				"delta": map[string]any{"stop_reason": ev.Stop, "stop_sequence": nil},
				"usage": usageOf(ev.Usage),
			})
			send("message_stop", map[string]any{})
			flush()
			return reply{usage: ev.Usage, ok: true}
		}
		flush()
	}
}

// collectMessage 把解码事件拼成一条非流式响应。只有收到 EvStop 才算完整；
// 中途报错或断流一律返回 failure，已收到的半截内容丢弃——不把半句话当成 end_turn 交给客户端。
func collectMessage(dec *kiro.Decoder, first kiro.Event, msgID, model string) (anthropic.Response, reply) {
	var blocks []anthropic.ResponseBlock
	var args []string // 与 blocks 同下标，tool_use 的参数片段
	last := func() *anthropic.ResponseBlock {
		if len(blocks) == 0 {
			return nil
		}
		return &blocks[len(blocks)-1]
	}
	add := func(b anthropic.ResponseBlock) {
		blocks = append(blocks, b)
		args = append(args, "")
	}
	finish := func(stop string, u kiro.Usage) anthropic.Response {
		for i := range blocks {
			if blocks[i].Type != "tool_use" {
				continue
			}
			blocks[i].Input = json.RawMessage("{}")
			if json.Valid([]byte(args[i])) && len(args[i]) > 0 {
				blocks[i].Input = json.RawMessage(args[i])
			}
		}
		if blocks == nil {
			blocks = []anthropic.ResponseBlock{}
		}
		return anthropic.Response{
			ID: msgID, Type: "message", Role: "assistant", Model: model, Content: blocks,
			StopReason: stop, Usage: usageOf(u),
		}
	}

	ev, err := first, error(nil)
	for ; err == nil; ev, err = dec.Next() {
		switch ev.Kind {
		case kiro.EvText:
			if b := last(); b != nil && b.Type == "text" {
				b.Text += ev.Text
			} else {
				add(anthropic.ResponseBlock{Type: "text", Text: ev.Text})
			}
		case kiro.EvThink:
			if b := last(); b != nil && b.Type == "thinking" {
				*b.Thinking += ev.Text
			} else {
				t, sig := ev.Text, ""
				add(anthropic.ResponseBlock{Type: "thinking", Thinking: &t, Signature: &sig})
			}
		case kiro.EvSig:
			if b := last(); b != nil && b.Type == "thinking" {
				*b.Signature += ev.Text
			}
		case kiro.EvToolStart:
			add(anthropic.ResponseBlock{Type: "tool_use", ID: ev.ID, Name: ev.Name})
		case kiro.EvToolArgs:
			if b := last(); b != nil && b.Type == "tool_use" {
				args[len(args)-1] += ev.Text
			}
		case kiro.EvError:
			return anthropic.Response{}, failed(ev.Text)
		case kiro.EvStop:
			return finish(ev.Stop, ev.Usage), reply{usage: ev.Usage, ok: true}
		}
	}
	return anthropic.Response{}, failed(brokeOff)
}

func usageOf(u kiro.Usage) anthropic.Usage {
	return anthropic.Usage{
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheWrite,
	}
}
