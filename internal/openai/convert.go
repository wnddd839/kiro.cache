package openai

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"kiro-proxy/internal/anthropic"
)

// ChatOptions 是 Chat Completions 请求里不进 anthropic.Request、但写回复时要用的选项。
type ChatOptions struct {
	IncludeUsage bool   // 流式末尾发 usage chunk；未给 stream_options 时为 true
	User         string // 请求里的 user 字段
	// Dropped 是 drop 模式下跳过的内置工具与历史项（「位置: 类型」），调用方打日志用
	Dropped []string
}

// ResponsesOptions 是 Responses 请求里不进 anthropic.Request 的选项。
type ResponsesOptions struct {
	User string
	// Tools 是需要在回复里还原的工具写法（custom / namespace）
	Tools ToolKinds
	// Dropped 同 ChatOptions.Dropped
	Dropped []string
}

// ErrPreviousResponse 是带 previous_response_id 的请求；本地不存历史，只接受完整输入。
var ErrPreviousResponse = errors.New("previous_response_id is not supported; send the full input")

// ---- Chat Completions ----

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	Reasoning           *reasoningOpt   `json:"reasoning"`
	PromptCacheKey      string          `json:"prompt_cache_key"`
	User                string          `json:"user"`
}

type streamOptions struct {
	IncludeUsage *bool `json:"include_usage"`
}

type reasoningOpt struct {
	Effort string `json:"effort"`
}

type chatMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Name         string          `json:"name"`
	ToolCalls    []chatToolCall  `json:"tool_calls"`
	ToolCallID   string          `json:"tool_call_id"`
	FunctionCall *chatFunction   `json:"function_call"` // 旧版 assistant 函数调用
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// FromChat 把 POST /v1/chat/completions 的请求体转成 anthropic.Request。hosted 同 FromResponses。
func FromChat(raw []byte, hosted string) (anthropic.Request, ChatOptions, error) {
	var in chatRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return anthropic.Request{}, ChatOptions{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(in.Messages) == 0 {
		return anthropic.Request{}, ChatOptions{}, errors.New("messages must not be empty")
	}
	req := anthropic.Request{
		Model:          in.Model,
		Stream:         in.Stream,
		MaxTokens:      cmp.Or(in.MaxCompletionTokens, in.MaxTokens),
		PromptCacheKey: in.PromptCacheKey,
	}
	opts := ChatOptions{IncludeUsage: true, User: in.User}
	if in.StreamOptions != nil && in.StreamOptions.IncludeUsage != nil {
		opts.IncludeUsage = *in.StreamOptions.IncludeUsage
	}

	var b builder
	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			parts, err := parseParts(m.Content)
			if err != nil {
				return req, opts, fmt.Errorf("messages[%d].content: %w", i, err)
			}
			for _, p := range parts {
				if p.Type == "text" {
					req.System = append(req.System, p)
				}
			}
		case "user":
			parts, err := parseParts(m.Content)
			if err != nil {
				return req, opts, fmt.Errorf("messages[%d].content: %w", i, err)
			}
			b.add("user", parts...)
		case "assistant":
			parts, err := parseParts(m.Content)
			if err != nil {
				return req, opts, fmt.Errorf("messages[%d].content: %w", i, err)
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, toolUse(tc.ID, tc.Function.Name, tc.Function.Arguments))
			}
			if fc := m.FunctionCall; fc != nil {
				parts = append(parts, toolUse(fc.Name, fc.Name, fc.Arguments))
			}
			b.add("assistant", parts...)
		case "tool", "function":
			parts, err := parseParts(m.Content)
			if err != nil {
				return req, opts, fmt.Errorf("messages[%d].content: %w", i, err)
			}
			b.add("user", toolResult(cmp.Or(m.ToolCallID, m.Name), parts))
		default:
			return req, opts, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(b.msgs) == 0 {
		return req, opts, errors.New("messages must contain at least one non-system message")
	}
	req.Messages = b.msgs

	for i, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			if hosted == HostedDrop {
				opts.Dropped = append(opts.Dropped, fmt.Sprintf("tools[%d]: %s", i, t.Type))
				continue
			}
			return req, opts, hostedErr(fmt.Sprintf("tools[%d]", i), t.Type)
		}
		req.Tools = append(req.Tools, tool(t.Function.Name, t.Function.Description, t.Function.Parameters))
	}
	req.ToolChoice = toolChoice(in.ToolChoice)
	effort := in.ReasoningEffort
	if effort == "" && in.Reasoning != nil {
		effort = in.Reasoning.Effort
	}
	setEffort(&req, effort)
	return req, opts, nil
}

