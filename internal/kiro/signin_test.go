package kiro

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startSignIn runs a SignIn against c on a free port. The portal URL it
// would open arrives on the returned channel; Run's result on the other.
func startSignIn(t *testing.T, ctx context.Context, c *Client, timeout time.Duration) (<-chan *url.URL, <-chan signInResult) {
	t.Helper()
	opened := make(chan *url.URL, 1)
	done := make(chan signInResult, 1)
	s := &SignIn{
		Client:    c,
		Ports:     []int{0},
		Timeout:   timeout,
		PortalURL: "https://portal.test/",
		Open: func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				t.Errorf("portal url %q: %v", raw, err)
			}
			opened <- u
			return errors.New("no browser in tests") // not fatal
		},
	}
	go func() {
		l, err := s.Run(ctx)
		done <- signInResult{l, err}
	}()
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("Open was never called")
	case u := <-opened:
		opened <- u
	}
	return opened, done
}

func portal(t *testing.T, opened <-chan *url.URL) (u *url.URL, state, redirect string) {
	t.Helper()
	u = <-opened
	q := u.Query()
	if u.Scheme != "https" || u.Host != "portal.test" || u.Path != "/signin" {
		t.Errorf("portal = %s", u)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_from") != "KiroIDE" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Errorf("portal query = %v", q)
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://localhost:") {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	return u, q.Get("state"), q.Get("redirect_uri")
}

// noFollow is a browser stand-in that shows redirects instead of following them.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(t *testing.T, u string) (*http.Response, string) {
	t.Helper()
	res, err := noFollow.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func result(t *testing.T, done <-chan signInResult) signInResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return signInResult{}
	}
}

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func usageOK(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"userInfo": map[string]any{"email": "me@example.com"},
		"subscriptionInfo": map[string]any{"subscriptionTitle": "KIRO PRO"}})
}

func TestSignInGoogle(t *testing.T) {
	var tokenBody map[string]string
	var tokenUA string
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case r.URL.Path == "/auth/oauth/token":
			_ = json.Unmarshal(body, &tokenBody)
			tokenUA = r.Header.Get("User-Agent")
			writeJSON(w, 200, map[string]any{"accessToken": "acc", "refreshToken": "ref", "expiresIn": 1200,
				"profileArn": "arn:aws:codewhisperer:us-east-1:1:profile/P"})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			usageOK(w)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	opened, done := startSignIn(t, t.Context(), c, time.Minute)
	u, state, redirect := portal(t, opened)

	res, body := get(t, redirect+"/oauth/callback?"+url.Values{"code": {"the-code"}, "state": {state}, "login_option": {"google"}}.Encode())
	if res.StatusCode != 200 || !strings.Contains(body, "You&#39;re signed in") || !strings.Contains(body, "me@example.com") {
		t.Errorf("callback = %d %s", res.StatusCode, body)
	}
	r := result(t, done)
	if r.err != nil {
		t.Fatal(r.err)
	}
	cr := r.login.Cred
	if cr.Method != MethodSocial || cr.AccessToken != "acc" || cr.RefreshToken != "ref" || cr.Region != "us-east-1" ||
		cr.ProfileArn != "arn:aws:codewhisperer:us-east-1:1:profile/P" {
		t.Errorf("cred = %+v", cr)
	}
	if d := time.Until(cr.ExpiresAt); d < 19*time.Minute || d > 21*time.Minute {
		t.Errorf("expires in %v", d)
	}
	if r.login.Email != "me@example.com" || r.login.Plan == "" {
		t.Errorf("login = %+v", r.login)
	}
	if tokenBody["code"] != "the-code" || tokenBody["redirect_uri"] != redirect+"/oauth/callback?login_option=google" ||
		s256(tokenBody["code_verifier"]) != u.Query().Get("code_challenge") {
		t.Errorf("token body = %v", tokenBody)
	}
	if !strings.HasPrefix(tokenUA, "KiroIDE-") {
		t.Errorf("user-agent = %q", tokenUA)
	}
	if n := len(f.requests()); n != 2 {
		t.Errorf("requests = %d, want token + usage", n)
	}
	// the listener is closed once Run returns
	if _, err := noFollow.Get(redirect + "/oauth/callback"); err == nil {
		t.Error("callback port still open after Run")
	}
}

