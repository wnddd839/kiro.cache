package kiro

import (
	"encoding/json"
	"kiro-proxy/internal/anthropic"
	"strings"
	"testing"
)

func TestNativeThinkingFields(t *testing.T) {
	var schema ModelRequestSchema
	if err := json.Unmarshal([]byte(`{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive","disabled"]},"display":{"enum":["summarized","omitted"]}}},"output_config":{"properties":{"effort":{"enum":["low","medium","high","max"],"default":"high"}}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		thinking     *anthropic.Thinking
		effort, want string
		fail         bool
	}{
		{name: "no request", want: "null"},
		{name: "default from catalog", thinking: &anthropic.Thinking{Type: "adaptive"}, want: `{"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"}}`},
		{name: "budget mapped to medium", thinking: &anthropic.Thinking{Type: "enabled", BudgetTokens: 16000}, want: `{"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"medium"}}`},
		{name: "effort wins over budget", thinking: &anthropic.Thinking{Type: "enabled", BudgetTokens: 16000}, effort: "max", want: `{"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"}}`},
		{name: "xhigh falls back to high", effort: "xhigh", want: `{"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"}}`},
		{name: "explicit disabled", thinking: &anthropic.Thinking{Type: "disabled"}, effort: "high", want: `{"thinking":{"type":"disabled"}}`},
		{name: "invalid effort", effort: "unlimited", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &anthropic.Request{Thinking: tc.thinking}
			if tc.effort != "" {
				req.OutputConfig = &anthropic.OutputConfig{Effort: tc.effort}
			}
			fields, err := ThinkingFields(req, &schema)
			if tc.fail {
				if err == nil {
					t.Fatal("invalid effort accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(fields)
			if string(raw) != tc.want {
				t.Fatalf("fields=%s want=%s", raw, tc.want)
			}
		})
	}
	req := parseReq(t, `{"system":"stable prefix","thinking":{"type":"enabled","budget_tokens":16000},"messages":[{"role":"user","content":"hello"}]}`)
	native, _ := build(t, req, BuildOptions{Model: "claude-sonnet-4.6", RequestSchema: &schema, ThinkingBudget: 16000})
	if firstUser(native).Content != "stable prefix\n\nhello" || native.Fields == nil {
		t.Fatalf("native parameters changed prompt: %+v", native)
	}
	legacy, _ := build(t, req, BuildOptions{Model: "claude-sonnet-4.5", ThinkingBudget: 16000})
	if !strings.HasPrefix(firstUser(legacy).Content, "<thinking_mode>") || legacy.Fields != nil {
		t.Fatalf("schema-less model lost legacy thinking: %+v", legacy)
	}
}
