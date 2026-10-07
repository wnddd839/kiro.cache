// Package openai 把 OpenAI Chat Completions / Responses 协议接到内部形状：
// 请求转成 anthropic.Request，回复由实现 turn.Sink 的写出器转成 OpenAI 的 SSE 或 JSON。
package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"kiro-proxy/internal/turn"
)

// NewID 返回 prefix + 24 位十六进制随机串。
func NewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// ErrorType 把 HTTP 状态映射为 OpenAI 的 error.type 与 error.code；code 可为空。
func ErrorType(status int) (typ, code string) {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error", "invalid_api_key"
	case status == http.StatusPaymentRequired:
		return "insufficient_quota", "insufficient_quota"
	case status == http.StatusForbidden:
		return "permission_error", ""
	case status == http.StatusNotFound:
		return "not_found_error", ""
	case status == http.StatusTooManyRequests:
		return "rate_limit_error", "rate_limit_exceeded"
	case status >= 500:
		return "server_error", ""
	}
	return "invalid_request_error", ""
}

// ErrorBody 是 OpenAI 风格错误。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是错误内容；Code 为空时序列化为 null。
type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

func errorBody(status int, message string) ErrorBody {
	typ, code := ErrorType(status)
	d := ErrorDetail{Message: message, Type: typ}
	if code != "" {
		d.Code = &code
	}
	return ErrorBody{Error: d}
}

// WriteError 写一个 OpenAI 风格的 JSON 错误响应。
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody(status, message))
}

// finishReason 把内部 stop 原因映射为 Chat Completions 的 finish_reason。
func finishReason(stop string) string {
	switch stop {
	case turn.StopToolUse:
		return "tool_calls"
	case turn.StopMaxTokens:
		return "length"
	case turn.StopRefusal:
		return "content_filter"
	}
	return "stop"
}

// Usage 是 Chat Completions 的 usage，外加非标准的缓存 / credits 字段。
type Usage struct {
	PromptTokens            int               `json:"prompt_tokens"`
	CompletionTokens        int               `json:"completion_tokens"`
	TotalTokens             int               `json:"total_tokens"`
	PromptTokensDetails     PromptDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails CompletionDetails `json:"completion_tokens_details"`

	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	Credits                  float64 `json:"credits"`
}

// PromptDetails 是输入侧明细。
type PromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CompletionDetails 是输出侧明细。
type CompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ChatUsage 把内部记账转成 Chat Completions usage。
func ChatUsage(u turn.Usage) Usage {
	p := u.PromptTokens()
	return Usage{
		PromptTokens:             p,
		CompletionTokens:         u.Output,
		TotalTokens:              p + u.Output,
		PromptTokensDetails:      PromptDetails{CachedTokens: u.CacheRead},
		CompletionTokensDetails:  CompletionDetails{ReasoningTokens: u.Reasoning},
		CacheCreationInputTokens: u.CacheWrite,
		CacheReadInputTokens:     u.CacheRead,
		Credits:                  u.Credits,
	}
}

// RespUsage 是 Responses API 的 usage，外加非标准的缓存 / credits 字段。
type RespUsage struct {
	InputTokens         int               `json:"input_tokens"`
	InputTokensDetails  PromptDetails     `json:"input_tokens_details"`
	OutputTokens        int               `json:"output_tokens"`
	OutputTokensDetails CompletionDetails `json:"output_tokens_details"`
	TotalTokens         int               `json:"total_tokens"`

	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	Credits                  float64 `json:"credits"`
}

// ResponsesUsage 把内部记账转成 Responses usage。
func ResponsesUsage(u turn.Usage) RespUsage {
	p := u.PromptTokens()
	return RespUsage{
		InputTokens:              p,
		InputTokensDetails:       PromptDetails{CachedTokens: u.CacheRead},
		OutputTokens:             u.Output,
		OutputTokensDetails:      CompletionDetails{ReasoningTokens: u.Reasoning},
		TotalTokens:              p + u.Output,
		CacheCreationInputTokens: u.CacheWrite,
		CacheReadInputTokens:     u.CacheRead,
		Credits:                  u.Credits,
	}
}

// sse 是 SSE 写出器：首次写入时发响应头，每帧之后 flush。
type sse struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func newSSE(w http.ResponseWriter) sse {
	return sse{w: w, rc: http.NewResponseController(w)}
}

func (s *sse) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

func (s *sse) write(p []byte) {
	s.start()
	_, _ = s.w.Write(p)
	_ = s.rc.Flush()
}

// data 写一个匿名事件 `data: {...}`。
func (s *sse) data(v any) {
	b, _ := json.Marshal(v)
	s.write(fmt.Appendf(nil, "data: %s\n\n", b))
}

// event 写一个命名事件。
func (s *sse) event(typ string, v any) {
	b, _ := json.Marshal(v)
	s.write(fmt.Appendf(nil, "event: %s\ndata: %s\n\n", typ, b))
}

func (s *sse) comment() { s.write([]byte(": ping\n\n")) }

func (s *sse) doneMarker() { s.write([]byte("data: [DONE]\n\n")) }
