package openai

import (
	"encoding/json"
	"errors"
	"testing"

	"kiro-proxy/internal/anthropic"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFromChat(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		check func(t *testing.T, r anthropic.Request, o ChatOptions)
	}{
		{"system merge", `{"model":"m","stream":true,"messages":[
			{"role":"system","content":"a"},
			{"role":"developer","content":[{"type":"text","text":"b"}]},
			{"role":"user","content":"hi"},
			{"role":"user","content":"again"}]}`,
			func(t *testing.T, r anthropic.Request, o ChatOptions) {
				if r.Model != "m" || !r.Stream {
					t.Errorf("model/stream = %q %v", r.Model, r.Stream)
				}
				if got := mustJSON(t, r.System); got != `[{"type":"text","text":"a"},{"type":"text","text":"b"}]` {
					t.Errorf("system = %s", got)
				}
				if len(r.Messages) != 1 || len(r.Messages[0].Content) != 2 {
					t.Errorf("messages = %s", mustJSON(t, r.Messages))
				}
				if !o.IncludeUsage {
					t.Error("include_usage default should be true")
				}
			}},
		{"images", `{"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
			{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
			func(t *testing.T, r anthropic.Request, _ ChatOptions) {
				want := `[{"role":"user","content":[{"type":"text","text":"look"},` +
					`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},` +
					`{"type":"text","text":"[image: https://x/y.png]"}]}]`
				if got := mustJSON(t, r.Messages); got != want {
					t.Errorf("got  %s\nwant %s", got, want)
				}
			}},
		{"tool calls and tool merge", `{"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"reasoning_content":"x","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},
				{"id":"c2","type":"function","function":{"name":"g","arguments":"oops"}}]},
			{"role":"tool","tool_call_id":"c1","content":"r1"},
			{"role":"tool","tool_call_id":"c2","content":[{"type":"text","text":"r2"}]},
			{"role":"function","name":"legacy","content":"r3"}]}`,
			func(t *testing.T, r anthropic.Request, _ ChatOptions) {
				want := `[{"role":"user","content":[{"type":"text","text":"q"}]},` +
					`{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f","input":{"a":1}},` +
					`{"type":"tool_use","id":"c2","name":"g","input":{}}]},` +
					`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"r1"}]},` +
					`{"type":"tool_result","tool_use_id":"c2","content":[{"type":"text","text":"r2"}]},` +
					`{"type":"tool_result","tool_use_id":"legacy","content":[{"type":"text","text":"r3"}]}]}]`
				if got := mustJSON(t, r.Messages); got != want {
					t.Errorf("got  %s\nwant %s", got, want)
				}
			}},
		{"tools and limits", `{"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}},
				{"type":"function","function":{"name":"g"}}],
			"max_tokens":10,"max_completion_tokens":20,"prompt_cache_key":"k","user":"u",
			"stream_options":{"include_usage":false},"foo":1}`,
			func(t *testing.T, r anthropic.Request, o ChatOptions) {
				want := `[{"name":"f","description":"d","input_schema":{"type":"object","properties":{}}},{"name":"g","input_schema":{"type":"object"}}]`
				if got := mustJSON(t, r.Tools); got != want {
					t.Errorf("tools = %s", got)
				}
				if r.MaxTokens != 20 || r.PromptCacheKey != "k" || o.User != "u" || o.IncludeUsage {
					t.Errorf("max=%d key=%q user=%q usage=%v", r.MaxTokens, r.PromptCacheKey, o.User, o.IncludeUsage)
				}
				if r.Thinking != nil || r.OutputConfig != nil {
					t.Error("thinking should be off")
				}
			}},
		{"effort on", `{"messages":[{"role":"user","content":"q"}],"reasoning_effort":"high"}`,
			func(t *testing.T, r anthropic.Request, _ ChatOptions) {
				if r.Thinking == nil || r.Thinking.Type != "enabled" || r.OutputConfig == nil || r.OutputConfig.Effort != "high" {
					t.Errorf("thinking=%+v output=%+v", r.Thinking, r.OutputConfig)
				}
			}},
		{"effort object", `{"messages":[{"role":"user","content":"q"}],"reasoning":{"effort":"xhigh"}}`,
			func(t *testing.T, r anthropic.Request, _ ChatOptions) {
				if r.OutputConfig == nil || r.OutputConfig.Effort != "xhigh" {
					t.Errorf("output=%+v", r.OutputConfig)
				}
			}},
		{"effort minimal off", `{"messages":[{"role":"user","content":"q"}],"reasoning_effort":"minimal"}`,
			func(t *testing.T, r anthropic.Request, _ ChatOptions) {
				if r.Thinking != nil || r.OutputConfig != nil {
					t.Error("minimal should be off")
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, o, err := FromChat([]byte(tt.body), "")
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, r, o)
		})
	}
}

