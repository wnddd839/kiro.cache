package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeKiro is a local stand-in for every Kiro endpoint. Each request is
// recorded; handle answers it.
type fakeKiro struct {
	mu     sync.Mutex
	reqs   []recorded
	handle func(w http.ResponseWriter, r *http.Request, body []byte)
}

type recorded struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         []byte
}

func newFake(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body []byte)) (*fakeKiro, *Client) {
	t.Helper()
	f := &fakeKiro{handle: handle}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
		f.mu.Unlock()
		f.handle(w, r, body)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.Client())
	c.RuntimeURL = func(r string) string { return srv.URL + "/runtime/" + r }
	c.ManagementURL = func(r string) string { return srv.URL + "/mgmt/" + r }
	c.RefreshURL = func(r string) string { return srv.URL + "/refresh/" + r + "/refreshToken" }
	c.OIDCURL = func(r string) string { return srv.URL + "/oidc/" + r }
	c.AuthService = srv.URL + "/auth"
	return f, c
}

func (f *fakeKiro) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(t *testing.T, b []byte) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("request body %q: %v", b, err)
	}
	return m
}

func TestRefreshSocial(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"accessToken": "new-access", "refreshToken": "new-refresh", "expiresIn": 1800, "profileArn": "arn:aws:codewhisperer:eu-central-1:1:profile/P"})
	})
	old := Cred{Method: MethodSocial, AccessToken: "old", RefreshToken: "old-refresh", Region: "us-east-1"}
	before := time.Now()
	got, err := c.Refresh(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.ProfileArn != "arn:aws:codewhisperer:eu-central-1:1:profile/P" {
		t.Errorf("cred = %+v", got)
	}
	if d := got.ExpiresAt.Sub(before); d < 1790*time.Second || d > 1810*time.Second {
		t.Errorf("expires in %v, want ~30m", d)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/refresh/us-east-1/refreshToken" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	if b := decodeBody(t, r.Body); len(b) != 1 || b["refreshToken"] != "old-refresh" {
		t.Errorf("body = %v", b)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Kiro-Desktop/") {
		t.Errorf("user-agent = %q", ua)
	}
}

func TestRefreshKeepsOldRefreshTokenAndDefaults(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"accessToken": "a2"})
	})
	old := Cred{RefreshToken: "r1", ProfileArn: "arn:keep"} // empty method = social, empty region = us-east-1
	got, err := c.Refresh(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "a2" || got.RefreshToken != "r1" || got.ProfileArn != "arn:keep" {
		t.Errorf("cred = %+v", got)
	}
	if d := time.Until(got.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("default expiry %v, want ~1h", d)
	}
	if p := f.requests()[0].Path; p != "/refresh/us-east-1/refreshToken" {
		t.Errorf("path = %q", p)
	}
}

