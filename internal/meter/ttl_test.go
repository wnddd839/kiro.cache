package meter

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/turn"
)

func ephemeral(ttl string) *anthropic.CacheControl {
	return &anthropic.CacheControl{Type: "ephemeral", TTL: ttl}
}

// #5：1h 条目命中后按自己的 TTL 续期（不是 5m）；每个查到的命中都续期。
func TestHitRenewsOwnTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := NewCache(func() time.Time { return now })
	r := req("hello")
	r.System[0].CacheControl = ephemeral("1h")
	c.Commit("a", Analyze(r, "m", 0, ModeExplicit))

	// 50 分钟后命中：应续到 now+1h，而不是 now+5m
	now = now.Add(50 * time.Minute)
	if s := c.Commit("a", Analyze(r, "m", 0, ModeExplicit)); s.CacheRead == 0 {
		t.Fatalf("hit expected: %+v", s)
	}
	now = now.Add(30 * time.Minute) // 距上次命中 30 分钟：5m 续期会已过期
	if s := c.Peek("a", Analyze(r, "m", 0, ModeExplicit)); s.CacheRead == 0 {
		t.Fatalf("1h entry renewed only 5m: %+v", s)
	}

	// 两个断点：较短前缀的 1h 条目也要续期，不只是最深的那个
	c2 := NewCache(func() time.Time { return now })
	r2 := req("hello", "answer", "more")
	r2.System[0].CacheControl = ephemeral("1h")
	r2.Messages[2].Content[0].CacheControl = ephemeral("")
	c2.Commit("a", Analyze(r2, "m", 0, ModeExplicit))
	now = now.Add(4 * time.Minute)
	c2.Commit("a", Analyze(r2, "m", 0, ModeExplicit)) // 两个断点都命中
	now = now.Add(30 * time.Minute)                   // 5m 条目过期，1h 条目应还在
	s := c2.Peek("a", Analyze(r2, "m", 0, ModeExplicit))
	p := Analyze(r2, "m", 0, ModeExplicit)
	if s.CacheRead != p.cum[p.sysEnd-1] {
		t.Fatalf("system 1h entry not renewed: %+v (system %d)", s, p.cum[p.sysEnd-1])
	}
}

