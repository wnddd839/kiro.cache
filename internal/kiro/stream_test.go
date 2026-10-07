package kiro

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// encodeFrame builds one AWS event-stream message. CRCs are not checked by
// the decoder, so they are written as arbitrary values.
func encodeFrame(headers [][2]string, payload string) []byte {
	var h bytes.Buffer
	for _, kv := range headers {
		h.WriteByte(byte(len(kv[0])))
		h.WriteString(kv[0])
		h.WriteByte(7)
		_ = binary.Write(&h, binary.BigEndian, uint16(len(kv[1])))
		h.WriteString(kv[1])
	}
	total := 12 + h.Len() + len(payload) + 4
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(total))
	_ = binary.Write(&b, binary.BigEndian, uint32(h.Len()))
	_ = binary.Write(&b, binary.BigEndian, uint32(0xdeadbeef))
	b.Write(h.Bytes())
	b.WriteString(payload)
	_ = binary.Write(&b, binary.BigEndian, uint32(0xcafebabe))
	return b.Bytes()
}

func event(kind, payload string) []byte {
	return encodeFrame([][2]string{{":message-type", "event"}, {":event-type", kind}, {":content-type", "application/json"}}, payload)
}

func stream(frames ...[]byte) io.Reader { return bytes.NewReader(bytes.Join(frames, nil)) }

// drain reads every event and checks Next keeps returning io.EOF afterwards.
func drain(t *testing.T, d *Decoder) []Event {
	t.Helper()
	var evs []Event
	for range 10_000 {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			for range 2 {
				if _, err := d.Next(); !errors.Is(err, io.EOF) {
					t.Fatalf("Next after EOF = %v, want io.EOF", err)
				}
			}
			if n := len(evs); n == 0 || (evs[n-1].Kind != EvStop && evs[n-1].Kind != EvError) {
				t.Fatalf("last event is not stop/error: %+v", evs)
			}
			return evs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		evs = append(evs, ev)
	}
	t.Fatal("decoder did not end")
	return nil
}

func ofKind(evs []Event, k EventKind) []Event {
	return slices.DeleteFunc(slices.Clone(evs), func(e Event) bool { return e.Kind != k })
}

func joined(evs []Event, k EventKind) string {
	var sb strings.Builder
	for _, e := range ofKind(evs, k) {
		sb.WriteString(e.Text)
	}
	return sb.String()
}

