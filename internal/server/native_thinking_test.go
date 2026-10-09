package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"kiro-proxy/internal/usage"
)

func TestNativeThinkingAcrossProtocols(t *testing.T) {
	for _, model := range []struct{ id, path string }{{"claude-sonnet-4.6", "output_config"}, {"gpt-5.6-luna", "reasoning"}} {
		for _, protocol := range []struct {
			path string
			body func(string) string
		}{
			{"/v1/messages", func(e string) string {
				return fmt.Sprintf(`{"model":%q,"system":%q,"cache_control":{"type":"ephemeral"},"output_config":{"effort":%q},"messages":[{"role":"user","content":"hello"}]}`, model.id, bigSystem, e)
			}},
			{"/v1/chat/completions", func(e string) string {
				return fmt.Sprintf(`{"model":%q,"reasoning_effort":%q,"messages":[{"role":"system","content":%q},{"role":"user","content":"hello"}]}`, model.id, e, bigSystem)
			}},
			{"/v1/responses", func(e string) string {
				return fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"instructions":%q,"input":"hello"}`, model.id, e, bigSystem)
			}},
		} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s%s/stream=%v", model.id, protocol.path, streaming), func(t *testing.T) {
					h := newHarness(t, 1, nil)
					h.up.modelsJSON = fmt.Sprintf(`{"models":[{"modelId":%q,"additionalModelRequestFieldsSchema":{"properties":{%q:{"properties":{"effort":{"enum":["none","low","medium","high","max"],"default":"medium"}}},"thinking":{"properties":{"type":{"enum":["adaptive","disabled"]},"display":{"enum":["summarized"]}}}}}}]}`, model.id, model.path)
					h.up.stream = func(string) []byte {
						return append(frame("assistantResponseEvent", `{"content":"ok"}`), frame("meteringEvent", `{"usage":0.1}`)...)
					}
					efforts := []string{"low", "high", "high", "low"}
					if model.path == "reasoning" {
						efforts = append(efforts, "none")
					}
					for i, effort := range efforts {
						body := protocol.body(effort)
						if streaming {
							body = strings.Replace(body, `"model":`, `"stream":true,"model":`, 1)
						}
						res := h.post(t, protocol.path, body, nil)
						if _, err := io.Copy(io.Discard, res.Body); err != nil {
							t.Fatal(err)
						}
						if res.StatusCode != http.StatusOK {
							t.Fatalf("status %d", res.StatusCode)
						}
						e := waitRequests(t, h, usage.Query{}, int64(i+1)).Entries[0]
						cold := i < 2 || effort == "none"
						if cold && (e.CacheRead != 0 || e.CacheWrite == 0) {
							t.Fatalf("new effort should have a cold local prefix: %+v", e)
						}
						if !cold && (e.CacheRead == 0 || e.CacheWrite != 0) {
							t.Fatalf("repeated effort should reuse its local prefix: %+v", e)
						}
						h.up.mu.Lock()
						call := h.up.calls[i].body
						h.up.mu.Unlock()
						fields, ok := call["additionalModelRequestFields"].(map[string]any)
						if !ok {
							t.Fatalf("native fields missing: %+v", call)
						}
						config, ok := fields[model.path].(map[string]any)
						if !ok || config["effort"] != effort {
							t.Fatalf("effort was lost or pinned: %+v", fields)
						}
						encoded, _ := json.Marshal(call["conversationState"])
						if strings.Contains(string(encoded), "thinking_mode") || strings.Contains(string(encoded), "max_thinking_length") {
							t.Fatalf("native effort leaked into prompt: %s", encoded)
						}
					}
					ids := h.up.convIDs()
					if ids[0] != ids[1] {
						t.Fatalf("effort change rotated conversation: %v", ids)
					}
					h.up.mu.Lock()
					first, _ := json.Marshal(h.up.calls[0].body["conversationState"])
					second, _ := json.Marshal(h.up.calls[1].body["conversationState"])
					h.up.mu.Unlock()
					if string(first) != string(second) {
						t.Fatalf("conversation prefix changed:\n%s\n%s", first, second)
					}
				})
			}
		}
	}
}

func TestToolDescriptionLimitAndMetering(t *testing.T) {
	for _, description := range []string{strings.Repeat("a", 10240), strings.Repeat("a", 10241), strings.Repeat("a", 10239) + "中文", strings.Repeat("😀", 2561)} {
		t.Run(fmt.Sprint(len(description)), func(t *testing.T) {
			h := newHarness(t, 1, nil)
			h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
			body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4.5", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "tools": []any{map[string]any{"name": "read", "description": description, "input_schema": map[string]any{"type": "object"}}}})
			decodeUsage(t, h.post(t, "/v1/messages", string(body), nil))
			h.up.mu.Lock()
			state := h.up.calls[0].body["conversationState"].(map[string]any)
			current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
			tools := current["userInputMessageContext"].(map[string]any)["tools"].([]any)
			sent := tools[0].(map[string]any)["toolSpecification"].(map[string]any)["description"].(string)
			h.up.mu.Unlock()
			if len(sent) > 10240 || !utf8.ValidString(sent) || !strings.HasPrefix(description, sent) {
				t.Fatalf("unsafe truncation: bytes=%d valid=%v", len(sent), utf8.ValidString(sent))
			}
			var normalized map[string]any
			_ = json.Unmarshal(body, &normalized)
			normalized["tools"].([]any)[0].(map[string]any)["description"] = sent
			trimmed, _ := json.Marshal(normalized)
			var counts []int
			for _, b := range [][]byte{body, trimmed} {
				res := h.post(t, "/v1/messages/count_tokens", string(b), nil)
				var count struct {
					Input int `json:"input_tokens"`
				}
				if err := json.NewDecoder(res.Body).Decode(&count); err != nil {
					t.Fatal(err)
				}
				counts = append(counts, count.Input)
			}
			e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
			if counts[0] != counts[1] || e.Input+e.CacheRead+e.CacheWrite != counts[1] {
				t.Fatalf("meter billed discarded description: counts=%v entry=%+v", counts, e)
			}
		})
	}
}
