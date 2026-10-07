package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"kiro-proxy/internal/turn"
)

var usage = turn.Usage{Input: 10, CacheRead: 80, CacheWrite: 5, Output: 30, Reasoning: 7, Credits: 0.25, CostUSD: 0.0123}

// script 按固定顺序驱动 Sink，同时用 Collector 收一份 Turn。
func script(s turn.Sink, stop string) {
	s.Begin(turn.Usage{Input: 10, CacheRead: 80, CacheWrite: 5})
	s.Thinking("hmm")
	s.Signature("sig")
	s.Ping()
	s.Text("hello")
	s.ToolStart("call_1", "f")
	s.ToolArgs(`{"a":`)
	s.ToolArgs(`1}`)
	s.End(stop, usage)
}

func checkHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("X-Accel-Buffering") != "no" {
		t.Errorf("headers = %v", h)
	}
	if !rec.Flushed {
		t.Error("not flushed")
	}
}

type frame struct {
	event   string
	data    string
	comment bool
}

func frames(t *testing.T, body string) []frame {
	t.Helper()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("body not terminated: %q", body)
	}
	var out []frame
	for raw := range strings.SplitSeq(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		var f frame
		for line := range strings.SplitSeq(raw, "\n") {
			switch {
			case strings.HasPrefix(line, ": "):
				f.comment = true
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("bad line %q", line)
			}
		}
		out = append(out, f)
	}
	return out
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return m
}

// ---- Chat ----

func TestChatStream(t *testing.T) {
	rec := httptest.NewRecorder()
	script(NewChatStream(rec, "chatcmpl-1", "m", 42, true), turn.StopToolUse)
	checkHeaders(t, rec)

	fs := frames(t, rec.Body.String())
	// role, reasoning, ping, content, tool start, args x2, finish, usage, [DONE]
	if len(fs) != 10 {
		t.Fatalf("frames = %d:\n%s", len(fs), rec.Body.String())
	}
	if !fs[2].comment {
		t.Error("frame 2 should be ping comment")
	}
	if fs[9].data != "[DONE]" {
		t.Errorf("last = %q", fs[9].data)
	}

	deltas := []string{
		`{"role":"assistant","content":""}`,
		`{"reasoning_content":"hmm"}`,
		``,
		`{"content":"hello"}`,
		`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]}`,
		`{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}`,
		`{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}`,
		`{}`,
	}
	for i, want := range deltas {
		if want == "" {
			continue
		}
		m := decode(t, fs[i].data)
		if m["id"] != "chatcmpl-1" || m["object"] != "chat.completion.chunk" || m["created"] != float64(42) || m["model"] != "m" {
			t.Errorf("frame %d envelope: %s", i, fs[i].data)
		}
		ch := m["choices"].([]any)[0].(map[string]any)
		if ch["index"] != float64(0) {
			t.Errorf("frame %d choice index: %s", i, fs[i].data)
		}
		// 按原始文本比较，字段顺序也要稳定
		if !strings.Contains(fs[i].data, `"delta":`+want+`,`) {
			t.Errorf("frame %d = %s, want delta %s", i, fs[i].data, want)
		}
		fr, ok := ch["finish_reason"]
		if !ok {
			t.Errorf("frame %d: finish_reason missing", i)
		}
		if i < 7 && fr != nil || i == 7 && fr != "tool_calls" {
			t.Errorf("frame %d finish_reason = %v", i, fr)
		}
	}
	// 原始文本里 index:0 必须显式出现
	if !strings.Contains(fs[5].data, `"index":0,"function"`) {
		t.Errorf("index 0 missing: %s", fs[5].data)
	}

	u := decode(t, fs[8].data)
	if got := mustJSON(t, u["choices"]); got != `[]` {
		t.Errorf("usage chunk choices = %s", got)
	}
	want := `{"cache_creation_input_tokens":5,"cache_read_input_tokens":80,"completion_tokens":30,` +
		`"completion_tokens_details":{"reasoning_tokens":7},"credits":0.25,` +
		`"prompt_tokens":95,"prompt_tokens_details":{"cached_tokens":80},"total_tokens":125}`
	if got := mustJSON(t, u["usage"]); got != want {
		t.Errorf("usage = %s\nwant    %s", got, want)
	}
}

func TestChatStreamSecondToolIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewChatStream(rec, "id", "m", 1, false)
	s.Begin(turn.Usage{})
	s.ToolStart("a", "f")
	s.ToolStart("b", "g")
	s.ToolArgs("{}")
	s.End(turn.StopToolUse, turn.Usage{})
	fs := frames(t, rec.Body.String())
	if len(fs) != 6 { // role, start a, start b, args, finish, [DONE]；无 usage chunk
		t.Fatalf("frames = %d:\n%s", len(fs), rec.Body.String())
	}
	if !strings.Contains(fs[2].data, `"index":1,"id":"b"`) || !strings.Contains(fs[3].data, `"index":1,`) {
		t.Errorf("second tool index: %s / %s", fs[2].data, fs[3].data)
	}
}

func TestChatStreamFinishReasons(t *testing.T) {
	for stop, want := range map[string]string{
		turn.StopEndTurn: "stop", turn.StopToolUse: "tool_calls", turn.StopMaxTokens: "length", turn.StopRefusal: "content_filter",
	} {
		rec := httptest.NewRecorder()
		s := NewChatStream(rec, "id", "m", 1, false)
		s.Begin(turn.Usage{})
		s.End(stop, turn.Usage{})
		fs := frames(t, rec.Body.String())
		if !strings.Contains(fs[1].data, `"finish_reason":"`+want+`"`) {
			t.Errorf("%s: %s", stop, fs[1].data)
		}
	}
}

func TestChatStreamFail(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewChatStream(rec, "id", "m", 1, true)
	s.Begin(turn.Usage{})
	s.Text("par")
	s.Fail(http.StatusTooManyRequests, "slow down")
	fs := frames(t, rec.Body.String())
	if len(fs) != 4 {
		t.Fatalf("frames = %d", len(fs))
	}
	if fs[2].data != `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}` {
		t.Errorf("error frame = %s", fs[2].data)
	}
	if fs[3].data != "[DONE]" {
		t.Errorf("last = %q", fs[3].data)
	}
}

func collect(stop string) *turn.Turn {
	var c turn.Collector
	script(&c, stop)
	c.Turn.ID, c.Turn.Model = "x_1", "m"
	return &c.Turn
}

