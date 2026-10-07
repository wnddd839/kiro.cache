package probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/kiro"
)

func frame(eventType, payload string) []byte {
	var h []byte
	for _, kv := range [][2]string{{":message-type", "event"}, {":event-type", eventType}} {
		h = append(h, byte(len(kv[0])))
		h = append(h, kv[0]...)
		h = append(h, 7)
		h = binary.BigEndian.AppendUint16(h, uint16(len(kv[1])))
		h = append(h, kv[1]...)
	}
	total := 12 + len(h) + len(payload) + 4
	out := binary.BigEndian.AppendUint32(nil, uint32(total))
	out = binary.BigEndian.AppendUint32(out, uint32(len(h)))
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, h...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, 0)
}

// fakeKiro 模拟：缓存按前缀内容、TTL 5 分钟（用假时钟）；credits = 4/M × 未缓存上下文 + 1/M × 缓存读 + 180/M × 输出。
type fakeKiro struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	now    func() time.Time
	used   float64
	report bool // 是否报 tokenUsage
}

const hidden = 4000

func (f *fakeKiro) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runtime/{region}/generateAssistantResponse", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		st := body["conversationState"].(map[string]any)
		hist, _ := st["history"].([]any)
		first := hist[0].(map[string]any)["userInputMessage"].(map[string]any)["content"].(string)
		cur := st["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)["content"].(string)
		prefixTok := len(first) / 4
		out := 2
		if strings.Contains(cur, "600 words") {
			out = 800
		}
		f.mu.Lock()
		now := f.now()
		read := 0
		if t, ok := f.seen[first]; ok && now.Sub(t) < 5*time.Minute {
			read = prefixTok
		}
		f.seen[first] = now
		uncached := prefixTok - read + hidden
		credits := (4*float64(uncached) + 1*float64(read) + 180*float64(out)) / 1e6
		f.used += credits
		f.mu.Unlock()
		w.Write(frame("assistantResponseEvent", fmt.Sprintf(`{"content":%q}`, strings.Repeat("word", out))))
		if f.report {
			w.Write(frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"cacheReadInputTokens":%d,"outputTokens":%d,"totalTokens":%d}}`, uncached, read, out, uncached+read+out)))
		}
		w.Write(frame("contextUsageEvent", fmt.Sprintf(`{"contextUsagePercentage":%v}`, float64(prefixTok+hidden+out)/2000)))
		w.Write(frame("meteringEvent", fmt.Sprintf(`{"unit":"credit","usage":%v}`, credits)))
	})
	mux.HandleFunc("GET /mgmt/{region}/Get-Usage-Limits", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		u := f.used
		f.mu.Unlock()
		fmt.Fprintf(w, `{"usageBreakdownList":[{"currentUsageWithPrecision":%v,"usageLimitWithPrecision":1000}]}`, u)
	})
	return mux
}

func newRunner(t *testing.T, f *fakeKiro) (*Runner, *bytes.Buffer) {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := kiro.NewClient(srv.Client())
	c.RuntimeURL = func(r string) string { return srv.URL + "/runtime/" + r }
	c.ManagementURL = func(r string) string { return srv.URL + "/mgmt/" + r }
	var buf bytes.Buffer
	cred := kiro.Cred{Method: kiro.MethodSocial, AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour), ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/x"}
	return &Runner{T: Target{Client: c, Model: "claude-sonnet-4.5", Cred: func(context.Context) (kiro.Cred, error) { return cred, nil }}, Out: &buf}, &buf
}

func TestCreditsFitRecoversRates(t *testing.T) {
	f := &fakeKiro{seen: map[string]time.Time{}, now: time.Now}
	r, buf := newRunner(t, f)
	s, err := r.Run(t.Context(), "credits", Options{Settle: time.Millisecond})
	if err != nil || s.Calls != 9 || math.Abs(s.Unassigned) > 1e-9 {
		t.Fatalf("summary %+v err %v", s, err)
	}
	recs, err := Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	rep := Analyze(recs)
	c := rep.Credits
	if c == nil || math.Abs(c.ColdPerM-4) > 0.2 || math.Abs(c.OutputPerM-180) > 10 || c.WarmOverCold > 0.5 {
		t.Fatalf("fit = %+v", c)
	}
	if len(rep.Limits) != 1 || math.Abs(rep.Limits[0].Diff) > 1e-9 {
		t.Fatalf("limits = %+v", rep.Limits)
	}
	if rep.Usage.WithTokenUsage != 0 {
		t.Fatalf("usage = %+v", rep.Usage)
	}
}

func TestTTLFindings(t *testing.T) {
	// 假时钟：短间隔的两次读都完成后，把上游的时钟推过 5 分钟；长间隔的读会未命中。
	// 按进度而不是按墙钟推进：-race 下调度慢，按墙钟会让结果抽风。
	var mu sync.Mutex
	clock := time.Unix(1_700_000_000, 0)
	var f *fakeKiro
	shortReads := 0
	f = &fakeKiro{seen: map[string]time.Time{}, report: true, now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }}
	r, buf := newRunner(t, f)
	r.Log = func(format string, a ...any) {
		if strings.Contains(fmt.Sprintf(format, a...), "gap=1ms") {
			mu.Lock()
			if shortReads++; shortReads == 2 {
				clock = clock.Add(6 * time.Minute)
			}
			mu.Unlock()
		}
	}
	gaps := []time.Duration{time.Millisecond, 300 * time.Millisecond}
	if _, err := r.Run(t.Context(), "ttl", Options{Gaps: gaps, Prefix: 2000, Settle: time.Millisecond, Parallel: 4}); err != nil {
		t.Fatal(err)
	}
	recs, _ := Read(buf)
	rep := Analyze(recs)
	if len(rep.TTL) != 4 || rep.Usage.WithTokenUsage == 0 {
		t.Fatalf("ttl = %+v usage %+v", rep.TTL, rep.Usage)
	}
	for _, x := range rep.TTL {
		want := "no"
		if x.Gap == "1ms" {
			want = "yes"
		}
		if x.Hit != want {
			t.Fatalf("gap %s hit=%s want %s: %+v", x.Gap, x.Hit, want, rep.TTL)
		}
	}
	var out bytes.Buffer
	rep.Text(&out)
	if !strings.Contains(out.String(), "[1] TTL") || !strings.Contains(out.String(), "[4] ttl") {
		t.Fatalf("text = %s", out.String())
	}
}

func TestCachePointExperimentSendsPoints(t *testing.T) {
	f := &fakeKiro{seen: map[string]time.Time{}, now: time.Now}
	r, buf := newRunner(t, f)
	if _, err := r.Run(t.Context(), "cachepoint", Options{Prefix: 1000, Settle: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	recs, _ := Read(buf)
	rep := Analyze(recs)
	if len(rep.CachePoint) != 5 || rep.CachePoint[0].Points != "none" || rep.CachePoint[4].Points != "first-user+assistant+tools" {
		t.Fatalf("cachepoint = %+v", rep.CachePoint)
	}
}
