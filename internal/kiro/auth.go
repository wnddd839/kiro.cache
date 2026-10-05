package kiro

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Method 是账号的登录方式，决定 refresh 怎么走。
type Method string

const (
	MethodSocial      Method = "social"       // Google / GitHub，走 Kiro auth service
	MethodIDC         Method = "idc"          // Builder ID / IAM Identity Center，走 AWS OIDC
	MethodExternalIdP Method = "external-idp" // 公司 IdP
	MethodAPIKey      Method = "apikey"       // ksk_…
)

// BuilderIDProfile 是 Builder ID 登录使用的服务 profile（它没有自己的 profile）。
const BuilderIDProfile = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"

const desktopUA = "Kiro-Desktop/0.2.13 (darwin; arm64)"

// Cred 是一个账号的凭证。
type Cred struct {
	Method       Method    `json:"method"`
	AccessToken  string    `json:"access_token,omitzero"`
	RefreshToken string    `json:"refresh_token,omitzero"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Region       string    `json:"region,omitzero"` // 登录所在区（OIDC / refresh 用）
	ProfileArn   string    `json:"profile_arn,omitzero"`
	ClientID     string    `json:"client_id,omitzero"`
	ClientSecret string    `json:"client_secret,omitzero"`
	TokenURL     string    `json:"token_url,omitzero"` // external-idp
	BuilderID    bool      `json:"builder_id,omitzero"`
}

// Fresh 报告 token 距过期是否还有超过 lead 的时间。API key 与未知过期视为新鲜。
func (c Cred) Fresh(lead time.Duration) bool {
	return c.Method == MethodAPIKey || c.ExpiresAt.IsZero() || time.Until(c.ExpiresAt) > lead
}

// TokenType 是 tokentype 头的值。
func (c Cred) TokenType() string {
	switch c.Method {
	case MethodAPIKey:
		return "API_KEY"
	case MethodExternalIdP:
		return "EXTERNAL_IDP"
	}
	return ""
}

// APIRegion 是 runtime / management API 所在区：profile ARN 里的区，否则按登录区就近。
func (c Cred) APIRegion() string {
	if parts := strings.Split(c.ProfileArn, ":"); len(parts) > 4 && parts[3] != "" {
		return parts[3]
	}
	if strings.HasPrefix(c.Region, "eu-") {
		return "eu-central-1"
	}
	return "us-east-1"
}

func (c Cred) setAuth(h http.Header) {
	h.Set("Authorization", "Bearer "+c.AccessToken)
	if t := c.TokenType(); t != "" {
		h.Set("tokentype", t)
	}
}

// Error 是 Kiro 的失败。Gone 表示凭证本身失效（refresh 被拒 / 没有 refresh token）。
type Error struct {
	Status  int
	Message string
	Body    string
	Gone    bool
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("kiro: %s (%d)", e.Message, e.Status)
	}
	return "kiro: " + e.Message
}

// IsGone 报告 err 是否意味着账号需要重新登录。
func IsGone(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Gone
}

// Client 访问 Kiro 的 auth / runtime / management。端点可换，测试用本地服务替代。
type Client struct {
	HTTP          *http.Client
	RuntimeURL    func(region string) string
	ManagementURL func(region string) string
	RefreshURL    func(region string) string
	OIDCURL       func(region string) string
	AuthService   string
}

// NewClient 用生产端点建客户端。h 为 nil 时用不带总超时的默认客户端（流式响应可能很长）。
func NewClient(h *http.Client) *Client {
	if h == nil {
		h = &http.Client{}
	}
	return &Client{
		HTTP:          h,
		RuntimeURL:    func(r string) string { return "https://runtime." + r + ".kiro.dev" },
		ManagementURL: func(r string) string { return "https://management." + r + ".kiro.dev" },
		RefreshURL:    func(r string) string { return "https://prod." + r + ".auth.desktop.kiro.dev/refreshToken" },
		OIDCURL:       func(r string) string { return "https://oidc." + r + ".amazonaws.com" },
		AuthService:   "https://prod.us-east-1.auth.desktop.kiro.dev",
	}
}

// Refresh 用 refresh token 换新 access token。返回的 Cred 可能带新的 refresh token，调用方必须持久化。
func (c *Client) Refresh(ctx context.Context, cred Cred) (Cred, error) {
	if cred.Method == MethodAPIKey {
		return cred, &Error{Message: "API key cannot be refreshed", Gone: true}
	}
	if cred.RefreshToken == "" {
		return cred, &Error{Message: "no refresh token; sign in again", Gone: true}
	}
	var out struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
		// external-idp 是标准 OAuth 字段
		OAuthAccess  string `json:"access_token"`
		OAuthRefresh string `json:"refresh_token"`
		OAuthExpires int    `json:"expires_in"`
	}
	region := cmp.Or(cred.Region, "us-east-1")
	var err error
	switch cred.Method {
	case MethodSocial, "":
		err = c.postJSON(ctx, c.RefreshURL(region), map[string]string{"refreshToken": cred.RefreshToken},
			map[string]string{"User-Agent": desktopUA}, &out)
	case MethodIDC:
		err = c.postJSON(ctx, c.OIDCURL(region)+"/token", map[string]string{
			"clientId": cred.ClientID, "clientSecret": cred.ClientSecret,
			"refreshToken": cred.RefreshToken, "grantType": "refresh_token",
		}, nil, &out)
	case MethodExternalIdP:
		if cred.TokenURL == "" {
			return cred, &Error{Message: "external IdP has no token URL", Gone: true}
		}
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {cred.ClientID}, "refresh_token": {cred.RefreshToken}}
		err = c.do(ctx, http.MethodPost, cred.TokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil, &out)
	default:
		return cred, &Error{Message: "unknown sign-in method " + string(cred.Method), Gone: true}
	}
	if err != nil {
		if e, ok := errors.AsType[*Error](err); ok && (e.Status == 400 || e.Status == 401 || e.Status == 403) {
			e.Gone = true
		}
		return cred, fmt.Errorf("refresh: %w", err)
	}
	access := cmp.Or(out.AccessToken, out.OAuthAccess)
	if access == "" {
		return cred, &Error{Message: "refresh returned no access token", Gone: true}
	}
	n := cred
	n.AccessToken = access
	if r := cmp.Or(out.RefreshToken, out.OAuthRefresh); r != "" {
		n.RefreshToken = r
	}
	expires := cmp.Or(out.ExpiresIn, out.OAuthExpires, 3600)
	n.ExpiresAt = time.Now().Add(time.Duration(expires) * time.Second)
	if out.ProfileArn != "" {
		n.ProfileArn = out.ProfileArn
	}
	return n, nil
}

// Profile 找出一个没有标明 profile 的登录该用的 profile ARN。
func (c *Client) Profile(ctx context.Context, cred Cred) (string, error) {
	if cred.Method == MethodAPIKey {
		var out struct {
			Profile struct {
				Arn string `json:"arn"`
			} `json:"profile"`
		}
		h := http.Header{}
		cred.setAuth(h)
		h.Set("X-Amz-Target", "AmazonCodeWhispererService.GetProfile")
		if err := c.do(ctx, http.MethodPost, c.ManagementURL("us-east-1")+"/", "application/x-amz-json-1.0", strings.NewReader("{}"), h, &out); err != nil {
			return "", err
		}
		if out.Profile.Arn == "" {
			return "", &Error{Message: "no profile for this API key"}
		}
		return out.Profile.Arn, nil
	}
	if cred.BuilderID {
		return BuilderIDProfile, nil
	}
	var last error
	for _, region := range []string{"us-east-1", "eu-central-1"} {
		var out struct {
			Profiles []struct {
				Arn string `json:"arn"`
			} `json:"profiles"`
		}
		h := http.Header{}
		cred.setAuth(h)
		err := c.do(ctx, http.MethodPost, c.ManagementURL(region)+"/List-Available-Profiles", "application/json", strings.NewReader("{}"), h, &out)
		if err == nil {
			for _, p := range out.Profiles {
				if p.Arn != "" {
					return p.Arn, nil
				}
			}
			continue
		}
		if e, ok := errors.AsType[*Error](err); ok && cred.Method == MethodIDC && e.Status == 403 && strings.Contains(e.Body, "Builder ID") {
			return BuilderIDProfile, nil
		}
		last = err
	}
	return "", cmp.Or(last, error(&Error{Message: "no profile for this sign-in"}))
}

// Model 是账号可用的一个模型。
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Output  int    `json:"output"`
	Images  bool   `json:"images"`
}

// ListModels 是 List-Available-Models，默认模型排第一。
func (c *Client) ListModels(ctx context.Context, cred Cred) ([]Model, error) {
	var out struct {
		Models []struct {
			ModelID     string `json:"modelId"`
			ModelName   string `json:"modelName"`
			TokenLimits struct {
				MaxInput  int `json:"maxInputTokens"`
				MaxOutput int `json:"maxOutputTokens"`
			} `json:"tokenLimits"`
			SupportedInputTypes []string `json:"supportedInputTypes"`
		} `json:"models"`
		DefaultModel struct {
			ModelID string `json:"modelId"`
		} `json:"defaultModel"`
	}
	q := url.Values{"origin": {"KIRO_CLI"}}
	if cred.ProfileArn != "" {
		q.Set("profileArn", cred.ProfileArn)
	}
	if err := c.management(ctx, cred, "List-Available-Models", q, &out); err != nil {
		return nil, err
	}
	var list []Model
	for _, m := range out.Models {
		if m.ModelID == "" {
			continue
		}
		model := Model{ID: m.ModelID, Name: cmp.Or(m.ModelName, m.ModelID), Context: m.TokenLimits.MaxInput, Output: m.TokenLimits.MaxOutput}
		for _, t := range m.SupportedInputTypes {
			if strings.EqualFold(t, "IMAGE") {
				model.Images = true
			}
		}
		if m.ModelID == out.DefaultModel.ModelID {
			list = append([]Model{model}, list...)
		} else {
			list = append(list, model)
		}
	}
	if len(list) == 0 {
		return nil, &Error{Message: "no models listed"}
	}
	return list, nil
}

// Limits 是 Get-Usage-Limits 的摘要。
type Limits struct {
	Email   string    `json:"email,omitzero"`
	Plan    string    `json:"plan,omitzero"`
	Used    float64   `json:"used"`
	Limit   float64   `json:"limit"`
	ResetAt time.Time `json:"reset_at,omitzero"`
}

// Remaining 是剩余额度比例 [0,1]；没有上限信息时返回 1。
func (l Limits) Remaining() float64 {
	if l.Limit <= 0 {
		return 1
	}
	return max(0, 1-l.Used/l.Limit)
}

// UsageLimits 是 Get-Usage-Limits：邮箱、套餐、credits。
func (c *Client) UsageLimits(ctx context.Context, cred Cred) (Limits, error) {
	var out struct {
		NextDateReset      float64 `json:"nextDateReset"`
		UsageBreakdownList []struct {
			Used          float64 `json:"currentUsageWithPrecision"`
			Limit         float64 `json:"usageLimitWithPrecision"`
			NextDateReset float64 `json:"nextDateReset"`
			FreeTrialInfo *struct {
				Status string  `json:"freeTrialStatus"`
				Used   float64 `json:"currentUsageWithPrecision"`
				Limit  float64 `json:"usageLimitWithPrecision"`
			} `json:"freeTrialInfo"`
		} `json:"usageBreakdownList"`
		SubscriptionInfo struct {
			Title string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		UserInfo struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	}
	q := url.Values{"origin": {"KIRO_CLI"}, "resourceType": {"CREDIT"}, "isEmailRequired": {"true"}}
	if cred.ProfileArn != "" {
		q.Set("profileArn", cred.ProfileArn)
	}
	if err := c.management(ctx, cred, "Get-Usage-Limits", q, &out); err != nil {
		return Limits{}, err
	}
	l := Limits{Email: out.UserInfo.Email, Plan: planName(out.SubscriptionInfo.Title)}
	reset := out.NextDateReset
	for _, u := range out.UsageBreakdownList {
		if t := u.FreeTrialInfo; t != nil && strings.EqualFold(t.Status, "ACTIVE") && t.Limit > 0 {
			l.Used += t.Used
			l.Limit += t.Limit
		}
		if u.Limit > 0 {
			l.Used += u.Used
			l.Limit += u.Limit
			reset = cmp.Or(u.NextDateReset, reset)
		}
	}
	if reset > 0 {
		l.ResetAt = time.Unix(int64(reset), 0)
	}
	return l, nil
}

func planName(title string) string {
	words := strings.Fields(strings.ToLower(title))
	for i, w := range words {
		if w == "kiro" {
			words[i] = "Kiro"
		} else {
			r, n := utf8.DecodeRuneInString(w)
			words[i] = string(unicode.ToUpper(r)) + w[n:]
		}
	}
	return strings.Join(words, " ")
}

// Generate 发一次 generateAssistantResponse。调用方负责关闭 Body。
func (c *Client) Generate(ctx context.Context, cred Cred, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RuntimeURL(cred.APIRegion())+"/generateAssistantResponse", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	cred.setAuth(req.Header)
	ua := "aws-sdk-rust/1.0.0 ua/2.1 os/other lang/rust api/codewhispererstreaming#1.28.3 m/E app/AmazonQ-For-CLI md/appVersion-1.28.3-" +
		strings.ReplaceAll(NewUUID(), "-", "")
	for k, v := range map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/vnd.amazon.eventstream",
		"x-amzn-codewhisperer-optout": "true",
		"amz-sdk-invocation-id":       NewUUID(),
		"amz-sdk-request":             "attempt=1; max=1",
		"x-amzn-kiro-agent-mode":      "vibe",
		"x-amz-user-agent":            ua,
		"User-Agent":                  ua,
	} {
		req.Header.Set(k, v)
	}
	return c.HTTP.Do(req)
}

func (c *Client) management(ctx context.Context, cred Cred, method string, q url.Values, out any) error {
	h := http.Header{}
	cred.setAuth(h)
	u := c.ManagementURL(cred.APIRegion()) + "/" + method + "?" + q.Encode()
	return c.do(ctx, http.MethodGet, u, "", nil, h, out)
}

func (c *Client) postJSON(ctx context.Context, u string, in any, headers map[string]string, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return c.do(ctx, http.MethodPost, u, "application/json", bytes.NewReader(b), h, out)
}

// do 发一个小请求（管理 / 鉴权），带 30s 超时，把 JSON 响应解到 out。
func (c *Client) do(ctx context.Context, method, u, contentType string, body io.Reader, h http.Header, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		return &Error{Status: res.StatusCode, Message: errorMessage(res.StatusCode, raw), Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func errorMessage(status int, body []byte) string {
	var e struct {
		Message          string `json:"message"`
		Reason           string `json:"reason"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil {
		if m := cmp.Or(e.Message, e.ErrorDescription, e.Error); m != "" {
			msg = m
			if e.Reason != "" {
				msg += " (" + e.Reason + ")"
			}
		}
	}
	return cmp.Or(msg, http.StatusText(status))
}
