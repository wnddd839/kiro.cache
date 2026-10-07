package openai

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-proxy/internal/turn"
)

// Codex 的新写法：custom（apply_patch）、namespace 分组、additional_tools 输入项都转换，不悄悄丢。
func TestFromResponsesCodexTools(t *testing.T) {
	body := `{"model":"m",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"grammar","syntax":"lark","definition":"start: \"*** Begin Patch\""}},
			{"type":"namespace","name":"mcp__fs","description":"File tools.","tools":[
				{"type":"function","name":"read","description":"Read a file.","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}
			]}
		],
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"late","description":"Loaded later.","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"fix it"},
			{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
			{"type":"custom_tool_call_output","call_id":"c1","output":"done"},
			{"type":"function_call","call_id":"c2","namespace":"mcp__fs","name":"read","arguments":"{\"p\":\"a\"}"},
			{"type":"function_call_output","call_id":"c2","output":"text"}
		]}`
	r, o, err := FromResponses([]byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range r.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "apply_patch,mcp__fs__read,late" {
		t.Fatalf("tools = %v", names)
	}
	if d := r.Tools[0].Description; !strings.Contains(d, "Apply a patch.") || !strings.Contains(d, "lark grammar") || !strings.Contains(d, "*** Begin Patch") {
		t.Errorf("custom description = %q", d)
	}
	if string(r.Tools[0].InputSchema) != string(customInputSchema) {
		t.Errorf("custom schema = %s", r.Tools[0].InputSchema)
	}
	if !o.Tools["apply_patch"].Custom || o.Tools["mcp__fs__read"].Namespace != "mcp__fs" || o.Tools["mcp__fs__read"].Name != "read" {
		t.Errorf("kinds = %+v", o.Tools)
	}
	// 历史：custom_tool_call 的原文包成 {"input": ...}；namespace 调用用全名
	asst := r.Messages[1].Content
	if asst[0].Name != "apply_patch" || string(asst[0].Input) != `{"input":"*** Begin Patch\n*** End Patch"}` {
		t.Errorf("custom history = %+v %s", asst[0], asst[0].Input)
	}
	if r.Messages[3].Content[0].Name != "mcp__fs__read" {
		t.Errorf("namespace history = %+v", r.Messages[3].Content[0])
	}
}

// 转不了的：明确 400（不再悄悄丢）；配置为 drop 时跳过。
func TestHostedToolsRejected(t *testing.T) {
	for _, body := range []string{
		`{"input":"q","tools":[{"type":"web_search"}]}`,
		`{"input":"q","tools":[{"type":"namespace","name":"n","tools":[{"type":"file_search"}]}]}`,
		`{"input":[{"type":"additional_tools","tools":[{"type":"tool_search"}]},{"role":"user","content":"q"}]}`,
		`{"input":[{"role":"user","content":"q"},{"type":"web_search_call","id":"w"}]}`,
	} {
		_, _, err := FromResponses([]byte(body), "")
		if err == nil || !strings.Contains(err.Error(), "Kiro") {
			t.Errorf("%s: err = %v, want a clear rejection", body, err)
		}
		if _, _, err := FromResponses([]byte(body), HostedDrop); err != nil {
			t.Errorf("%s: drop mode err = %v", body, err)
		}
	}
	if _, _, err := FromChat([]byte(`{"messages":[{"role":"user","content":"q"}],"tools":[{"type":"web_search"}]}`), ""); err == nil {
		t.Error("chat hosted tool accepted silently")
	}
	// drop 模式记下跳过了什么（调用方打 debug 日志）
	_, ro, _ := FromResponses([]byte(`{"input":[{"role":"user","content":"q"},{"type":"web_search_call","id":"w"}],"tools":[{"type":"web_search"},{"type":"namespace","name":"n","tools":[{"type":"file_search"}]}]}`), HostedDrop)
	if got := strings.Join(ro.Dropped, ", "); got != "input[1]: web_search_call, tools[0]: web_search, tools[1].tools[0]: file_search" {
		t.Errorf("responses dropped = %q", got)
	}
	_, co, _ := FromChat([]byte(`{"messages":[{"role":"user","content":"q"}],"tools":[{"type":"web_search"}]}`), HostedDrop)
	if got := strings.Join(co.Dropped, ", "); got != "tools[0]: web_search" {
		t.Errorf("chat dropped = %q", got)
	}
}

// 输出：调用还原成 custom_tool_call（input 原文）与 function_call{namespace, 短名}，流式与非流式一致。
func TestResponsesRestoreToolCalls(t *testing.T) {
	kinds := ToolKinds{
		"apply_patch":   {Custom: true, Name: "apply_patch"},
		"mcp__fs__read": {Namespace: "mcp__fs", Name: "read"},
	}
	drive := func(s turn.Sink) {
		s.Begin(turn.Usage{})
		s.ToolStart("c1", "apply_patch")
		s.ToolArgs(`{"input":"*** Begin`)
		s.ToolArgs(` Patch"}`)
		s.ToolStart("c2", "mcp__fs__read")
		s.ToolArgs(`{"p":"a"}`)
		s.End(turn.StopToolUse, turn.Usage{})
	}
	var c turn.Collector
	drive(&c)
	obj := Response(&c.Turn, 1, kinds)
	check := func(where string, out []OutputItem) {
		t.Helper()
		if len(out) != 2 {
			t.Fatalf("%s: output %+v", where, out)
		}
		if out[0].Type != "custom_tool_call" || out[0].Name != "apply_patch" || out[0].Input == nil || *out[0].Input != "*** Begin Patch" || out[0].Arguments != nil {
			t.Errorf("%s: custom = %+v", where, out[0])
		}
		if out[1].Type != "function_call" || out[1].Name != "read" || out[1].Namespace != "mcp__fs" || *out[1].Arguments != `{"p":"a"}` {
			t.Errorf("%s: namespaced = %+v", where, out[1])
		}
	}
	check("non-stream", obj.Output)

	rec := httptest.NewRecorder()
	drive(NewResponsesStream(rec, "resp_1", "m", 1, kinds))
	var final ResponseObject
	var sawCustomDone, sawCustomArgsDelta bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		d, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type     string
			Response ResponseObject
			Delta    string
			Input    string
		}
		_ = json.Unmarshal([]byte(d), &ev)
		switch ev.Type {
		case "response.completed":
			final = ev.Response
		case "response.custom_tool_call_input.done":
			sawCustomDone = ev.Input == "*** Begin Patch"
		case "response.function_call_arguments.delta":
			sawCustomArgsDelta = sawCustomArgsDelta || strings.Contains(ev.Delta, "Begin")
		}
	}
	if !sawCustomDone || sawCustomArgsDelta {
		t.Errorf("stream: custom input done=%v, leaked wrapped args as function delta=%v", sawCustomDone, sawCustomArgsDelta)
	}
	check("stream", final.Output)
}
