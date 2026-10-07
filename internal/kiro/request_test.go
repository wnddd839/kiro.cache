package kiro

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"kiro-proxy/internal/anthropic"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func parseReq(t *testing.T, s string) *anthropic.Request {
	t.Helper()
	var r anthropic.Request
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return &r
}

func build(t *testing.T, req *anthropic.Request, opts BuildOptions) (body, map[string]string) {
	t.Helper()
	payload, names, err := Build(req, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var b body
	if err := json.Unmarshal(payload, &b); err != nil {
		t.Fatalf("unmarshal payload: %v\n%s", err, payload)
	}
	return b, names
}

func firstUser(b body) *userMsg {
	if len(b.State.History) > 0 {
		return b.State.History[0].User
	}
	return b.State.CurrentMessage.User
}

func TestBuildSystemAndThinkingTag(t *testing.T) {
	const tag = "<thinking_mode>enabled</thinking_mode><max_thinking_length>12345</max_thinking_length>"
	tests := []struct {
		name   string
		req    string
		budget int
		want   string
	}{
		{"none", `{"messages":[{"role":"user","content":"hi"}]}`, 0, "hi"},
		{"system string", `{"system":"be nice","messages":[{"role":"user","content":"hi"}]}`, 0, "be nice\n\nhi"},
		{"system blocks", `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`, 0, "a\nb\n\nhi"},
		{"budget only", `{"messages":[{"role":"user","content":"hi"}]}`, 12345, tag + "\n\nhi"},
		{"budget and system", `{"system":"be nice","messages":[{"role":"user","content":"hi"}]}`, 12345, tag + "\nbe nice\n\nhi"},
		{"system goes to first user, not current", `{"system":"S","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"},{"role":"user","content":"three"}]}`, 0, "S\n\none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := build(t, parseReq(t, tt.req), BuildOptions{Model: "claude-sonnet-4.5", ThinkingBudget: tt.budget})
			if got := firstUser(b).Content; got != tt.want {
				t.Errorf("first user content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildEnvelope(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m1", ProfileArn: "arn:x", ConversationID: "conv-1"})
	if b.AgentMode != "vibe" || b.State.AgentTaskType != "vibe" || b.State.ChatTriggerType != "MANUAL" {
		t.Errorf("envelope = %+v", b)
	}
	if b.ProfileArn != "arn:x" {
		t.Errorf("profileArn = %q", b.ProfileArn)
	}
	cur := b.State.CurrentMessage.User
	if cur.ModelID != "m1" || cur.Origin != "KIRO_CLI" {
		t.Errorf("current = %+v", cur)
	}
	if b.State.History != nil {
		t.Errorf("history = %+v, want none", b.State.History)
	}

	payload, _, _ := Build(req, BuildOptions{Model: "m1"})
	if bytes.Contains(payload, []byte(`"profileArn"`)) || bytes.Contains(payload, []byte(`"history"`)) {
		t.Errorf("empty profileArn/history should be omitted: %s", payload)
	}
}

func TestBuildConversationID(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	b, _ := build(t, req, BuildOptions{Model: "m", ConversationID: "fixed-id"})
	if b.State.ConversationID != "fixed-id" {
		t.Errorf("conversationId = %q, want fixed-id", b.State.ConversationID)
	}

	b1, _ := build(t, req, BuildOptions{Model: "m"})
	b2, _ := build(t, req, BuildOptions{Model: "m"})
	for _, id := range []string{b1.State.ConversationID, b2.State.ConversationID} {
		if !uuidRe.MatchString(id) {
			t.Errorf("random conversationId %q is not a v4 UUID", id)
		}
	}
	if b1.State.ConversationID == b2.State.ConversationID {
		t.Errorf("random conversationIds collide: %q", b1.State.ConversationID)
	}
}

func TestBuildHistoryAndCurrent(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"a"},
		{"role":"assistant","content":"b"},
		{"role":"user","content":"c"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	h := b.State.History
	if len(h) != 2 || h[0].User == nil || h[0].User.Content != "a" || h[1].Asst == nil || h[1].Asst.Content != "b" {
		t.Fatalf("history = %+v", h)
	}
	if got := b.State.CurrentMessage.User.Content; got != "c" {
		t.Errorf("current = %q, want c", got)
	}
}

func TestBuildLeadingAssistantDropped(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"assistant","content":"greeting"},
		{"role":"user","content":"a"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	if len(b.State.History) != 0 {
		t.Errorf("history = %+v, want none", b.State.History)
	}
	if got := b.State.CurrentMessage.User.Content; got != "a" {
		t.Errorf("current = %q, want a", got)
	}
}

func TestBuildTrailingAssistant(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"a"},
		{"role":"assistant","content":"prefill"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	h := b.State.History
	if len(h) != 2 || h[1].Asst == nil || h[1].Asst.Content != "prefill" {
		t.Fatalf("history = %+v", h)
	}
	cur := b.State.CurrentMessage.User
	if cur.Content != proceed || cur.Context != nil {
		t.Errorf("current = %+v, want %q", cur, proceed)
	}

	// Only assistant messages: the whole thing collapses to a placeholder user.
	b, _ = build(t, parseReq(t, `{"messages":[{"role":"assistant","content":"x"}]}`), BuildOptions{Model: "m"})
	if len(b.State.History) != 0 || b.State.CurrentMessage.User.Content != proceed {
		t.Errorf("assistant-only = %+v", b.State)
	}
}

func TestBuildThinkingOnlyTurnSkipped(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"a"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"s"}]},
		{"role":"user","content":"b"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	if len(b.State.History) != 0 || b.State.CurrentMessage.User.Content != "a\n\nb" {
		t.Errorf("state = %+v; want users merged once the thinking-only turn is dropped", b.State)
	}
}

func TestBuildToolResultPairing(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"stale","content":"old"}]},
		{"role":"user","content":"go"},
		{"role":"assistant","content":[
			{"type":"text","text":"calling"},
			{"type":"tool_use","id":"a","name":"read","input":{"p":1}},
			{"type":"tool_use","id":"b","name":"read","input":"not-an-object"}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"z","content":"orphan"},
			{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"ok"}]},
			{"type":"tool_result","tool_use_id":"a","content":"dup"}]}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})

	h := b.State.History
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2: %+v", len(h), h)
	}
	// The leading user's tool_result answered no call: that message is dropped
	// when it has nothing else; here it merged with "go", so only its context goes.
	if h[0].User.Content != "go" || h[0].User.Context != nil {
		t.Errorf("first user = %+v, want content go and no context", h[0].User)
	}
	uses := h[1].Asst.ToolUses
	if len(uses) != 2 || string(uses[0].Input) != `{"p":1}` || string(uses[1].Input) != `{}` {
		t.Errorf("tool uses = %+v", uses)
	}

	cur := b.State.CurrentMessage.User
	if cur.Context == nil {
		t.Fatal("current has no context")
	}
	want := []toolResult{
		{ToolUseID: "a", Status: "success", Content: []textPart{{Text: "ok"}}},
		{ToolUseID: "b", Status: "error", Content: []textPart{{Text: noResult}}},
	}
	if got := cur.Context.ToolResults; !slices.EqualFunc(got, want, func(x, y toolResult) bool {
		return x.ToolUseID == y.ToolUseID && x.Status == y.Status && slices.Equal(x.Content, y.Content)
	}) {
		t.Errorf("tool results = %+v, want %+v", got, want)
	}
	if !strings.HasPrefix(noResult, "Tool use was interrupted") {
		t.Errorf("noResult = %q", noResult)
	}
	// Tool-result-only current message has no text and keeps empty content.
	if cur.Content != "" {
		t.Errorf("current content = %q, want empty", cur.Content)
	}
}

