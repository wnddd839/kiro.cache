package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

// kiroPlain 是真实 Kiro 的样子：不报 tokenUsage，只报上下文百分比与 credits。
func kiroPlain(string) []byte {
	var b []byte
	b = append(b, frame("assistantResponseEvent", `{"content":"hello world"}`)...)
	b = append(b, frame("contextUsageEvent", `{"contextUsagePercentage":2.5}`)...)
	b = append(b, frame("meteringEvent", `{"unit":"credit","unitPlural":"credits","usage":0.5}`)...)
	return b
}

var bigSystem = strings.Repeat("You are a meticulous senior engineer. ", 300)

func convo(turns ...string) string {
	var msgs []string
	for i, t := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":%q}`, role, t))
	}
	return fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":100,"system":%q,"messages":[%s]}`, bigSystem, strings.Join(msgs, ","))
}

// cached 给请求加顶层 cache_control（Anthropic 自动缓存：断点在最后一块）。ttl 为空是 5m。
func cached(body, ttl string) string {
	cc := `{"type":"ephemeral"}`
	if ttl != "" {
		cc = fmt.Sprintf(`{"type":"ephemeral","ttl":%q}`, ttl)
	}
	return strings.Replace(body, `"max_tokens":100,`, `"max_tokens":100,"cache_control":`+cc+`,`, 1)
}

type usageBody struct {
	Input      int     `json:"input_tokens"`
	Output     int     `json:"output_tokens"`
	CacheRead  int     `json:"cache_read_input_tokens"`
	CacheWrite int     `json:"cache_creation_input_tokens"`
	Credits    float64 `json:"credits"`
}

func decodeUsage(t *testing.T, res *http.Response) usageBody {
	t.Helper()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var msg struct{ Usage usageBody }
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	return msg.Usage
}

func TestKiroInputBilledDownstream(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4.5", "deepseek-3.2"} {
		for _, p := range []proto{protoAnthropic, protoChat, protoResponses} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", model, p, streaming), func(t *testing.T) {
					h := newHarness(t, 1, nil)
					h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
					body := map[string]any{"model": model, "stream": streaming}
					path := "/v1/messages"
					switch p {
					case protoChat:
						path = "/v1/chat/completions"
						body["messages"] = []any{map[string]string{"role": "system", "content": bigSystem}, map[string]string{"role": "user", "content": "hi"}}
					case protoResponses:
						path = "/v1/responses"
						body["instructions"], body["input"] = bigSystem, "hi"
					default:
						body["system"] = bigSystem
						body["messages"] = []any{map[string]string{"role": "user", "content": "hi"}}
					}
					raw, _ := json.Marshal(body)
					read := func() (int, int, int) {
						res := h.post(t, path, string(raw), nil)
						if res.StatusCode != http.StatusOK {
							t.Fatalf("status %d", res.StatusCode)
						}
						var final map[string]any
						parse := func(raw []byte) {
							var event struct {
								Usage    map[string]any
								Response struct{ Usage map[string]any }
							}
							if err := json.Unmarshal(raw, &event); err != nil {
								t.Fatal(err)
							}
							if event.Usage != nil {
								final = event.Usage
							}
							if event.Response.Usage != nil {
								final = event.Response.Usage
							}
						}
						if streaming {
							sc := bufio.NewScanner(res.Body)
							for sc.Scan() {
								if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok && data != "[DONE]" {
									parse([]byte(data))
								}
							}
							if err := sc.Err(); err != nil {
								t.Fatal(err)
							}
						} else {
							data, err := io.ReadAll(res.Body)
							if err != nil {
								t.Fatal(err)
							}
							parse(data)
						}
						if final == nil {
							t.Fatal("missing final usage")
						}
						read, write := int(final["cache_read_input_tokens"].(float64)), int(final["cache_creation_input_tokens"].(float64))
						inKey := "input_tokens"
						if p == protoChat {
							inKey = "prompt_tokens"
						}
						in := int(final[inKey].(float64))
						if p != protoAnthropic {
							in -= read + write
						}
						return in, read, write
					}
					hidden := kiro.HiddenTokens(model)
					coldIn, coldRead, coldWrite := read()
					warmIn, warmRead, warmWrite := read()
					if coldIn != hidden || coldRead != 0 || coldWrite == 0 || warmIn != hidden || warmRead != coldWrite || warmWrite != 0 {
						t.Fatalf("hidden=%d cold=(%d,%d,%d) warm=(%d,%d,%d)", hidden, coldIn, coldRead, coldWrite, warmIn, warmRead, warmWrite)
					}
					if p == protoAnthropic {
						res := h.post(t, "/v1/messages/count_tokens", string(raw), nil)
						var count struct {
							Input int `json:"input_tokens"`
						}
						if err := json.NewDecoder(res.Body).Decode(&count); err != nil {
							t.Fatal(err)
						}
						if count.Input != coldIn+coldRead+coldWrite {
							t.Fatalf("count_tokens=%d differs from billed input=%d", count.Input, coldIn+coldRead+coldWrite)
						}
					}
					entries := waitRequests(t, h, usage.Query{}, 2).Entries
					for _, e := range entries {
						if e.Input != hidden || e.CostUSD != h.s.pricer.Cost(model, turn.Usage{Input: e.Input, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Output: e.Output}) {
							t.Fatalf("hidden input missing from billing: %+v", e)
						}
						wantCredits := meter.EstimateCredits(meter.CreditRateOf(model, nil), turn.Usage{Input: e.Input - hidden, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Output: e.Output}, hidden)
						if e.Credits != wantCredits {
							t.Fatalf("hidden input counted twice in credits: got %v want %v", e.Credits, wantCredits)
						}
					}
				})
			}
		}
	}
}

