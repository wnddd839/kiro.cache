package openai

import (
	"net/http"

	"kiro-proxy/internal/turn"
)

// ---- 流式 ----

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitzero"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role             string          `json:"role,omitzero"`
	Content          *string         `json:"content,omitzero"`
	ReasoningContent string          `json:"reasoning_content,omitzero"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitzero"`
}

// toolCallDelta 的 index 必须始终出现，包括 0。
type toolCallDelta struct {
	Index    int           `json:"index"`
	ID       string        `json:"id,omitzero"`
	Type     string        `json:"type,omitzero"`
	Function functionDelta `json:"function"`
}

type functionDelta struct {
	Name      string  `json:"name,omitzero"`
	Arguments *string `json:"arguments,omitzero"`
}

type chatStream struct {
	sse
	id, model    string
	created      int64
	includeUsage bool
	tools        int // 已开始的工具调用数
}

// NewChatStream 返回写 chat.completion.chunk SSE 的 Sink。
func NewChatStream(w http.ResponseWriter, id, model string, created int64, includeUsage bool) turn.Sink {
	return &chatStream{sse: newSSE(w), id: id, model: model, created: created, includeUsage: includeUsage}
}

func (c *chatStream) send(d delta, finish *string) {
	c.data(chunk{
		ID: c.id, Object: "chat.completion.chunk", Created: c.created, Model: c.model,
		Choices: []chunkChoice{{Delta: d, FinishReason: finish}},
	})
}

func (c *chatStream) Begin(turn.Usage) {
	c.start()
	empty := ""
	c.send(delta{Role: "assistant", Content: &empty}, nil)
}

func (c *chatStream) Text(s string) {
	if s != "" {
		c.send(delta{Content: &s}, nil)
	}
}

func (c *chatStream) Thinking(s string) {
	if s != "" {
		c.send(delta{ReasoningContent: s}, nil)
	}
}

func (c *chatStream) Signature(string) {}

func (c *chatStream) ToolStart(id, name string) {
	empty := ""
	c.send(delta{ToolCalls: []toolCallDelta{{
		Index: c.tools, ID: id, Type: "function",
		Function: functionDelta{Name: name, Arguments: &empty},
	}}}, nil)
	c.tools++
}

func (c *chatStream) ToolArgs(partial string) {
	if c.tools == 0 || partial == "" {
		return
	}
	c.send(delta{ToolCalls: []toolCallDelta{{
		Index: c.tools - 1, Function: functionDelta{Arguments: &partial},
	}}}, nil)
}

func (c *chatStream) Ping() { c.comment() }

func (c *chatStream) End(stop string, u turn.Usage) {
	fr := finishReason(stop)
	c.send(delta{}, &fr)
	if c.includeUsage {
		usage := ChatUsage(u)
		c.data(chunk{
			ID: c.id, Object: "chat.completion.chunk", Created: c.created, Model: c.model,
			Choices: []chunkChoice{}, Usage: &usage,
		})
	}
	c.doneMarker()
}

func (c *chatStream) Fail(status int, message string) {
	c.data(errorBody(status, message))
	c.doneMarker()
}

// ---- 非流式 ----

// ChatResponse 是非流式 chat.completion 响应体。
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// ChatChoice 是一个候选回复。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatMessage 是回复消息；只有工具调用时 Content 为 null。
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitzero"`
	ToolCalls        []ToolCall `json:"tool_calls,omitzero"`
}

// ToolCall 是一次工具调用；Arguments 是 JSON 字符串。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是被调用的函数。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletion 把收集好的 Turn 渲染为非流式 chat.completion。
func ChatCompletion(t *turn.Turn, created int64) ChatResponse {
	msg := ChatMessage{Role: "assistant"}
	var text string
	for _, b := range t.Blocks {
		switch b.Kind {
		case "text":
			text += b.Text
		case "thinking":
			msg.ReasoningContent += b.Text
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID: b.ID, Type: "function", Function: FunctionCall{Name: b.Name, Arguments: args},
			})
		}
	}
	if text != "" || len(msg.ToolCalls) == 0 {
		msg.Content = &text
	}
	return ChatResponse{
		ID: t.ID, Object: "chat.completion", Created: created, Model: t.Model,
		Choices: []ChatChoice{{Message: msg, FinishReason: finishReason(t.Stop)}},
		Usage:   ChatUsage(t.Usage),
	}
}
