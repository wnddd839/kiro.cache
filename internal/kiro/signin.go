package kiro

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PortalURL 是 Kiro 的登录页。
const PortalURL = "https://app.kiro.dev"

// CallbackPorts 是 Kiro 登录页会把浏览器送回的端口（IDE 监听的那些）。
var CallbackPorts = []int{3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153}

// SignInTimeout 是浏览器登录的默认时限。
const SignInTimeout = 10 * time.Minute

// signInScopes 是 IDE 向 AWS 要的权限。
var signInScopes = []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations",
	"codewhisperer:transformations", "codewhisperer:taskassist"}

// ideUA 是 IDE 向 Kiro auth service 报的身份。
var ideUA = sync.OnceValue(func() string {
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte("magpie:" + host))
	return "KiroIDE-1.1.70-" + hex.EncodeToString(sum[:])
})

// SignIn 是 IDE 式的浏览器登录：打开 Kiro 登录页，浏览器回到本机端口。
// Google / GitHub 带回一个 code 由 Kiro auth service 换 token；Builder ID /
// IAM Identity Center 带回去 AWS 登录的地址，再用为此注册的 client 走一遍 OAuth。
type SignIn struct {
	Client    *Client
	Ports     []int                  // 依次尝试的回调端口；nil 用 CallbackPorts。0 = 任意空闲端口（测试用）
	Open      func(url string) error // 打开浏览器；nil 用 OpenBrowser。失败不致命，会打印 URL
	Timeout   time.Duration          // 0 = SignInTimeout
	PortalURL string                 // "" = PortalURL
}

// Login 是一次成功的浏览器登录。Email / Plan 尽力获取，可能为空。
type Login struct {
	Cred  Cred
	Email string
	Plan  string
}

type awsSignIn struct {
	region, clientID, clientSecret, provider, state, verifier string
	clientExpires                                             time.Time
}

type signInResult struct {
	login Login
	err   error
}

// Run 监听回调端口、打开登录页，等浏览器回来。ctx 取消或超时则放弃；结束时关闭监听。
func (s *SignIn) Run(ctx context.Context) (Login, error) {
	c := cmp.Or(s.Client, NewClient(nil))
	ports := s.Ports
	if len(ports) == 0 {
		ports = CallbackPorts
	}
	ln, err := listenCallback(ports)
	if err != nil {
		return Login{}, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state, verifier := randURL(24), randURL(48)
	redirect := "http://localhost:" + strconv.Itoa(port)
	awsRedirect := "http://127.0.0.1:" + strconv.Itoa(port) + "/oauth/callback"

	ctx, cancel := context.WithTimeoutCause(ctx, cmp.Or(s.Timeout, SignInTimeout), errors.New("kiro: the sign-in timed out"))
	defer cancel()

	var (
		mu      sync.Mutex // 回调逐个处理
		waiting = true
		aws     awsSignIn // Kiro 的页面把浏览器送去 AWS 之后才有
		done    = make(chan signInResult, 1)
	)
	finish := func(r signInResult) {
		if waiting {
			waiting = false
			done <- r
		}
	}

	callback := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		if !waiting {
			writePage(w, false, "This sign-in is over", "Start it again.")
			return
		}
		fail := func(msg string) {
			finish(signInResult{err: errors.New("kiro: sign-in failed: " + msg)})
			writePage(w, false, "Sign-in didn't finish", msg)
		}
		if e := q.Get("error"); e != "" {
			fail(cmp.Or(q.Get("error_description"), e))
			return
		}
		var cred Cred
		switch {
		case aws.state != "" && q.Get("state") == aws.state:
			// 从 AWS 回来
			code := q.Get("code")
			if code == "" {
				fail("AWS sent back no code")
				return
			}
			t, err := signInToken(r.Context(), c, c.OIDCURL(aws.region)+"/token", map[string]string{
				"clientId": aws.clientID, "clientSecret": aws.clientSecret, "grantType": "authorization_code",
				"redirectUri": awsRedirect, "code": code, "codeVerifier": aws.verifier,
			}, "AWS")
			if err != nil {
				fail(err.Error())
				return
			}
			cred = Cred{Method: MethodIDC, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: expiry(t.ExpiresIn),
				Region: aws.region, ClientID: aws.clientID, ClientSecret: aws.clientSecret, ClientSecretExpiresAt: aws.clientExpires,
				BuilderID: strings.EqualFold(aws.provider, "BuilderId")}
		case q.Get("state") != state:
			// 不是这次登录的：别人的页面，或旧标签页
			writePage(w, false, "This link isn't from this sign-in", "Start it again.")
			return
		default:
			switch opt := q.Get("login_option"); opt {
			case "google", "github":
				t, err := signInToken(r.Context(), c, c.AuthService+"/oauth/token", map[string]string{
					"code": q.Get("code"), "code_verifier": verifier,
					"redirect_uri": redirect + r.URL.Path + "?login_option=" + opt,
				}, "Kiro")
				if err != nil {
					fail(err.Error())
					return
				}
				cred = Cred{Method: MethodSocial, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: expiry(t.ExpiresIn),
					Region: "us-east-1", ProfileArn: t.ProfileArn}
			case "builderid", "awsidc", "internal":
				// 去 AWS，用为这次登录注册的 client
				issuer, region := q.Get("issuer_url"), q.Get("idc_region")
				if issuer == "" || region == "" {
					fail("Kiro's page didn't say where to sign in at AWS")
					return
				}
				var reg struct {
					ClientID     string `json:"clientId"`
					ClientSecret string `json:"clientSecret"`
					ExpiresAt    int64  `json:"clientSecretExpiresAt"` // Unix 秒
				}
				err := signInPost(r.Context(), c, c.OIDCURL(region)+"/client/register", map[string]any{
					"clientName": "Kiro IDE", "clientType": "public", "scopes": signInScopes,
					"grantTypes": []string{"authorization_code", "refresh_token"}, "redirectUris": []string{"http://127.0.0.1/oauth/callback"},
					"issuerUrl": issuer,
				}, &reg)
				if err == nil && reg.ClientID == "" {
					err = errors.New("AWS registered no client")
				}
				if err != nil {
					fail(err.Error())
					return
				}
				aws = awsSignIn{region: region, clientID: reg.ClientID, clientSecret: reg.ClientSecret, clientExpires: unixOrZero(reg.ExpiresAt),
					provider: map[string]string{"builderid": "BuilderId", "awsidc": "Enterprise", "internal": "Internal"}[opt],
					state:    randURL(24), verifier: randURL(48)}
				a := url.Values{"response_type": {"code"}, "client_id": {aws.clientID}, "redirect_uri": {awsRedirect},
					"scopes": {strings.Join(signInScopes, ",")}, "state": {aws.state},
					"code_challenge": {challenge(aws.verifier)}, "code_challenge_method": {"S256"}}
				http.Redirect(w, r, c.OIDCURL(region)+"/authorize?"+a.Encode(), http.StatusFound)
				return
			case "external_idp":
				fail("A company's own identity provider can't be signed in to here yet; sign in with the Kiro IDE and use import-ide")
				return
			default:
				fail(fmt.Sprintf("Kiro's page came back with a sign-in kiro-proxy doesn't know (%q)", opt))
				return
			}
		}
		login := whoIs(r.Context(), c, cred)
		finish(signInResult{login: login})
		writePage(w, true, "You're signed in", cmp.Or(login.Email, "Kiro account")+" is signed in. You can close this tab.")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/callback", callback)
	mux.HandleFunc("GET /signin/callback", callback)
	srv := &http.Server{Handler: mux, BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // Shutdown 后返回 ErrServerClosed
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx) // 等正在写的结果页写完，并关闭监听
	}()

	q := url.Values{"state": {state}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"redirect_uri": {redirect}, "redirect_from": {"KiroIDE"}}
	portal := strings.TrimSuffix(cmp.Or(s.PortalURL, PortalURL), "/") + "/signin?" + q.Encode()
	open := s.Open
	if open == nil {
		open = OpenBrowser
	}
	if err := open(portal); err != nil {
		fmt.Fprintf(os.Stderr, "couldn't open a browser (%v); open this URL to sign in:\n%s\n", err, portal)
	}

	select {
	case r := <-done:
		return r.login, r.err
	case <-ctx.Done():
		mu.Lock()
		waiting = false
		mu.Unlock()
		select {
		case r := <-done: // 刚好在取消前完成
			return r.login, r.err
		default:
			return Login{}, context.Cause(ctx)
		}
	}
}