func TestToolChoice(t *testing.T) {
	tests := []struct{ in, want string }{
		{`"none"`, `{"type":"none"}`},
		{`"auto"`, `{"type":"auto"}`},
		{`"required"`, `{"type":"any"}`},
		{`{"type":"function","function":{"name":"f"}}`, `{"name":"f","type":"tool"}`},
		{`{"type":"function","name":"g"}`, `{"name":"g","type":"tool"}`},
		{`"weird"`, ``},
	}
	for _, tt := range tests {
		r, _, err := FromChat([]byte(`{"messages":[{"role":"user","content":"q"}],"tool_choice":`+tt.in+`}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if string(r.ToolChoice) != tt.want {
			t.Errorf("chat %s → %s, want %s", tt.in, r.ToolChoice, tt.want)
		}
		r, _, err = FromResponses([]byte(`{"input":"q","tool_choice":`+tt.in+`}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if string(r.ToolChoice) != tt.want {
			t.Errorf("responses %s → %s, want %s", tt.in, r.ToolChoice, tt.want)
		}
	}
}

func TestFromChatErrors(t *testing.T) {
	for _, body := range []string{`{`, `{"messages":[]}`, `{}`, `{"messages":[{"role":"system","content":"s"}]}`} {
		if _, _, err := FromChat([]byte(body), ""); err == nil {
			t.Errorf("%s: want error", body)
		}
	}
}

func TestFromResponses(t *testing.T) {
	body := `{"model":"m","instructions":"sys","stream":true,"max_output_tokens":99,
		"reasoning":{"effort":"medium"},"prompt_cache_key":"k","user":"u","store":false,"metadata":{"a":"b"},
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"input":[
			{"role":"developer","content":"dev"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},
				{"type":"input_image","image_url":"data:image/jpeg;base64,AAA"},
				{"type":"input_image","image_url":"http://h/i.jpg"}]},
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}"},
			{"type":"function_call","call_id":"c2","name":"f","arguments":""},
			{"type":"function_call_output","call_id":"c1","output":"o1"},
			{"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"o2"}]}
		]}`
	r, o, err := FromResponses([]byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, r.System); got != `[{"type":"text","text":"sys"},{"type":"text","text":"dev"}]` {
		t.Errorf("system = %s", got)
	}
	want := `[{"role":"user","content":[{"type":"text","text":"hi"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAA"}},` +
		`{"type":"text","text":"[image: http://h/i.jpg]"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"},` +
		`{"type":"tool_use","id":"c1","name":"f","input":{"x":1}},` +
		`{"type":"tool_use","id":"c2","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"o1"}]},` +
		`{"type":"tool_result","tool_use_id":"c2","content":[{"type":"text","text":"o2"}]}]}]`
	if got := mustJSON(t, r.Messages); got != want {
		t.Errorf("messages\ngot  %s\nwant %s", got, want)
	}
	if got := mustJSON(t, r.Tools); got != `[{"name":"f","description":"d","input_schema":{"type":"object"}}]` {
		t.Errorf("tools = %s", got)
	}
	if r.Model != "m" || !r.Stream || r.MaxTokens != 99 || r.PromptCacheKey != "k" || o.User != "u" {
		t.Errorf("scalars: %+v %+v", r, o)
	}
	if r.OutputConfig == nil || r.OutputConfig.Effort != "medium" || r.Thinking == nil {
		t.Errorf("effort: %+v %+v", r.OutputConfig, r.Thinking)
	}
}

func TestFromResponsesStringInput(t *testing.T) {
	r, _, err := FromResponses([]byte(`{"input":"hello","reasoning":{"effort":"none"}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, r.Messages); got != `[{"role":"user","content":[{"type":"text","text":"hello"}]}]` {
		t.Errorf("messages = %s", got)
	}
	if r.Thinking == nil || r.Thinking.Type != "disabled" || r.OutputConfig == nil || r.OutputConfig.Effort != "none" {
		t.Error("effort none must remain an explicit native disable")
	}
}

func TestFromResponsesErrors(t *testing.T) {
	_, _, err := FromResponses([]byte(`{"input":"q","previous_response_id":"resp_1"}`), "")
	if !errors.Is(err, ErrPreviousResponse) || err.Error() != "previous_response_id is not supported; send the full input" {
		t.Errorf("err = %v", err)
	}
	for _, body := range []string{`nope`, `{}`, `{"input":[]}`, `{"input":[{"type":"reasoning"}]}`} {
		if _, _, err := FromResponses([]byte(body), ""); err == nil {
			t.Errorf("%s: want error", body)
		}
	}
}
