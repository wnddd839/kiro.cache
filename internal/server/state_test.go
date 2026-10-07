package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/pool"
)

// 重启后：同一会话回到同一号、沿用同一个 conversationId，本地 prompt cache 仍判命中。
func TestSessionStateSurvivesRestart(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "promptcache.json")
	h := newHarness(t, 2, func(c *config.Config) { c.CacheFile = cacheFile })
	h.up.stream = kiroPlain
	hdr := map[string]string{"X-Session-Id": "s"}
	decodeUsage(t, h.post(t, "/v1/messages", cached(convo("fix the bug"), ""), hdr))
	if err := h.s.saveState(); err != nil {
		t.Fatal(err)
	}
	firstTok, firstConv := h.up.tokens()[0], h.up.convIDs()[0]

	// 重启：从同一号池文件重建号池（钉号表是空的），再建服务
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p2, err := pool.Open(h.poolPath, pool.Options{Client: h.pool.Client(), Logger: log, PinWait: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Pins().Dump()) != 0 {
		t.Fatal("fresh pool already has pins")
	}
	// 后台落盘协程要在 TempDir 清理前停下，否则 Windows 上删不掉目录
	bg, stop := context.WithCancel(context.Background())
	s2, err := New(bg, h.s.cfg, p2, log, Stores{})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(); s2.Wait() })
	srv := httptest.NewServer(s2.Handler())
	t.Cleanup(srv.Close)
	h.srv, h.s, h.pool = srv, s2, p2

	u := decodeUsage(t, h.post(t, "/v1/messages", cached(convo("fix the bug", "hello world", "and tests"), ""), hdr))
	if u.CacheRead == 0 {
		t.Fatalf("prompt cache lost across restart: %+v", u)
	}
	if tok, conv := h.up.tokens()[1], h.up.convIDs()[1]; tok != firstTok || conv != firstConv {
		t.Fatalf("after restart token %s conv %s, want %s %s", tok, conv, firstTok, firstConv)
	}
}

// 只有 prompt cache、没有会话快照（旧版本或半套状态）：两个都不用。
func TestHalfStateDiscarded(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "promptcache.json")
	c := meter.NewCache(nil)
	if err := c.Save(cacheFile); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, 1, func(c *config.Config) { c.CacheFile = cacheFile })
	if _, _, err := h.s.loadState(); err == nil {
		t.Fatal("prompt cache without session state must be rejected")
	}
	if _, err := os.Stat(sessionFile(cacheFile)); !os.IsNotExist(err) {
		t.Fatalf("session file: %v", err)
	}
}

// 会话快照在、prompt cache 文件不在：会话快照也不用。
func TestSessionsWithoutCacheDiscarded(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "promptcache.json")
	h := newHarness(t, 1, func(c *config.Config) { c.CacheFile = cacheFile })
	h.up.stream = kiroPlain
	decodeUsage(t, h.post(t, "/v1/messages", cached(convo("x"), ""), map[string]string{"X-Session-Id": "s"}))
	if err := h.s.saveState(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cacheFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.loadState(); err == nil {
		t.Fatal("session state without prompt cache must be rejected")
	}
}
