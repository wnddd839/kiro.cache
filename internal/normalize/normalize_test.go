package normalize

import (
	"encoding/json"
	"reflect"
	"testing"

	"kiro-go/internal/anthropic"
)

func parse(t *testing.T, raw string) *anthropic.Request {
	t.Helper()
	var req anthropic.Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &req
}

func systemTexts(req *anthropic.Request) []string {
	var out []string
	for _, b := range req.System {
		out = append(out, b.Text)
	}
	return out
}

func toolNames(req *anthropic.Request) []string {
	var out []string
	for _, tl := range req.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func TestRequestRemovesBillingHeader(t *testing.T) {
	req := parse(t, `{
		"model": "m",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=1.0.88.abc; cc_entrypoint=cli; cch=abc;"},
			{"type": "text", "text": "You are Claude Code."},
			{"type": "text", "text": "  \n x-anthropic-billing-header: cch=def;"},
			{"type": "text", "text": "Mentions x-anthropic-billing-header: inside, keep."},
			{"type": "text", "text": "Be concise."}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	Request(req, Options{})
	want := []string{"You are Claude Code.", "Mentions x-anthropic-billing-header: inside, keep.", "Be concise."}
	if got := systemTexts(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("system = %q, want %q", got, want)
	}
}

func TestRequestSystemStringForm(t *testing.T) {
	req := parse(t, `{"model":"m","system":"plain system","messages":[]}`)
	Request(req, Options{})
	if got := systemTexts(req); !reflect.DeepEqual(got, []string{"plain system"}) {
		t.Fatalf("system = %q", got)
	}

	req = parse(t, `{"model":"m","system":"x-anthropic-billing-header: cch=1;","messages":[]}`)
	Request(req, Options{})
	if len(req.System) != 0 {
		t.Fatalf("billing-only string system = %q, want empty", systemTexts(req))
	}
}

func TestRequestBillingHeaderStableAcrossRequests(t *testing.T) {
	mk := func(cch string) *anthropic.Request {
		return parse(t, `{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2; cch=`+cch+`;"},{"type":"text","text":"base"}],"messages":[]}`)
	}
	a, b := mk("aaa"), mk("bbb")
	Request(a, Options{})
	Request(b, Options{})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("normalized requests differ:\n%s\n%s", ja, jb)
	}
}

const toolsReq = `{
	"model": "m",
	"messages": [],
	"tools": [
		{"name": "mcp__z", "description": "z"},
		{"name": "Bash", "description": "first"},
		{"name": "apply"},
		{"name": "Bash", "description": "second"},
		{"name": "Read"}
	]
}`

func TestRequestSortTools(t *testing.T) {
	req := parse(t, toolsReq)
	Request(req, Options{SortTools: true})
	want := []string{"Bash", "Bash", "Read", "apply", "mcp__z"}
	if got := toolNames(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	if req.Tools[0].Description != "first" || req.Tools[1].Description != "second" {
		t.Errorf("sort not stable: %q, %q", req.Tools[0].Description, req.Tools[1].Description)
	}
}

func TestRequestSortToolsOff(t *testing.T) {
	req := parse(t, toolsReq)
	Request(req, Options{})
	want := []string{"mcp__z", "Bash", "apply", "Bash", "Read"}
	if got := toolNames(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %q, want untouched %q", got, want)
	}
}

func TestRequestSystemStrip(t *testing.T) {
	res, err := Compile([]string{`(?m)^Working directory: .*$`, `Today is \d{4}-\d{2}-\d{2}\.`})
	if err != nil {
		t.Fatal(err)
	}
	req := parse(t, `{
		"model": "m",
		"system": [
			{"type": "text", "text": "Intro.\nWorking directory: /home/u/p\nToday is 2026-10-05. End."},
			{"type": "text", "text": "untouched"}
		],
		"messages": [{"role": "user", "content": "Working directory: /x"}]
	}`)
	Request(req, Options{SystemStrip: res})
	want := []string{"Intro.\n\n End.", "untouched"}
	if got := systemTexts(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("system = %q, want %q", got, want)
	}
	if got := req.Messages[0].Content.Text(""); got != "Working directory: /x" {
		t.Errorf("messages must not be stripped: %q", got)
	}
}

func TestCompile(t *testing.T) {
	res, err := Compile(nil)
	if err != nil || len(res) != 0 {
		t.Fatalf("Compile(nil) = %v, %v", res, err)
	}
	if _, err := Compile([]string{"ok", "("}); err == nil {
		t.Fatal("invalid pattern: want error")
	}
}

func TestMetadataSession(t *testing.T) {
	cases := []struct {
		name string
		meta *anthropic.Metadata
		want string
	}{
		{"nil metadata", nil, ""},
		{"empty user id", &anthropic.Metadata{}, ""},
		{
			"legacy",
			&anthropic.Metadata{UserID: "user_abc_account_x_session_0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"},
			"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		},
		{
			"json",
			&anthropic.Metadata{UserID: `{"device_id":"dev","account_uuid":"acc","session_id":"abcd1234-5678-90ab-cdef-1234567890ab"}`},
			"abcd1234-5678-90ab-cdef-1234567890ab",
		},
		{
			"json with spaces",
			&anthropic.Metadata{UserID: `{"session_id" : "abcd1234-0000"}`},
			"abcd1234-0000",
		},
		{"no session", &anthropic.Metadata{UserID: "user_abc_account_x"}, ""},
		{"too short", &anthropic.Metadata{UserID: "user_abc_session_abc"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &anthropic.Request{Metadata: tc.meta}
			if got := MetadataSession(req); got != tc.want {
				t.Fatalf("MetadataSession = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMetadataSessionFromJSON(t *testing.T) {
	req := parse(t, `{"model":"m","messages":[],"metadata":{"user_id":"{\"session_id\":\"abcd1234-ef56\"}"}}`)
	if got := MetadataSession(req); got != "abcd1234-ef56" {
		t.Fatalf("MetadataSession = %q", got)
	}
}

func TestPinMessages(t *testing.T) {
	req := parse(t, `{
		"model": "m",
		"system": [{"type": "text", "text": "sys A"}, {"type": "text", "text": "sys B"}],
		"messages": [
			{"role": "assistant", "content": "prefill"},
			{"role": "user", "content": [{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AA=="}}]},
			{"role": "user", "content": [{"type": "text", "text": "hello"}, {"type": "text", "text": "world"}]},
			{"role": "assistant", "content": "reply"},
			{"role": "user", "content": "second turn"}
		]
	}`)
	got := PinMessages(req)
	want := []map[string]any{
		{"role": "system", "content": "sys A\nsys B"},
		{"role": "user", "content": "hello world"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PinMessages = %v, want %v", got, want)
	}
}

func TestPinMessagesNoSystemStringContent(t *testing.T) {
	req := parse(t, `{"model":"m","messages":[{"role":"user","content":"first"},{"role":"user","content":"next"}]}`)
	got := PinMessages(req)
	want := []map[string]any{{"role": "user", "content": "first"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PinMessages = %v, want %v", got, want)
	}
	if got := PinMessages(&anthropic.Request{}); got != nil {
		t.Errorf("empty request = %v, want nil", got)
	}
}

func TestPinMessagesStableAfterNormalize(t *testing.T) {
	mk := func(cch, second string) *anthropic.Request {
		return parse(t, `{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cch=`+cch+`;"},{"type":"text","text":"sys"}],
			"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"`+second+`"}]}`)
	}
	a, b := mk("111", "x"), mk("222", "y")
	Request(a, Options{})
	Request(b, Options{})
	if !reflect.DeepEqual(PinMessages(a), PinMessages(b)) {
		t.Fatalf("pin messages differ: %v vs %v", PinMessages(a), PinMessages(b))
	}
}