func TestRefreshIDC(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"accessToken": "idc-access", "refreshToken": "idc-refresh", "expiresIn": 3600})
	})
	cred := Cred{Method: MethodIDC, RefreshToken: "rt", ClientID: "cid", ClientSecret: "csec", Region: "eu-west-1"}
	got, err := c.Refresh(t.Context(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "idc-access" || got.RefreshToken != "idc-refresh" || got.ClientID != "cid" {
		t.Errorf("cred = %+v", got)
	}
	r := f.requests()[0]
	if r.Method != http.MethodPost || r.Path != "/oidc/eu-west-1/token" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	want := map[string]string{"clientId": "cid", "clientSecret": "csec", "refreshToken": "rt", "grantType": "refresh_token"}
	if b := decodeBody(t, r.Body); fmt.Sprint(b) != fmt.Sprint(want) {
		t.Errorf("body = %v, want %v", b, want)
	}
}

func TestRefreshExternalIdP(t *testing.T) {
	var got []recorded
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, recorded{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
		writeJSON(w, 200, map[string]any{"access_token": "ext-a", "refresh_token": "ext-r", "expires_in": 60})
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.Client())
	c.IdPHosts = []string{"127.0.0.1"}
	cred := Cred{Method: MethodExternalIdP, RefreshToken: "rt", ClientID: "cid", ClientSecret: "cs", TokenURL: srv.URL + "/idp/token"}
	n, err := c.Refresh(t.Context(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if n.AccessToken != "ext-a" || n.RefreshToken != "ext-r" {
		t.Errorf("cred = %+v", n)
	}
	r := got[0]
	if r.Path != "/idp/token" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("request = %s %v", r.Path, r.Header)
	}
	form, _ := url.ParseQuery(string(r.Body))
	if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "cid" || form.Get("refresh_token") != "rt" || form.Get("client_secret") != "cs" {
		t.Errorf("form = %v", form)
	}

	// 不在允许列表 / 非 https：不发请求，凭证判失效
	for _, u := range []string{"https://evil.example/token", "http://login.microsoftonline.com/x/oauth2/v2.0/token", "https://user:pw@login.microsoftonline.com/t", "https://login.microsoftonline.com.evil.example/t"} {
		c2 := NewClient(srv.Client())
		bad := cred
		bad.TokenURL = u
		before := len(got)
		if _, err := c2.Refresh(t.Context(), bad); err == nil || !IsGone(err) || len(got) != before {
			t.Errorf("%s: err %v (gone %v), requests %d", u, err, IsGone(err), len(got)-before)
		}
	}
	if err := CheckTokenURL("https://login.microsoftonline.com/tenant/oauth2/v2.0/token", nil); err != nil {
		t.Errorf("entra rejected: %v", err)
	}
}

func TestRefreshFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   any
		cred   Cred
		gone   bool
		calls  int
	}{
		// 只有 400 + invalid_grant 是永久失效；其它 4xx 由号池连续计数
		{"401 not gone by itself", 401, map[string]string{"message": "Invalid refresh token"}, Cred{Method: MethodSocial, RefreshToken: "r"}, false, 1},
		{"400 invalid_grant", 400, map[string]string{"error": "invalid_grant"}, Cred{Method: MethodIDC, RefreshToken: "r"}, true, 1},
		{"400 other", 400, map[string]string{"error": "invalid_request"}, Cred{Method: MethodSocial, RefreshToken: "r"}, false, 1},
		{"403 not gone", 403, map[string]string{"message": "no"}, Cred{Method: MethodSocial, RefreshToken: "r"}, false, 1},
		{"500 is not gone", 500, map[string]string{"message": "boom"}, Cred{Method: MethodSocial, RefreshToken: "r"}, false, 1},
		{"200 without token", 200, map[string]string{}, Cred{Method: MethodSocial, RefreshToken: "r"}, true, 1},
		{"no refresh token", 200, nil, Cred{Method: MethodSocial, AccessToken: "a"}, true, 0},
		{"api key", 200, nil, Cred{Method: MethodAPIKey, AccessToken: "ksk_x", RefreshToken: "r"}, true, 0},
		{"external idp without url", 200, nil, Cred{Method: MethodExternalIdP, RefreshToken: "r"}, true, 0},
		{"unknown method", 200, nil, Cred{Method: "weird", RefreshToken: "r"}, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { writeJSON(w, tt.status, tt.body) })
			got, err := c.Refresh(t.Context(), tt.cred)
			if err == nil {
				t.Fatalf("Refresh succeeded: %+v", got)
			}
			if IsGone(err) != tt.gone {
				t.Errorf("IsGone(%v) = %v, want %v", err, !tt.gone, tt.gone)
			}
			if got != tt.cred {
				t.Errorf("cred changed on failure: %+v", got)
			}
			if n := len(f.requests()); n != tt.calls {
				t.Errorf("requests = %d, want %d", n, tt.calls)
			}
			if tt.calls > 0 && tt.status != 200 {
				e, ok := errors.AsType[*Error](err)
				if !ok || e.Status != tt.status {
					t.Errorf("err = %#v, want *Error with status %d", err, tt.status)
				}
			}
		})
	}
}

func TestIsGone(t *testing.T) {
	if IsGone(nil) || IsGone(errors.New("x")) || IsGone(&Error{Status: 500}) {
		t.Error("IsGone true for non-gone errors")
	}
	if !IsGone(fmt.Errorf("wrap: %w", &Error{Gone: true})) {
		t.Error("IsGone false for wrapped gone error")
	}
}

func TestErrorString(t *testing.T) {
	if got := (&Error{Status: 403, Message: "no"}).Error(); got != "kiro: no (403)" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&Error{Message: "no"}).Error(); got != "kiro: no" {
		t.Errorf("Error() = %q", got)
	}
}

