package openai

import (
	"cmp"
	"net/http"
	"strings"

	"kiro-proxy/internal/turn"
)

// ResponseObject 是 Responses API 的 response 对象：非流式响应体，也嵌在流式生命周期事件里。
type ResponseObject struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Error             *RespError         `json:"error"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details"`
	Model             string             `json:"model"`
	Output            []OutputItem       `json:"output"`
	OutputText        string             `json:"output_text"`
	Usage             *RespUsage         `json:"usage"`
}

// RespError 是 response.error。
type RespError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IncompleteDetails 说明 status=incomplete 的原因。
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// OutputItem 是 output 里的一项：reasoning / message / function_call。
type OutputItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitzero"`

	// message
	Role    string          `json:"role,omitzero"`
	Content []OutputContent `json:"content,omitzero"`

	// reasoning
	Summary []SummaryPart `json:"summary,omitzero"`

	// function_call / custom_tool_call
	CallID    string  `json:"call_id,omitzero"`
	Name      string  `json:"name,omitzero"`
	Namespace string  `json:"namespace,omitzero"`
	Arguments *string `json:"arguments,omitzero"` // function_call
	Input     *string `json:"input,omitzero"`     // custom_tool_call
}

// OutputContent 是 message 的内容片段。
type OutputContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// SummaryPart 是 reasoning 的摘要片段。
type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func reasoningItem(id, text string, done bool) OutputItem {
	it := OutputItem{Type: "reasoning", ID: id, Summary: []SummaryPart{}}
	if done {
		it.Summary = []SummaryPart{{Type: "summary_text", Text: text}}
	}
	return it
}

func messageItem(id, text string, done bool) OutputItem {
	it := OutputItem{Type: "message", ID: id, Status: "in_progress", Role: "assistant", Content: []OutputContent{}}
	if done {
		it.Status = "completed"
		it.Content = []OutputContent{textPart(text)}
	}
	return it
}

func callItem(id, callID, name, args string, done bool) OutputItem {
	it := OutputItem{Type: "function_call", ID: id, Status: "in_progress", CallID: callID, Name: name, Arguments: &args}
	if done {
		it.Status = "completed"
	}
	return it
}

// toolItem 是按下游原始写法还原的工具调用项：function_call（可带 namespace）或 custom_tool_call。
func toolItem(kinds ToolKinds, id, callID, name, args string, done bool) OutputItem {
	typ, ns, short, payload := kinds.Restore(name, args)
	if typ == "function_call" {
		it := callItem(id, callID, short, payload, done)
		it.Namespace = ns
		return it
	}
	it := OutputItem{Type: "custom_tool_call", ID: id, Status: "in_progress", CallID: callID, Name: short, Namespace: ns, Input: &payload}
	if done {
		it.Status = "completed"
	}
	return it
}

func textPart(text string) OutputContent {
	return OutputContent{Type: "output_text", Text: text, Annotations: []any{}}
}

// respStatus 把 stop 原因映射为 response status。
func respStatus(stop string) (string, *IncompleteDetails) {
	if stop == turn.StopMaxTokens {
		return "incomplete", &IncompleteDetails{Reason: "max_output_tokens"}
	}
	return "completed", nil
}

// Response 把收集好的 Turn 渲染为非流式 response 对象。kinds 用来把工具调用还原成下游写法（可为 nil）。
func Response(t *turn.Turn, created int64, kinds ToolKinds) ResponseObject {
	out := []OutputItem{}
	for _, b := range t.Blocks {
		switch b.Kind {
		case "thinking":
			out = append(out, reasoningItem(NewID("rs_"), b.Text, true))
		case "text":
			out = append(out, messageItem(NewID("msg_"), b.Text, true))
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			out = append(out, toolItem(kinds, NewID("fc_"), b.ID, b.Name, args, true))
		}
	}
	status, inc := respStatus(t.Stop)
	usage := ResponsesUsage(t.Usage)
	return ResponseObject{
		ID: t.ID, Object: "response", CreatedAt: created, Status: status, IncompleteDetails: inc,
		Model: t.Model, Output: out, OutputText: t.Text(), Usage: &usage,
	}
}

// ---- 流式 ----

type responsesStream struct {
	sse
	id, model string
	created   int64
	seq       int

	out  []OutputItem // 已关闭的 item
	text strings.Builder

	// 当前打开的 item；kind 为空表示没有。其 output_index 是 len(out)。
	kind         string
	itemID       string
	callID, name string
	buf          strings.Builder

	kinds ToolKinds // 工具调用还原成下游写法
}

// NewResponsesStream 返回写 Responses API 命名事件 SSE 的 Sink。kinds 用来还原 custom / namespace 工具调用（可为 nil）。
func NewResponsesStream(w http.ResponseWriter, id, model string, created int64, kinds ToolKinds) turn.Sink {
	return &responsesStream{sse: newSSE(w), id: id, model: model, created: created, kinds: kinds}
}

// emit 写一个命名事件，补上 type 与递增的 sequence_number。
func (r *responsesStream) emit(typ string, data map[string]any) {
	data["type"] = typ
	data["sequence_number"] = r.seq
	r.seq++
	r.event(typ, data)
}