func TestBuildLeadingOrphanResultDropped(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"}]},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"q"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	if len(b.State.History) != 0 {
		t.Errorf("history = %+v, want none (orphan result and following assistant dropped)", b.State.History)
	}
	if cur := b.State.CurrentMessage.User; cur.Content != "q" || cur.Context != nil {
		t.Errorf("current = %+v", cur)
	}
}

func TestBuildToolResultErrorAndEmpty(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"x","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","is_error":true}]}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	r := b.State.CurrentMessage.User.Context.ToolResults
	if len(r) != 1 || r[0].Status != "error" || r[0].Content[0].Text != "(no output)" {
		t.Errorf("results = %+v", r)
	}
}

func TestBuildMergesSameRole(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":"b"},
		{"role":"assistant","content":[{"type":"text","text":"c"},{"type":"tool_use","id":"1","name":"t","input":{}}]},
		{"role":"assistant","content":[{"type":"text","text":"d"},{"type":"tool_use","id":"2","name":"t","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"r1"}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"2","content":"r2"},{"type":"text","text":"e"}]}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	h := b.State.History
	if len(h) != 2 {
		t.Fatalf("history = %+v", h)
	}
	if h[0].User.Content != "a\n\nb" {
		t.Errorf("merged user = %q", h[0].User.Content)
	}
	if a := h[1].Asst; a.Content != "c\n\nd" || len(a.ToolUses) != 2 {
		t.Errorf("merged assistant = %+v", a)
	}
	cur := b.State.CurrentMessage.User
	if cur.Content != "e" {
		t.Errorf("current content = %q", cur.Content)
	}
	var ids []string
	for _, r := range cur.Context.ToolResults {
		ids = append(ids, r.ToolUseID+"="+r.Status)
	}
	if want := []string{"1=success", "2=success"}; !slices.Equal(ids, want) {
		t.Errorf("results = %v, want %v", ids, want)
	}
}

func TestBuildLatestImagesOnly(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":[{"type":"text","text":"one"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OLD"}}]},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":[{"type":"text","text":"two"},
			{"type":"image","source":{"type":"base64","media_type":"image/jpg","data":"NEW1"}},
			{"type":"image","source":{"type":"url","url":"http://x"}}]},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":"three"}]}`)
	b, _ := build(t, req, BuildOptions{Model: "m"})
	h := b.State.History
	if len(h) != 4 {
		t.Fatalf("history = %+v", h)
	}
	if h[0].User.Images != nil {
		t.Errorf("older images kept: %+v", h[0].User.Images)
	}
	if got := h[2].User.Images; len(got) != 1 || got[0].Format != "jpeg" || got[0].Source.Bytes != "NEW1" {
		t.Errorf("latest images = %+v", got)
	}
	if b.State.CurrentMessage.User.Images != nil {
		t.Errorf("current images = %+v", b.State.CurrentMessage.User.Images)
	}
}