func TestReadFrame(t *testing.T) {
	raw := encodeFrame([][2]string{{":event-type", "x"}, {"k", ""}}, `{"a":1}`)
	f, err := readFrame(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if f.headers[":event-type"] != "x" || f.headers["k"] != "" || len(f.headers) != 2 {
		t.Errorf("headers = %v", f.headers)
	}
	if string(f.payload) != `{"a":1}` {
		t.Errorf("payload = %q", f.payload)
	}

	for name, data := range map[string][]byte{
		"too short":  {0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0},
		"hlen > len": {0, 0, 0, 16, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		if _, err := readFrame(bufio.NewReader(bytes.NewReader(data))); !errors.Is(err, errMalformed) {
			t.Errorf("%s: err = %v, want errMalformed", name, err)
		}
	}
	if _, err := readFrame(bufio.NewReader(bytes.NewReader(raw[:len(raw)-3]))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated body: err = %v, want ErrUnexpectedEOF", err)
	}
	if _, err := readFrame(bufio.NewReader(bytes.NewReader(nil))); !errors.Is(err, io.EOF) {
		t.Errorf("empty: err = %v, want EOF", err)
	}
}

func TestParseHeadersSkipsNonString(t *testing.T) {
	var h bytes.Buffer
	h.Write([]byte{1, 'b', 0})             // bool true
	h.Write([]byte{1, 'i', 4, 0, 0, 0, 7}) // int32
	h.Write([]byte{1, 'y', 6, 0, 2, 9, 9}) // bytes
	h.Write([]byte{1, 's', 7, 0, 2, 'o', 'k'})
	h.Write([]byte{1, 'u', 9})
	h.Write(make([]byte, 16)) // uuid
	h.Write([]byte{1, 't', 7, 0, 1, 'z'})
	got := parseHeaders(h.Bytes())
	if len(got) != 2 || got["s"] != "ok" || got["t"] != "z" {
		t.Errorf("headers = %v", got)
	}
	// Truncated header data stops parsing without panicking.
	if got := parseHeaders([]byte{5, 'a'}); len(got) != 0 {
		t.Errorf("truncated = %v", got)
	}
}

func TestDecoderText(t *testing.T) {
	d := NewDecoder(stream(
		event("assistantResponseEvent", `{"content":"Hello"}`),
		event("assistantResponseEvent", `{"content":""}`),
		event("assistantResponseEvent", `{"content":", world"}`),
		event("unknownEvent", `{"content":"ignored"}`),
		event("assistantResponseEvent", `not json`),
	), false, 0, nil)
	evs := drain(t, d)
	want := []Event{
		{Kind: EvText, Text: "Hello"},
		{Kind: EvText, Text: ", world"},
		{Kind: EvStop, Stop: "end_turn", Usage: Usage{Output: (12 + 3) / 4}},
	}
	if !slices.Equal(evs, want) {
		t.Errorf("events =\n%+v\nwant\n%+v", evs, want)
	}
}

func TestDecoderThinkingAcrossChunks(t *testing.T) {
	d := NewDecoder(stream(
		event("assistantResponseEvent", `{"content":"<thin"}`),
		event("assistantResponseEvent", `{"content":"king>abc</thi"}`),
		event("assistantResponseEvent", `{"content":"nking>hello"}`),
	), true, 0, nil)
	evs := drain(t, d)
	want := []Event{{Kind: EvThink, Text: "abc"}, {Kind: EvText, Text: "hello"}}
	if got := evs[:len(evs)-1]; !slices.Equal(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
}

func TestThinkParser(t *testing.T) {
	type ev struct {
		kind EventKind
		text string
	}
	tests := []struct {
		name   string
		chunks []string
		want   []ev
	}{
		{"no tag", []string{"hi ", "there"}, []ev{{EvText, "hi "}, {EvText, "there"}}},
		{"short non-tag held then flushed", []string{"<t", "x"}, []ev{{EvText, "<tx"}}},
		{"leading whitespace and newlines", []string{"\n <thinking>\n\nabc</thinking>\n\nok"}, []ev{{EvThink, "abc"}, {EvText, "ok"}}},
		{"byte by byte", strings.Split("<thinking>ab</thinking>cd", ""), []ev{{EvThink, "a"}, {EvThink, "b"}, {EvText, "c"}, {EvText, "d"}}},
		{"close tag partial held", []string{"<thinking>x</th", "ey"}, []ev{{EvThink, "x"}, {EvThink, "</they"}}},
		{"tag only at start", []string{"a<thinking>b</thinking>"}, []ev{{EvText, "a<thinking>b</thinking>"}}},
		{"unterminated thinking flushed as think", []string{"<thinking>abc</thin"}, []ev{{EvThink, "abc"}, {EvThink, "</thin"}}},
		{"only a prefix of the tag", []string{"<thi"}, []ev{{EvText, "<thi"}}},
		{"empty thinking", []string{"<thinking></thinking>x"}, []ev{{EvText, "x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p thinkParser
			var got []ev
			for _, c := range tt.chunks {
				for _, e := range p.feed(c) {
					got = append(got, ev{e.Kind, e.Text})
				}
			}
			for _, e := range p.end() {
				got = append(got, ev{e.Kind, e.Text})
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("events = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPartialSuffix(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want int
	}{{"abc</thi", 5}, {"abc<", 1}, {"abc", 0}, {"</thinking", 10}, {"", 0}} {
		if got := partialSuffix(tt.s, thinkClose); got != tt.want {
			t.Errorf("partialSuffix(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestDecoderReasoningEvents(t *testing.T) {
	d := NewDecoder(stream(
		event("reasoningContentEvent", `{"text":"pondering"}`),
		event("reasoningContentEvent", `{"signature":"sig=="}`),
		event("assistantResponseEvent", `{"content":"answer"}`),
	), false, 0, nil)
	evs := drain(t, d)
	want := []Event{{Kind: EvThink, Text: "pondering"}, {Kind: EvSig, Text: "sig=="}, {Kind: EvText, Text: "answer"}}
	if got := evs[:len(evs)-1]; !slices.Equal(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
}

func TestDecoderToolUse(t *testing.T) {
	names := map[string]string{"mcp__srv__a_b_1234abcd": "mcp__srv__a.b"}
	d := NewDecoder(stream(
		event("assistantResponseEvent", `{"content":"<thinking>plan</thinking>let me look"}`),
		event("toolUseEvent", `{"toolUseId":"t1","name":"read","input":""}`),
		event("toolUseEvent", `{"toolUseId":"t1","name":"read","input":"{\"path\":"}`),
		event("toolUseEvent", `{"toolUseId":"t1","name":"read","input":"\"a.go\"}"}`),
		event("toolUseEvent", `{"toolUseId":"t1","name":"read","stop":true}`),
		event("toolUseEvent", `{"toolUseId":"t2","name":"mcp__srv__a_b_1234abcd","input":{"q":1}}`),
		event("toolUseEvent", `{"toolUseId":"t3","name":"noargs","input":{}}`),
	), true, 0, names)
	evs := drain(t, d)
	want := []Event{
		{Kind: EvThink, Text: "plan"},
		{Kind: EvText, Text: "let me look"},
		{Kind: EvToolStart, ID: "t1", Name: "read"},
		{Kind: EvToolArgs, Text: `{"path":`},
		{Kind: EvToolArgs, Text: `"a.go"}`},
		{Kind: EvToolStart, ID: "t2", Name: "mcp__srv__a.b"},
		{Kind: EvToolArgs, Text: `{"q":1}`},
		{Kind: EvToolStart, ID: "t3", Name: "noargs"},
	}
	if got := evs[:len(evs)-1]; !slices.Equal(got, want) {
		t.Errorf("events =\n%+v\nwant\n%+v", got, want)
	}
	if last := evs[len(evs)-1]; last.Kind != EvStop || last.Stop != "tool_use" {
		t.Errorf("stop = %+v, want tool_use", last)
	}
}

func TestDecoderToolStartFlushesHeldText(t *testing.T) {
	// "<thi" is held as a possible tag; a tool call means it was plain text.
	d := NewDecoder(stream(
		event("assistantResponseEvent", `{"content":"<thi"}`),
		event("toolUseEvent", `{"toolUseId":"t1","name":"x"}`),
	), true, 0, nil)
	evs := drain(t, d)
	want := []Event{{Kind: EvText, Text: "<thi"}, {Kind: EvToolStart, ID: "t1", Name: "x"}}
	if got := evs[:len(evs)-1]; !slices.Equal(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
}

func TestDecoderUsageAndStop(t *testing.T) {
	tests := []struct {
		name   string
		frames [][]byte
		window int
		stop   string
		usage  Usage
	}{
		{
			name: "token usage",
			frames: [][]byte{
				event("assistantResponseEvent", `{"content":"hi"}`),
				event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":100,"inputTokens":999,"outputTokens":20,"cacheReadInputTokens":3000,"cacheWriteInputTokens":400}}`),
			},
			stop:  "end_turn",
			usage: Usage{Input: 100, Output: 20, CacheRead: 3000, CacheWrite: 400, Reported: true, OutputReported: true},
		},
		{
			// 多条：取最后一条，不累加；uncached 缺失时 inputTokens − cacheRead − cacheWrite
			name: "last of several; uncached derived from inputTokens",
			frames: [][]byte{
				event("messageMetadataEvent", `{"tokenUsage":{"inputTokens":10,"outputTokens":1}}`),
				event("metadataEvent", `{"tokenUsage":{"inputTokens":12,"outputTokens":2,"cacheReadInputTokens":7}}`),
			},
			stop:  "end_turn",
			usage: Usage{Input: 5, Output: 2, CacheRead: 7, Reported: true, OutputReported: true},
		},
		{
			name: "input estimated from context percentage; credits summed",
			frames: [][]byte{
				event("assistantResponseEvent", `{"content":"12345678"}`),
				event("contextUsageEvent", `{"contextUsagePercentage":25}`),
				event("meteringEvent", `{"unit":"credit","unitPlural":"credits","usage":0.25}`),
				event("meteringEvent", `{"unit":"credit","usage":0.125}`),
			},
			window: 200_000,
			stop:   "end_turn",
			usage:  Usage{Input: 50_000, Output: 2, Credits: 0.375, ContextPct: 25},
		},
		{
			name:   "max tokens",
			frames: [][]byte{event("metadataEvent", `{"stopReason":"max_tokens"}`)},
			stop:   "max_tokens",
		},
		{
			name:   "content filtered",
			frames: [][]byte{event("metadataEvent", `{"stopReason":"CONTENT_FILTERED"}`)},
			stop:   "refusal",
		},
		{
			name: "tool use beats max tokens",
			frames: [][]byte{
				event("toolUseEvent", `{"toolUseId":"a","name":"n"}`),
				event("metadataEvent", `{"stopReason":"MAX_TOKENS","tokenUsage":{"outputTokens":9}}`),
			},
			stop:  "tool_use",
			usage: Usage{Output: 9, OutputReported: true}, // 只报输出：输入侧不算上游已报
		},
		{
			name: "output-only tokenUsage keeps context estimate",
			frames: [][]byte{
				event("assistantResponseEvent", `{"content":"hi"}`),
				event("metadataEvent", `{"tokenUsage":{"outputTokens":4,"contextUsagePercentage":10}}`),
			},
			window: 200_000,
			stop:   "end_turn",
			usage:  Usage{Input: 20_000, Output: 4, ContextPct: 10, OutputReported: true},
		},
		{
			name: "all-cached input is reported, not replaced by context estimate",
			frames: [][]byte{
				event("metadataEvent", `{"tokenUsage":{"cacheReadInputTokens":3000,"outputTokens":5,"contextUsagePercentage":10}}`),
			},
			window: 200_000,
			stop:   "end_turn",
			usage:  Usage{Output: 5, CacheRead: 3000, ContextPct: 10, Reported: true, OutputReported: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs := drain(t, NewDecoder(stream(tt.frames...), false, tt.window, nil))
			last := evs[len(evs)-1]
			got := last.Usage
			// 交叉核对用的诊断字段单独测
			got.UsageEvents, got.MeteringEvents, got.MeteringUnit, got.TotalTokens, got.Normalized, got.UsageRaw = 0, 0, "", 0, 0, nil
			if last.Kind != EvStop || last.Stop != tt.stop || got != tt.usage {
				t.Errorf("stop = %+v, want stop %q usage %+v", last, tt.stop, tt.usage)
			}
		})
	}
}

func TestDecoderErrors(t *testing.T) {
	full := event("assistantResponseEvent", `{"content":"partial"}`)
	tests := []struct {
		name   string
		in     []byte
		prefix string
		exact  string
	}{
		{
			name:  "exception frame",
			in:    encodeFrame([][2]string{{":message-type", "exception"}, {":exception-type", "ValidationException"}}, `{"message":"bad input","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`),
			exact: "ValidationException: bad input (CONTENT_LENGTH_EXCEEDS_THRESHOLD)",
		},
		{
			name:  "error frame with code",
			in:    encodeFrame([][2]string{{":message-type", "error"}, {":error-code", "InternalError"}}, `oops`),
			exact: "InternalError: oops",
		},
		{
			name:  "throttling exception",
			in:    encodeFrame([][2]string{{":message-type", "exception"}, {":exception-type", "ThrottlingException"}}, `{"Message":"slow down"}`),
			exact: "rate limited: slow down",
		},
		{
			name:  "error event type",
			in:    event("validationError", `{"message":"nope"}`),
			exact: "validationError: nope",
		},
		{
			name:   "truncated frame",
			in:     full[:len(full)-5],
			prefix: "the reply broke off",
		},
		{
			name:   "truncated prelude",
			in:     slices.Concat(full, []byte{0, 0, 0}),
			prefix: "the reply broke off",
		},
		{
			name:   "malformed",
			in:     slices.Concat(full, []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}),
			prefix: "the reply broke off: " + errMalformed.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs := drain(t, NewDecoder(bytes.NewReader(tt.in), false, 0, nil))
			last := evs[len(evs)-1]
			if last.Kind != EvError {
				t.Fatalf("last = %+v, want EvError", last)
			}
			if tt.exact != "" && last.Text != tt.exact {
				t.Errorf("error = %q, want %q", last.Text, tt.exact)
			}
			if tt.prefix != "" && !strings.HasPrefix(last.Text, tt.prefix) {
				t.Errorf("error = %q, want prefix %q", last.Text, tt.prefix)
			}
			if len(ofKind(evs, EvStop)) != 0 {
				t.Errorf("stop emitted alongside error: %+v", evs)
			}
		})
	}
}

func TestDecoderErrorStopsReading(t *testing.T) {
	d := NewDecoder(stream(
		event("assistantResponseEvent", `{"content":"a"}`),
		encodeFrame([][2]string{{":message-type", "exception"}, {":exception-type", "X"}}, `{"message":"m"}`),
		event("assistantResponseEvent", `{"content":"after"}`),
	), false, 0, nil)
	evs := drain(t, d)
	want := []Event{{Kind: EvText, Text: "a"}, {Kind: EvError, Text: "X: m"}}
	if !slices.Equal(evs, want) {
		t.Errorf("events = %+v, want %+v", evs, want)
	}
}

func TestDecoderEmptyStream(t *testing.T) {
	evs := drain(t, NewDecoder(bytes.NewReader(nil), true, 0, nil))
	want := []Event{{Kind: EvStop, Stop: "end_turn"}}
	if !slices.Equal(evs, want) {
		t.Errorf("events = %+v, want %+v", evs, want)
	}
}

func TestDecoderThinkingFlushedOnEnd(t *testing.T) {
	d := NewDecoder(stream(event("assistantResponseEvent", `{"content":"<thinking>abc</thi"}`)), true, 0, nil)
	evs := drain(t, d)
	if got := joined(evs, EvThink); got != "abc</thi" {
		t.Errorf("think = %q", got)
	}
	if got := joined(evs, EvText); got != "" {
		t.Errorf("text = %q", got)
	}
}

func TestDecoderDiagnostics(t *testing.T) {
	var trace []string
	d := NewDecoder(stream(
		event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":2,"totalTokens":4100,"normalizedTokenUsage":0.75}}`),
		event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":3,"totalTokens":4113}}`),
		event("meteringEvent", `{"unit":"credit","usage":0.1}`),
	), false, 0, nil)
	d.Trace = func(ev string, _ []byte) { trace = append(trace, ev) }
	evs := drain(t, d)
	u := evs[len(evs)-1].Usage
	if u.UsageEvents != 2 || u.TotalTokens != 4113 || u.Normalized != 0.75 || u.MeteringEvents != 1 || u.MeteringUnit != "credit" {
		t.Fatalf("diagnostics = %+v", u)
	}
	if len(trace) != 3 || trace[2] != "meteringEvent" {
		t.Fatalf("trace = %v", trace)
	}
}

// 字段缺失与值为 0 区分开：uncached 显式为 0 就是 0（全部命中），不再回落到 inputTokens。
func TestTokenUsageZeroVsMissing(t *testing.T) {
	evs := drain(t, NewDecoder(stream(
		event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":0,"inputTokens":3000,"cacheReadInputTokens":3000,"outputTokens":4}}`),
	), false, 0, nil))
	if u := evs[len(evs)-1].Usage; u.Input != 0 || u.CacheRead != 3000 || !u.Reported {
		t.Fatalf("explicit zero uncached: %+v", u)
	}
	// 多条：原值全部保留
	evs = drain(t, NewDecoder(stream(
		event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":100,"outputTokens":1}}`),
		event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":100,"outputTokens":9}}`),
		event("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":100,"outputTokens":20}}`),
	), false, 0, nil))
	u := evs[len(evs)-1].Usage
	if u.Input != 100 || u.Output != 20 || u.UsageEvents != 3 || u.UsageRaw == nil || len(*u.UsageRaw) != 3 {
		t.Fatalf("several tokenUsage: %+v raw %v", u, u.UsageRaw)
	}
}