// listenCallback 在 127.0.0.1 上监听第一个空闲端口。
func listenCallback(ports []int) (net.Listener, error) {
	for _, p := range ports {
		if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); err == nil {
			return ln, nil
		}
	}
	return nil, errors.New("kiro: the ports Kiro's sign-in comes back to are all busy; close the Kiro IDE's sign-in and try again")
}

type signInTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
	ProfileArn   string `json:"profileArn"`
}

func signInToken(ctx context.Context, c *Client, u string, in any, who string) (signInTokens, error) {
	var t signInTokens
	if err := signInPost(ctx, c, u, in, &t); err != nil {
		return t, err
	}
	if t.AccessToken == "" {
		return t, errors.New(who + " sent back no token")
	}
	return t, nil
}

// signInPost 向 Kiro auth service 或 AWS 登录发 JSON，带 IDE 的 User-Agent。
func signInPost(ctx context.Context, c *Client, u string, in, out any) error {
	err := c.postJSON(ctx, u, in, map[string]string{"User-Agent": ideUA()}, out)
	if e, ok := errors.AsType[*Error](err); ok {
		return fmt.Errorf("the sign-in was refused (%d): %s", e.Status, e.Message)
	}
	return err
}

// whoIs 尽力补上 profile，并取邮箱与套餐。失败不影响登录。
func whoIs(ctx context.Context, c *Client, cred Cred) Login {
	if cred.ProfileArn == "" {
		if arn, err := c.Profile(ctx, cred); err == nil {
			cred.ProfileArn = arn
		}
	}
	l := Login{Cred: cred}
	if lim, err := c.UsageLimits(ctx, cred); err == nil {
		l.Email, l.Plan = lim.Email, lim.Plan
	}
	return l
}

func expiry(seconds int) time.Time {
	if seconds <= 0 {
		seconds = 3600
	}
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

func randURL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func writePage(w http.ResponseWriter, ok bool, title, text string) {
	mark, color := "✕", "#c0392b"
	if ok {
		mark, color = "✓", "#2a9d5c"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	t := html.EscapeString(title)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<style>body{font:15px system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0;color:#222;background:#fafafa}
@media (prefers-color-scheme:dark){body{color:#eee;background:#161616}}main{text-align:center;max-width:28rem;padding:1rem}
.d{font-size:2rem;color:%s}</style>
<main><div class="d">%s</div><h1>%s</h1><p>%s</p></main>`, t, color, mark, t, html.EscapeString(text))
}

// OpenBrowser 用系统默认浏览器打开 u。
func OpenBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// 不经 cmd /c start：它会把 URL 里的 & 当命令分隔符
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // 只为回收进程
	return nil
}

// unixOrZero 把 Unix 秒转成时间；0 是未知。
func unixOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