// ---- Responses ----

type responsesRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions"`
	Input              json.RawMessage `json:"input"`
	Tools              []respToolDef   `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	MaxOutputTokens    int             `json:"max_output_tokens"`
	Reasoning          *reasoningOpt   `json:"reasoning"`
	Stream             bool            `json:"stream"`
	PromptCacheKey     string          `json:"prompt_cache_key"`
	User               string          `json:"user"`
	PreviousResponseID string          `json:"previous_response_id"`
}

type respItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`     // custom_tool_call
	Namespace string          `json:"namespace"` // 带 namespace 的 function_call
	Output    json.RawMessage `json:"output"`
}

// FromResponses 把 POST /v1/responses 的请求体转成 anthropic.Request。
// hosted 是内置工具（web_search 等）的处理：HostedReject（默认，空串同）或 HostedDrop。
func FromResponses(raw []byte, hosted string) (anthropic.Request, ResponsesOptions, error) {
	var in responsesRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return anthropic.Request{}, ResponsesOptions{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	opts := ResponsesOptions{User: in.User, Tools: ToolKinds{}}
	var extraTools []respToolDef // additional_tools 输入项里的
	if in.PreviousResponseID != "" {
		return anthropic.Request{}, opts, ErrPreviousResponse
	}
	req := anthropic.Request{
		Model:          in.Model,
		Stream:         in.Stream,
		MaxTokens:      in.MaxOutputTokens,
		PromptCacheKey: in.PromptCacheKey,
	}
	if in.Instructions != "" {
		req.System = append(req.System, anthropic.Block{Type: "text", Text: in.Instructions})
	}

	var b builder
	input := bytes.TrimSpace(in.Input)
	switch {
	case len(input) == 0 || bytes.Equal(input, []byte("null")):
	case input[0] == '"':
		var s string
		if err := json.Unmarshal(input, &s); err != nil {
			return req, opts, fmt.Errorf("input: %w", err)
		}
		b.add("user", textBlocks(s)...)
	default:
		var items []respItem
		if err := json.Unmarshal(input, &items); err != nil {
			return req, opts, fmt.Errorf("input: %w", err)
		}
		var rawItems []json.RawMessage
		_ = json.Unmarshal(input, &rawItems)
		for i, it := range items {
			typ := it.Type
			if typ == "" && it.Role != "" {
				typ = "message"
			}
			switch typ {
			case "message":
				parts, err := parseParts(it.Content)
				if err != nil {
					return req, opts, fmt.Errorf("input[%d].content: %w", i, err)
				}
				switch it.Role {
				case "system", "developer":
					for _, p := range parts {
						if p.Type == "text" {
							req.System = append(req.System, p)
						}
					}
				case "user", "assistant":
					b.add(it.Role, parts...)
				default:
					return req, opts, fmt.Errorf("input[%d]: unsupported role %q", i, it.Role)
				}
			case "function_call":
				b.add("assistant", toolUse(cmp.Or(it.CallID, it.ID), callName(it.Namespace, it.Name), it.Arguments))
			case "custom_tool_call":
				b.add("assistant", toolUse(cmp.Or(it.CallID, it.ID), callName(it.Namespace, it.Name), customCallArgs(it.Input)))
			case "function_call_output", "custom_tool_call_output":
				parts, err := parseParts(it.Output)
				if err != nil {
					return req, opts, fmt.Errorf("input[%d].output: %w", i, err)
				}
				b.add("user", toolResult(it.CallID, parts))
			case "additional_tools":
				ts, err := additionalTools(rawItems[i])
				if err != nil {
					return req, opts, fmt.Errorf("input[%d]: %w", i, err)
				}
				extraTools = append(extraTools, ts...)
			case "reasoning":
				// 历史 reasoning 不发给上游（与 Anthropic 早先轮次的 thinking 一致）
			default:
				// 内置工具的历史调用（web_search_call 等）与其它不认识的项：不悄悄丢，说清楚
				if hosted == HostedDrop && (strings.HasSuffix(typ, "_call") || strings.HasSuffix(typ, "_call_output")) {
					opts.Dropped = append(opts.Dropped, fmt.Sprintf("input[%d]: %s", i, typ))
					continue
				}
				return req, opts, fmt.Errorf("input[%d]: item type %q cannot be sent through Kiro", i, typ)
			}
		}
	}
	if len(b.msgs) == 0 {
		return req, opts, errors.New("input must contain at least one message")
	}
	req.Messages = b.msgs

	tools, err := convertTools(in.Tools, "tools", opts.Tools, hosted, &opts.Dropped)
	if err != nil {
		return req, opts, err
	}
	extra, err := convertTools(extraTools, "additional_tools", opts.Tools, hosted, &opts.Dropped)
	if err != nil {
		return req, opts, err
	}
	seen := map[string]bool{}
	for _, t := range append(tools, extra...) {
		if seen[t.Name] {
			continue // 同名工具只声明一次
		}
		seen[t.Name] = true
		req.Tools = append(req.Tools, t)
	}
	req.ToolChoice = toolChoice(in.ToolChoice)
	if in.Reasoning != nil {
		setEffort(&req, in.Reasoning.Effort)
	}
	return req, opts, nil
}

// ---- 共用 ----

// builder 按角色合并相邻消息；空内容不成消息。
type builder struct{ msgs []anthropic.Message }

func (b *builder) add(role string, blocks ...anthropic.Block) {
	if len(blocks) == 0 {
		return
	}
	if n := len(b.msgs); n > 0 && b.msgs[n-1].Role == role {
		b.msgs[n-1].Content = append(b.msgs[n-1].Content, blocks...)
		return
	}
	b.msgs = append(b.msgs, anthropic.Message{Role: role, Content: blocks})
}

func textBlocks(s string) []anthropic.Block {
	if s == "" {
		return nil
	}
	return []anthropic.Block{{Type: "text", Text: s}}
}

// part 是 OpenAI 内容片段，兼容 Chat 与 Responses 的写法。
type part struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"` // string 或 {url}
}

