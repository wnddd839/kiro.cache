package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

// 没设 admin_token：只放行本机（回环连接 + 本机 Host）。局域网与 DNS rebinding 都拒。
func TestAdminWithoutTokenOnlyLoopback(t *testing.T) {
	h := newHarness(t, 1, nil)
	for _, tc := range []struct {
		remote, host string
		ok           bool
	}{
		{"127.0.0.1:5000", "127.0.0.1:8787", true},
		{"[::1]:5000", "[::1]:8787", true},
		{"127.0.0.1:5000", "localhost:8787", true},
		{"192.168.1.20:5000", "192.168.1.10:8787", false}, // 局域网
		{"127.0.0.1:5000", "evil.example:8787", false},    // DNS rebinding
	} {
		r := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
		r.RemoteAddr, r.Host = tc.remote, tc.host
		w := httptest.NewRecorder()
		h.s.Handler().ServeHTTP(w, r)
		if got := w.Code == http.StatusOK; got != tc.ok {
			t.Errorf("remote %s host %s: status %d, want ok=%v", tc.remote, tc.host, w.Code, tc.ok)
		}
	}
	// 设了 token：局域网带 token 可以，本机不带也不行
	h2 := newHarness(t, 1, func(c *config.Config) { c.AdminToken = "adm" })
	r := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	r.RemoteAddr, r.Host = "192.168.1.20:5000", "192.168.1.10:8787"
	r.Header.Set("X-Admin-Token", "adm")
	w := httptest.NewRecorder()
	h2.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("lan with token: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	r.RemoteAddr, r.Host = "127.0.0.1:5000", "127.0.0.1:8787"
	w = httptest.NewRecorder()
	h2.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("loopback without token when token set: %d", w.Code)
	}
}

