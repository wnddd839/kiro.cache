package meter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/turn"
)

func TestCount(t *testing.T) {
	for _, tc := range []struct {
		s      string
		lo, hi int
	}{
		{"", 0, 0},
		{"hi", 1, 1},
		{strings.Repeat("word ", 100), 100, 120},
		{strings.Repeat("中文", 50), 40, 130}, // 重复串会被 BPE 合并，偏少
	} {
		if n := Count(tc.s); n < tc.lo || n > tc.hi {
			t.Errorf("Count(%.20q) = %d, want [%d,%d]", tc.s, n, tc.lo, tc.hi)
		}
	}
}

// testdata/kiro_truth.json 是发到 Kiro 实测的语料：kiro 字段是上游 contextUsagePercentage 换算的 token
// （已减去空请求基线）。合计偏差要 ≤ 1%，单份真实语料平均 ≤ 5%、最大 ≤ 15%。
func TestCountMatchesKiro(t *testing.T) {
	raw, err := os.ReadFile("testdata/kiro_truth.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string `json:"name"`
		Synthetic bool   `json:"synthetic"`
		Kiro      int    `json:"kiro"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	var got, want, n int
	var absErr float64
	for _, c := range cases {
		c1 := Count(c.Text)
		e := float64(c1-c.Kiro) / float64(c.Kiro)
		t.Logf("%-16s kiro=%5d err=%+5.1f%%", c.Name, c.Kiro, e*100)
		if c.Synthetic {
			continue // base64 / hex / 纯数字：只看不考
		}
		if math.Abs(e) > 0.15 {
			t.Errorf("%s: off by %.1f%%", c.Name, e*100)
		}
		got += c1
		want += c.Kiro
		absErr += math.Abs(e)
		n++
	}
	if agg := float64(got-want) / float64(want); math.Abs(agg) > 0.01 {
		t.Errorf("aggregate off by %.2f%%", agg*100)
	}
	if mean := absErr / float64(n); mean > 0.05 {
		t.Errorf("mean abs error %.2f%%", mean*100)
	}
}

func pngB64(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestImage(t *testing.T) {
	if n := Image(pngB64(t, 200, 150)); n != 40 {
		t.Errorf("200x150 = %d, want 40", n)
	}
	if n := Image(pngB64(t, 4000, 3000)); n != imageMaxTokens {
		t.Errorf("big = %d, want cap %d", n, imageMaxTokens)
	}
	if n := Image("not base64!!"); n != imageUnknownTokens {
		t.Errorf("garbage = %d", n)
	}
}

func TestCalibrate(t *testing.T) {
	u := turn.Usage{Input: 4100, KiroInput: 4000, CacheRead: 800, CacheWrite: 100, Output: 50, Reasoning: 20, Credits: 0.5}
	// 上游内容 840 = 本地 1050 × 0.8
	got := Calibrate(u, 4000+840, 4000, false)
	want := turn.Usage{Input: 4080, KiroInput: 4000, CacheRead: 640, CacheWrite: 80, Output: 40, Reasoning: 16, Credits: 0.5}
	if got != want {
		t.Fatalf("calibrated = %+v, want %+v", got, want)
	}
	if got.PromptTokens()+got.Output != 4840 {
		t.Fatalf("total %d, want upstream 4840 including Kiro input", got.PromptTokens()+got.Output)
	}
	// 舍入差归到普通输入，总量始终等于上游
	odd := Calibrate(turn.Usage{Input: 3, CacheRead: 3, CacheWrite: 3, Output: 3}, 13, 0, false)
	if odd.PromptTokens()+odd.Output != 13 || odd.Input < 0 {
		t.Fatalf("rounding = %+v", odd)
	}
	// 比例离谱、上游不足基线、本地为空：不动
	for _, tc := range []struct{ up, hidden int }{{4000 + 5000, 4000}, {4000 + 100, 4000}, {3000, 4000}} {
		if got := Calibrate(u, tc.up, tc.hidden, false); got != u {
			t.Errorf("Calibrate(%d,%d) = %+v, want unchanged", tc.up, tc.hidden, got)
		}
	}
	if got := Calibrate(turn.Usage{}, 5000, 4000, false); got != (turn.Usage{}) {
		t.Errorf("empty = %+v", got)
	}
}

var longSys = strings.Repeat("You are a careful assistant. ", 300) // 约 2.3k token

func req(turns ...string) *anthropic.Request {
	r := &anthropic.Request{System: anthropic.Content{{Type: "text", Text: longSys}}}
	for i, s := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		r.Messages = append(r.Messages, anthropic.Message{Role: role, Content: anthropic.Content{{Type: "text", Text: s}}})
	}
	return r
}

func TestCacheAutoConversation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := NewCache(func() time.Time { return now })

	p1 := Analyze(req("hello"), "m", 0, ModeAuto)
	s1 := c.Commit("acc", p1)
	if s1.CacheRead != 0 || s1.CacheWrite != p1.Tokens || s1.Input != 0 {
		t.Fatalf("turn1 = %+v, tokens %d", s1, p1.Tokens)
	}

	now = now.Add(time.Minute)
	p2 := Analyze(req("hello", "hi there", "next question"), "m", 0, ModeAuto)
	s2 := c.Commit("acc", p2)
	if s2.CacheRead != p1.Tokens {
		t.Fatalf("turn2 read = %d, want %d (%+v)", s2.CacheRead, p1.Tokens, s2)
	}
	if s2.CacheRead+s2.CacheWrite+s2.Input != p2.Tokens || s2.CacheWrite != p2.Tokens-p1.Tokens {
		t.Fatalf("turn2 split = %+v, tokens %d", s2, p2.Tokens)
	}

	// 另一个号上没有缓存
	if s := c.Peek("other", p2); s.CacheRead != 0 {
		t.Fatalf("other scope read %d", s.CacheRead)
	}
	// 换模型（seed）不命中
	if s := c.Peek("acc", Analyze(req("hello"), "m2", 0, ModeAuto)); s.CacheRead != 0 {
		t.Fatalf("other model read %d", s.CacheRead)
	}
	// 改了历史：只有 system 前缀命中
	edited := Analyze(req("HELLO", "hi there", "next question"), "m", 0, ModeAuto)
	if s := c.Peek("acc", edited); s.CacheRead != edited.cum[edited.sysEnd-1] {
		t.Fatalf("edited read %d, want system prefix %d", s.CacheRead, edited.cum[edited.sysEnd-1])
	}

	// 过期
	now = now.Add(6 * time.Minute)
	if s := c.Peek("acc", p2); s.CacheRead != 0 {
		t.Fatalf("expired read %d", s.CacheRead)
	}
}

func TestCacheExplicitAndShort(t *testing.T) {
	c := NewCache(nil)
	// 没有 cache_control：explicit 模式不缓存
	p := Analyze(req("hello"), "m", 0, ModeExplicit)
	if s := c.Commit("a", p); s.Input != p.Tokens || s.CacheWrite != 0 {
		t.Fatalf("explicit no marks = %+v", s)
	}
	// 有断点（1h）
	r := req("hello")
	r.System[0].CacheControl = &anthropic.CacheControl{Type: "ephemeral", TTL: "1h"}
	p = Analyze(r, "m", 0, ModeExplicit)
	s := c.Commit("a", p)
	sys := p.cum[p.sysEnd-1]
	if s.CacheWrite != sys || s.CacheWrite1h != sys || s.Input != p.Tokens-sys {
		t.Fatalf("explicit 1h = %+v (sys %d, total %d)", s, sys, p.Tokens)
	}
	if s := c.Commit("a", p); s.CacheRead != sys || s.CacheWrite != 0 {
		t.Fatalf("explicit reread = %+v", s)
	}
	// 太短不写
	short := &anthropic.Request{Messages: []anthropic.Message{{Role: "user", Content: anthropic.Content{{Type: "text", Text: "x"}}}}}
	ps := Analyze(short, "m", 0, ModeAuto)
	if s := c.Commit("a", ps); s.CacheWrite != 0 || s.Input != ps.Tokens {
		t.Fatalf("short = %+v", s)
	}
	// off
	po := Analyze(r, "m", 0, ModeOff)
	if s := c.Commit("a", po); s.Input != po.Tokens {
		t.Fatalf("off = %+v", s)
	}
	// auto 忽略客户端 1h cache_control，始终 5m 滑动 TTL
	auto := Analyze(r, "m", 0, ModeAuto)
	sa := c.Commit("b", auto)
	if sa.CacheWrite1h != 0 || sa.CacheWrite == 0 {
		t.Fatalf("auto should ignore client 1h TTL: %+v", sa)
	}
}

func TestCacheIgnoresCacheControlAndThinking(t *testing.T) {
	a := req("hello", "answer")
	b := req("hello", "answer")
	b.Messages[0].Content[0].CacheControl = &anthropic.CacheControl{Type: "ephemeral"}
	b.Messages[1].Content = append(anthropic.Content{{Type: "thinking", Thinking: "secret", Signature: "sig"}}, b.Messages[1].Content...)
	a.Messages[1].Content = append(anthropic.Content{{Type: "thinking", Thinking: "other", Signature: "x"}}, a.Messages[1].Content...)
	pa, pb := Analyze(a, "m", 0, ModeOff), Analyze(b, "m", 0, ModeOff)
	if pa.hash[len(pa.hash)-1] != pb.hash[len(pb.hash)-1] {
		t.Fatal("fingerprint depends on cache_control / thinking")
	}
}

func TestPricer(t *testing.T) {
	p := &Pricer{CreditUSD: 0.02}
	pr, ok := p.PriceOf("claude-sonnet-4.5")
	if !ok || pr.Input != 3 || pr.CacheRead != 0.3 || pr.CacheWrite != 3.75 || pr.CacheWrite1h != 6 || pr.Source != "builtin" {
		t.Fatalf("sonnet = %+v %v", pr, ok)
	}
	for model, in := range map[string]float64{
		"claude-opus-4.1": 15, "claude-opus-4": 15, "claude-opus-4.5": 5, "claude-opus-5.5": 4,
		"claude-opus-4.9": 5, // 未知版本走家族条目，不被 claude-opus-4 吃掉
		"claude-sonnet-5": 2, "claude-sonnet-4": 3, "claude-sonnet-4.5-thinking": 3,
		"deepseek-3.2": 0.28, "deepseek/deepseek-v3.2": 0.28, "Qwen3-Coder-Next": 0.12,
	} {
		if pr, _ := p.PriceOf(model); pr.Input != in {
			t.Errorf("%s input = %v, want %v", model, pr.Input, in)
		}
	}
	if pr, _ := p.PriceOf("claude-opus-5.5"); pr.CacheRead != 0.2 {
		t.Errorf("opus 5.5 cache read = %v (0.05x)", pr.CacheRead)
	}
	if pr, _ := p.PriceOf("glm-5"); pr.CacheWrite != pr.Input {
		t.Errorf("glm cache write = %v, want = input (no write premium)", pr.CacheWrite)
	}
	if _, ok := p.PriceOf("mystery"); ok {
		t.Fatal("unknown model matched")
	}
	u := turn.Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000, Credits: 2}
	if got := p.APICost("claude-sonnet-4.5", u); math.Abs(got-(3+15+0.3+3.75)) > 1e-9 {
		t.Fatalf("api cost = %v", got)
	}
	u1h := u
	u1h.CacheWrite1h = 1_000_000
	if got := p.APICost("claude-sonnet-4.5", u1h); math.Abs(got-(3+15+0.3+6)) > 1e-9 {
		t.Fatalf("api cost 1h = %v", got)
	}
	// GPT-5.6：输入超过 272K 整单走长上下文档
	short := turn.Usage{Input: 100_000, Output: 1000}
	long := turn.Usage{Input: 300_000, Output: 1000}
	if got, want := p.APICost("gpt-5.6-luna", short), (100_000*0.2+1000*1.2)/1e6; math.Abs(got-want) > 1e-12 {
		t.Errorf("luna short = %v, want %v", got, want)
	}
	if got, want := p.APICost("gpt-5.6-luna", long), (300_000*0.4+1000*1.8)/1e6; math.Abs(got-want) > 1e-12 {
		t.Errorf("luna long = %v, want %v", got, want)
	}
	p.Basis = BasisCredits
	if got := p.Cost("claude-sonnet-4.5", u); math.Abs(got-0.04) > 1e-12 {
		t.Fatalf("credit cost = %v", got)
	}

	// 在线价覆盖内置，配置覆盖在线
	p.SetOnline(map[string]Price{"claude-sonnet-4.5": {Input: 2.5, Output: 12, Source: "openrouter"}, "New-Model": {Input: 1, Output: 1}}, time.Now())
	if pr, _ := p.PriceOf("claude-sonnet-4.5"); pr.Input != 2.5 || pr.Source != "openrouter" {
		t.Errorf("online = %+v", pr)
	}
	if _, ok := p.PriceOf("new-model"); !ok {
		t.Error("online key not canonicalised")
	}
	p.Overrides = map[string]Price{"claude-sonnet": {Input: 1, Output: 2}}
	if pr, _ := p.PriceOf("claude-sonnet-4.5"); pr.Input != 1 || math.Abs(pr.CacheRead-0.1) > 1e-12 || pr.Source != "config" {
		t.Fatalf("override = %+v", pr)
	}
}

func TestKeepOnlineDropsNonKiro(t *testing.T) {
	p := &Pricer{}
	p.SetOnline(map[string]Price{
		"claude-sonnet-4.5": {Input: 2.5, Output: 12, Source: "openrouter"},
		"gpt-4o":            {Input: 5, Output: 15, Source: "openrouter"},
	}, time.Now())
	p.KeepOnline(IsBuiltinID)
	online, _ := p.Online()
	if _, ok := online["gpt-4o"]; ok {
		t.Fatal("non-Kiro online entry kept")
	}
	tab := p.Table()
	if _, ok := tab["gpt-4o"]; ok {
		t.Fatal("non-Kiro leaked into table")
	}
	if tab["claude-sonnet-4.5"].Input != 2.5 || tab["claude-sonnet-4.5"].Source != "openrouter" {
		t.Errorf("sonnet = %+v", tab["claude-sonnet-4.5"])
	}
}

func TestParseOpenRouter(t *testing.T) {
	b, err := os.ReadFile("testdata/openrouter_models.json")
	if err != nil {
		t.Fatal(err)
	}
	or, err := ParseOpenRouter(b)
	if err != nil {
		t.Fatal(err)
	}
	if d := or["deepseek-3.2"]; d.Input != 0.28 || d.Output != 0.42 || d.CacheRead != 0.028 || d.CacheWrite != 0.28 {
		t.Errorf("deepseek-3.2 = %+v", d)
	}
	if q := or["qwen3-coder-next"]; q.Input != 0.12 || q.Output != 0.8 {
		t.Errorf("qwen3-coder-next = %+v", q)
	}
	s := or["claude-sonnet-4.5"]
	if s.Input != 3 || s.Output != 15 || s.CacheWrite != 3.75 || s.CacheWrite1h != 6 || s.CacheRead != 0.3 {
		t.Errorf("sonnet = %+v", s)
	}
	if s.LongAbove != 200_000 || s.Long == nil || s.Long.Input != 6 || s.Long.Output != 22.5 || s.Long.CacheWrite != 7.5 || s.Long.CacheWrite1h != 12 {
		t.Errorf("sonnet long = above %d %+v", s.LongAbove, s.Long)
	}
	sol := or["gpt-5.6-sol"]
	if sol.Input != 2 || sol.Output != 10 || sol.CacheWrite != 2.5 || sol.CacheRead != 0.2 || sol.LongAbove != 272_000 || sol.Long == nil || sol.Long.Input != 4 || sol.Long.Output != 15 {
		t.Errorf("gpt-5.6-sol = %+v long %+v", sol, sol.Long)
	}
	if _, ok := or["claude-sonnet-4.5:batch"]; ok {
		t.Error("variant kept")
	}
}

func TestOnlinePersist(t *testing.T) {
	path := t.TempDir() + "/prices.json"
	a := &Pricer{}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	a.SetOnline(map[string]Price{"glm-5": {Input: 0.7, Output: 2, Source: "openrouter"}}, at)
	if err := a.SaveOnline(path); err != nil {
		t.Fatal(err)
	}
	b := &Pricer{}
	if n, err := b.LoadOnline(path); err != nil || n != 1 {
		t.Fatalf("load = %d %v", n, err)
	}
	if pr, _ := b.PriceOf("glm-5"); pr.Input != 0.7 {
		t.Errorf("loaded = %+v", pr)
	}
	if _, got := b.Online(); !got.Equal(at) {
		t.Errorf("fetched_at = %v", got)
	}
	if n, err := (&Pricer{}).LoadOnline(t.TempDir() + "/missing.json"); n != 0 || err != nil {
		t.Errorf("missing = %d %v", n, err)
	}
}
