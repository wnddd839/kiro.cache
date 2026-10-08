package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startSignInManual 起一个带 Manual 通道的登录，返回该通道与结果通道。
func startSignInManual(t *testing.T, ctx context.Context, c *Client) (chan<- Callback, <-chan signInResult, <-chan *url.URL) {
	t.Helper()
	manual := make(chan Callback)
	opened := make(chan *url.URL, 1)
	done := make(chan signInResult, 1)
	s := &SignIn{
		Client:    c,
		Ports:     []int{0},
		Timeout:   time.Minute,
		PortalURL: "https://portal.test/",
		Manual:    manual,
		Open: func(raw string) error {
			u, _ := url.Parse(raw)
			opened <- u
			return errors.New("no browser in tests")
		},
	}
	go func() {
		l, err := s.Run(ctx)
		done <- signInResult{l, err}
	}()
	return manual, done, opened
}

func submit(t *testing.T, manual chan<- Callback, raw string) CallbackResult {
	t.Helper()
	reply := make(chan CallbackResult, 1)
	select {
	case manual <- Callback{URL: raw, Reply: reply}:
	case <-time.After(5 * time.Second):
		t.Fatal("Run stopped reading the manual channel")
	}
	select {
	case r := <-reply:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no reply to the manual callback")
		return CallbackResult{}
	}
}

// 服务器场景：浏览器回调到不了本进程，把完整回调 URL 贴进来完成登录。
func TestSignInManualCallback(t *testing.T) {
	var tokenBody map[string]string
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case r.URL.Path == "/auth/oauth/token":
			_ = json.Unmarshal(body, &tokenBody)
			writeJSON(w, 200, map[string]any{"accessToken": "acc", "refreshToken": "ref", "expiresIn": 1200,
				"profileArn": "arn:aws:codewhisperer:us-east-1:1:profile/P"})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			usageOK(w)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	manual, done, opened := startSignInManual(t, t.Context(), c)
	state := (<-opened).Query().Get("state")

	// 完整 URL
	res := submit(t, manual, "http://localhost:3128/oauth/callback?login_option=github&code=the-code&state="+url.QueryEscape(state))
	if !res.Done || res.Err != nil || res.Login.Cred.AccessToken != "acc" {
		t.Fatalf("manual callback = %+v", res)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.login.Email != "me@example.com" {
		t.Errorf("login = %+v", r.login)
	}
	if tokenBody["code"] != "the-code" || !strings.HasPrefix(tokenBody["redirect_uri"], "http://localhost:") {
		t.Errorf("token body = %v", tokenBody)
	}
}

// 只粘贴 ? 后的查询串也可以。
func TestSignInManualCallbackQueryOnly(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.URL.Path == "/auth/oauth/token":
			writeJSON(w, 200, map[string]any{"accessToken": "acc", "refreshToken": "ref", "expiresIn": 60})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			usageOK(w)
		}
	})
	manual, done, opened := startSignInManual(t, t.Context(), c)
	state := (<-opened).Query().Get("state")

	res := submit(t, manual, "code=abc&login_option=google&state="+url.QueryEscape(state))
	if !res.Done || res.Err != nil {
		t.Fatalf("query-only callback = %+v", res)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
}