// {"import":"ide","path":...} 不能读任意文件（之后还会写回）。
func TestImportIDERejectsArbitraryPath(t *testing.T) {
	h := newHarness(t, 0, nil)
	dir := t.TempDir()
	other := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(other, []byte(`{"accessToken":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{other, "relative/kiro-auth-token.json", filepath.Join(dir, "missing", "kiro-auth-token.json")} {
		status, body := h.api(t, http.MethodPost, "/admin/accounts", fmt.Sprintf(`{"import":"ide","path":%q}`, p))
		if status != http.StatusBadRequest {
			t.Errorf("path %s: status %d %v", p, status, body)
		}
	}
	if h.pool.Len() != 0 {
		t.Fatal("account added from arbitrary file")
	}
	if _, err := kiro.CheckIDEPath(filepath.Join(dir, "kiro-auth-token.json")); err == nil {
		t.Fatal("missing file must be rejected")
	}
	good := filepath.Join(dir, "kiro-auth-token.json")
	if err := os.WriteFile(good, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := kiro.CheckIDEPath(good); err != nil {
		t.Fatalf("real token path rejected: %v", err)
	}
}

// 上游只报 outputTokens：输入侧不能被 0 覆盖，仍按上下文百分比校准。
func TestPartialTokenUsageKeepsLocalInput(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		var b []byte
		b = append(b, frame("assistantResponseEvent", `{"content":"hello world"}`)...)
		b = append(b, frame("metadataEvent", `{"tokenUsage":{"outputTokens":3}}`)...)
		b = append(b, frame("contextUsageEvent", `{"contextUsagePercentage":2.5}`)...)
		return b
	}
	u := decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	if u.Input+u.CacheRead+u.CacheWrite == 0 {
		t.Fatalf("input zeroed by partial tokenUsage: %+v", u)
	}
	if u.Output != 3 {
		t.Fatalf("output = %d, want upstream 3", u.Output)
	}
}

// 非流式中途失败换号：第一次花掉的 credits 也入账。
func TestFailoverAttemptCreditsRecorded(t *testing.T) {
	h := newHarness(t, 2, nil)
	var mu sync.Mutex
	bad := ""
	h.up.stream = func(tok string) []byte {
		mu.Lock()
		if bad == "" {
			bad = tok
		}
		first := tok == bad
		mu.Unlock()
		if first {
			return append(append(frame("assistantResponseEvent", `{"content":"partial "}`),
				frame("meteringEvent", `{"unit":"credit","usage":0.25}`)...),
				frame("throttlingError", `{"message":"Rate limited"}`)...)
		}
		return kiroPlain(tok)
	}
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	v := waitRequests(t, h, usage.Query{}, 1)
	if got := v.Totals.Credits; got < 0.749 || got > 0.751 {
		t.Fatalf("credits = %v, want 0.25 (failed attempt) + 0.5", got)
	}
}

// 客户端中断（Esc）：已扣的 credits 与输入入账，不是 0。
func TestClientAbortStillRecorded(t *testing.T) {
	h := newHarness(t, 1, nil)
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	// 写一半就停住，等客户端断开
	blocking := &blockingStream{unblock: unblock}
	h.up.streamWriter = blocking.write

	ctx, cancel := context.WithCancel(t.Context())
	body := strings.Replace(convo("hi"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "text_delta") {
			break
		}
	}
	cancel() // 用户按 Esc
	res.Body.Close()

	v := waitRequests(t, h, usage.Query{}, 1)
	e := v.Entries[0]
	if e.Credits < 0.249 || e.Input+e.CacheRead+e.CacheWrite == 0 || e.Output == 0 {
		t.Fatalf("aborted request recorded as %+v", e)
	}
}

type blockingStream struct{ unblock chan struct{} }

func (b *blockingStream) write(w http.ResponseWriter, r *http.Request) {
	w.Write(frame("assistantResponseEvent", `{"content":"partial answer"}`))
	w.Write(frame("meteringEvent", `{"unit":"credit","usage":0.25}`))
	w.(http.Flusher).Flush()
	select {
	case <-r.Context().Done():
	case <-b.unblock:
	}
}

// 并行请求：准入预留额度，请求数上限不会被同时到达的请求穿透。
func TestAdmissionReservesBudget(t *testing.T) {
	h := newHarness(t, 1, nil)
	gate := make(chan struct{})
	var openGate sync.Once
	release := func() { openGate.Do(func() { close(gate) }) }
	t.Cleanup(release) // 失败时也放行，否则 httptest 关服务会一直等
	h.up.streamWriter = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(kiroPlain(""))
	}
	k, err := h.s.keys.Create(keys.Key{Name: "team", LimitRequests: 2})
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"x-api-key": k.Secret}
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for range 6 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/v1/messages", strings.NewReader(convo("hi")))
			for k, v := range auth {
				req.Header.Set(k, v)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			codes <- res.StatusCode
		})
	}
	// 等拒掉的 4 个先返回，再放行上游
	deadline := time.After(3 * time.Second)
	refused := 0
	for refused < 4 {
		select {
		case c := <-codes:
			if c != http.StatusPaymentRequired {
				t.Fatalf("unexpected status %d before upstream answered", c)
			}
			refused++
		case <-deadline:
			release()
			t.Fatalf("only %d of 4 over-limit requests refused while 2 in flight", refused)
		}
	}
	release()
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("admitted request status %d", c)
		}
	}
	if r := h.s.holds.reserved(k.ID); r.Requests != 0 {
		t.Fatalf("holds leaked: %+v", r)
	}
}

// 不同下游 key 发同样的短提示不共用上游会话。
func TestThreadIsolatedPerKey(t *testing.T) {
	h := newHarness(t, 1, nil)
	a, _ := h.s.keys.Create(keys.Key{Name: "a"})
	b, _ := h.s.keys.Create(keys.Key{Name: "b"})
	body := fmt.Sprintf(turn1, "x")
	for _, k := range []keys.Key{a, a, b} {
		if res := h.post(t, "/v1/messages", body, map[string]string{"x-api-key": k.Secret, "X-Session-Id": "title"}); res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
	}
	ids := h.up.convIDs()
	if ids[0] != ids[1] {
		t.Fatalf("same key should keep its conversation: %v", ids)
	}
	if ids[2] == ids[0] {
		t.Fatalf("different keys share a conversation: %v", ids)
	}
}

// 下游最小缓存长度按 Anthropic 表计量，不跟随上游目录的 minimumTokensPerCacheCheckpoint。
func TestCacheMinimumUsesAnthropicTable(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = kiroPlain
	h.s.models.merge([]kiro.Model{{ID: "claude-sonnet-4.5", Context: 200000,
		PromptCaching: &kiro.PromptCaching{Supported: true, MinTokens: 1 << 20}}})
	short := strings.Replace(convo("hi"), bigSystem, "short system", 1)
	u := decodeUsage(t, h.post(t, "/v1/messages", short, nil))
	if u.CacheRead != 0 || u.CacheWrite != 0 {
		t.Fatalf("prefix below Anthropic minimum must not cache: %+v", u)
	}
	cold := decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	warm := decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	if cold.CacheWrite == 0 || warm.CacheRead == 0 {
		t.Fatalf("upstream minimum must not suppress local metering: cold=%+v warm=%+v", cold, warm)
	}
}

// 换号重试：失败的尝试单独一行（retried）、不计请求数；两行 Request 相同。
func TestRetriedAttemptOwnRow(t *testing.T) {
	h := newHarness(t, 2, nil)
	var mu sync.Mutex
	bad := ""
	h.up.stream = func(tok string) []byte {
		mu.Lock()
		if bad == "" {
			bad = tok
		}
		first := tok == bad
		mu.Unlock()
		if first {
			return append(append(frame("assistantResponseEvent", `{"content":"partial "}`),
				frame("meteringEvent", `{"unit":"credit","usage":0.25}`)...),
				frame("throttlingError", `{"message":"Rate limited"}`)...)
		}
		return kiroPlain(tok)
	}
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	v := waitRequests(t, h, usage.Query{}, 1)
	if v.Totals.Retried != 1 || v.TotalEntries != 2 {
		t.Fatalf("totals %+v entries %d", v.Totals, v.TotalEntries)
	}
	ok, retry := v.Entries[0], v.Entries[1]
	if !retry.Retried || retry.Aborted || retry.Credits != 0.25 || retry.CostUSD != 0 || retry.UpstreamUSD <= 0 || retry.Account == ok.Account || retry.Request != ok.Request || ok.Request == "" {
		t.Fatalf("retry row %+v / ok row %+v", retry, ok)
	}
	if ok.Retried || ok.Credits != 0.5 || !ok.OK() {
		t.Fatalf("final row %+v", ok)
	}
}

// 中断前上游还没报 meteringEvent：credits 按 token 估，标 aborted + credits_estimated。
func TestAbortWithoutMeteringEstimatesCredits(t *testing.T) {
	h := newHarness(t, 1, nil)
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	h.up.streamWriter = func(w http.ResponseWriter, r *http.Request) {
		w.Write(frame("assistantResponseEvent", `{"content":"partial answer"}`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	body := strings.Replace(convo("hi"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/messages", strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "text_delta") {
			break
		}
	}
	cancel()
	res.Body.Close()
	v := waitRequests(t, h, usage.Query{}, 1)
	e := v.Entries[0]
	if !e.Aborted || !e.CreditsEstimated || e.Credits <= 0 || e.UpstreamUSD <= 0 || e.CostUSD <= 0 || v.Totals.Aborted != 1 {
		t.Fatalf("aborted entry %+v", e)
	}
}

// 两本账：每笔记对下游收费与上游成本；亏的笔单独计。
func TestTwoLedgersAndLoss(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.CreditUSD = 0.02 })
	h.up.stream = func(string) []byte { // 极小请求但上游 credits 高，仍需保留亏损统计
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("meteringEvent", `{"unit":"credit","usage":1}`)...)
	}
	decodeUsage(t, h.post(t, "/v1/messages", `{"model":"claude-sonnet-4.5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil))
	v := waitRequests(t, h, usage.Query{}, 1)
	e := v.Entries[0]
	if math.Abs(e.UpstreamUSD-0.02) > 1e-12 || e.CostUSD <= 0 || !e.Loss() {
		t.Fatalf("entry %+v", e)
	}
	if v.Totals.Losses != 1 || math.Abs(v.Totals.LossUSD-(e.UpstreamUSD-e.CostUSD)) > 1e-12 {
		t.Fatalf("totals %+v", v.Totals)
	}
	st, body := h.do(t, http.MethodGet, "/admin/billing", nil)
	if st != 200 || !strings.Contains(string(body), `"losses":1`) || !strings.Contains(string(body), `"upstream_usd"`) || !strings.Contains(string(body), `"recon"`) {
		t.Fatalf("billing %d %s", st, body)
	}
}

// 封号的 403：不刷新 token、停用该号、换号拿到回复。
func TestBannedAccountDisabledNotRefreshed(t *testing.T) {
	h := newHarness(t, 2, nil)
	bad := ""
	var mu sync.Mutex
	h.up.reply = func(tok string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if bad == "" {
			bad = tok
		}
		if tok == bad {
			return 403, `{"message":"Your account is TemporarilySuspended"}`
		}
		return 200, ""
	}
	if res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil); res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	id := "a" + strings.TrimPrefix(bad, "tok")
	v, _ := h.pool.Get(id)
	if !v.Disabled || !strings.Contains(v.Note, "suspended") {
		t.Fatalf("banned account not disabled: %+v", v)
	}
	n := 0
	for _, tok := range h.up.tokens() {
		if tok == bad {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("banned account hit %d times (refresh+retry should be skipped)", n)
	}
}

// 上游限流窗口：冷却按 x-amzn-kiro-ratelimit-retry-after。
func TestUpstreamRetryAfterCools(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.replyHeader = http.Header{"X-Amzn-Kiro-Ratelimit-Retry-After": {"120000"}}
	h.up.reply = func(string) (int, string) { return 429, `{"message":"Too many requests"}` }
	h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	v, _ := h.pool.Get("a0")
	if left := time.Until(v.CooldownUntil); left < 110*time.Second || left > 121*time.Second {
		t.Fatalf("cooldown %v, want ~2m from retry-after", left)
	}
}

// P0：先失败后成功的非流式请求——下游只收一次；重试亏损一条；key 已用只增加成功那次的金额；
// 预留全额释放；失败尝试的缓存写入不向下游收费。
func TestFailoverChargesDownstreamOnce(t *testing.T) {
	body := cached(convo("fix the bug"), "1h")

	// 对照：同样的请求一次成功，下游应收多少
	ref := newHarness(t, 1, nil)
	ref.up.stream = kiroPlain
	decodeUsage(t, ref.post(t, "/v1/messages", body, nil))
	want := waitRequests(t, ref, usage.Query{}, 1).Entries[0].CostUSD

	h := newHarness(t, 2, func(c *config.Config) { c.CreditUSD = 0.02 })
	k, err := h.s.keys.Create(keys.Key{Name: "team", LimitUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	bad := ""
	h.up.stream = func(tok string) []byte {
		mu.Lock()
		if bad == "" {
			bad = tok
		}
		first := tok == bad
		mu.Unlock()
		if first {
			return append(append(frame("assistantResponseEvent", `{"content":"partial "}`),
				frame("meteringEvent", `{"unit":"credit","usage":0.25}`)...),
				frame("throttlingError", `{"message":"Rate limited"}`)...)
		}
		return kiroPlain(tok)
	}
	u := decodeUsage(t, h.post(t, "/v1/messages", body, map[string]string{"x-api-key": k.Secret}))
	if u.CacheWrite == 0 {
		t.Fatalf("successful attempt should be billed its own cache write: %+v", u)
	}
	v := waitRequests(t, h, usage.Query{Key: k.ID}, 1)
	if v.TotalEntries != 2 || v.Totals.Requests != 1 {
		t.Fatalf("entries %d requests %d", v.TotalEntries, v.Totals.Requests)
	}
	if math.Abs(v.Totals.CostUSD-want) > 1e-12 {
		t.Fatalf("downstream charged %v, want one successful attempt %v", v.Totals.CostUSD, want)
	}
	if v.Totals.RetryLosses != 1 || math.Abs(v.Totals.RetryLossUSD-0.25*0.02) > 1e-12 || v.Totals.Aborted != 0 {
		t.Fatalf("retry loss %+v", v.Totals)
	}
	// key 已用：只有成功那次的金额与 credits
	spent := h.s.usage.Spend(k.ID, time.Time{})
	if math.Abs(spent.CostUSD-want) > 1e-12 || spent.Requests != 1 || math.Abs(spent.BillableCredits()-0.5) > 1e-12 {
		t.Fatalf("key spent %+v (billable credits %v)", spent, spent.BillableCredits())
	}
	if r := h.s.holds.reserved(k.ID); r.Requests != 0 || r.CostUSD != 0 {
		t.Fatalf("hold not released: %+v", r)
	}
}

// 对话请求的 403 不是明确的 token 失效：不刷新、不在同号重试，计一次失败并换号。
// 是 bearer token invalid：强制刷新后同号再试。
func TestChat403RefreshOnlyForTokenErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		sameAgain  bool // 失败的号是否被再请求一次（刷新后重试）
	}{
		{"policy 403", `{"message":"User is not authorized for this profile"}`, false},
		{"firewall 403", `<html>blocked</html>`, false},
		{"token 403", `{"message":"The bearer token included in the request is invalid."}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 2, nil)
			var mu sync.Mutex
			bad, hits := "", 0
			h.up.reply = func(tok string) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				if bad == "" {
					bad = tok
				}
				if tok == bad {
					hits++
					return 403, tc.body
				}
				return 200, ""
			}
			h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
			if got := hits > 1; got != tc.sameAgain {
				t.Fatalf("bad account requested %d times, want retry-on-same=%v", hits, tc.sameAgain)
			}
			h.up.mu.Lock()
			refreshed := h.up.refreshes
			h.up.mu.Unlock()
			if want := map[bool]int{true: 1, false: 0}[tc.sameAgain]; refreshed != want {
				t.Fatalf("refreshes = %d, want %d", refreshed, want)
			}
			v, _ := h.pool.Get("a" + strings.TrimPrefix(bad, "tok"))
			if v.Disabled {
				t.Fatalf("403 must not disable: %+v", v)
			}
		})
	}
}

// 上游报了 tokenUsage：保留 Kiro 输入，取最后一次报告，不累加也不重复加基线。
func TestReportedConservative(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		b = append(b, frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":3000,"cacheReadInputTokens":9000,"outputTokens":1}}`)...)
		b = append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":3000,"cacheReadInputTokens":%d,"outputTokens":2}}`, 9000))...)
		return b
	}
	u := decodeUsage(t, h.post(t, "/v1/messages", convo("x"), nil))
	if u.Input != 3000 || u.CacheRead != 9000 || u.Output != 2 {
		t.Fatalf("usage %+v, want input 3000, cache_read 9000, output 2", u)
	}
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	wantCredits := meter.EstimateCredits(meter.CreditRateOf(e.Model, nil), turn.Usage{Input: 3000, CacheRead: 9000, Output: 2}, 0)
	if e.Input != 3000 || e.CacheRead != 9000 || e.Credits != wantCredits {
		t.Fatalf("reported input should not add another Kiro baseline: %+v", e)
	}
}

// 反代后面：连接来自回环，但带了转发头，不算本机。
func TestAdminBehindProxyNotLoopback(t *testing.T) {
	h := newHarness(t, 1, nil)
	for _, hdr := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		r := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:5000", "127.0.0.1:8787"
		r.Header.Set(hdr, "203.0.113.9")
		w := httptest.NewRecorder()
		h.s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", hdr, w.Code)
		}
	}
	// 设了 admin_token：反代后面带 token 可以访问
	h2 := newHarness(t, 1, func(c *config.Config) { c.AdminToken = "adm" })
	r := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	r.RemoteAddr, r.Host = "127.0.0.1:5000", "proxy.example"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("X-Admin-Token", "adm")
	w := httptest.NewRecorder()
	h2.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("with token behind proxy: %d", w.Code)
	}
}