func (r *responsesStream) object(status string) ResponseObject {
	return ResponseObject{
		ID: r.id, Object: "response", CreatedAt: r.created, Status: status,
		Model: r.model, Output: append([]OutputItem{}, r.out...), OutputText: r.text.String(),
	}
}

func (r *responsesStream) Begin(turn.Usage) {
	r.start()
	r.emit("response.created", map[string]any{"response": r.object("in_progress")})
	r.emit("response.in_progress", map[string]any{"response": r.object("in_progress")})
}

// open 关闭当前 item 并打开一个新的。
func (r *responsesStream) open(kind string) {
	r.closeItem()
	r.kind = kind
	r.buf.Reset()
	idx := len(r.out)
	switch kind {
	case "reasoning":
		r.itemID = NewID("rs_")
		r.emit("response.output_item.added", map[string]any{
			"output_index": idx, "item": reasoningItem(r.itemID, "", false),
		})
		r.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": r.itemID, "output_index": idx, "summary_index": 0,
			"part": SummaryPart{Type: "summary_text"},
		})
	case "message":
		r.itemID = NewID("msg_")
		r.emit("response.output_item.added", map[string]any{
			"output_index": idx, "item": messageItem(r.itemID, "", false),
		})
		r.emit("response.content_part.added", map[string]any{
			"item_id": r.itemID, "output_index": idx, "content_index": 0, "part": textPart(""),
		})
	case "function_call":
		r.itemID = NewID("fc_")
		r.emit("response.output_item.added", map[string]any{
			"output_index": idx, "item": toolItem(r.kinds, r.itemID, r.callID, r.name, "", false),
		})
	}
}

// closeItem 发当前 item 的收尾事件并记入 out。
func (r *responsesStream) closeItem() {
	idx, s := len(r.out), r.buf.String()
	var item OutputItem
	switch r.kind {
	case "":
		return
	case "reasoning":
		item = reasoningItem(r.itemID, s, true)
		r.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": r.itemID, "output_index": idx, "summary_index": 0, "text": s,
		})
		r.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": r.itemID, "output_index": idx, "summary_index": 0, "part": item.Summary[0],
		})
	case "message":
		item = messageItem(r.itemID, s, true)
		r.emit("response.output_text.done", map[string]any{
			"item_id": r.itemID, "output_index": idx, "content_index": 0, "text": s,
		})
		r.emit("response.content_part.done", map[string]any{
			"item_id": r.itemID, "output_index": idx, "content_index": 0, "part": item.Content[0],
		})
	case "function_call":
		item = toolItem(r.kinds, r.itemID, r.callID, r.name, s, true)
		if item.Type == "custom_tool_call" {
			// custom 工具的输入要等参数收全、取出 input 字段才知道：一次发完
			r.emit("response.custom_tool_call_input.delta", map[string]any{
				"item_id": r.itemID, "output_index": idx, "delta": *item.Input,
			})
			r.emit("response.custom_tool_call_input.done", map[string]any{
				"item_id": r.itemID, "output_index": idx, "input": *item.Input,
			})
		} else {
			r.emit("response.function_call_arguments.done", map[string]any{
				"item_id": r.itemID, "output_index": idx, "arguments": s,
			})
		}
	}
	r.emit("response.output_item.done", map[string]any{"output_index": idx, "item": item})
	r.out = append(r.out, item)
	r.kind = ""
}

func (r *responsesStream) Thinking(s string) {
	if s == "" {
		return
	}
	if r.kind != "reasoning" {
		r.open("reasoning")
	}
	r.buf.WriteString(s)
	r.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": r.itemID, "output_index": len(r.out), "summary_index": 0, "delta": s,
	})
}

func (r *responsesStream) Text(s string) {
	if s == "" {
		return
	}
	if r.kind != "message" {
		r.open("message")
	}
	r.buf.WriteString(s)
	r.text.WriteString(s)
	r.emit("response.output_text.delta", map[string]any{
		"item_id": r.itemID, "output_index": len(r.out), "content_index": 0, "delta": s,
	})
}

func (r *responsesStream) Signature(string) {}

func (r *responsesStream) ToolStart(id, name string) {
	r.closeItem()
	r.callID, r.name = id, name
	r.open("function_call")
}

func (r *responsesStream) ToolArgs(partial string) {
	if r.kind != "function_call" || partial == "" {
		return
	}
	r.buf.WriteString(partial)
	if k, ok := r.kinds[r.name]; ok && k.Custom {
		return // custom 工具：参数是包着 input 的 JSON，结束时取出原文再发
	}
	r.emit("response.function_call_arguments.delta", map[string]any{
		"item_id": r.itemID, "output_index": len(r.out), "delta": partial,
	})
}

func (r *responsesStream) Ping() { r.comment() }

func (r *responsesStream) End(stop string, u turn.Usage) {
	r.closeItem()
	status, inc := respStatus(stop)
	obj := r.object(status)
	obj.IncompleteDetails = inc
	usage := ResponsesUsage(u)
	obj.Usage = &usage
	r.emit("response."+status, map[string]any{"response": obj})
}

func (r *responsesStream) Fail(status int, message string) {
	r.start()
	// code 优先用具体 code，没有就用 type
	typ, code := ErrorType(status)
	code = cmp.Or(code, typ)
	obj := r.object("failed")
	obj.Error = &RespError{Code: code, Message: message}
	r.emit("response.failed", map[string]any{"response": obj})
}