// 不属于本次登录的 state：不结束登录，返回可重试错误，之后仍能贴对的那条。
func TestSignInManualForeignStateThenRetry(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.URL.Path == "/auth/oauth/token":
			writeJSON(w, 200, map[string]any{"accessToken": "acc", "refreshToken": "ref", "expiresIn": 60})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			usageOK(w)
		}
	})
	manual, done, opened := startSignInManual(t, t.Context(), c)
	state := (<-opened).Query().Get("state")

	bad := submit(t, manual, "http://localhost:3128/oauth/callback?login_option=github&code=x&state=someone-else")
	if bad.Err == nil || bad.Done {
		t.Fatalf("foreign callback = %+v", bad)
	}
	select {
	case r := <-done:
		t.Fatalf("Run ended on a foreign callback: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	res := submit(t, manual, "http://localhost:3128/oauth/callback?login_option=github&code=ok&state="+url.QueryEscape(state))
	if !res.Done || res.Err != nil {
		t.Fatalf("retry = %+v", res)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
}

// Builder ID：第一次回调让浏览器去 AWS，返回 next_url；贴错地址报错。
func TestSignInManualBuilderIDNextURL(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.URL.Path == "/oidc/us-east-1/client/register":
			writeJSON(w, 200, map[string]any{"clientId": "cid", "clientSecret": "csec"})
		case r.URL.Path == "/oidc/us-east-1/token":
			writeJSON(w, 200, map[string]any{"accessToken": "idc-acc", "refreshToken": "idc-ref", "expiresIn": 3600})
		case strings.HasSuffix(r.URL.Path, "/Get-Usage-Limits"):
			usageOK(w)
		}
	})
	manual, done, opened := startSignInManual(t, t.Context(), c)
	state := (<-opened).Query().Get("state")

	next := submit(t, manual, "http://localhost:3128/oauth/callback?state="+url.QueryEscape(state)+
		"&login_option=builderid&issuer_url="+url.QueryEscape("https://view.awsapps.com/start")+"&idc_region=us-east-1")
	if next.Err != nil || next.Done || !strings.Contains(next.NextURL, "/oidc/us-east-1/authorize?") || !strings.Contains(next.NextURL, "client_id=cid") {
		t.Fatalf("builderid first callback = %+v", next)
	}
	// 解析出 AWS 的 state，模拟从 AWS 回来再贴一次
	u, err := url.Parse(next.NextURL)
	if err != nil {
		t.Fatal(err)
	}
	awsState := u.Query().Get("state")
	final := submit(t, manual, "http://127.0.0.1:3128/oauth/callback?code=aws-code&state="+url.QueryEscape(awsState))
	if !final.Done || final.Err != nil || final.Login.Cred.Method != MethodIDC {
		t.Fatalf("builderid final callback = %+v", final)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
}

func TestParseCallbackURL(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantQ   string
		wantErr bool
	}{
		{"http://localhost:3128/oauth/callback?code=a&state=b", "code=a&state=b", false},
		// 实际服务器登录时用户从地址栏复制的那条：端口 3128 + github
		{
			"http://localhost:3128/oauth/callback?login_option=github&code=512e26bb-fe9d-4bcd-8ae3-ee513ddd014d&state=8xcOHhHWD2AHh1mjxqW-_bXzpU3-GOxb",
			"code=512e26bb-fe9d-4bcd-8ae3-ee513ddd014d&login_option=github&state=8xcOHhHWD2AHh1mjxqW-_bXzpU3-GOxb",
			false,
		},
		{"?code=a&state=b", "code=a&state=b", false},
		{"code=a&state=b", "code=a&state=b", false},
		{"http://localhost:3128/oauth/callback?error=access_denied", "error=access_denied", false},
		{"", "", true},
		{"not a url", "", true},
		{"http://localhost:3128/oauth/callback", "", true},
	} {
		u, err := parseCallbackURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCallbackURL(%q) = %v, want error", tc.in, u)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCallbackURL(%q): %v", tc.in, err)
			continue
		}
		if u.Query().Encode() != tc.wantQ {
			t.Errorf("parseCallbackURL(%q) query = %q, want %q", tc.in, u.Query().Encode(), tc.wantQ)
		}
	}
}

// 手工通道关闭后，登录协程不 panic，仍正常等人。
func TestSignInManualChannelClosed(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {})
	manual := make(chan Callback)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan signInResult, 1)
	s := &SignIn{Client: c, Ports: []int{0}, Timeout: time.Minute, PortalURL: "https://portal.test/",
		Manual: manual, Open: func(string) error { return nil }}
	go func() {
		l, err := s.Run(ctx)
		done <- signInResult{l, err}
	}()
	time.Sleep(50 * time.Millisecond)
	close(manual)
	select {
	case r := <-done:
		t.Fatalf("Run returned from a closed manual channel: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	if r := <-done; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", r.err)
	}
}