// 流式：message_start 的每一项 usage 都不大于 message_delta 的对应项。
// 覆盖上下文校准把本地估算往下压的情况（上游内容只有本地估算的一半多），以及缓存命中。
func TestStreamStartNotAboveEnd(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.s.models.merge([]kiro.Model{{ID: "claude-sonnet-4.5", Name: "s", Context: 200_000}})
	body := strings.Replace(cached(convo("fix the bug"), ""), `"max_tokens":100,`, `"max_tokens":100,"stream":true,`, 1)
	res := h.post(t, "/v1/messages/count_tokens", body, nil)
	var ct struct {
		InputTokens int `json:"input_tokens"`
	}
	_ = json.NewDecoder(res.Body).Decode(&ct)
	for _, scale := range []float64{0.52, 1.0, 1.9} {
		content := int(float64(ct.InputTokens-kiro.HiddenTokens("claude-sonnet-4.5")) * scale)
		h.up.stream = func(string) []byte {
			b := frame("assistantResponseEvent", `{"content":"hello world"}`)
			b = append(b, frame("contextUsageEvent", fmt.Sprintf(`{"contextUsagePercentage":%v}`, float64(content+kiro.HiddenTokens("claude-sonnet-4.5"))/2000))...)
			return append(b, frame("meteringEvent", `{"unit":"credit","usage":0.5}`)...)
		}
		for turn := range 2 { // 第二轮命中缓存
			res := h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": fmt.Sprint("s", scale)})
			var start, end map[string]float64
			sc := bufio.NewScanner(res.Body)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				d, ok := strings.CutPrefix(sc.Text(), "data: ")
				if !ok {
					continue
				}
				var ev struct {
					Type    string
					Message struct{ Usage map[string]any }
					Usage   map[string]any
				}
				_ = json.Unmarshal([]byte(d), &ev)
				switch ev.Type {
				case "message_start":
					start = nums(ev.Message.Usage)
				case "message_delta":
					end = nums(ev.Usage)
				}
			}
			if start == nil || end == nil {
				t.Fatalf("scale %v turn %d: missing start/delta", scale, turn)
			}
			for k, v := range start {
				if v > end[k] {
					t.Fatalf("scale %v turn %d: message_start %s=%v > message_delta %v (start %v end %v)", scale, turn, k, v, end[k], start, end)
				}
			}
		}
	}
}

