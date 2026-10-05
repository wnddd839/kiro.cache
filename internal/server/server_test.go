package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/internal/config"
	"kiro-go/internal/kiro"
	"kiro-go/internal/pool"
)

// fakeKiro 是本地上游：记录每次 generateAssistantResponse 的 token 与 body。
type fakeKiro struct {
	mu    sync.Mutex
	calls []upstreamCall
	// reply 按 token 决定响应；返回 status!=200 时写 body 作为错误
	reply func(token string) (status int, body string)
	// stream 非 nil 时替代默认的 200 事件流（用于中途报错 / 断流）
	stream func(token string) []byte
}

type upstreamCall struct {
	token string
	body  map[string]any
}

func (f *fakeKiro) convIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.body["conversationState"].(map[string]any)["conversationId"].(string))
	}
	return out
}

func (f *fakeKiro) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.token)
	}
	return out
}

func (f *fakeKiro) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runtime/{region}/generateAssistantResponse", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("upstream body: %v", err)
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.calls = append(f.calls, upstreamCall{token: token, body: body})
		f.mu.Unlock()
		status, errBody := 200, ""
		if f.reply != nil {
			status, errBody = f.reply(token)
		}
		if status != 200 {
			w.WriteHeader(status)
			io.WriteString(w, errBody)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		if f.stream != nil {
			if b := f.stream(token); b != nil {
				w.Write(b)
				return
			}
		}
		w.Write(frame("assistantResponseEvent", `{"content":"hello "}`))
		w.Write(frame("assistantResponseEvent", `{"content":"world"}`))
		w.Write(frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":3,"cacheReadInputTokens":900,"cacheWriteInputTokens":5}}`))
	})
	mux.HandleFunc("GET /mgmt/{region}/List-Available-Models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.5","modelName":"Sonnet","tokenLimits":{"maxInputTokens":200000}}],"defaultModel":{"modelId":"claude-sonnet-4.5"}}`)
	})
	mux.HandleFunc("GET /mgmt/{region}/Get-Usage-Limits", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	})
	return mux
}

// frame 编码一条 AWS event stream 消息。CRC 不校验，填 0。
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

type harness struct {
	srv  *httptest.Server
	up   *fakeKiro
	pool *pool.Pool
}

func newHarness(t *testing.T, accounts int, mutate func(*config.Config)) *harness {
	t.Helper()
	up := &fakeKiro{}
	upSrv := httptest.NewServer(up.handler(t))
	t.Cleanup(upSrv.Close)

	client := kiro.NewClient(upSrv.Client())
	client.RuntimeURL = func(r string) string { return upSrv.URL + "/runtime/" + r }
	client.ManagementURL = func(r string) string { return upSrv.URL + "/mgmt/" + r }

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pool.Open(filepath.Join(t.TempDir(), "accounts.json"), pool.Options{Client: client, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	for i := range accounts {
		_, err := p.Add(pool.Account{ID: fmt.Sprintf("a%d", i), Cred: kiro.Cred{
			Method: kiro.MethodSocial, AccessToken: fmt.Sprintf("tok%d", i), RefreshToken: "r",
			ExpiresAt: time.Now().Add(time.Hour), ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/x",
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(t.Context(), cfg, p, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{srv: srv, up: up, pool: p}
}

func (h *harness) post(t *testing.T, path, body string, header map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

const turn1 = `{"model":"claude-sonnet-4-5-20250929","max_tokens":100,
"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1; cch=%s;"},{"type":"text","text":"You are helpful."}],
"messages":[{"role":"user","content":"fix the bug"}]}`

const turn2 = `{"model":"claude-sonnet-4-5-20250929","max_tokens":100,
"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1; cch=%s;"},{"type":"text","text":"You are helpful."}],
"messages":[{"role":"user","content":"fix the bug"},{"role":"assistant","content":"ok"},{"role":"user","content":"and tests"}]}`

func TestNonStreamResponseAndUsage(t *testing.T) {
	h := newHarness(t, 1, nil)
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "aaa"), nil)
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var msg struct {
		Content []struct{ Type, Text string }
		Usage   map[string]int
		Model   string
	}
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello world" {
		t.Fatalf("content = %+v", msg.Content)
	}
	if msg.Usage["cache_read_input_tokens"] != 900 || msg.Usage["input_tokens"] != 10 {
		t.Fatalf("usage = %v", msg.Usage)
	}
	if msg.Model != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model echoed = %q", msg.Model)
	}
	// 上游收到的是 Kiro 模型 id，且 billing header 已剥离
	body := h.up.calls[0].body
	cur := body["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if cur["modelId"] != "claude-sonnet-4.5" {
		t.Fatalf("modelId = %v", cur["modelId"])
	}
	if c := cur["content"].(string); strings.Contains(c, "billing") || !strings.HasPrefix(c, "You are helpful.") {
		t.Fatalf("content = %q", c)
	}
	if got := h.pool.Totals(); got.CacheRead != 900 || got.Requests != 1 {
		t.Fatalf("totals = %+v", got)
	}
}

// 同一会话两轮：同一号、同一 conversationId、history 前缀字节一致，即使 billing cch 变了。
func TestSameSessionReusesAccountAndConversation(t *testing.T) {
	h := newHarness(t, 3, nil)
	for _, body := range []string{fmt.Sprintf(turn1, "aaa"), fmt.Sprintf(turn2, "bbb")} {
		if res := h.post(t, "/v1/messages", body, nil); res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
	}
	ids, toks := h.up.convIDs(), h.up.tokens()
	if ids[0] != ids[1] {
		t.Fatalf("conversationId changed across turns: %v", ids)
	}
	if toks[0] != toks[1] {
		t.Fatalf("account changed across turns: %v", toks)
	}
	first := h.up.calls[0].body["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"]
	hist := h.up.calls[1].body["conversationState"].(map[string]any)["history"].([]any)[0].(map[string]any)["userInputMessage"]
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(hist)
	if string(a) != string(b) {
		t.Fatalf("first message prefix not byte-stable:\n%s\n%s", a, b)
	}
}

func TestRandomModeRotatesConversation(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.ConversationMode = config.ConversationRandom })
	h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	h.post(t, "/v1/messages", fmt.Sprintf(turn2, "a"), nil)
	if ids := h.up.convIDs(); ids[0] == ids[1] {
		t.Fatal("random mode must not reuse conversationId")
	}
}

// 客户端回退 / 编辑历史：不再是前缀，conversationId 轮换。
func TestRewrittenHistoryRotatesConversation(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.post(t, "/v1/messages", fmt.Sprintf(turn2, "a"), nil)
	edited := strings.Replace(fmt.Sprintf(turn2, "a"), `"content":"ok"`, `"content":"changed"`, 1)
	h.post(t, "/v1/messages", edited, nil)
	if ids := h.up.convIDs(); ids[0] == ids[1] {
		t.Fatal("edited history must mint a new conversationId")
	}
}

func TestDifferentSessionsSpreadAcrossAccounts(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), map[string]string{"X-Session-Id": "s1"})
	h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), map[string]string{"X-Session-Id": "s2"})
	h.post(t, "/v1/messages", fmt.Sprintf(turn2, "a"), map[string]string{"X-Session-Id": "s1"})
	toks := h.up.tokens()
	if toks[0] == toks[1] {
		t.Fatalf("idle accounts: new session should go to the least recently used one: %v", toks)
	}
	if toks[2] != toks[0] {
		t.Fatalf("session s1 must stick to its account: %v", toks)
	}
}

// 配额用尽：换号重试，会话改钉新号，新号上 conversationId 不同。
func TestQuotaFailoverRepinsAndRotatesConversation(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	firstTok := h.up.tokens()[0]
	h.up.reply = func(token string) (int, string) {
		if token == firstTok {
			return 400, `{"message":"limit","reason":"MONTHLY_REQUEST_COUNT"}`
		}
		return 200, ""
	}
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn2, "a"), nil)
	if res.StatusCode != 200 {
		t.Fatalf("failover status %d", res.StatusCode)
	}
	toks, ids := h.up.tokens(), h.up.convIDs()
	if len(toks) != 3 || toks[1] != firstTok || toks[2] == firstTok {
		t.Fatalf("expected retry on the other account: %v", toks)
	}
	if ids[2] == ids[0] {
		t.Fatal("new account must get a new conversationId")
	}
	h.post(t, "/v1/messages", fmt.Sprintf(turn2, "a"), nil)
	if toks := h.up.tokens(); toks[3] != toks[2] {
		t.Fatalf("session must be re-pinned to the new account: %v", toks)
	}
	if v, _ := h.pool.Get("a" + strings.TrimPrefix(firstTok, "tok")); v.CooldownUntil.Before(time.Now().Add(30 * time.Minute)) {
		t.Fatal("quota-exhausted account must cool down")
	}
}

func TestFatalErrorNotRetried(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.up.reply = func(string) (int, string) {
		return 400, `{"message":"Input is too long","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`
	}
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	if res.StatusCode != 400 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if n := len(h.up.tokens()); n != 1 {
		t.Fatalf("fatal error retried %d times", n)
	}
}

