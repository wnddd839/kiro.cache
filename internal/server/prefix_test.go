package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/usage"
)

func TestUpstreamPrefixTraceAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, 1, func(c *config.Config) { c.DebugRequestsDir = dir })
	h.up.modelsJSON = `{"models":[{"modelId":"claude-sonnet-5.5","additionalModelRequestFieldsSchema":{"properties":{"output_config":{"properties":{"effort":{"enum":["low","high"],"default":"low"}}},"thinking":{"properties":{"type":{"enum":["adaptive"]}}}}}}]}`
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	messages := `[{"role":"user","content":[{"type":"text","text":"first question"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OLD"}}]}]`
	tools := `[{"name":"read","description":"Read file","input_schema":{"type":"object","properties":{"b":{"type":"number"},"a":{"type":"string"}}}},{"name":"write","description":"Write file","input_schema":{"type":"object"}}]`
	toolsReverse := `[{"name":"write","description":"Write file","input_schema":{"type":"object"}},{"name":"read","description":"Read file","input_schema":{"properties":{"a":{"type":"string"},"b":{"type":"number"}},"type":"object"}}]`
	system := "stable date 2026-10-09 " + bigSystem
	session := map[string]string{"X-Session-Id": "prefix-audit"}
	post := func(msgs, definitions, sys, effort string) requestTrace {
		t.Helper()
		body := fmt.Sprintf(`{"model":"claude-sonnet-5.5","system":[{"type":"text","text":%q},{"type":"text","text":%q}],"tools":%s,"output_config":{"effort":%q},"messages":%s}`, sys, "x-anthropic-billing-header: cch="+kiro.NewUUID(), definitions, effort, msgs)
		decodeUsage(t, h.post(t, "/v1/messages", body, session))
		n := len(h.up.tokens())
		waitRequests(t, h, usage.Query{}, int64(n))
		files, err := filepath.Glob(filepath.Join(dir, "*.latest.meta.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("trace files=%v err=%v", files, err)
		}
		raw, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		var trace requestTrace
		if err := json.Unmarshal(raw, &trace); err != nil {
			t.Fatal(err)
		}
		captured, err := os.ReadFile(filepath.Join(dir, trace.Request))
		if err != nil {
			t.Fatal(err)
		}
		h.up.mu.Lock()
		actual := bytes.Clone(h.up.calls[n-1].raw)
		h.up.mu.Unlock()
		if !bytes.Equal(captured, actual) {
			t.Fatal("trace differs from bytes actually received upstream")
		}
		return trace
	}
	first := post(messages, tools, system, "low")
	if first.Prefix != nil {
		t.Fatal("first request has previous prefix")
	}
	messages = strings.TrimSuffix(messages, "]") + `,{"role":"assistant","content":[{"type":"thinking","thinking":"old thoughts","signature":"sig1"},{"type":"text","text":"ok"}]},{"role":"user","content":[{"type":"text","text":"second question"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"NEW"}}]}]`
	second := post(messages, toolsReverse, system, "low")
	checkStable := func(trace requestTrace) {
		t.Helper()
		p := trace.Prefix
		if p == nil || !p.MessagesExtend || !p.SameTools || !p.SameAccount || !p.SameConversation || !p.SameParameters || p.FirstDifferentByte != nil {
			t.Fatalf("stable append changed prefix: %+v", p)
		}
	}
	checkStable(second)
	h.up.mu.Lock()
	history := h.up.calls[1].body["conversationState"].(map[string]any)["history"].([]any)
	h.up.mu.Unlock()
	old := history[0].(map[string]any)["userInputMessage"].(map[string]any)["images"].([]any)
	if old[0].(map[string]any)["source"].(map[string]any)["bytes"] != "OLD" {
		t.Fatal("new image removed old image")
	}
	messages = strings.TrimSuffix(messages, "]") + `,{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{"b":2,"a":"file"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"file content"}]}]}]`
	checkStable(post(messages, tools, system, "low"))
	reordered := strings.ReplaceAll(messages, `{"b":2,"a":"file"}`, `{"a":"file","b":2}`)
	reordered = strings.ReplaceAll(reordered, "old thoughts", "different thoughts")
	reordered = strings.ReplaceAll(reordered, "sig1", "sig2")
	checkStable(post(reordered, toolsReverse, system, "low"))
	changedSystem := post(reordered, toolsReverse, strings.Replace(system, "2026-10-09", "2026-10-10", 1), "low")
	if p := changedSystem.Prefix; p.MessagesExtend || p.FirstDifferentByte == nil || p.SameConversation {
		t.Fatalf("system change not diagnosed: %+v", p)
	}
	changedTools := post(reordered, strings.Replace(toolsReverse, "Read file", "Read a different file", 1), strings.Replace(system, "2026-10-09", "2026-10-10", 1), "low")
	if p := changedTools.Prefix; p.SameTools || !p.MessagesExtend {
		t.Fatalf("tool change not diagnosed: %+v", p)
	}
	changedEffort := post(reordered, strings.Replace(toolsReverse, "Read file", "Read a different file", 1), strings.Replace(system, "2026-10-09", "2026-10-10", 1), "high")
	if p := changedEffort.Prefix; p.SameParameters || !p.MessagesExtend {
		t.Fatalf("effort change not diagnosed: %+v", p)
	}
}

