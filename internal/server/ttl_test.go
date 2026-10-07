package server

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/usage"
)

type fullUsage struct {
	Input         int `json:"input_tokens"`
	Output        int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheWrite    int `json:"cache_creation_input_tokens"`
	CacheCreation struct {
		E5m int `json:"ephemeral_5m_input_tokens"`
		E1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func decodeFull(t *testing.T, res *http.Response) fullUsage {
	t.Helper()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var m struct{ Usage fullUsage }
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m.Usage
}

// 客户端声明 1h：响应里 5m / 1h 拆分与总量自洽；账本记 cache_write_1h，费用按 2 倍。
func TestAnthropic1hBilled(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	u := decodeFull(t, h.post(t, "/v1/messages", cached(convo("fix the bug"), "1h"), nil))
	if u.CacheWrite == 0 || u.CacheCreation.E1h != u.CacheWrite || u.CacheCreation.E5m != 0 {
		t.Fatalf("usage = %+v", u)
	}
	v := waitRequests(t, h, usage.Query{}, 1)
	e := v.Entries[0]
	if e.CacheWrite1h != u.CacheWrite || v.Totals.CacheWrite1h != int64(u.CacheWrite) {
		t.Fatalf("journal 1h = %d / %d, want %d", e.CacheWrite1h, v.Totals.CacheWrite1h, u.CacheWrite)
	}
	// sonnet 4.5：输入 $3、输出 $15、1h 写入 $6 / M
	want := (float64(u.Input)*3 + float64(u.Output)*15 + float64(u.CacheWrite)*6) / 1e6
	if math.Abs(e.CostUSD-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v (1h at 2x)", e.CostUSD, want)
	}

	// cache_ttl=5m：同样的请求全按 5m
	h5 := newHarness(t, 1, func(c *config.Config) { c.CacheTTL = config.CacheTTL5m })
	h5.up.stream = kiroPlain
	u5 := decodeFull(t, h5.post(t, "/v1/messages", cached(convo("fix the bug"), "1h"), nil))
	if u5.CacheCreation.E1h != 0 || u5.CacheCreation.E5m != u5.CacheWrite || u5.CacheWrite == 0 {
		t.Fatalf("cache_ttl=5m usage = %+v", u5)
	}
}

// Anthropic 协议认 cache_control：没有断点就没有缓存读写；OpenAI 协议走 auto。
func TestCacheModeByProtocol(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	u := decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	if u.CacheWrite != 0 || u.CacheRead != 0 {
		t.Fatalf("anthropic without cache_control cached: %+v", u)
	}
	chat := fmt.Sprintf(`{"model":"claude-sonnet-4.5","messages":[{"role":"system","content":%q},{"role":"user","content":"hi"}]}`, bigSystem)
	res := h.post(t, "/v1/chat/completions", chat, nil)
	var out struct {
		Usage struct {
			Write int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.Usage.Write == 0 {
		t.Fatalf("openai chat should auto-cache: %+v %v", out, err)
	}
}

// #7：上游报了真实 cacheWrite 时，1h 按本地占比搬过去。
func TestReportedCacheWriteKeeps1hShare(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":2,"cacheWriteInputTokens":3000}}`)...)
	}
	u := decodeFull(t, h.post(t, "/v1/messages", cached(convo("x"), "1h"), nil))
	if u.CacheWrite != 3000 || u.CacheCreation.E1h != 3000 {
		t.Fatalf("usage = %+v", u)
	}
}

// 钉的号只是并发打满时：借用别的号答复，但会话仍钉在原号。
func TestOverflowDoesNotRepin(t *testing.T) {
	h := newHarness(t, 2, nil)
	for _, id := range []string{"a0", "a1"} {
		if err := h.pool.SetMaxConcurrent(id, 1); err != nil {
			t.Fatal(err)
		}
	}
	hdr := map[string]string{"X-Session-Id": "s"}
	body := fmt.Sprintf(turn1, "a")
	if res := h.post(t, "/v1/messages", body, hdr); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	pinned := h.up.tokens()[0]

	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	h.up.streamWriter = func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == pinned {
			// 先发首帧：流式在这时就已钉好，之后才是借用请求
			w.Write(frame("assistantResponseEvent", `{"content":"working "}`))
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		w.Write(frame("assistantResponseEvent", `{"content":"ok"}`))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 流式：开始输出时就已钉好，之后的借用请求若改钉会覆盖它
		stream := strings.Replace(fmt.Sprintf(turn2, "a"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
		res := h.post(t, "/v1/messages", stream, hdr) // 占住原号
		_, _ = io.Copy(io.Discard, res.Body)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(h.up.tokens()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn2, "b"), hdr) // 原号忙：借用
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	if got := res.Header.Get("X-Kiro-Account"); got == "" || "tok"+strings.TrimPrefix(got, "a") == pinned {
		t.Fatalf("overflow should use the other account, got %s (pinned %s)", got, pinned)
	}
	open()
	<-done
	// 原号空了：下一轮回到原号（借用没有把会话改钉）
	res = h.post(t, "/v1/messages", fmt.Sprintf(turn2, "c"), hdr)
	if got := res.Header.Get("X-Kiro-Account"); "tok"+strings.TrimPrefix(got, "a") != pinned {
		t.Fatalf("session re-pinned to %s (pinned token %s)", got, pinned)
	}
}