func TestAllAccountsCoolingReturns429(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.reply = func(string) (int, string) { return 429, `{"message":"Too many requests"}` }
	h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "b"), nil)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
}

func TestStreamSSE(t *testing.T) {
	h := newHarness(t, 1, nil)
	body := strings.Replace(fmt.Sprintf(turn1, "a"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
	res := h.post(t, "/v1/messages", body, nil)
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var events []string
	var text strings.Builder
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		line := sc.Text()
		if e, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, e)
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(d, "text_delta") {
			var v struct{ Delta struct{ Text string } }
			_ = json.Unmarshal([]byte(d), &v)
			text.WriteString(v.Delta.Text)
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(d, "message_delta") && !strings.Contains(d, `"cache_read_input_tokens":900`) {
			t.Fatalf("message_delta usage missing cache read: %s", d)
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", events)
	}
	if text.String() != "hello world" {
		t.Fatalf("text = %q", text.String())
	}
}

// 下游路径前缀修复：各种 base URL 写法都落到 messages。
func TestPathPrefixes(t *testing.T) {
	h := newHarness(t, 1, nil)
	for _, p := range []string{"/v1/messages", "/messages", "/v1/v1/messages", "/anthropic/v1/messages", "/v1/messages/", "//v1//messages"} {
		if res := h.post(t, p, fmt.Sprintf(turn1, "a"), nil); res.StatusCode != 200 {
			t.Errorf("%s -> %d", p, res.StatusCode)
		}
	}
	res := h.post(t, "/v1/messages/count_tokens", fmt.Sprintf(turn1, "a"), nil)
	var ct map[string]int
	_ = json.NewDecoder(res.Body).Decode(&ct)
	if res.StatusCode != 200 || ct["input_tokens"] == 0 {
		t.Fatalf("count_tokens %d %v", res.StatusCode, ct)
	}
}

func TestAPIKeyRequired(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.APIKeys = []string{"sk-local"} })
	if res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil); res.StatusCode != 401 {
		t.Fatalf("no key: %d", res.StatusCode)
	}
	if res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), map[string]string{"X-Api-Key": "sk-local"}); res.StatusCode != 200 {
		t.Fatalf("x-api-key: %d", res.StatusCode)
	}
	if res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), map[string]string{"Authorization": "Bearer sk-local"}); res.StatusCode != 200 {
		t.Fatalf("bearer: %d", res.StatusCode)
	}
}

