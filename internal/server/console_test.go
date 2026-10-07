package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/kiro"
)

func (h *harness) do(t *testing.T, method, path string, header map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestConsolePageServed(t *testing.T) {
	h := newHarness(t, 0, nil)
	for _, tc := range []struct {
		path, accept string
		html         bool
	}{
		{"/", "text/html,application/xhtml+xml", true},
		{"/ui", "", true},
		{"/", "", false}, // 探活脚本
		{"/health", "text/html", false},
	} {
		status, body := h.do(t, http.MethodGet, tc.path, map[string]string{"Accept": tc.accept})
		if status != 200 {
			t.Fatalf("%s: status %d", tc.path, status)
		}
		if got := strings.Contains(string(body), "<!DOCTYPE html>") || strings.Contains(string(body), "<!doctype html>"); got != tc.html {
			t.Fatalf("%s accept=%q: html=%v, body %.80s", tc.path, tc.accept, got, body)
		}
	}
}

func TestConsoleHTMLNoExternalResources(t *testing.T) {
	page := string(consoleHTML)
	for _, bad := range []string{"fonts.googleapis", "fonts.gstatic", "feTurbulence", "<script src=\"http", "<link rel=\"stylesheet\" href=\"http", "`"} {
		if strings.Contains(page, bad) {
			t.Errorf("admin.html contains %q", bad)
		}
	}
}

func TestAdminCrossSiteWriteRejected(t *testing.T) {
	h := newHarness(t, 1, nil)
	status, _ := h.do(t, http.MethodPost, "/admin/accounts/a0/disable", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if status != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d, want 403", status)
	}
	if v, _ := h.pool.Get("a0"); v.Disabled {
		t.Fatal("cross-site request disabled the account")
	}
	status, _ = h.do(t, http.MethodPost, "/admin/accounts/a0/disable", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if status != 200 {
		t.Fatalf("same-origin POST: %d", status)
	}
	if status, _ := h.do(t, http.MethodGet, "/admin/accounts", map[string]string{"Sec-Fetch-Site": "cross-site"}); status != 200 {
		t.Fatalf("cross-site GET: %d", status)
	}
}

func TestAdminOverview(t *testing.T) {
	h := newHarness(t, 2, func(c *config.Config) { c.APIKeys = []string{"k"}; c.ReportedUsage = kiro.ReportedRaw })
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), map[string]string{"x-api-key": "k"})
	if res.StatusCode != 200 {
		t.Fatalf("messages: %d", res.StatusCode)
	}
	if err := h.pool.SetDisabled("a1", true, "x"); err != nil {
		t.Fatal(err)
	}
	status, body := h.do(t, http.MethodGet, "/admin/overview", nil)
	if status != 200 {
		t.Fatalf("overview: %d %s", status, body)
	}
	var ov struct {
		Version  string
		Auth     map[string]bool
		Config   map[string]any
		Accounts struct{ Total, Enabled, Disabled int }
		Totals   struct {
			Requests  int64 `json:"requests"`
			CacheRead int64 `json:"cache_read_tokens"`
		}
		HitRate float64 `json:"cache_hit_rate"`
	}
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Accounts.Total != 2 || ov.Accounts.Enabled != 1 || ov.Accounts.Disabled != 1 {
		t.Fatalf("accounts = %+v", ov.Accounts)
	}
	if ov.Totals.Requests != 1 || ov.Totals.CacheRead != 900 || ov.HitRate <= 0.9 {
		t.Fatalf("totals = %+v rate %v", ov.Totals, ov.HitRate)
	}
	if !ov.Auth["api_keys"] || ov.Auth["admin_token"] || ov.Config["conversation_mode"] != "session" || ov.Version != "dev" {
		t.Fatalf("overview = %s", body)
	}
	if strings.Contains(string(body), `"k"`) {
		t.Fatalf("overview leaks api key: %s", body)
	}
}

func TestAdminRequestLog(t *testing.T) {
	h := newHarness(t, 2, func(c *config.Config) { c.ReportedUsage = kiro.ReportedRaw })
	h.up.reply = func(token string) (int, string) {
		if token == "tok0" {
			return 429, `{"message":"Too many requests"}`
		}
		return 200, ""
	}
	if err := h.pool.SetDisabled("a1", true, ""); err != nil {
		t.Fatal(err)
	}
	// a0 限流 → 无号可换 → 429
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	res.Body.Close()
	if err := h.pool.SetDisabled("a1", false, ""); err != nil {
		t.Fatal(err)
	}
	// a0 冷却中 → a1 成功
	res = h.post(t, "/v1/messages", fmt.Sprintf(turn1, "b"), map[string]string{"X-Session-Id": "s1"})
	res.Body.Close()

	status, body := h.do(t, http.MethodGet, "/admin/requests?limit=10", nil)
	if status != 200 {
		t.Fatalf("requests: %d", status)
	}
	var out struct{ Requests []requestRecord }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Requests) != 2 {
		t.Fatalf("requests = %s", body)
	}
	ok, failed := out.Requests[0], out.Requests[1] // 新的在前
	if !ok.OK || ok.Account != "a1" || ok.CacheRead != 900 || ok.Status != 200 || ok.Thread == "" || ok.Conv == "" {
		t.Fatalf("ok record = %+v", ok)
	}
	if failed.OK || failed.Account != "a0" || failed.Status != 429 || failed.Error == "" {
		t.Fatalf("failed record = %+v", failed)
	}
	if strings.Contains(string(body), "fix the bug") || strings.Contains(string(body), "tok0") {
		t.Fatalf("request log leaks content or tokens: %s", body)
	}
}

