package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// 同一会话在两个号之间来回（换号后回退、并发分流）：每个号各自只用一个 conversationId，
// 不因来回切换而重铸；历史持续增长时各号的前缀判断互不干扰。
func TestAlternatingAccountsKeepOneConversationEach(t *testing.T) {
	h := newHarness(t, 2, nil)
	hdr := map[string]string{"X-Session-Id": "same"}
	bodies := []string{fmt.Sprintf(turn1, "a"), fmt.Sprintf(turn1, "b"), fmt.Sprintf(turn2, "c"), fmt.Sprintf(turn2, "d"), fmt.Sprintf(turn2, "e"), fmt.Sprintf(turn2, "f")}
	for i, body := range bodies {
		use, off := fmt.Sprintf("a%d", i%2), fmt.Sprintf("a%d", 1-i%2)
		mustSet(t, h, use, false)
		mustSet(t, h, off, true)
		if res := h.post(t, "/v1/messages", body, hdr); res.StatusCode != 200 {
			t.Fatalf("round %d: status %d", i, res.StatusCode)
		}
	}
	toks, ids := h.up.tokens(), h.up.convIDs()
	byToken := map[string][]string{}
	for i, tok := range toks {
		byToken[tok] = append(byToken[tok], ids[i])
	}
	if len(byToken) != 2 {
		t.Fatalf("expected both accounts used: %v", toks)
	}
	for tok, list := range byToken {
		if n := len(slices.Compact(slices.Clone(list))); n != 1 {
			t.Errorf("%s: conversationId re-minted across alternation: %v", tok, list)
		}
	}
	if byToken["tok0"][0] == byToken["tok1"][0] {
		t.Error("accounts must not share a conversationId")
	}
}

// 并发同一会话：同一号上的并发首请求共享同一个 conversationId。
func TestConcurrentSameSessionSameAccountSharesConversation(t *testing.T) {
	h := newHarness(t, 1, nil)
	hdr := map[string]string{"X-Session-Id": "same"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), hdr)
			_, _ = io.Copy(io.Discard, res.Body)
		})
	}
	wg.Wait()
	if ids := slices.Compact(slices.Sorted(slices.Values(h.up.convIDs()))); len(ids) != 1 {
		t.Fatalf("conversation ids = %v", ids)
	}
}

func mustSet(t *testing.T, h *harness, id string, disabled bool) {
	t.Helper()
	if err := h.pool.SetDisabled(id, disabled, "test"); err != nil {
		t.Fatal(err)
	}
}

// 半截文字后限流：非流式不能把半句话当 end_turn 给出去，应换号拿到完整回复；失败号被冷却。
func TestNonStreamMidReplyFailureFailsOver(t *testing.T) {
	h := newHarness(t, 2, nil)
	var bad string
	h.up.stream = func(tok string) []byte {
		if bad == "" {
			bad = tok
		}
		if tok != bad {
			return nil
		}
		return append(frame("assistantResponseEvent", `{"content":"partial "}`),
			frame("throttlingError", `{"message":"Rate limited"}`)...)
	}
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	var msg struct {
		Content    []struct{ Text string }
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil || res.StatusCode != 200 {
		t.Fatalf("status %d err %v", res.StatusCode, err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello world" || msg.StopReason != "end_turn" {
		t.Fatalf("got %+v, want the full reply from the other account", msg)
	}
	if toks := h.up.tokens(); len(toks) != 2 || toks[0] != bad || toks[1] == bad {
		t.Fatalf("tokens %v", toks)
	}
	if v, _ := h.pool.Get("a" + strings.TrimPrefix(bad, "tok")); v.CooldownUntil.IsZero() {
		t.Fatal("throttled account must cool down")
	}
}

// 半截后断流且没有别的号：非流式回 5xx，不是 200 + end_turn。
func TestNonStreamBrokenReplyIsError(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"partial "}`)
		return append(b, frame("assistantResponseEvent", `{"content":"x"}`)[:10]...) // 截断的帧
	}
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 500 || strings.Contains(string(raw), "end_turn") {
		t.Fatalf("status %d body %s", res.StatusCode, raw)
	}
}

// 流式中途异常：下游收到 error 事件（没有 message_stop），并且该号被记为限流冷却。
func TestStreamMidReplyErrorReportedAndRecorded(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		return append(frame("assistantResponseEvent", `{"content":"partial "}`),
			frame("throttlingError", `{"message":"Rate limited"}`)...)
	}
	body := strings.Replace(fmt.Sprintf(turn1, "a"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
	res := h.post(t, "/v1/messages", body, nil)
	var events []string
	var errData string
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if e, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, e)
		}
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && strings.Contains(d, `"type":"error"`) {
			errData = d
		}
	}
	if events[len(events)-1] != "error" || slices.Contains(events, "message_stop") {
		t.Fatalf("events = %v", events)
	}
	if !strings.Contains(errData, "rate_limit_error") {
		t.Fatalf("error event = %s", errData)
	}
	v, _ := h.pool.Get("a0")
	if v.CooldownUntil.IsZero() || v.Stats.Errors != 1 {
		t.Fatalf("failure not recorded on account: cooldown %v errors %d", v.CooldownUntil, v.Stats.Errors)
	}
}

// 号池拿不到模型列表时，/v1/models 不编造 auto。
func TestModelsEmptyCatalogIsError(t *testing.T) {
	h := newHarness(t, 0, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/v1/models", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(raw), `"auto"`) {
		t.Fatalf("status %d body %s", res.StatusCode, raw)
	}
}

func TestModelsListsAccountModels(t *testing.T) {
	h := newHarness(t, 1, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/v1/models", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct{ Data []struct{ ID string } }
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 || len(out.Data) == 0 || out.Data[0].ID != "claude-sonnet-4.5" {
		t.Fatalf("status %d data %+v err %v", res.StatusCode, out.Data, err)
	}
}