func TestSignInBuilderID(t *testing.T) {
	var regBody map[string]any
	var tokenBody map[string]string
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case r.URL.Path == "/oidc/us-east-1/client/register":
			_ = json.Unmarshal(body, &regBody)
			writeJSON(w, 200, map[string]any{"clientId": "cid", "clientSecret": "csec"})
		case r.URL.Path == "/oidc/us-east-1/token":
			_ = json.Unmarshal(body, &tokenBody)
			writeJSON(w, 200, map[string]any{"accessToken": "idc-acc", "refreshToken": "idc-ref", "expiresIn": 3600})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			if got := r.URL.Query().Get("profileArn"); got != BuilderIDProfile {
				t.Errorf("usage profileArn = %q", got)
			}
			usageOK(w)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	opened, done := startSignIn(t, t.Context(), c, time.Minute)
	_, state, redirect := portal(t, opened)

	res, _ := get(t, redirect+"/signin/callback?"+url.Values{"state": {state}, "login_option": {"builderid"},
		"issuer_url": {"https://view.awsapps.com/start"}, "idc_region": {"us-east-1"}}.Encode())
	if res.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", res.StatusCode)
	}
	if regBody["issuerUrl"] != "https://view.awsapps.com/start" || regBody["clientName"] != "Kiro IDE" || regBody["clientType"] != "public" {
		t.Errorf("register body = %v", regBody)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(c.OIDCURL("us-east-1"), "/")
	if !strings.HasPrefix(loc.String(), base+"/authorize?") {
		t.Errorf("location = %s", loc)
	}
	lq := loc.Query()
	awsRedirect := strings.Replace(redirect, "localhost", "127.0.0.1", 1) + "/oauth/callback"
	if lq.Get("client_id") != "cid" || lq.Get("response_type") != "code" || lq.Get("redirect_uri") != awsRedirect ||
		lq.Get("code_challenge_method") != "S256" || lq.Get("state") == "" || lq.Get("state") == state ||
		!strings.Contains(lq.Get("scopes"), "codewhisperer:conversations") {
		t.Errorf("authorize query = %v", lq)
	}

	// AWS sends the browser back with its own state
	res, body := get(t, awsRedirect+"?"+url.Values{"code": {"aws-code"}, "state": {lq.Get("state")}}.Encode())
	if res.StatusCode != 200 || !strings.Contains(body, "signed in") {
		t.Errorf("aws callback = %d %s", res.StatusCode, body)
	}
	r := result(t, done)
	if r.err != nil {
		t.Fatal(r.err)
	}
	cr := r.login.Cred
	if cr.Method != MethodIDC || cr.AccessToken != "idc-acc" || cr.RefreshToken != "idc-ref" || cr.Region != "us-east-1" ||
		cr.ClientID != "cid" || cr.ClientSecret != "csec" || !cr.BuilderID || cr.ProfileArn != BuilderIDProfile {
		t.Errorf("cred = %+v", cr)
	}
	want := map[string]string{"clientId": "cid", "clientSecret": "csec", "grantType": "authorization_code",
		"redirectUri": awsRedirect, "code": "aws-code"}
	for k, v := range want {
		if tokenBody[k] != v {
			t.Errorf("token %s = %q, want %q", k, tokenBody[k], v)
		}
	}
	if s256(tokenBody["codeVerifier"]) != lq.Get("code_challenge") {
		t.Errorf("code verifier doesn't match the challenge")
	}
}

func TestSignInStateMismatch(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opened, done := startSignIn(t, ctx, c, time.Minute)
	_, _, redirect := portal(t, opened)

	res, body := get(t, redirect+"/oauth/callback?"+url.Values{"code": {"x"}, "state": {"someone-else"}, "login_option": {"google"}}.Encode())
	if res.StatusCode != 200 || !strings.Contains(body, "isn&#39;t from this sign-in") {
		t.Errorf("callback = %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, redirect+"/elsewhere"); res.StatusCode != 404 {
		t.Errorf("other path = %d", res.StatusCode)
	}
	select {
	case r := <-done:
		t.Fatalf("Run returned on a foreign callback: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if r := result(t, done); !errors.Is(r.err, context.Canceled) {
		t.Errorf("err = %v, want canceled", r.err)
	}
}

func TestSignInErrorParam(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	opened, done := startSignIn(t, t.Context(), c, time.Minute)
	_, state, redirect := portal(t, opened)

	res, body := get(t, redirect+"/oauth/callback?"+url.Values{"state": {state}, "error": {"access_denied"},
		"error_description": {"user said no"}}.Encode())
	if res.StatusCode != 200 || !strings.Contains(body, "didn&#39;t finish") || !strings.Contains(body, "user said no") {
		t.Errorf("callback = %d %s", res.StatusCode, body)
	}
	r := result(t, done)
	if r.err == nil || !strings.Contains(r.err.Error(), "user said no") {
		t.Errorf("err = %v", r.err)
	}
}

func TestSignInUnsupportedOption(t *testing.T) {
	for _, opt := range []string{"external_idp", "carrier-pigeon"} {
		t.Run(opt, func(t *testing.T) {
			_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			})
			opened, done := startSignIn(t, t.Context(), c, time.Minute)
			_, state, redirect := portal(t, opened)
			if _, body := get(t, redirect+"/oauth/callback?"+url.Values{"state": {state}, "login_option": {opt}}.Encode()); !strings.Contains(body, "didn&#39;t finish") {
				t.Errorf("page = %s", body)
			}
			if r := result(t, done); r.err == nil {
				t.Error("Run succeeded")
			}
		})
	}
}

func TestSignInTokenRefused(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "code expired"})
	})
	opened, done := startSignIn(t, t.Context(), c, time.Minute)
	_, state, redirect := portal(t, opened)
	get(t, redirect+"/oauth/callback?"+url.Values{"code": {"c"}, "state": {state}, "login_option": {"github"}}.Encode())
	r := result(t, done)
	if r.err == nil || !strings.Contains(r.err.Error(), "refused (400)") || !strings.Contains(r.err.Error(), "code expired") {
		t.Errorf("err = %v", r.err)
	}
}

func TestSignInTimeout(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {})
	opened, done := startSignIn(t, t.Context(), c, 50*time.Millisecond)
	_, _, redirect := portal(t, opened)
	r := result(t, done)
	if r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
		t.Errorf("err = %v", r.err)
	}
	if _, err := noFollow.Get(redirect + "/oauth/callback"); err == nil {
		t.Error("callback port still open after timeout")
	}
}

func TestSignInPortsBusy(t *testing.T) {
	ln, err := listenCallback([]int{0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	s := &SignIn{Ports: []int{busy}, Open: func(string) error { t.Error("opened"); return nil }}
	if _, err := s.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("err = %v", err)
	}
}