func TestImageFormat(t *testing.T) {
	for in, want := range map[string]string{
		"image/png": "png", "image/JPG": "jpeg", "image/webp": "webp", "": "png", "application/pdf": "png", "image/": "png",
	} {
		if got := imageFormat(in); got != want {
			t.Errorf("imageFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildTools(t *testing.T) {
	long := "mcp__some-really-long-server-name__" + strings.Repeat("x", 60) + ".tool"
	short := ToolName(long)
	req := parseReq(t, `{
		"tools":[
			{"type":"web_search_20250305","name":"web_search"},
			{"name":"read","description":"Read a file","input_schema":{"type":"object","properties":{"p":{"type":"string"}}}},
			{"name":"bare"},
			{"type":"custom","name":"read","description":"dup"},
			{"name":"`+long+`","description":"mcp","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"1","name":"gone","input":{}},
				{"type":"tool_use","id":"2","name":"`+long+`","input":{}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"1","content":"r"},
				{"type":"tool_result","tool_use_id":"2","content":"r"}]}]}`)
	b, names := build(t, req, BuildOptions{Model: "m"})

	tools := b.State.CurrentMessage.User.Context.Tools
	type spec struct{ name, desc, schema string }
	var got []spec
	for _, tl := range tools {
		got = append(got, spec{tl.Spec.Name, tl.Spec.Description, string(tl.Spec.InputSchema.JSON)})
	}
	want := []spec{
		{"read", "Read a file", `{"type":"object","properties":{"p":{"type":"string"}}}`},
		{"bare", "bare", string(emptySchema)},
		{short, "mcp", `{"type":"object"}`},
		{"gone", "Tool", string(emptySchema)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("tools =\n%+v\nwant\n%+v", got, want)
	}

	if short == long || len(short) > 64 || !toolNameRe.MatchString(short) {
		t.Errorf("ToolName(long) = %q, not a valid rewritten name", short)
	}
	if uses := b.State.History[1].Asst.ToolUses; uses[1].Name != short {
		t.Errorf("history tool name = %q, want %q", uses[1].Name, short)
	}
	if len(names) != 1 || names[short] != long {
		t.Errorf("names = %v, want {%q: %q}", names, short, long)
	}
}

func TestBuildNoToolsNoContext(t *testing.T) {
	b, names := build(t, parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), BuildOptions{Model: "m"})
	if b.State.CurrentMessage.User.Context != nil {
		t.Errorf("context = %+v, want nil", b.State.CurrentMessage.User.Context)
	}
	if len(names) != 0 {
		t.Errorf("names = %v", names)
	}
}

const richRequest = `{
	"system":[{"type":"text","text":"sys"}],
	"tools":[{"name":"read","input_schema":{"type":"object","properties":{"b":{},"a":{}}}},{"name":"mcp__srv__a.b.c"}],
	"messages":[
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"read","input":{"z":1,"a":2}},{"type":"tool_use","id":"call/xyz?","name":"mcp__srv__a.b.c","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"data"}]},
		{"role":"assistant","content":"done"},
		{"role":"user","content":[{"type":"document","title":"T","source":{"type":"text","data":"doc"}}]}]}`

func TestBuildDeterministic(t *testing.T) {
	req := parseReq(t, richRequest)
	opts := BuildOptions{Model: "claude-sonnet-4.5", ProfileArn: "arn:p", ConversationID: "c-1", ThinkingBudget: 20_000}
	first, _, err := Build(req, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Build must not mutate req, and must be safe to call concurrently.
	var wg sync.WaitGroup
	outs := make([][]byte, 16)
	for i := range outs {
		wg.Go(func() {
			outs[i], _, _ = Build(req, opts)
		})
	}
	wg.Wait()
	for i, out := range outs {
		if !bytes.Equal(first, out) {
			t.Fatalf("build %d differs:\n%s\n%s", i, first, out)
		}
	}
	// A freshly parsed copy gives the same bytes too.
	again, _, _ := Build(parseReq(t, richRequest), opts)
	if !bytes.Equal(first, again) {
		t.Fatalf("reparsed build differs:\n%s\n%s", first, again)
	}

	b, _ := build(t, req, opts)
	if uses := b.State.History[1].Asst.ToolUses; uses[1].ToolUseID != ToolID("call/xyz?") || !strings.HasPrefix(uses[1].ToolUseID, "t_") {
		t.Errorf("invalid tool id not rewritten: %+v", uses[1])
	}
	if got := b.State.CurrentMessage.User.Content; got != "T\ndoc" {
		t.Errorf("document text = %q", got)
	}
}

func TestToolID(t *testing.T) {
	for _, id := range []string{"toolu_01ABC", "call:1.2-3", "a", strings.Repeat("x", 64)} {
		if got := ToolID(id); got != id {
			t.Errorf("ToolID(%q) = %q, want unchanged", id, got)
		}
	}
	for _, id := range []string{"", "has space", "call/1", strings.Repeat("x", 65), "ünï"} {
		got := ToolID(id)
		if !strings.HasPrefix(got, "t_") || len(got) != 2+32 {
			t.Errorf("ToolID(%q) = %q, want t_ + 32 chars", id, got)
		}
		if !toolIDRe.MatchString(got) {
			t.Errorf("ToolID(%q) = %q is itself invalid", id, got)
		}
		if again := ToolID(id); again != got {
			t.Errorf("ToolID(%q) unstable: %q vs %q", id, got, again)
		}
		if ToolID(got) != got {
			t.Errorf("ToolID not idempotent for %q", got)
		}
	}
	if ToolID("a b") == ToolID("a  b") {
		t.Error("distinct ids hashed to the same value")
	}
}

func TestToolName(t *testing.T) {
	for _, n := range []string{"read", "mcp__srv__tool", "a-b_c", strings.Repeat("n", 64)} {
		if got := ToolName(n); got != n {
			t.Errorf("ToolName(%q) = %q, want unchanged", n, got)
		}
	}
	for _, n := range []string{"mcp__srv__a.b", strings.Repeat("n", 65), "with space"} {
		got := ToolName(n)
		if got == n || !toolNameRe.MatchString(got) || ToolName(n) != got || ToolName(got) != got {
			t.Errorf("ToolName(%q) = %q: want a stable valid rewrite", n, got)
		}
	}
}

func TestThinkingBudget(t *testing.T) {
	type th = anthropic.Thinking
	tests := []struct {
		name     string
		model    string
		thinking *th
		effort   string
		want     int
	}{
		{"nothing asked", "claude-sonnet-4.5", nil, "", 0},
		{"disabled", "claude-sonnet-4.5", &th{Type: "disabled"}, "", 0},
		{"effort low alone is off", "claude-sonnet-4.5", nil, "low", 0},
		{"effort low with thinking", "claude-sonnet-4.5", &th{Type: "enabled"}, "low", 10_000},
		{"effort medium", "claude-sonnet-4.5", nil, "medium", 20_000},
		{"effort high", "claude-sonnet-4.5", nil, "high", 30_000},
		{"effort xhigh", "claude-opus-4.5", nil, "xhigh", 50_000},
		{"effort max", "claude-opus-4.5", nil, "max", 50_000},
		{"effort beats budget", "claude-sonnet-4.5", &th{Type: "enabled", BudgetTokens: 1234}, "high", 30_000},
		{"budget tokens", "claude-sonnet-4.5", &th{Type: "enabled", BudgetTokens: 12_345}, "", 12_345},
		{"enabled default", "claude-sonnet-4.5", &th{Type: "enabled"}, "", 20_000},
		{"adaptive", "CLAUDE-Haiku", &th{Type: "adaptive"}, "", 20_000},
		{"auto model", "auto", nil, "high", 30_000},
		{"non-claude model", "qwen3-coder", &th{Type: "enabled", BudgetTokens: 5000}, "high", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &anthropic.Request{Thinking: tt.thinking}
			if tt.effort != "" {
				req.OutputConfig = &anthropic.OutputConfig{Effort: tt.effort}
			}
			if got := ThinkingBudget(req, tt.model); got != tt.want {
				t.Errorf("ThinkingBudget = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		u := NewUUID()
		if !uuidRe.MatchString(u) {
			t.Fatalf("NewUUID() = %q, not a v4 UUID", u)
		}
		if seen[u] {
			t.Fatalf("NewUUID repeated %q", u)
		}
		seen[u] = true
	}
}

func TestBuildCachePoints(t *testing.T) {
	req := &anthropic.Request{
		System: anthropic.Content{{Type: "text", Text: "sys"}},
		Tools:  []anthropic.Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []anthropic.Message{
			{Role: "user", Content: anthropic.Content{{Type: "text", Text: "q1"}}},
			{Role: "assistant", Content: anthropic.Content{{Type: "text", Text: "a1"}}},
			{Role: "user", Content: anthropic.Content{{Type: "text", Text: "q2"}}},
		},
	}
	off, _, _ := Build(req, BuildOptions{Model: "m", ConversationID: "c"})
	if strings.Contains(string(off), "cachePoint") {
		t.Fatalf("cachePoint sent while off: %s", off)
	}
	on, _, _ := Build(req, BuildOptions{Model: "m", ConversationID: "c", CachePoints: CachePoints{FirstUser: true, Assistant: true, Tools: true}})
	if n := strings.Count(string(on), `"cachePoint":{"type":"default"}`); n != 3 {
		t.Fatalf("cachePoints = %d in %s", n, on)
	}
	var b struct {
		State struct {
			Current struct {
				User struct {
					Context struct {
						Tools []map[string]any `json:"tools"`
					} `json:"userInputMessageContext"`
				} `json:"userInputMessage"`
			} `json:"currentMessage"`
		} `json:"conversationState"`
	}
	if err := json.Unmarshal(on, &b); err != nil {
		t.Fatal(err)
	}
	tools := b.State.Current.User.Context.Tools
	if len(tools) != 2 || tools[1]["cachePoint"] == nil || tools[1]["toolSpecification"] != nil {
		t.Fatalf("tools = %v", tools)
	}
}