// 同一会话中途关掉 thinking：首条消息前缀不变。
func TestThinkingPinnedPerThread(t *testing.T) {
	h := newHarness(t, 1, nil)
	withThink := strings.Replace(fmt.Sprintf(turn1, "a"), `"max_tokens":100`, `"max_tokens":100,"thinking":{"type":"enabled","budget_tokens":16000}`, 1)
	h.post(t, "/v1/messages", withThink, nil)
	h.post(t, "/v1/messages", fmt.Sprintf(turn2, "a"), nil)
	hist := h.up.calls[1].body["conversationState"].(map[string]any)["history"].([]any)[0].(map[string]any)["userInputMessage"].(map[string]any)
	if c := hist["content"].(string); !strings.HasPrefix(c, "<thinking_mode>enabled</thinking_mode><max_thinking_length>16000<") {
		t.Fatalf("thinking prefix not pinned: %q", c[:min(80, len(c))])
	}
}

func TestAdminListAndStats(t *testing.T) {
	h := newHarness(t, 2, func(c *config.Config) { c.AdminToken = "adm" })
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/admin/accounts", nil)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatalf("admin without token: %d", res.StatusCode)
	}
	req.Header.Set("X-Admin-Token", "adm")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if strings.Contains(string(raw), "tok0") || strings.Contains(string(raw), `"refresh_token"`) {
		t.Fatalf("admin list leaks tokens: %s", raw)
	}
	var out struct{ Accounts []pool.View }
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Accounts) != 2 {
		t.Fatalf("accounts = %s (%v)", raw, err)
	}
}
