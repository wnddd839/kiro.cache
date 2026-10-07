package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"kiro-proxy/internal/usage"
)

func (h *harness) api(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func TestAdminKeysLifecycleAndBilling(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.up.stream = kiroPlain

	st, k := h.api(t, "POST", "/admin/keys", `{"name":"team-a","limit_usd":10,"period":"month","models":["claude-*"]}`)
	if st != http.StatusCreated {
		t.Fatalf("create %d %v", st, k)
	}
	id, secret := k["id"].(string), k["secret"].(string)
	if !strings.HasPrefix(secret, "sk-kiro-") || k["remaining_usd"].(float64) != 10 {
		t.Fatalf("created key = %v", k)
	}
	if st, _ := h.api(t, "POST", "/admin/keys", `{"name":""}`); st != 400 {
		t.Fatalf("empty name %d", st)
	}

	// 用这把 key 请求一次
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": secret}))
	waitRequests(t, h, usage.Query{Key: id}, 1)

	st, list := h.api(t, "GET", "/admin/keys", "")
	keysList := list["keys"].([]any)
	if st != 200 || len(keysList) != 1 {
		t.Fatalf("list %d %v", st, list)
	}
	kv := keysList[0].(map[string]any)
	if _, ok := kv["secret"]; ok || kv["hint"] == "" || kv["hint"] == nil {
		t.Fatalf("key list must not expose the secret: %v", kv)
	}
	spent := kv["spent"].(map[string]any)
	if spent["requests"].(float64) != 1 || spent["credits"].(float64) != 0.5 || kv["last_used"] == nil {
		t.Fatalf("key view = %v", kv)
	}
	if kv["remaining_usd"].(float64) >= 10 {
		t.Fatalf("remaining not reduced: %v", kv["remaining_usd"])
	}

	// PATCH 只改给出的字段
	st, up := h.api(t, "PATCH", "/admin/keys/"+id, `{"rpm":30}`)
	if st != 200 || up["rpm"].(float64) != 30 || up["limit_usd"].(float64) != 10 || up["name"] != "team-a" || up["secret"] != nil {
		t.Fatalf("patch %d %v", st, up)
	}
	st, rot := h.api(t, "POST", "/admin/keys/"+id+"/rotate", "")
	if st != 200 || rot["secret"] == secret || !strings.HasPrefix(rot["secret"].(string), "sk-kiro-") {
		t.Fatalf("rotate %d %v", st, rot)
	}

	// 账单
	st, bill := h.api(t, "GET", "/admin/billing", "")
	if st != 200 {
		t.Fatalf("billing %d %v", st, bill)
	}
	tot := bill["totals"].(map[string]any)
	if tot["requests"].(float64) != 1 || tot["credits"].(float64) != 0.5 {
		t.Fatalf("billing totals = %v", tot)
	}
	byKey := bill["by_key"].([]any)[0].(map[string]any)
	if byKey["label"] != "team-a" || byKey["share"].(float64) != 1 {
		t.Fatalf("by_key = %v", byKey)
	}
	if len(bill["by_account"].([]any)) != 1 || len(bill["daily"].([]any)) == 0 {
		t.Fatalf("billing breakdown = %v", bill)
	}

	// 用量查询：相对时间、过滤、非法参数
	st, u := h.api(t, "GET", "/admin/usage?from=7d&protocol=anthropic&key="+id, "")
	if st != 200 || u["totals"].(map[string]any)["requests"].(float64) != 1 || len(u["entries"].([]any)) != 1 {
		t.Fatalf("usage %d %v", st, u)
	}
	if st, _ := h.api(t, "GET", "/admin/usage?from=yesterday", ""); st != 400 {
		t.Fatalf("bad from %d", st)
	}

	if st, _ := h.api(t, "DELETE", "/admin/keys/"+id, ""); st != http.StatusNoContent {
		t.Fatalf("delete %d", st)
	}
	if st, _ := h.api(t, "DELETE", "/admin/keys/"+id, ""); st != 404 {
		t.Fatalf("delete twice %d", st)
	}
}

func TestAdminBatchAccounts(t *testing.T) {
	h := newHarness(t, 3, nil)
	st, out := h.api(t, "POST", "/admin/accounts/batch", `{"action":"disable","ids":["a0","a1","nope"]}`)
	if st != 200 || out["ok"].(float64) != 2 || out["failed"].(float64) != 1 {
		t.Fatalf("disable %d %v", st, out)
	}
	if v, _ := h.pool.Get("a1"); !v.Disabled {
		t.Fatal("a1 not disabled")
	}
	// ids 空 = 全部
	st, out = h.api(t, "POST", "/admin/accounts/batch", `{"action":"test"}`)
	if st != 200 || out["ok"].(float64) != 3 {
		t.Fatalf("test all %d %v", st, out)
	}
	if st, _ := h.api(t, "POST", "/admin/accounts/batch", `{"action":"delete"}`); st != 400 {
		t.Fatalf("delete-all should need ids, got %d", st)
	}
	if st, _ := h.api(t, "POST", "/admin/accounts/batch", `{"action":"explode"}`); st != 400 {
		t.Fatalf("bad action %d", st)
	}
	st, out = h.api(t, "POST", "/admin/accounts/batch", `{"action":"delete","ids":["a2"]}`)
	if st != 200 || out["ok"].(float64) != 1 || h.pool.Len() != 2 {
		t.Fatalf("delete %d %v len %d", st, out, h.pool.Len())
	}
}

func TestAdminPricesAndOverview(t *testing.T) {
	h := newHarness(t, 1, nil)
	st, p := h.api(t, "GET", "/admin/prices", "")
	if st != 200 || p["cost_basis"] != "api" || p["prices"].(map[string]any)["claude-sonnet"] == nil {
		t.Fatalf("prices %d %v", st, p)
	}
	st, o := h.api(t, "GET", "/admin/overview", "")
	if st != 200 || o["today"] == nil || o["month"] == nil || o["billing"] == nil {
		t.Fatalf("overview %d %v", st, o)
	}
}