func TestCredHelpers(t *testing.T) {
	for _, tt := range []struct {
		cred Cred
		want string
	}{
		{Cred{ProfileArn: "arn:aws:codewhisperer:eu-central-1:123:profile/X"}, "eu-central-1"},
		{Cred{ProfileArn: "arn:aws:codewhisperer:us-west-2:123:profile/X", Region: "eu-west-1"}, "us-west-2"},
		{Cred{Region: "eu-west-1"}, "eu-central-1"},
		{Cred{Region: "ap-south-1"}, "us-east-1"},
		{Cred{}, "us-east-1"},
		{Cred{ProfileArn: "garbage", Region: "eu-north-1"}, "eu-central-1"},
		{Cred{ProfileArn: "arn:aws:codewhisperer::123:profile/X"}, "us-east-1"},
	} {
		if got := tt.cred.APIRegion(); got != tt.want {
			t.Errorf("APIRegion(%+v) = %q, want %q", tt.cred, got, tt.want)
		}
	}

	for m, want := range map[Method]string{MethodAPIKey: "API_KEY", MethodExternalIdP: "EXTERNAL_IDP", MethodSocial: "", MethodIDC: ""} {
		if got := (Cred{Method: m}).TokenType(); got != want {
			t.Errorf("TokenType(%s) = %q, want %q", m, got, want)
		}
	}

	lead := 5 * time.Minute
	for _, tt := range []struct {
		cred Cred
		want bool
	}{
		{Cred{Method: MethodAPIKey, ExpiresAt: time.Now().Add(-time.Hour)}, true},
		{Cred{Method: MethodSocial}, true},
		{Cred{Method: MethodSocial, ExpiresAt: time.Now().Add(time.Hour)}, true},
		{Cred{Method: MethodSocial, ExpiresAt: time.Now().Add(time.Minute)}, false},
		{Cred{Method: MethodIDC, ExpiresAt: time.Now().Add(-time.Minute)}, false},
	} {
		if got := tt.cred.Fresh(lead); got != tt.want {
			t.Errorf("Fresh(%+v) = %v, want %v", tt.cred, got, tt.want)
		}
	}
}

func TestListModels(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{
			"models": []map[string]any{
				{"modelId": "claude-haiku-4.5", "modelName": "Haiku", "tokenLimits": map[string]int{"maxInputTokens": 200000, "maxOutputTokens": 64000},
					"rateMultiplier": 0.4, "rateUnit": "Credit",
					"promptCaching": map[string]any{"maximumCacheCheckpointsPerRequest": 4, "minimumTokensPerCacheCheckpoint": 1024, "supportsPromptCaching": true}},
				{"modelId": ""},
				{"modelId": "claude-sonnet-4.5", "supportedInputTypes": []string{"TEXT", "image"}, "rateMultiplier": 1.3},
				{"modelId": "auto", "modelName": "Auto", "rateMultiplier": 1},
			},
			"defaultModel": map[string]string{"modelId": "auto"},
		})
	})
	cred := Cred{Method: MethodSocial, AccessToken: "tok", ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/P"}
	got, err := c.ListModels(t.Context(), cred)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "auto", Name: "Auto", Multiplier: 1, Default: true},
		{ID: "claude-haiku-4.5", Name: "Haiku", Context: 200000, Output: 64000, Multiplier: 0.4, RateUnit: "Credit",
			PromptCaching: &PromptCaching{Supported: true, MaxCheckpoints: 4, MinTokens: 1024}},
		{ID: "claude-sonnet-4.5", Name: "claude-sonnet-4.5", Images: true, Multiplier: 1.3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("models =\n%+v\nwant\n%+v", got, want)
	}
	r := f.requests()[0]
	if r.Method != http.MethodGet || r.Path != "/mgmt/eu-central-1/List-Available-Models" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	if r.Query.Get("origin") != "KIRO_CLI" || r.Query.Get("profileArn") != cred.ProfileArn {
		t.Errorf("query = %v", r.Query)
	}
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("tokentype") != "" {
		t.Errorf("auth headers = %v", r.Header)
	}
}

func TestListModelsEmptyAndError(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"models": []any{}})
	})
	if _, err := c.ListModels(t.Context(), Cred{}); err == nil {
		t.Error("empty model list: want error")
	}

	_, c = newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 403, map[string]string{"message": "denied"})
	})
	_, err := c.ListModels(t.Context(), Cred{})
	if e, ok := errors.AsType[*Error](err); !ok || e.Status != 403 || e.Message != "denied" || !strings.Contains(e.Body, "denied") || e.Gone {
		t.Errorf("err = %#v", err)
	}
}