// nums 把 usage 里的数字展平（cache_creation 子对象展开）。
func nums(u map[string]any) map[string]float64 {
	out := map[string]float64{}
	for k, v := range u {
		switch x := v.(type) {
		case float64:
			out[k] = x
		case map[string]any:
			for k2, v2 := range x {
				if f, ok := v2.(float64); ok {
					out[k+"."+k2] = f
				}
			}
		}
	}
	return out
}

// /v1/responses 带内置工具：默认（drop）跳过并正常回复，reject 时明确的 400；
// custom 工具调用端到端还原成 custom_tool_call。
func TestResponsesCodexEndToEnd(t *testing.T) {
	const hosted = `{"model":"claude-sonnet-4.5","input":"q","tools":[{"type":"web_search"}]}`
	rejecting := newHarness(t, 1, func(c *config.Config) { c.OpenAIHostedTools = "reject" })
	res := rejecting.post(t, "/v1/responses", hosted, nil)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 400 || !strings.Contains(string(b), "web_search") {
		t.Fatalf("hosted tool (reject): %d %s", res.StatusCode, b)
	}
	h := newHarness(t, 1, nil)
	res = h.post(t, "/v1/responses", hosted, nil)
	b, _ = io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("hosted tool (default drop): %d %s", res.StatusCode, b)
	}
	if up, _ := json.Marshal(h.up.calls[0].body); strings.Contains(string(up), "web_search") {
		t.Fatalf("web_search sent upstream: %s", up)
	}
	h.up.stream = func(string) []byte {
		b := frame("toolUseEvent", `{"toolUseId":"tu1","name":"apply_patch","input":"{\"input\":\"*** Begin Patch\"}"}`)
		b = append(b, frame("toolUseEvent", `{"toolUseId":"tu1","name":"apply_patch","stop":true}`)...)
		return append(b, frame("meteringEvent", `{"unit":"credit","usage":0.1}`)...)
	}
	res = h.post(t, "/v1/responses", `{"model":"claude-sonnet-4.5","input":"patch it","tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch."}]}`, nil)
	var out struct {
		Output []struct {
			Type, Name, CallID string
			Input              *string
		} `json:"output"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
		t.Fatalf("status %d err %v", res.StatusCode, err)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "custom_tool_call" || out.Output[0].Input == nil || *out.Output[0].Input != "*** Begin Patch" {
		t.Fatalf("output = %+v", out.Output)
	}
	// 发给上游的是带 input 字符串参数的函数
	b2, _ := json.Marshal(h.up.calls[len(h.up.calls)-1].body)
	if !strings.Contains(string(b2), `"name":"apply_patch"`) || !strings.Contains(string(b2), `"required":["input"]`) {
		t.Fatalf("upstream tools: %s", b2)
	}
}

// 只有一个号、复用的空闲连接被对端关掉（还没收到任何响应字节就 EOF）：在同一号上用新连接重试一次，
// 下游拿到 200；不算失败、不冷却、不记 retried 行，账本只有一行。
func TestConnDroppedBeforeResponseRetriesSameAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			h := newHarness(t, 1, nil)
			h.up.hangups = 1
			body := fmt.Sprintf(turn1, "aaa")
			if stream {
				body = strings.Replace(body, `"max_tokens":100,`, `"max_tokens":100,"stream":true,`, 1)
			}
			res := h.post(t, "/v1/messages", body, nil)
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || !strings.Contains(string(b), "hello") {
				t.Fatalf("status %d: %s", res.StatusCode, b)
			}
			if n := len(h.up.tokens()); n != 1 {
				t.Fatalf("upstream served %d requests, want 1 after the hangup", n)
			}
			v := waitRequests(t, h, usage.Query{}, 1)
			if len(v.Entries) != 1 || v.Totals.Retried != 0 || v.Entries[0].Status != 200 || v.Entries[0].Attempts != 1 {
				t.Fatalf("journal = %+v entries %+v", v.Totals, v.Entries)
			}
			a, _ := h.pool.Get("a0")
			if !a.CooldownUntil.IsZero() || a.Stats.Errors != 0 {
				t.Fatalf("account cooled / failed: until %v why %q errors %d", a.CooldownUntil, a.CooldownWhy, a.Stats.Errors)
			}
		})
	}
}
