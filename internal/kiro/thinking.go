package kiro

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"kiro-proxy/internal/anthropic"
)

// ModelRequestSchema 是模型目录中原生参数说明所需的 JSON Schema 子集。
type ModelRequestSchema struct {
	Properties map[string]ModelRequestSchema `json:"properties,omitzero"`
	Enum       []string                      `json:"enum,omitzero"`
	Default    json.RawMessage               `json:"default,omitzero"`
}

// ModelRequestFields 放在请求根部，不属于 prompt 前缀。
type ModelRequestFields struct {
	Thinking     *modelThinking          `json:"thinking,omitzero"`
	OutputConfig *anthropic.OutputConfig `json:"output_config,omitzero"`
	Reasoning    *anthropic.OutputConfig `json:"reasoning,omitzero"`
}

type modelThinking struct {
	Type    string `json:"type"`
	Display string `json:"display,omitzero"`
}

// ThinkingFields 按模型声明选择原生字段。schema 非空但没有思考参数时不发送。
// budget_tokens 在只支持 effort 的模型上映射为强度，未知强度明确报错。
func ThinkingFields(req *anthropic.Request, schema *ModelRequestSchema) (*ModelRequestFields, error) {
	if schema == nil {
		return nil, nil
	}
	var effort string
	if req.OutputConfig != nil {
		effort = req.OutputConfig.Effort
	}
	on := req.Thinking != nil && (req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive")
	disabled := req.Thinking != nil && req.Thinking.Type == "disabled"
	if !on && effort == "" && !disabled {
		return nil, nil
	}
	for _, path := range []string{"output_config", "reasoning"} {
		param := schema.Properties[path].Properties["effort"]
		if len(param.Enum) == 0 {
			continue
		}
		fields := &ModelRequestFields{}
		if disabled || effort == "none" {
			if path == "reasoning" && slices.Contains(param.Enum, "none") {
				fields.Reasoning = &anthropic.OutputConfig{Effort: "none"}
				return fields, nil
			}
			if slices.Contains(schema.Properties["thinking"].Properties["type"].Enum, "disabled") {
				fields.Thinking = &modelThinking{Type: "disabled"}
				return fields, nil
			}
			return nil, fmt.Errorf("model does not support disabling thinking")
		}
		if effort == "" && req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
			switch n := req.Thinking.BudgetTokens; {
			case n <= 10_000:
				effort = "low"
			case n <= 20_000:
				effort = "medium"
			case n <= 30_000:
				effort = "high"
			default:
				effort = "max"
			}
		}
		if effort == "" {
			_ = json.Unmarshal(param.Default, &effort)
			if effort == "" {
				effort = "medium"
			}
		}
		// 较老的 Claude schema 没有 xhigh，降到 high，不提高消耗。
		if effort == "xhigh" && !slices.Contains(param.Enum, effort) && slices.Contains(param.Enum, "high") {
			effort = "high"
		}
		if !slices.Contains(param.Enum, effort) {
			return nil, fmt.Errorf("unsupported thinking effort %q; model accepts %s", effort, strings.Join(param.Enum, ", "))
		}
		config := &anthropic.OutputConfig{Effort: effort}
		if path == "reasoning" {
			fields.Reasoning = config
		} else {
			fields.OutputConfig = config
			thinking := schema.Properties["thinking"]
			if slices.Contains(thinking.Properties["type"].Enum, "adaptive") {
				fields.Thinking = &modelThinking{Type: "adaptive"}
				if slices.Contains(thinking.Properties["display"].Enum, "summarized") {
					fields.Thinking.Display = "summarized"
				}
			}
		}
		return fields, nil
	}
	return nil, nil
}

// Thinks 报告模型是否支持旧版 Kiro 的 prompt 式 thinking：Claude 系与 auto。
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
	if typ == "disabled" || effort == "none" {
		return 0
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