func TestUsageLimits(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{
			"nextDateReset": 1700000000,
			"usageBreakdownList": []map[string]any{
				{"currentUsageWithPrecision": 10.5, "usageLimitWithPrecision": 50, "nextDateReset": 1800000000,
					"freeTrialInfo": map[string]any{"freeTrialStatus": "active", "currentUsageWithPrecision": 100, "usageLimitWithPrecision": 500}},
				{"currentUsageWithPrecision": 4, "usageLimitWithPrecision": 0},
				{"currentUsageWithPrecision": 1.5, "usageLimitWithPrecision": 10,
					"freeTrialInfo": map[string]any{"freeTrialStatus": "EXPIRED", "currentUsageWithPrecision": 9, "usageLimitWithPrecision": 9}},
			},
			"subscriptionInfo": map[string]string{"subscriptionTitle": "KIRO PRO"},
			"userInfo":         map[string]string{"email": "me@example.com"},
		})
	})
	cred := Cred{Method: MethodAPIKey, AccessToken: "ksk_1"}
	l, err := c.UsageLimits(t.Context(), cred)
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{Email: "me@example.com", Plan: "Kiro Pro", Used: 112, Limit: 560, ResetAt: time.Unix(1800000000, 0)}
	if l != want {
		t.Errorf("limits = %+v, want %+v", l, want)
	}
	if rem := l.Remaining(); rem < 0.79 || rem > 0.81 {
		t.Errorf("Remaining = %v, want 0.8", rem)
	}
	r := f.requests()[0]
	if r.Path != "/mgmt/us-east-1/Get-Usage-Limits" {
		t.Errorf("path = %q", r.Path)
	}
	if q := r.Query; q.Get("resourceType") != "CREDIT" || q.Get("isEmailRequired") != "true" || q.Get("origin") != "KIRO_CLI" || q.Has("profileArn") {
		t.Errorf("query = %v", q)
	}
	if r.Header.Get("Authorization") != "Bearer ksk_1" || r.Header.Get("tokentype") != "API_KEY" {
		t.Errorf("headers = %v", r.Header)
	}
}

func TestUsageLimitsTopLevelReset(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"nextDateReset": 1700000000})
	})
	l, err := c.UsageLimits(t.Context(), Cred{})
	if err != nil {
		t.Fatal(err)
	}
	if !l.ResetAt.Equal(time.Unix(1700000000, 0)) || l.Plan != "" || l.Remaining() != 1 {
		t.Errorf("limits = %+v", l)
	}
}

func TestLimitsRemaining(t *testing.T) {
	for _, tt := range []struct {
		l    Limits
		want float64
	}{
		{Limits{}, 1},
		{Limits{Used: 5, Limit: 10}, 0.5},
		{Limits{Used: 20, Limit: 10}, 0},
	} {
		if got := tt.l.Remaining(); got != tt.want {
			t.Errorf("Remaining(%+v) = %v, want %v", tt.l, got, tt.want)
		}
	}
}