func TestLocalCacheMeteringAcrossTurns(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	session := map[string]string{"X-Claude-Code-Session-Id": "s1"}

	u1 := decodeUsage(t, h.post(t, "/v1/messages", cached(convo("fix the bug"), ""), session))
	if u1.CacheRead != 0 || u1.CacheWrite < 1024 || u1.Output == 0 || u1.Credits != 0.5 {
		t.Fatalf("turn 1 usage = %+v", u1)
	}
	u2 := decodeUsage(t, h.post(t, "/v1/messages", cached(convo("fix the bug", "hello world", "and tests"), ""), session))
	if u2.CacheRead != u1.CacheWrite+u1.Input-kiro.HiddenTokens("claude-sonnet-4.5") {
		t.Fatalf("turn 2 should read turn 1's prefix: %+v after %+v", u2, u1)
	}
	if u2.CacheWrite == 0 {
		t.Fatalf("turn 2 should write its new suffix: %+v", u2)
	}

	// 账本：两笔、credits 记上
	v := waitRequests(t, h, usage.Query{}, 2)
	if v.Totals.Requests != 2 || v.Totals.Credits != 1 || v.Totals.CacheRead != int64(u2.CacheRead) {
		t.Fatalf("journal totals = %+v", v.Totals)
	}
	if e := v.Entries[0]; e.Protocol != "anthropic" || e.Account != "a0" || e.Context != 5000 || e.Status != 200 {
		t.Fatalf("latest entry = %+v", e)
	}
	if st := h.pool.Totals(); st.Credits != 1 {
		t.Fatalf("pool totals = %+v", st)
	}
}

func TestUnsupportedPromptCachingDisablesMeter(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	h.s.models.merge([]kiro.Model{{
		ID: "deepseek-3.2", Name: "deepseek",
		PromptCaching: &kiro.PromptCaching{Supported: false},
	}})
	body := `{"model":"deepseek-3.2","max_tokens":100,"system":"` + bigSystem + `","messages":[{"role":"user","content":"hi"}]}`
	u1 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
	if u1.CacheRead != 0 || u1.CacheWrite != 0 || u1.Input == 0 {
		t.Fatalf("unsupported caching should be all input: %+v", u1)
	}
	u2 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
	if u2.CacheRead != 0 || u2.CacheWrite != 0 {
		t.Fatalf("second turn still no cache: %+v", u2)
	}
}

