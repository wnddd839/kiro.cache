package meter

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"kiro-proxy/internal/anthropic"
)

func TestCacheSaveLoad(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	c := NewCache(clock)
	r := req("hello", "hi there", "next question")
	p := Analyze(r, "m", 0, ModeAuto)
	c.Commit("a", p)

	path := filepath.Join(t.TempDir(), "pc.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	d := NewCache(clock)
	n, err := d.Load(path)
	if err != nil || n == 0 {
		t.Fatalf("load n=%d err=%v", n, err)
	}
	if s := d.Peek("a", p); s.CacheRead != p.Tokens {
		t.Fatalf("after reload read=%d want %d", s.CacheRead, p.Tokens)
	}
	// 过期条目不载入
	now = now.Add(time.Hour)
	e := NewCache(clock)
	if n, _ := e.Load(path); n != 0 {
		t.Fatalf("expired entries loaded: %d", n)
	}
	// 文件不存在不算错
	if n, err := e.Load(filepath.Join(t.TempDir(), "none.json")); n != 0 || err != nil {
		t.Fatalf("missing file: %d %v", n, err)
	}
}

// 自动断点不受 20 块回看限制：一轮加了很多块（并行工具结果）仍能命中上一轮的前缀。
func TestAutoBreakpointDeepLookback(t *testing.T) {
	c := NewCache(nil)
	r := req("hello")
	c.Commit("a", Analyze(r, "m", 0, ModeAuto))
	var blocks anthropic.Content
	for i := range 30 {
		blocks = append(blocks, anthropic.Block{Type: "text", Text: fmt.Sprintf("tool output %d", i)})
	}
	r.Messages = append(r.Messages, anthropic.Message{Role: "assistant", Content: anthropic.Content{{Type: "text", Text: "ok"}}},
		anthropic.Message{Role: "user", Content: blocks})
	p := Analyze(r, "m", 0, ModeAuto)
	if s := c.Peek("a", p); s.CacheRead == 0 {
		t.Fatalf("auto breakpoint should look back past 20 blocks: %+v", s)
	}
	// explicit 模式的 cache_control 仍是 Anthropic 的 20 块限制
	r.Messages[len(r.Messages)-1].Content[29].CacheControl = &anthropic.CacheControl{Type: "ephemeral"}
	p = Analyze(r, "m", 0, ModeExplicit)
	if s := c.Peek("a", p); s.CacheRead != 0 {
		t.Fatalf("explicit breakpoint must keep 20-block lookback: %+v", s)
	}
}