func TestChatCompletion(t *testing.T) {
	r := ChatCompletion(collect(turn.StopToolUse), 7)
	got := mustJSON(t, r)
	want := `{"id":"x_1","object":"chat.completion","created":7,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"hmm",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":95,"completion_tokens":30,"total_tokens":125,"prompt_tokens_details":{"cached_tokens":80},` +
		`"completion_tokens_details":{"reasoning_tokens":7},"cache_creation_input_tokens":5,"cache_read_input_tokens":80,"credits":0.25}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// 只有工具调用：content 为 null
	tt := &turn.Turn{ID: "x", Blocks: []turn.Block{{Kind: "tool_use", ID: "c", Name: "f", Input: json.RawMessage(`{}`)}}, Stop: turn.StopToolUse}
	if got := mustJSON(t, ChatCompletion(tt, 1).Choices[0].Message); got != `{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}` {
		t.Errorf("tool-only = %s", got)
	}
	// 空回复：content 为 ""
	empty := &turn.Turn{ID: "x", Blocks: []turn.Block{}, Stop: turn.StopEndTurn}
	if got := mustJSON(t, ChatCompletion(empty, 1).Choices[0]); got != `{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}` {
		t.Errorf("empty = %s", got)
	}
}

// ---- Responses ----

func TestResponsesStream(t *testing.T) {
	rec := httptest.NewRecorder()
	script(NewResponsesStream(rec, "resp_1", "m", 42, nil), turn.StopToolUse)
	checkHeaders(t, rec)

	var events []string
	var datas []map[string]any
	pings := 0
	for _, f := range frames(t, rec.Body.String()) {
		if f.comment {
			pings++
			continue
		}
		m := decode(t, f.data)
		if m["type"] != f.event {
			t.Errorf("type %v != event %s", m["type"], f.event)
		}
		if m["sequence_number"] != float64(len(events)) {
			t.Errorf("%s sequence_number = %v, want %d", f.event, m["sequence_number"], len(events))
		}
		events = append(events, f.event)
		datas = append(datas, m)
	}
	if pings != 1 {
		t.Errorf("pings = %d", pings)
	}
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(events, "\n"), strings.Join(want, "\n"))
	}

	created := datas[0]["response"].(map[string]any)
	if created["status"] != "in_progress" || mustJSON(t, created["output"]) != "[]" || created["id"] != "resp_1" {
		t.Errorf("created = %v", created)
	}
	rsAdded := datas[2]["item"].(map[string]any)
	if rsAdded["type"] != "reasoning" || !strings.HasPrefix(rsAdded["id"].(string), "rs_") || mustJSON(t, rsAdded["summary"]) != "[]" {
		t.Errorf("reasoning added = %v", rsAdded)
	}
	if datas[2]["output_index"] != float64(0) || datas[4]["delta"] != "hmm" || datas[4]["summary_index"] != float64(0) {
		t.Errorf("reasoning delta = %v", datas[4])
	}
	if got := mustJSON(t, datas[7]["item"].(map[string]any)["summary"]); got != `[{"text":"hmm","type":"summary_text"}]` {
		t.Errorf("reasoning done summary = %s", got)
	}

	msgAdded := datas[8]["item"].(map[string]any)
	if mustJSON(t, msgAdded) != `{"content":[],"id":"`+msgAdded["id"].(string)+`","role":"assistant","status":"in_progress","type":"message"}` ||
		!strings.HasPrefix(msgAdded["id"].(string), "msg_") || datas[8]["output_index"] != float64(1) {
		t.Errorf("message added = %v", datas[8])
	}
	if mustJSON(t, datas[9]["part"]) != `{"annotations":[],"text":"","type":"output_text"}` {
		t.Errorf("content_part.added = %v", datas[9])
	}
	if d := datas[10]; d["item_id"] != msgAdded["id"] || d["content_index"] != float64(0) || d["delta"] != "hello" || d["output_index"] != float64(1) {
		t.Errorf("output_text.delta = %v", d)
	}
	if datas[11]["text"] != "hello" || datas[13]["item"].(map[string]any)["status"] != "completed" {
		t.Errorf("message close = %v / %v", datas[11], datas[13])
	}

	fc := datas[14]["item"].(map[string]any)
	if fc["type"] != "function_call" || !strings.HasPrefix(fc["id"].(string), "fc_") || fc["call_id"] != "call_1" ||
		fc["name"] != "f" || fc["arguments"] != "" || fc["status"] != "in_progress" || datas[14]["output_index"] != float64(2) {
		t.Errorf("function_call added = %v", datas[14])
	}
	if datas[15]["delta"] != `{"a":` || datas[16]["delta"] != `1}` || datas[15]["item_id"] != fc["id"] {
		t.Errorf("args deltas = %v / %v", datas[15], datas[16])
	}
	if datas[17]["arguments"] != `{"a":1}` || datas[18]["item"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Errorf("args done = %v", datas[17])
	}

	done := datas[19]["response"].(map[string]any)
	if done["status"] != "completed" || len(done["output"].([]any)) != 3 || done["output_text"] != "hello" || done["incomplete_details"] != nil {
		t.Errorf("completed = %v", done)
	}
	u := done["usage"].(map[string]any)
	if u["input_tokens"] != float64(95) || u["output_tokens"] != float64(30) || u["total_tokens"] != float64(125) ||
		mustJSON(t, u["input_tokens_details"]) != `{"cached_tokens":80}` || mustJSON(t, u["output_tokens_details"]) != `{"reasoning_tokens":7}` ||
		u["credits"] != 0.25 || u["cache_creation_input_tokens"] != float64(5) {
		t.Errorf("usage = %v", u)
	}
}

func TestResponsesStreamIncomplete(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewResponsesStream(rec, "resp_1", "m", 1, nil)
	s.Begin(turn.Usage{})
	s.Text("cut")
	s.End(turn.StopMaxTokens, turn.Usage{})
	fs := frames(t, rec.Body.String())
	last := fs[len(fs)-1]
	if last.event != "response.incomplete" {
		t.Fatalf("last event = %s", last.event)
	}
	r := decode(t, last.data)["response"].(map[string]any)
	if r["status"] != "incomplete" || mustJSON(t, r["incomplete_details"]) != `{"reason":"max_output_tokens"}` {
		t.Errorf("response = %v", r)
	}
}

func TestResponsesStreamFail(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewResponsesStream(rec, "resp_1", "m", 1, nil)
	s.Begin(turn.Usage{})
	s.Fail(http.StatusInternalServerError, "boom")
	fs := frames(t, rec.Body.String())
	if len(fs) != 3 || fs[2].event != "response.failed" {
		t.Fatalf("frames = %+v", fs)
	}
	m := decode(t, fs[2].data)
	if m["sequence_number"] != float64(2) {
		t.Errorf("seq = %v", m["sequence_number"])
	}
	r := m["response"].(map[string]any)
	if r["status"] != "failed" || mustJSON(t, r["error"]) != `{"code":"server_error","message":"boom"}` {
		t.Errorf("response = %v", r)
	}
}

func TestResponse(t *testing.T) {
	r := Response(collect(turn.StopToolUse), 9, nil)
	if r.ID != "x_1" || r.Object != "response" || r.CreatedAt != 9 || r.Status != "completed" || r.Model != "m" || r.OutputText != "hello" {
		t.Errorf("header = %+v", r)
	}
	if len(r.Output) != 3 {
		t.Fatalf("output = %+v", r.Output)
	}
	rs, msg, fc := r.Output[0], r.Output[1], r.Output[2]
	if rs.Type != "reasoning" || mustJSON(t, rs.Summary) != `[{"type":"summary_text","text":"hmm"}]` {
		t.Errorf("reasoning = %+v", rs)
	}
	if msg.Type != "message" || msg.Status != "completed" || mustJSON(t, msg.Content) != `[{"type":"output_text","text":"hello","annotations":[]}]` {
		t.Errorf("message = %+v", msg)
	}
	if fc.Type != "function_call" || fc.CallID != "call_1" || fc.Name != "f" || *fc.Arguments != `{"a":1}` || !strings.HasPrefix(fc.ID, "fc_") {
		t.Errorf("function_call = %+v", fc)
	}
	if got := mustJSON(t, r.Usage); got != `{"input_tokens":95,"input_tokens_details":{"cached_tokens":80},"output_tokens":30,`+
		`"output_tokens_details":{"reasoning_tokens":7},"total_tokens":125,"cache_creation_input_tokens":5,"cache_read_input_tokens":80,"credits":0.25}` {
		t.Errorf("usage = %s", got)
	}

	inc := Response(&turn.Turn{ID: "x", Blocks: []turn.Block{}, Stop: turn.StopMaxTokens}, 1, nil)
	if inc.Status != "incomplete" || inc.IncompleteDetails == nil || inc.IncompleteDetails.Reason != "max_output_tokens" || mustJSON(t, inc.Output) != "[]" {
		t.Errorf("incomplete = %+v", inc)
	}
}

// ---- 错误与 ID ----

func TestWriteError(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{400, `{"error":{"message":"m","type":"invalid_request_error","code":null}}`},
		{401, `{"error":{"message":"m","type":"authentication_error","code":"invalid_api_key"}}`},
		{402, `{"error":{"message":"m","type":"insufficient_quota","code":"insufficient_quota"}}`},
		{403, `{"error":{"message":"m","type":"permission_error","code":null}}`},
		{404, `{"error":{"message":"m","type":"not_found_error","code":null}}`},
		{429, `{"error":{"message":"m","type":"rate_limit_error","code":"rate_limit_exceeded"}}`},
		{502, `{"error":{"message":"m","type":"server_error","code":null}}`},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		WriteError(rec, tt.status, "m")
		if rec.Code != tt.status || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%d: code=%d ct=%q", tt.status, rec.Code, rec.Header().Get("Content-Type"))
		}
		if got := strings.TrimSpace(rec.Body.String()); got != tt.want {
			t.Errorf("%d: %s", tt.status, got)
		}
	}
}

func TestNewID(t *testing.T) {
	a, b := NewID("resp_"), NewID("resp_")
	if len(a) != len("resp_")+24 || !strings.HasPrefix(a, "resp_") || a == b {
		t.Errorf("ids %q %q", a, b)
	}
	if strings.Trim(a[5:], "0123456789abcdef") != "" {
		t.Errorf("not hex: %q", a)
	}
}