func TestPlanName(t *testing.T) {
	for in, want := range map[string]string{
		"KIRO PRO":        "Kiro Pro",
		"KIRO FREE":       "Kiro Free",
		"kiro  pro+ ":     "Kiro Pro+",
		"Q DEVELOPER PRO": "Q Developer Pro",
		"":                "",
	} {
		if got := planName(in); got != want {
			t.Errorf("planName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerate(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(event("assistantResponseEvent", `{"content":"hi"}`))
	})
	tests := []struct {
		name      string
		cred      Cred
		path      string
		tokenType string
	}{
		{"social", Cred{Method: MethodSocial, AccessToken: "acc", ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/P"}, "/runtime/eu-central-1/generateAssistantResponse", ""},
		{"api key", Cred{Method: MethodAPIKey, AccessToken: "ksk_abc"}, "/runtime/us-east-1/generateAssistantResponse", "API_KEY"},
		{"external idp", Cred{Method: MethodExternalIdP, AccessToken: "x", Region: "eu-west-1"}, "/runtime/eu-central-1/generateAssistantResponse", "EXTERNAL_IDP"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.Generate(t.Context(), tt.cred, []byte(`{"conversationState":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			evs := drain(t, NewDecoder(res.Body, false, 0, nil))
			if evs[0].Text != "hi" {
				t.Errorf("events = %+v", evs)
			}

			r := f.requests()[i]
			if r.Method != http.MethodPost || r.Path != tt.path {
				t.Errorf("request = %s %s, want POST %s", r.Method, r.Path, tt.path)
			}
			if string(r.Body) != `{"conversationState":{}}` {
				t.Errorf("body = %s", r.Body)
			}
			h := r.Header
			if got := h.Get("Authorization"); got != "Bearer "+tt.cred.AccessToken {
				t.Errorf("Authorization = %q", got)
			}
			if got := h.Get("tokentype"); got != tt.tokenType {
				t.Errorf("tokentype = %q, want %q", got, tt.tokenType)
			}
			for k, want := range map[string]string{
				"Content-Type":                "application/json",
				"Accept":                      "application/vnd.amazon.eventstream",
				"x-amzn-codewhisperer-optout": "true",
				"x-amzn-kiro-agent-mode":      "vibe",
				"amz-sdk-request":             "attempt=1; max=1",
			} {
				if got := h.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if !uuidRe.MatchString(h.Get("amz-sdk-invocation-id")) {
				t.Errorf("invocation id = %q", h.Get("amz-sdk-invocation-id"))
			}
			if ua := h.Get("User-Agent"); !strings.HasPrefix(ua, "aws-sdk-rust/") || !strings.HasPrefix(h.Get("x-amz-user-agent"), "aws-sdk-rust/") {
				t.Errorf("user agents = %q / %q", ua, h.Get("x-amz-user-agent"))
			}
		})
	}
}

func TestProfile(t *testing.T) {
	t.Run("builder id", func(t *testing.T) {
		f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(500) })
		got, err := c.Profile(t.Context(), Cred{Method: MethodIDC, BuilderID: true})
		if err != nil || got != BuilderIDProfile || len(f.requests()) != 0 {
			t.Errorf("Profile = %q, %v (requests %d)", got, err, len(f.requests()))
		}
	})
	t.Run("api key", func(t *testing.T) {
		f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, 200, map[string]any{"profile": map[string]string{"arn": "arn:key"}})
		})
		got, err := c.Profile(t.Context(), Cred{Method: MethodAPIKey, AccessToken: "ksk"})
		if err != nil || got != "arn:key" {
			t.Fatalf("Profile = %q, %v", got, err)
		}
		r := f.requests()[0]
		if r.Path != "/mgmt/us-east-1/" || r.Header.Get("X-Amz-Target") != "AmazonCodeWhispererService.GetProfile" || r.Header.Get("tokentype") != "API_KEY" {
			t.Errorf("request = %s %v", r.Path, r.Header)
		}
	})
	t.Run("falls back to eu", func(t *testing.T) {
		f, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if strings.Contains(r.URL.Path, "us-east-1") {
				writeJSON(w, 200, map[string]any{"profiles": []any{}})
				return
			}
			writeJSON(w, 200, map[string]any{"profiles": []map[string]string{{"arn": ""}, {"arn": "arn:eu"}}})
		})
		got, err := c.Profile(t.Context(), Cred{Method: MethodSocial, AccessToken: "a"})
		if err != nil || got != "arn:eu" {
			t.Fatalf("Profile = %q, %v", got, err)
		}
		reqs := f.requests()
		if len(reqs) != 2 || reqs[0].Path != "/mgmt/us-east-1/List-Available-Profiles" || reqs[1].Path != "/mgmt/eu-central-1/List-Available-Profiles" {
			t.Errorf("requests = %+v", reqs)
		}
	})
	t.Run("idc 403 builder id", func(t *testing.T) {
		_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, 403, map[string]string{"message": "Builder ID users have no profiles"})
		})
		got, err := c.Profile(t.Context(), Cred{Method: MethodIDC, AccessToken: "a"})
		if err != nil || got != BuilderIDProfile {
			t.Errorf("Profile = %q, %v", got, err)
		}
	})
	t.Run("none", func(t *testing.T) {
		_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, 200, map[string]any{"profiles": []any{}})
		})
		if got, err := c.Profile(t.Context(), Cred{Method: MethodSocial}); err == nil {
			t.Errorf("Profile = %q, want error", got)
		}
	})
	t.Run("error surfaces", func(t *testing.T) {
		_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, 500, map[string]string{"message": "down"})
		})
		_, err := c.Profile(t.Context(), Cred{Method: MethodSocial})
		if e, ok := errors.AsType[*Error](err); !ok || e.Status != 500 {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRefreshHonoursContext(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"accessToken": "a"})
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Refresh(ctx, Cred{Method: MethodSocial, RefreshToken: "r"})
	if !errors.Is(err, context.Canceled) || IsGone(err) {
		t.Errorf("err = %v, want context.Canceled and not gone", err)
	}
}

// IdC client 注册过期：标 Reregister，不是 Gone。
func TestRefreshIdCClientExpired(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 400, map[string]string{"error": "invalid_client", "error_description": "client expired"})
	})
	cred := Cred{Method: MethodIDC, RefreshToken: "r", ClientID: "c", ClientSecret: "s", ClientSecretExpiresAt: time.Now().Add(-time.Hour)}
	_, err := c.Refresh(t.Context(), cred)
	if !IsReregister(err) || IsGone(err) {
		t.Fatalf("err = %v reregister=%v gone=%v", err, IsReregister(err), IsGone(err))
	}
	// 没过期、也不是 invalid_client 的 400：普通失败
	_, c2 := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 400, map[string]string{"error": "slow_down"})
	})
	cred.ClientSecretExpiresAt = time.Now().Add(time.Hour)
	if _, err := c2.Refresh(t.Context(), cred); IsReregister(err) || IsGone(err) {
		t.Fatalf("plain 400 classified as %v", err)
	}
}

// 对话、OIDC 刷新、management 调用用同一套 kiro-cli 2.x 身份；版本可配。
func TestCLIIdentityEverywhere(t *testing.T) {
	var uas []string
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		uas = append(uas, r.Header.Get("User-Agent")+" | "+r.Header.Get("x-amz-user-agent"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			writeJSON(w, 200, map[string]any{"accessToken": "a2", "expiresIn": 60})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			writeJSON(w, 200, map[string]any{})
		default:
			w.WriteHeader(200)
		}
	})
	_ = f
	c.Identity = Identity{CLIVersion: "9.9.9", APIVersion: "0.1.1"}
	cred := Cred{Method: MethodIDC, AccessToken: "a", RefreshToken: "r", ClientID: "c", ClientSecret: "s", MachineID: "0123456789abcdef0123456789abcdef"}
	if _, err := c.Refresh(t.Context(), cred); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UsageLimits(t.Context(), cred); err != nil {
		t.Fatal(err)
	}
	res, err := c.Generate(t.Context(), cred, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	res, _ = c.Generate(t.Context(), cred, []byte(`{}`))
	res.Body.Close()
	base := "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.1 os/" + cliOS() + " lang/rust/1.92.0"
	want := base + " md/appVersion-9.9.9 app/AmazonQ-For-CLI | " + base + " m/F app/AmazonQ-For-CLI"
	if len(uas) != 4 {
		t.Fatalf("requests %v", uas)
	}
	for _, ua := range uas {
		if ua != want {
			t.Errorf("ua = %q, want %q", ua, want)
		}
	}
}

func TestConnDropped(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{io.EOF, true},
		{fmt.Errorf("post: %w", io.ErrUnexpectedEOF), true},
		{&url.Error{Op: "Post", URL: "u", Err: syscall.ECONNRESET}, true},
		{syscall.EPIPE, true},
		{errors.New("wsarecv: An existing connection was forcibly closed by the remote host."), true},
		{errors.New("read tcp: connection reset by peer"), true},
		{errors.New("dial tcp: connection refused"), false},
		{context.DeadlineExceeded, false},
	} {
		if got := ConnDropped(tc.err); got != tc.want {
			t.Errorf("ConnDropped(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestNewClientIdleConnTimeout(t *testing.T) {
	tr, ok := NewClient(nil).HTTP.Transport.(*http.Transport)
	if !ok || tr.IdleConnTimeout != IdleConnTimeout || tr.Proxy == nil {
		t.Fatalf("transport = %#v", NewClient(nil).HTTP.Transport)
	}
}

// 连接在收到任何响应字节前断开：用新连接重试一次；已收到部分响应再断：不重试（上游可能已处理）。
func TestGenerateRetriesOnlyBeforeFirstByte(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial string // 第一次关连接前写出的字节
		hits    int64
		ok      bool
	}{
		{"no bytes", "", 2, true},
		// 状态行已到、头没写完就断：Go 报 unexpected EOF，但上游已经开始回复
		{"partial header", "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if hits.Add(1) == 1 {
					conn, buf, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = buf.WriteString(tc.partial)
					_ = buf.Flush()
					conn.Close()
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer srv.Close()
			c := NewClient(srv.Client())
			c.RuntimeURL = func(string) string { return srv.URL }
			res, err := c.Generate(t.Context(), Cred{AccessToken: "t"}, []byte(`{}`))
			if err == nil {
				res.Body.Close()
			}
			if (err == nil) != tc.ok || hits.Load() != tc.hits {
				t.Fatalf("err %v hits %d, want ok %v hits %d", err, hits.Load(), tc.ok, tc.hits)
			}
		})
	}
}