// 上游的上下文占用（减去 Kiro 自带部分）是真实 token：下游看到的输入 + 输出总量必须等于它。
func TestUsageCalibratedToUpstreamContext(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.s.models.merge([]kiro.Model{{ID: "claude-sonnet-4.5", Name: "claude-sonnet-4.5", Context: 200_000}})
	body := cached(convo("fix the bug"), "")

	res := h.post(t, "/v1/messages/count_tokens", body, nil)
	var ct struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(res.Body).Decode(&ct); err != nil || ct.InputTokens == 0 {
		t.Fatalf("count_tokens = %+v %v", ct, err)
	}
	// 上游内容比本地估算少 15%（开放模型分词器的典型情况）
	content := (ct.InputTokens - kiro.HiddenTokens("claude-sonnet-4.5")) * 85 / 100
	upstream := content + kiro.HiddenTokens("claude-sonnet-4.5")
	h.up.stream = func(string) []byte {
		var b []byte
		b = append(b, frame("assistantResponseEvent", `{"content":"hello world"}`)...)
		b = append(b, frame("contextUsageEvent", fmt.Sprintf(`{"contextUsagePercentage":%v}`, float64(upstream)/2000))...)
		b = append(b, frame("meteringEvent", `{"unit":"credit","usage":0.5}`)...)
		return b
	}
	session := map[string]string{"X-Claude-Code-Session-Id": "cal"}
	u := decodeUsage(t, h.post(t, "/v1/messages", body, session))
	if got := u.Input + u.CacheRead + u.CacheWrite + u.Output; got != upstream {
		t.Fatalf("downstream total %d, want upstream %d (%+v, local %d)", got, upstream, u, ct.InputTokens)
	}
	if u.CacheWrite == 0 || u.Output == 0 || u.Credits != 0.5 {
		t.Fatalf("split lost: %+v", u)
	}

	// 流式：message_delta 的累计 usage 是校正后的
	res = h.post(t, "/v1/messages", strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1), session)
	var last usageBody
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || !strings.Contains(data, `"message_delta"`) {
			continue
		}
		var ev struct{ Usage usageBody }
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		last = ev.Usage
	}
	if got := last.Input + last.CacheRead + last.CacheWrite + last.Output; got != upstream || last.CacheRead == 0 {
		t.Fatalf("stream message_delta usage %+v total %d, want %d with cache read", last, got, upstream)
	}
}

func TestCacheControlIgnoredForThreadLineage(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	session := map[string]string{"X-Claude-Code-Session-Id": "s2"}
	// Claude Code 把断点挂在最后一条消息上，下一轮断点后移：不应视为历史改写
	body1 := `{"model":"claude-sonnet-4.5","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"q1","cache_control":{"type":"ephemeral"}}]}]}`
	body2 := `{"model":"claude-sonnet-4.5","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"q1"}]},{"role":"assistant","content":"a1"},{"role":"user","content":[{"type":"text","text":"q2","cache_control":{"type":"ephemeral"}}]}]}`
	decodeUsage(t, h.post(t, "/v1/messages", body1, session))
	decodeUsage(t, h.post(t, "/v1/messages", body2, session))
	ids := h.up.convIDs()
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("conversation ids = %v, want reused", ids)
	}
}

func TestOpenAIChatNonStreamAndStream(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	body := `{"model":"claude-sonnet-4.5","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	res := h.post(t, "/v1/chat/completions", body, nil)
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var cc struct {
		Object  string
		Choices []struct {
			Message      struct{ Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			PromptTokens int     `json:"prompt_tokens"`
			Credits      float64 `json:"credits"`
		}
	}
	if err := json.NewDecoder(res.Body).Decode(&cc); err != nil {
		t.Fatal(err)
	}
	if cc.Object != "chat.completion" || cc.Choices[0].Message.Content != "hello world" || cc.Choices[0].FinishReason != "stop" {
		t.Fatalf("chat completion = %+v", cc)
	}
	if cc.Usage.PromptTokens == 0 || cc.Usage.Credits != 0.5 {
		t.Fatalf("usage = %+v", cc.Usage)
	}
	// 流式
	res = h.post(t, "/chat/completions", strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1), nil)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	var text strings.Builder
	var sawUsage, sawDone bool
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var ch struct {
			Choices []struct{ Delta struct{ Content string } }
			Usage   *struct {
				PromptTokens int `json:"prompt_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			t.Fatalf("chunk %q: %v", data, err)
		}
		for _, c := range ch.Choices {
			text.WriteString(c.Delta.Content)
		}
		if ch.Usage != nil && ch.Usage.PromptTokens > 0 {
			sawUsage = true
		}
	}
	if text.String() != "hello world" || !sawUsage || !sawDone {
		t.Fatalf("stream text %q usage %v done %v", text.String(), sawUsage, sawDone)
	}

	waitRequests(t, h, usage.Query{Protocol: "openai-chat"}, 2)
}

