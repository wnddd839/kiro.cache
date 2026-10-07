package kiro

import (
	"crypto/rand"
	"fmt"
	"strings"

	"kiro-proxy/internal/anthropic"
)

// Thinks 报告模型是否支持 Kiro 的 prompt 式 thinking：Claude 系与 auto。
func Thinks(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "claude") || m == "auto"
}

// ThinkingBudget 是本次请求的 thinking 预算；0 表示不开。
// 规则对齐插件：effort 优先，其次 budget_tokens，默认 20k。
func ThinkingBudget(req *anthropic.Request, model string) int {
	var effort string
	if req.OutputConfig != nil {
		effort = req.OutputConfig.Effort
	}
	var typ string
	if req.Thinking != nil {
		typ = req.Thinking.Type
	}
	on := typ == "enabled" || typ == "adaptive" || (effort != "" && effort != "low")
	if !on || !Thinks(model) {
		return 0
	}
	switch effort {
	case "low":
		return 10_000
	case "high":
		return 30_000
	case "xhigh", "max":
		return 50_000
	}
	if effort == "" && req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		return req.Thinking.BudgetTokens
	}
	return 20_000
}

// NewUUID 生成 RFC 4122 v4 UUID。
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read 在 Go 1.24+ 不会失败
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