// 快照里保存 TTL；旧格式（单个过期时间）仍能载入。
func TestSnapshotKeepsTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	c := NewCache(clock)
	r := req("hello")
	r.System[0].CacheControl = ephemeral("1h")
	p := Analyze(r, "m", 0, ModeExplicit)
	c.Commit("a", p)
	path := filepath.Join(t.TempDir(), "pc.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	d := NewCache(clock)
	if _, err := d.Load(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	d.Commit("a", p)
	now = now.Add(30 * time.Minute)
	if s := d.Peek("a", p); s.CacheRead == 0 {
		t.Fatal("TTL lost across save/load: renewed as 5m")
	}

	old := filepath.Join(t.TempDir(), "old.json")
	raw, _ := json.Marshal(map[string]map[string]int64{"a": {"ff": now.Add(time.Minute).UnixMilli()}})
	if err := os.WriteFile(old, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := NewCache(clock).Load(old); n != 1 || err != nil {
		t.Fatalf("legacy snapshot: n=%d err=%v", n, err)
	}
}

// #10：工具的 cache_control（含 TTL）不进指纹。
func TestToolFingerprintIgnoresCacheControl(t *testing.T) {
	tool := func(cc *anthropic.CacheControl) *anthropic.Request {
		r := req("hello")
		r.Tools = []anthropic.Tool{{Name: "read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`), CacheControl: cc}}
		return r
	}
	a := Analyze(tool(nil), "m", 0, ModeOff)
	b := Analyze(tool(ephemeral("1h")), "m", 0, ModeOff)
	if a.hash[0] != b.hash[0] || a.hash[len(a.hash)-1] != b.hash[len(b.hash)-1] {
		t.Fatal("tool fingerprint depends on cache_control")
	}
}

// #9：超过 4 个断点只计前 4 个；5m 后面的 1h 降成 5m；都留警告。
func TestNormalizeMarks(t *testing.T) {
	r := req("a", "b", "c", "d", "e")
	r.System[0].CacheControl = ephemeral("")                // 5m
	r.Messages[0].Content[0].CacheControl = ephemeral("1h") // 1h 在 5m 后：降级
	for i := 1; i < 5; i++ {
		r.Messages[i].Content[0].CacheControl = ephemeral("")
	}
	p := Analyze(r, "m", 0, ModeExplicit)
	if len(p.marks) != MaxBreakpoints || len(p.Warnings) != 2 {
		t.Fatalf("marks %d warnings %v", len(p.marks), p.Warnings)
	}
	for _, mk := range p.marks {
		if mk.ttl != defaultTTL {
			t.Fatalf("1h after 5m kept: %+v", p.marks)
		}
	}
	// 合规的 1h → 5m 不警告
	ok := req("a", "b", "c")
	ok.System[0].CacheControl = ephemeral("1h")
	ok.Messages[2].Content[0].CacheControl = ephemeral("")
	if q := Analyze(ok, "m", 0, ModeExplicit); len(q.Warnings) != 0 || q.marks[0].ttl != longTTL {
		t.Fatalf("valid order rejected: %v %+v", q.Warnings, q.marks)
	}
}

// 三段计费：A 读、B−A 按 1h、C−B 按 5m。
func TestMixedTTLSplit(t *testing.T) {
	c := NewCache(nil)
	r := req("hello", "answer", "more")
	r.System[0].CacheControl = ephemeral("1h")
	r.Messages[2].Content[0].CacheControl = ephemeral("")
	p := Analyze(r, "m", 0, ModeExplicit)
	s := c.Commit("a", p)
	sys := p.cum[p.sysEnd-1]
	if s.CacheRead != 0 || s.CacheWrite != p.Tokens || s.CacheWrite1h != sys {
		t.Fatalf("split = %+v (system %d, total %d)", s, sys, p.Tokens)
	}
	u := turn.Usage{CacheWrite: s.CacheWrite, CacheWrite1h: s.CacheWrite1h}
	if u.CacheWrite5m() != p.Tokens-sys {
		t.Fatalf("5m part = %d", u.CacheWrite5m())
	}
}

// 顶层 cache_control：断点在最后一块。
func TestTopLevelCacheControl(t *testing.T) {
	r := req("hello", "answer", "more")
	r.CacheControl = ephemeral("1h")
	p := Analyze(r, "m", 0, ModeExplicit)
	if len(p.marks) != 1 || p.marks[0].at != len(p.cum)-1 || p.marks[0].ttl != longTTL {
		t.Fatalf("marks = %+v", p.marks)
	}
	p.CapTTL(defaultTTL)
	if p.marks[0].ttl != defaultTTL {
		t.Fatal("CapTTL")
	}
}

// #8：1h 缺省价只对 Claude 按 max(2×input, 5m 写入) 推导，其它模型不加价。
func TestCacheWrite1hOnlyClaude(t *testing.T) {
	p := &Pricer{}
	p.SetOnline(map[string]Price{
		"claude-sonnet-4.5": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.3},
		"deepseek-3.2":      {Input: 0.28, Output: 0.42, CacheWrite: 0.28, CacheRead: 0.028},
	}, time.Now())
	if pr, _ := p.PriceOf("claude-sonnet-4.5"); pr.CacheWrite1h != 6 {
		t.Errorf("claude 1h = %v, want 2×input", pr.CacheWrite1h)
	}
	if pr, _ := p.PriceOf("deepseek-3.2"); pr.CacheWrite1h != pr.CacheWrite {
		t.Errorf("deepseek 1h = %v, want = 5m write %v", pr.CacheWrite1h, pr.CacheWrite)
	}
	if pr, _ := p.PriceOf("glm-5"); pr.CacheWrite1h != pr.CacheWrite {
		t.Errorf("glm builtin 1h = %v", pr.CacheWrite1h)
	}
}

func TestAnthropicMinCacheable(t *testing.T) {
	for m, want := range map[string]int{
		"claude-sonnet-4.5": 1024, "claude-haiku-4.5": 4096, "claude-opus-4.5": 4096,
		"claude-opus-4.7": 2048, "claude-opus-5.5": 512, "claude-sonnet-4": 1024, "auto": 1024,
	} {
		if got := AnthropicMinCacheable(m); got != want {
			t.Errorf("%s = %d, want %d", m, got, want)
		}
	}
}

// #7：1h 占比按本地比例搬到新写入量上。
func TestScale1h(t *testing.T) {
	for _, tc := range []struct{ h, w, to, want int }{
		{500, 1000, 800, 400}, {1000, 1000, 700, 700}, {0, 1000, 700, 0}, {300, 0, 700, 0}, {300, 1000, 0, 0},
	} {
		if got := Scale1h(tc.h, tc.w, tc.to); got != tc.want {
			t.Errorf("Scale1h(%d,%d,%d) = %d, want %d", tc.h, tc.w, tc.to, got, tc.want)
		}
	}
	u := Calibrate(turn.Usage{Input: 4000, CacheWrite: 1000, CacheWrite1h: 500, Output: 100}, 4000+880, 4000, false)
	if u.CacheWrite1h*2 != u.CacheWrite {
		t.Fatalf("calibrated 1h share changed: %+v", u)
	}
}

// 旧条目已过期时重新写入：用这次的 TTL，不继承旧的 1h。
func TestExpiredEntryRewrittenWithNewTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := NewCache(func() time.Time { return now })
	r := req("hello")
	r.System[0].CacheControl = ephemeral("1h")
	c.Commit("a", Analyze(r, "m", 0, ModeExplicit))
	now = now.Add(2 * time.Hour) // 过期
	r.System[0].CacheControl = ephemeral("")
	c.Commit("a", Analyze(r, "m", 0, ModeExplicit)) // 5m 重新写入
	now = now.Add(4 * time.Minute)
	c.Commit("a", Analyze(r, "m", 0, ModeExplicit)) // 命中，按条目 TTL 续期
	now = now.Add(10 * time.Minute)
	if s := c.Peek("a", Analyze(r, "m", 0, ModeExplicit)); s.CacheRead != 0 {
		t.Fatalf("rewritten 5m entry kept the old 1h TTL: %+v", s)
	}
}

// 按 token 估 credits：缓存读按 0.53 折，隐藏 token 随前缀一起走冷 / 热。系数与 2026-10-07 的探测对得上。
func TestEstimateCreditsMatchesProbe(t *testing.T) {
	r := CreditRateOf("claude-sonnet-4.5", nil)
	// 探测：20k 冷前缀（上下文 27755，含隐藏）回 ok → 0.11425；热的同一前缀 → 0.0609
	cold := EstimateCredits(r, turn.Usage{Input: 27755 - 4052, Output: 1}, 4052)
	warm := EstimateCredits(r, turn.Usage{CacheRead: 27757 - 4052, Output: 1}, 4052)
	if math.Abs(cold-0.11425)/0.11425 > 0.05 || math.Abs(warm-0.0609)/0.0609 > 0.05 {
		t.Fatalf("estimate cold %.5f warm %.5f, probe 0.11425 / 0.0609", cold, warm)
	}
	if r.ReadFactor != DefaultReadFactor {
		t.Fatalf("read factor %v", r.ReadFactor)
	}
}