func TestConcurrentFirstTurnsSharePendingAccount(t *testing.T) {
	h := newHarness(t, 2, nil)
	started := make(chan struct{}, 4)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	h.up.streamWriter = func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(frame("assistantResponseEvent", `{"content":"ok"}`))
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			decodeUsage(t, h.post(t, "/v1/messages", convo("concurrent first turn"), map[string]string{"X-Session-Id": "pending-account"}))
		})
	}
	deadline := time.After(3 * time.Second)
	for range 4 {
		select {
		case <-started:
		case <-deadline:
			release()
			t.Fatal("concurrent requests did not reach upstream")
		}
	}
	tokens := h.up.tokens()
	for _, token := range tokens {
		if token != tokens[0] {
			release()
			t.Fatalf("first turns selected different accounts: %v", tokens)
		}
	}
	if len(h.pool.Pins().Dump()) != 0 {
		release()
		t.Fatal("pending account choice committed a pin before a successful non-stream reply")
	}
	release()
	wg.Wait()
	waitRequests(t, h, usage.Query{}, 4)
}

func TestLegacyThinkingBudgetPinnedForConcurrentFirstTurns(t *testing.T) {
	h := newHarness(t, 1, nil)
	if err := h.pool.SetMaxConcurrent("a0", 16); err != nil {
		t.Fatal(err)
	}
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			<-gate
			body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":"stable system","thinking":{"type":"enabled","budget_tokens":%d},"messages":[{"role":"user","content":"same question"}]}`, 10000+i*1000)
			decodeUsage(t, h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": "concurrent-thinking"}))
		})
	}
	close(gate)
	wg.Wait()
	waitRequests(t, h, usage.Query{}, 8)
	h.up.mu.Lock()
	defer h.up.mu.Unlock()
	var first []byte
	for _, call := range h.up.calls {
		state := call.body["conversationState"].(map[string]any)
		current, _ := json.Marshal(state["currentMessage"])
		if first == nil {
			first = current
		} else if !bytes.Equal(first, current) {
			t.Fatalf("concurrent first turns pinned different budgets:\n%s\n%s", first, current)
		}
	}
}

func TestHistoricalToolPlaceholdersDeterministic(t *testing.T) {
	for _, order := range []string{"z,a", "a,z"} {
		names := strings.Split(order, ",")
		request := fmt.Sprintf(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"tool_use","id":"1","name":%q,"input":{}},{"type":"tool_use","id":"2","name":%q,"input":{}}]},{"role":"user","content":"next"}]}`, names[0], names[1])
		h := newHarness(t, 1, nil)
		decodeUsage(t, h.post(t, "/v1/messages", request, nil))
		h.up.mu.Lock()
		state := h.up.calls[0].body["conversationState"].(map[string]any)
		h.up.mu.Unlock()
		user := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
		tools := user["userInputMessageContext"].(map[string]any)["tools"].([]any)
		for i, name := range []string{"a", "z"} {
			spec := tools[i].(map[string]any)["toolSpecification"].(map[string]any)
			if spec["name"] != name || spec["description"] != "Tool" {
				t.Fatalf("unstable historical tools: %+v", tools)
			}
		}
	}
}
