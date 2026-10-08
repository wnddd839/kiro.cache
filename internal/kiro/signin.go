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
	// Manual 是手工提交回调的入口。服务器上浏览器被 Kiro 重定向回 localhost 时到不了本进程，
	// 用户可以把地址栏里的完整回调 URL 贴进来。非 nil 时 Run 同时处理它。
	Manual <-chan Callback
}

// Callback 是一次手工提交的回调。URL 是浏览器地址栏里的完整地址（也可只给 ? 后的查询串）。
// Reply 必须能收一个值；Run 在 nil 与非 nil 之外总会有答复，不会阻塞调用方。
type Callback struct {
	URL   string
	Reply chan<- CallbackResult
}

// CallbackResult 是一次手工回调的结果。
//   - Done 为真：登录成功，Login 是结果（Run 随即返回，调用方负责入池）；
//   - NextURL 非空：还需要打开它继续（Builder ID / IdC 转 AWS 登录）；
//   - Err 非空：这次回调无效，登录仍在等待，可以再贴一次。
type CallbackResult struct {
	Done    bool
	Login   Login
	NextURL string
	Err     error
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

// callbackState 是一次登录期间处理回调需要的状态：state / PKCE / 回调地址，以及
// 从 Kiro 转 AWS 后那一段。
type callbackState struct {
	client      *Client
	state       string
	verifier    string
	redirect    string // http://localhost:<port>
	awsRedirect string // http://127.0.0.1:<port>/oauth/callback
	aws         awsSignIn
}

// outcome 是一次回调处理的结果。nextURL 非空表示还要打开它继续（AWS 登录）。
type outcome struct {
	login   Login
	nextURL string
	err     error
}

// errForeignCallback 是这个回调不属于本次登录（别人的页面 / 旧标签页 / 贴错 URL）。
// 不是失败：登录应继续等待正确的回调。
var errForeignCallback = errors.New("kiro: callback from a different sign-in")

// handle 处理一次回调（本地监听收到的，或手工提交的）。
func (st *callbackState) handle(ctx context.Context, u *url.URL) outcome {
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return outcome{err: errors.New(cmp.Or(q.Get("error_description"), e))}
	}
	var cred Cred
	switch {
	case st.aws.state != "" && q.Get("state") == st.aws.state:
		// 从 AWS 回来
		code := q.Get("code")
		if code == "" {
			return outcome{err: errors.New("AWS sent back no code")}
		}
		t, err := signInToken(ctx, st.client, st.client.OIDCURL(st.aws.region)+"/token", map[string]string{
			"clientId": st.aws.clientID, "clientSecret": st.aws.clientSecret, "grantType": "authorization_code",
			"redirectUri": st.awsRedirect, "code": code, "codeVerifier": st.aws.verifier,
		}, "AWS")
		if err != nil {
			return outcome{err: err}
		}
		cred = Cred{Method: MethodIDC, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: expiry(t.ExpiresIn),
			Region: st.aws.region, ClientID: st.aws.clientID, ClientSecret: st.aws.clientSecret, ClientSecretExpiresAt: st.aws.clientExpires,
			BuilderID: strings.EqualFold(st.aws.provider, "BuilderId")}
	case q.Get("state") != st.state:
		return outcome{err: errForeignCallback}
	default:
		switch opt := q.Get("login_option"); opt {
		case "google", "github":
			path := u.Path
			if path == "" {
				path = "/oauth/callback"
			}
			t, err := signInToken(ctx, st.client, st.client.AuthService+"/oauth/token", map[string]string{
				"code": q.Get("code"), "code_verifier": st.verifier,
				"redirect_uri": st.redirect + path + "?login_option=" + opt,
			}, "Kiro")
			if err != nil {
				return outcome{err: err}
			}
			cred = Cred{Method: MethodSocial, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: expiry(t.ExpiresIn),
				Region: "us-east-1", ProfileArn: t.ProfileArn}
		case "builderid", "awsidc", "internal":
			// 去 AWS，用为这次登录注册的 client
			issuer, region := q.Get("issuer_url"), q.Get("idc_region")
			if issuer == "" || region == "" {
				return outcome{err: errors.New("Kiro's page didn't say where to sign in at AWS")}
			}
			var reg struct {
				ClientID     string `json:"clientId"`
				ClientSecret string `json:"clientSecret"`
				ExpiresAt    int64  `json:"clientSecretExpiresAt"` // Unix 秒
			}
			err := signInPost(ctx, st.client, st.client.OIDCURL(region)+"/client/register", map[string]any{
				"clientName": "Kiro IDE", "clientType": "public", "scopes": signInScopes,
				"grantTypes": []string{"authorization_code", "refresh_token"}, "redirectUris": []string{"http://127.0.0.1/oauth/callback"},
				"issuerUrl": issuer,
			}, &reg)
			if err == nil && reg.ClientID == "" {
				err = errors.New("AWS registered no client")
			}
			if err != nil {
				return outcome{err: err}
			}
			st.aws = awsSignIn{region: region, clientID: reg.ClientID, clientSecret: reg.ClientSecret, clientExpires: unixOrZero(reg.ExpiresAt),
				provider: map[string]string{"builderid": "BuilderId", "awsidc": "Enterprise", "internal": "Internal"}[opt],
				state:    randURL(24), verifier: randURL(48)}
			a := url.Values{"response_type": {"code"}, "client_id": {st.aws.clientID}, "redirect_uri": {st.awsRedirect},
				"scopes": {strings.Join(signInScopes, ",")}, "state": {st.aws.state},
				"code_challenge": {challenge(st.aws.verifier)}, "code_challenge_method": {"S256"}}
			return outcome{nextURL: st.client.OIDCURL(region) + "/authorize?" + a.Encode()}
		case "external_idp":
			return outcome{err: errors.New("a company's own identity provider can't be signed in to here yet; sign in with the Kiro IDE and use import-ide")}
		default:
			return outcome{err: fmt.Errorf("Kiro's page came back with a sign-in kiro-proxy doesn't know (%q)", opt)}
		}
	}
	return outcome{login: whoIs(ctx, st.client, cred)}
}

