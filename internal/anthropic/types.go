// Package anthropic 是下游 Anthropic Messages API 的请求 / 响应形状。
// 只覆盖分发层要读写的字段；未知字段直接丢弃。
package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Request 是 POST /v1/messages 的请求体。
type Request struct {
	Model        string          `json:"model"`
	Messages     []Message       `json:"messages"`
	System       Content         `json:"system,omitzero"`
	Tools        []Tool          `json:"tools,omitzero"`
	ToolChoice   json.RawMessage `json:"tool_choice,omitzero"`
	MaxTokens    int             `json:"max_tokens,omitzero"`
	Stream       bool            `json:"stream,omitzero"`
	Thinking     *Thinking       `json:"thinking,omitzero"`
	OutputConfig *OutputConfig   `json:"output_config,omitzero"`
	Metadata     *Metadata       `json:"metadata,omitzero"`
	// CacheControl 是顶层的自动缓存断点：落在最后一块上，随对话增长后移。只用于本地计量。
	CacheControl *CacheControl `json:"cache_control,omitzero"`
	// PromptCacheKey 不是 Anthropic 字段；OpenAI 兼容客户端会带，用作会话键。
	PromptCacheKey string `json:"prompt_cache_key,omitzero"`
}

// Thinking 是 extended thinking 开关。
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitzero"`
}

// OutputConfig 承载 effort（low / medium / high / xhigh / max）。
type OutputConfig struct {
	Effort string `json:"effort,omitzero"`
}

// Metadata 里 Claude Code 会塞 session id。
type Metadata struct {
	UserID string `json:"user_id,omitzero"`
}

// Message 是一条对话消息。
type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// Tool 是客户端声明的工具。Type 非空且非 custom 的是服务端工具（web_search 等）。
type Tool struct {
	Type        string          `json:"type,omitzero"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitzero"`
	InputSchema json.RawMessage `json:"input_schema,omitzero"`
	// CacheControl 是 prompt cache 断点；只用于本地计量，不发给 Kiro。
	CacheControl *CacheControl `json:"cache_control,omitzero"`
}

// CacheControl 标记一个 prompt cache 断点：到这里为止的前缀写入缓存。
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitzero"` // "5m" | "1h"
}

// Block 是一个内容块。不同 type 用不同字段。
type Block struct {
	Type string `json:"type"`

	Text string `json:"text,omitzero"`

	// tool_use
	ID    string          `json:"id,omitzero"`
	Name  string          `json:"name,omitzero"`
	Input json.RawMessage `json:"input,omitzero"`

	// tool_result
	ToolUseID string  `json:"tool_use_id,omitzero"`
	Content   Content `json:"content,omitzero"`
	IsError   bool    `json:"is_error,omitzero"`

	// image / document
	Source *Source `json:"source,omitzero"`
	Title  string  `json:"title,omitzero"`

	// thinking
	Thinking  string `json:"thinking,omitzero"`
	Signature string `json:"signature,omitzero"`

	CacheControl *CacheControl `json:"cache_control,omitzero"`
}

// Source 是图片 / 文档来源。
type Source struct {
	Type      string  `json:"type"`
	MediaType string  `json:"media_type,omitzero"`
	Data      string  `json:"data,omitzero"`
	Content   Content `json:"content,omitzero"`
}

// Content 是 string 或 block 数组；字符串解成单个 text block。
type Content []Block

// UnmarshalJSON 接受字符串或数组。
func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || bytes.Equal(b, []byte("null")):
		*c = nil
		return nil
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content{{Type: "text", Text: s}}
		return nil
	}
	var blocks []Block
	if err := json.Unmarshal(b, &blocks); err != nil {
		return err
	}
	*c = blocks
	return nil
}

// Text 拼出所有 text block，用 sep 连接。
func (c Content) Text(sep string) string {
	var parts []string
	for _, b := range c {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, sep)
}

// Response 是非流式响应。
type Response struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []ResponseBlock `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        Usage           `json:"usage"`
}

// ResponseBlock 是响应里的内容块；Input 保持原始 JSON。
type ResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitzero"`
	Thinking  *string         `json:"thinking,omitzero"`
	Signature *string         `json:"signature,omitzero"`
	ID        string          `json:"id,omitzero"`
	Name      string          `json:"name,omitzero"`
	Input     json.RawMessage `json:"input,omitzero"`
}

// Usage 是 token 计数。Credits 是 Kiro 实际扣点（非 Anthropic 标准字段）。
// cache_creation_input_tokens = cache_creation 里两项之和，与 Anthropic 一致。
type Usage struct {
	InputTokens              int            `json:"input_tokens"`
	OutputTokens             int            `json:"output_tokens"`
	CacheReadInputTokens     int            `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitzero"`
	Credits                  float64        `json:"credits,omitzero"`
}

// CacheCreation 是缓存写入按 TTL 的拆分。
type CacheCreation struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

// ErrorBody 是 Anthropic 风格错误。
type ErrorBody struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是错误内容。
type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ErrorType 把 HTTP 状态映射为 Anthropic error.type。
func ErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402:
		return "billing_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	return "api_error"
}