func TestAdminRequestLogNoAccount(t *testing.T) {
	h := newHarness(t, 0, nil)
	res := h.post(t, "/v1/messages", fmt.Sprintf(turn1, "a"), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("empty pool: %d", res.StatusCode)
	}
	recs := h.s.reqlog.recent(10)
	if len(recs) != 1 || recs[0].Status != http.StatusServiceUnavailable || recs[0].Account != "" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestRequestLogRing(t *testing.T) {
	l := newRequestLog(3)
	if got := l.recent(10); len(got) != 0 {
		t.Fatalf("empty log: %v", got)
	}
	for i := range 5 {
		l.add(requestRecord{MS: int64(i)})
	}
	got := l.recent(10)
	if len(got) != 3 || got[0].MS != 4 || got[1].MS != 3 || got[2].MS != 2 {
		t.Fatalf("recent = %+v", got)
	}
	if got := l.recent(1); len(got) != 1 || got[0].MS != 4 {
		t.Fatalf("recent(1) = %+v", got)
	}
}

func TestAdminModels(t *testing.T) {
	h := newHarness(t, 1, nil)
	status, body := h.do(t, http.MethodPost, "/admin/models/refresh", nil)
	if status != 200 || !strings.Contains(string(body), `"claude-sonnet-4.5"`) || !strings.Contains(string(body), "updated_at") {
		t.Fatalf("models refresh: %d %s", status, body)
	}
}

func waitLogin(t *testing.T, h *harness, want string) loginState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := h.do(t, http.MethodGet, "/admin/login", nil)
		var st loginState
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatal(err)
		}
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("login state %+v, want %s", st, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdminLoginAddsAccount(t *testing.T) {
	h := newHarness(t, 0, nil)
	release := make(chan struct{})
	h.s.openBrowser = func(string) error { return nil }
	h.s.signIn = func(ctx context.Context, open func(string) error) (kiro.Login, error) {
		_ = open("https://app.kiro.dev/signin?x")
		<-release
		return kiro.Login{Email: "me@x.com", Cred: kiro.Cred{Method: kiro.MethodSocial, AccessToken: "new", RefreshToken: "r",
			ExpiresAt: time.Now().Add(time.Hour), ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/x"}}, nil
	}
	status, body := h.do(t, http.MethodPost, "/admin/login", nil)
	var st loginState
	_ = json.Unmarshal(body, &st)
	if status != http.StatusAccepted || st.State != "waiting" || st.URL == "" {
		t.Fatalf("start: %d %s", status, body)
	}
	if status, _ := h.do(t, http.MethodPost, "/admin/login", nil); status != http.StatusConflict {
		t.Fatalf("second start: %d, want 409", status)
	}
	close(release)
	st = waitLogin(t, h, "done")
	if st.Email != "me@x.com" || st.AccountID == "" {
		t.Fatalf("done = %+v", st)
	}
	if v, ok := h.pool.Get(st.AccountID); !ok || v.Label != "me@x.com" {
		t.Fatalf("account not added: %+v", v)
	}
}

func TestAdminLoginCancel(t *testing.T) {
	h := newHarness(t, 0, nil)
	h.s.openBrowser = func(string) error { return nil }
	h.s.signIn = func(ctx context.Context, open func(string) error) (kiro.Login, error) {
		_ = open("u")
		<-ctx.Done()
		return kiro.Login{}, context.Cause(ctx)
	}
	if status, _ := h.do(t, http.MethodPost, "/admin/login", nil); status != http.StatusAccepted {
		t.Fatalf("start: %d", status)
	}
	h.do(t, http.MethodDelete, "/admin/login", nil)
	st := waitLogin(t, h, "failed")
	if !strings.Contains(st.Error, errLoginCanceled.Error()) || h.pool.Len() != 0 {
		t.Fatalf("cancel = %+v", st)
	}
	// 取消后可以重新发起
	h.s.signIn = func(context.Context, func(string) error) (kiro.Login, error) {
		return kiro.Login{}, errors.New("ports busy")
	}
	status, body := h.do(t, http.MethodPost, "/admin/login", nil)
	if status != http.StatusAccepted {
		t.Fatalf("restart: %d %s", status, body)
	}
	if st := waitLogin(t, h, "failed"); st.Error != "ports busy" {
		t.Fatalf("restart failed = %+v", st)
	}
}
