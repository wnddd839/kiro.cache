package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
)

func TestPriceRefreshAndModelRows(t *testing.T) {
	file := filepath.Join(t.TempDir(), "prices.json")
	h := newHarness(t, 1, func(c *config.Config) { c.PricesFile = file })
	h.s.models.merge([]kiro.Model{
		{ID: "claude-sonnet-4.5", Name: "claude-sonnet-4.5", Context: 200000, Multiplier: 1.3, RateUnit: "Credit"},
		{ID: "brand-new", Name: "brand-new", Multiplier: 0.1},
	})

	calls := 0
	old := priceFetch
	t.Cleanup(func() { priceFetch = old })
	priceFetch = func(_ context.Context, _ *http.Client, want func(string) bool) (map[string]meter.Price, error) {
		calls++
		if !want("brand-new") || want("gpt-4o") {
			t.Errorf("want filter: brand-new=%v gpt-4o=%v", want("brand-new"), want("gpt-4o"))
		}
		if calls == 1 {
			return map[string]meter.Price{
				"claude-sonnet-4.5": {Input: 2.5, Output: 12, Source: "openrouter"},
				"brand-new":         {Input: 0.5, Output: 1, Source: "openrouter"},
			}, nil
		}
		// 第二次：OpenRouter 部分失败，只拿到新模型
		return map[string]meter.Price{"brand-new": {Input: 0.6, Output: 1, Source: "openrouter"}}, errors.New("openrouter: 503")
	}

	get := func(method, path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s %s: %d", method, path, res.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	d := get(http.MethodPost, "/admin/prices/refresh")
	if d["synced_at"] == nil || d["sync_error"] != nil {
		t.Errorf("refresh = %v", d)
	}
	if p, _ := h.s.pricer.PriceOf("claude-sonnet-4.5"); p.Input != 2.5 {
		t.Errorf("online price not applied: %+v", p)
	}

	// 部分失败：新值覆盖，缺的来源保留旧值，并报错误
	d = get(http.MethodPost, "/admin/prices/refresh")
	if d["sync_error"] == nil {
		t.Error("partial failure not reported")
	}
	if p, _ := h.s.pricer.PriceOf("claude-sonnet-4.5"); p.Input != 2.5 {
		t.Errorf("old entry dropped: %+v", p)
	}
	if p, _ := h.s.pricer.PriceOf("brand-new"); p.Input != 0.6 {
		t.Errorf("new entry not applied: %+v", p)
	}

	// 落盘后新进程能读回
	if n, err := (&meter.Pricer{}).LoadOnline(file); err != nil || n != 2 {
		t.Errorf("saved = %d %v", n, err)
	}

	m := get(http.MethodGet, "/admin/models")
	var rows []modelRow
	b, _ := json.Marshal(m["models"])
	_ = json.Unmarshal(b, &rows)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		switch r.ID {
		case "claude-sonnet-4.5":
			if r.Multiplier != 1.3 || !r.Priced || r.Price.Input != 2.5 || r.Price.Source != "openrouter" || r.CreditUSD != 0.02 {
				t.Errorf("sonnet row = %+v", r)
			}
		case "brand-new":
			if r.Multiplier != 0.1 || !r.Priced || r.Price.Input != 0.6 {
				t.Errorf("new row = %+v", r)
			}
		}
	}
}