// Run 监听回调端口、打开登录页，等浏览器回来。ctx 取消或超时则放弃；结束时关闭监听。
// 服务器上浏览器回调到不了本进程时，可同时用 s.Manual 手工提交回调 URL。
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
	redirect := "http://localhost:" + strconv.Itoa(port)
	st := &callbackState{
		client:      c,
		state:       randURL(24),
		verifier:    randURL(48),
		redirect:    redirect,
		awsRedirect: "http://127.0.0.1:" + strconv.Itoa(port) + "/oauth/callback",
	}

	ctx, cancel := context.WithTimeoutCause(ctx, cmp.Or(s.Timeout, SignInTimeout), errors.New("kiro: the sign-in timed out"))
	defer cancel()

	var (
		mu      sync.Mutex // 回调逐个处理
		waiting = true
		done    = make(chan signInResult, 1)
	)

	// settle 在锁内处理一次回调（本地或手工），并在真失败 / 成功时结束登录。
	// 返回处理结果；errForeignCallback 不结束登录，可以再贴。
	settle := func(ctx context.Context, u *url.URL) outcome {
		mu.Lock()
		defer mu.Unlock()
		if !waiting {
			return outcome{err: errors.New("kiro: this sign-in is over")}
		}
		out := st.handle(ctx, u)
		switch {
		case errors.Is(out.err, errForeignCallback):
			// 不是本次登录的回调：继续等正确的那个
		case out.err != nil:
			waiting = false
			done <- signInResult{err: errors.New("kiro: sign-in failed: " + strings.TrimPrefix(out.err.Error(), "kiro: "))}
		case out.nextURL == "":
			waiting = false
			done <- signInResult{login: out.login}
		}
		return out
	}

	callback := func(w http.ResponseWriter, r *http.Request) {
		out := settle(r.Context(), r.URL)
		switch {
		case errors.Is(out.err, errForeignCallback):
			writePage(w, false, "This link isn't from this sign-in", "Start it again.")
		case out.err != nil:
			writePage(w, false, "Sign-in didn't finish", strings.TrimPrefix(out.err.Error(), "kiro: "))
		case out.nextURL != "":
			http.Redirect(w, r, out.nextURL, http.StatusFound)
		default:
			writePage(w, true, "You're signed in", cmp.Or(out.login.Email, "Kiro account")+" is signed in. You can close this tab.")
		}
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

	// 手工回调：服务器上浏览器回不到本进程时，用户把回调 URL 贴进来。
	if s.Manual != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case cb, ok := <-s.Manual:
					if !ok {
						return
					}
					reply := cb.Reply
					u, err := parseCallbackURL(cb.URL)
					if err != nil {
						sendCallback(ctx, reply, CallbackResult{Err: err})
						continue
					}
					out := settle(ctx, u)
					res := CallbackResult{Err: out.err}
					switch {
					case errors.Is(out.err, errForeignCallback):
						// 不是本次登录的回调：不结束，允许再贴一次
						res.Err = errors.New("this link isn't from this sign-in; start it again and paste the newest callback URL")
					case out.err == nil && out.nextURL != "":
						res = CallbackResult{NextURL: out.nextURL}
					case out.err == nil:
						res = CallbackResult{Done: true, Login: out.login}
					}
					sendCallback(ctx, reply, res)
				}
			}
		}()
	}

	q := url.Values{"state": {st.state}, "code_challenge": {challenge(st.verifier)}, "code_challenge_method": {"S256"},
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

// sendCallback 给手工回调回一个结果，收方未读或已取消也不阻塞 Run。
func sendCallback(ctx context.Context, reply chan<- CallbackResult, res CallbackResult) {
	if reply == nil {
		return
	}
	select {
	case reply <- res:
	case <-ctx.Done():
	}
}

// parseCallbackURL 解析手工提交的回调地址。允许完整 URL，或只给 ? 后的查询串、
// 乃至裸的 code=…&state=…。
func parseCallbackURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("paste the full callback URL from the browser's address bar")
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		if q.Get("code") == "" && q.Get("state") == "" && q.Get("error") == "" {
			return nil, errors.New("that URL has no callback parameters; paste the full address from the browser's address bar")
		}
		return u, nil
	}
	trimmed := strings.TrimPrefix(raw, "?")
	q, err := url.ParseQuery(trimmed)
	if err != nil || (q.Get("code") == "" && q.Get("state") == "" && q.Get("error") == "") {
		return nil, errors.New("that doesn't look like a callback URL; paste the full address from the browser's address bar")
	}
	return &url.URL{Path: "/oauth/callback", RawQuery: trimmed}, nil
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
