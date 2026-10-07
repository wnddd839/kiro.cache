package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestReplayCaptured 把抓到的真实客户端请求按顺序回放，打印每轮的缓存拆分。
// KIRO_REPLAY_DIR 指向 {path, headers, body} JSON 文件目录；不设则跳过。
func TestReplayCaptured(t *testing.T) {
	dir := os.Getenv("KIRO_REPLAY_DIR")
	if dir == "" {
		t.Skip("KIRO_REPLAY_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	slices.Sort(files)
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	var in, read, write int
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		hdr := map[string]string{}
		for k, v := range c.Headers {
			if strings.HasPrefix(strings.ToLower(k), "x-claude") || strings.HasPrefix(strings.ToLower(k), "anthropic-") {
				hdr[k] = v
			}
		}
		// 强制非流式，便于读 usage
		var body map[string]any
		_ = json.Unmarshal(c.Body, &body)
		body["stream"] = false
		b, _ := json.Marshal(body)
		u := decodeUsage(t, h.post(t, "/v1/messages", string(b), hdr))
		total := u.Input + u.CacheRead + u.CacheWrite
		in, read, write = in+u.Input, read+u.CacheRead, write+u.CacheWrite
		fmt.Printf("%s input=%d cache_write=%d cache_read=%d output=%d hit=%.1f%%\n",
			filepath.Base(f), u.Input, u.CacheWrite, u.CacheRead, u.Output, 100*float64(u.CacheRead)/float64(max(1, total)))
	}
	fmt.Printf("TOTAL input=%d cache_write=%d cache_read=%d hit=%.1f%%\n", in, write, read, 100*float64(read)/float64(max(1, in+read+write)))
}