func TestOpenAIResponses(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	res := h.post(t, "/v1/responses", `{"model":"claude-sonnet-4.5","instructions":"be brief","input":"hi"}`, nil)
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var r struct {
		Object     string
		Status     string
		OutputText string `json:"output_text"`
		Usage      struct {
			InputTokens int `json:"input_tokens"`
		}
	}
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.Object != "response" || r.Status != "completed" || r.OutputText != "hello world" || r.Usage.InputTokens == 0 {
		t.Fatalf("response = %+v", r)
	}

	res = h.post(t, "/v1/responses", `{"model":"x","input":"hi","previous_response_id":"resp_1"}`, nil)
	if res.StatusCode != 400 {
		t.Fatalf("previous_response_id status %d", res.StatusCode)
	}
	var e struct {
		Error struct{ Message, Type string }
	}
	_ = json.NewDecoder(res.Body).Decode(&e)
	if e.Error.Type != "invalid_request_error" {
		t.Fatalf("openai error shape = %+v", e)
	}
}

func TestKeyBudgetAndModelAllowList(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	limited, err := h.s.keys.Create(keys.Key{Name: "alice", LimitCredits: 0.5, Models: []string{"claude-sonnet*"}})
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"Authorization": "Bearer " + limited.Secret}

	// 未知 key：库里有 key 就开始校验
	if res := h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": "nope"}); res.StatusCode != 401 {
		t.Fatalf("unknown key status %d", res.StatusCode)
	}
	// 不在白名单的模型
	if res := h.post(t, "/v1/messages", strings.Replace(convo("hi"), "claude-sonnet-4.5", "claude-opus-4.5", 1), auth); res.StatusCode != 403 {
		t.Fatalf("model not allowed status %d", res.StatusCode)
	}
	// 第一次用掉 0.5 credits
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), auth))
	// 预算用尽：402 + Retry-After；OpenAI 协议给 insufficient_quota
	res := h.post(t, "/v1/chat/completions", `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`, auth)
	if res.StatusCode != http.StatusPaymentRequired || res.Header.Get("Retry-After") == "" || res.Header.Get("X-Should-Retry") != "false" {
		t.Fatalf("over budget status %d retry %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	var e struct{ Error struct{ Type string } }
	_ = json.NewDecoder(res.Body).Decode(&e)
	if e.Error.Type != "insufficient_quota" {
		t.Fatalf("error type %q", e.Error.Type)
	}

	v := waitRequests(t, h, usage.Query{Key: limited.ID}, 1)
	if v.Totals.Requests != 1 || v.Totals.Credits != 0.5 || v.Entries[0].KeyName != "alice" {
		t.Fatalf("key spend = %+v", v.Totals)
	}
	// 禁用的 key
	if _, err := h.s.keys.Update(limited.ID, func(k *keys.Key) { k.Disabled = true }); err != nil {
		t.Fatal(err)
	}
	if res := h.post(t, "/v1/messages", convo("hi"), auth); res.StatusCode != 401 {
		t.Fatalf("disabled key status %d", res.StatusCode)
	}
}

func TestKeyRPM(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	k, _ := h.s.keys.Create(keys.Key{Name: "rpm", RPM: 1})
	auth := map[string]string{"x-api-key": k.Secret}
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), auth))
	res := h.post(t, "/v1/messages", convo("hi"), auth)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("rpm status %d", res.StatusCode)
	}
}

func TestConfigAPIKeysStillWork(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.APIKeys = []string{"legacy"} })
	h.up.stream = kiroPlain
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": "legacy"}))
	if res := h.post(t, "/v1/messages", convo("hi"), nil); res.StatusCode != 401 {
		t.Fatalf("no key status %d", res.StatusCode)
	}
}

func TestCostBasisCredits(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.CostBasis = "credits"; c.CreditUSD = 0.04 })
	h.up.stream = kiroPlain
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	v := waitRequests(t, h, usage.Query{}, 1)
	if got := v.Totals.CostUSD; got < 0.0199 || got > 0.0201 {
		t.Fatalf("cost = %v, want 0.5 credits × 0.04", got)
	}
}

// waitRequests 等账本里出现 n 笔：记账在 handler 返回前，可能晚于客户端读完响应。
func waitRequests(t *testing.T, h *harness, q usage.Query, n int64) usage.View {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		v := h.s.usage.Query(q)
		if v.Totals.Requests >= n || time.Now().After(deadline) {
			if v.Totals.Requests != n {
				t.Fatalf("journal requests = %d, want %d", v.Totals.Requests, n)
			}
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
}