// parseParts 解析 string 或片段数组：文字与图片保留，其余忽略；空文字丢弃。
func parseParts(raw json.RawMessage) ([]anthropic.Block, error) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
		return nil, nil
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return textBlocks(s), nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	var out []anthropic.Block
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			out = append(out, textBlocks(p.Text)...)
		case "image_url", "input_image":
			if b, ok := image(imageURL(p.ImageURL)); ok {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

func imageURL(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.URL
}

// image 把 data URL 转成 base64 图片块；远程 URL 不下载，留一句文字说明。
func image(url string) (anthropic.Block, bool) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		media, isB64 := strings.CutSuffix(meta, ";base64")
		if !ok || !isB64 || data == "" {
			return anthropic.Block{}, false
		}
		return anthropic.Block{Type: "image", Source: &anthropic.Source{
			Type: "base64", MediaType: media, Data: data,
		}}, true
	}
	if url == "" {
		return anthropic.Block{}, false
	}
	return anthropic.Block{Type: "text", Text: "[image: " + url + "]"}, true
}

// toolUse 构造 tool_use 块；arguments 不是 JSON 对象时用 {}。
func toolUse(id, name, args string) anthropic.Block {
	input := json.RawMessage("{}")
	if a := strings.TrimSpace(args); strings.HasPrefix(a, "{") && json.Valid([]byte(a)) {
		input = json.RawMessage(a)
	}
	return anthropic.Block{Type: "tool_use", ID: id, Name: name, Input: input}
}

func toolResult(id string, content []anthropic.Block) anthropic.Block {
	return anthropic.Block{Type: "tool_result", ToolUseID: id, Content: content}
}

func tool(name, desc string, params json.RawMessage) anthropic.Tool {
	schema := json.RawMessage(`{"type":"object"}`)
	if p := bytes.TrimSpace(params); len(p) > 0 && !bytes.Equal(p, []byte("null")) {
		schema = p
	}
	return anthropic.Tool{Name: name, Description: desc, InputSchema: schema}
}

// toolChoice 把 OpenAI tool_choice 映射为 Anthropic tool_choice；未知写法返回 nil。
func toolChoice(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		}
		return nil
	}
	var o struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Type != "function" {
		return nil
	}
	name := cmp.Or(o.Function.Name, o.Name)
	if name == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"type": "tool", "name": name})
	return b
}

// setEffort 打开 thinking 并记下 effort；minimal / none / 空 / 未知值都算关闭。
func setEffort(req *anthropic.Request, effort string) {
	switch e := strings.ToLower(strings.TrimSpace(effort)); e {
	case "low", "medium", "high", "xhigh", "max":
		req.OutputConfig = &anthropic.OutputConfig{Effort: e}
		req.Thinking = &anthropic.Thinking{Type: "enabled"}
	}
}
