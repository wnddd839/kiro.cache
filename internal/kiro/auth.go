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
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
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

// Identity 是对上游报的客户端身份。对话、OIDC 刷新、management 调用都用同一套 CLI 身份（Amazon Q CLI，
// 与 origin=KIRO_CLI 一致）；social 的 refresh 走 Kiro auth service，那里只认 Kiro-Desktop。不混用 IDE 的 UA。
type Identity struct {
	// CLIVersion 是 Amazon Q CLI 的版本（api/codewhispererstreaming#… 与 md/appVersion-…）。
	CLIVersion string `json:"cli_version,omitzero"`
	// DesktopUA 是 social refresh 用的 User-Agent。
	DesktopUA string `json:"desktop_ua,omitzero"`
}

// DefaultIdentity 是现在的默认身份（与之前写死的值一致）。
var DefaultIdentity = Identity{CLIVersion: "1.28.3", DesktopUA: "Kiro-Desktop/0.2.13 (darwin; arm64)"}

func (c *Client) identity() Identity {
	id := c.Identity
	if id.CLIVersion == "" {
		id.CLIVersion = DefaultIdentity.CLIVersion
	}
	if id.DesktopUA == "" {
		id.DesktopUA = DefaultIdentity.DesktopUA
	}
	return id
}

// cliUA 是 CLI 的 User-Agent。machineID 是号的固定机器码（空则不带后缀）。
func (c *Client) cliUA(machineID string) string {
	v := c.identity().CLIVersion
	ua := "aws-sdk-rust/1.0.0 ua/2.1 os/other lang/rust api/codewhispererstreaming#" + v + " m/E app/AmazonQ-For-CLI md/appVersion-" + v
	if machineID != "" {
		ua += "-" + machineID
	}
	return ua
}

// cliHeaders 是 CLI 身份的请求头（User-Agent 与 x-amz-user-agent 相同）。
func (c *Client) cliHeaders(h http.Header, machineID string) {
	ua := c.cliUA(machineID)
	h.Set("User-Agent", ua)
	h.Set("x-amz-user-agent", ua)
}

// NewMachineID 生成一个机器码：32 位十六进制。
func NewMachineID() string { return strings.ReplaceAll(NewUUID(), "-", "") }

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
	// ClientSecretExpiresAt 是 IdC client 注册的过期时间（RegisterClient 的 clientSecretExpiresAt）；零值为未知。
	ClientSecretExpiresAt time.Time `json:"client_secret_expires_at,omitzero"`
	TokenURL              string    `json:"token_url,omitzero"` // external-idp
	// MachineID 是这个号固定的机器码：号池第一次用时生成并持久化，之后不再随机。
	// 导入的来源带了机器码就沿用。空则请求里不带。
	MachineID string `json:"machine_id,omitzero"`
	BuilderID bool   `json:"builder_id,omitzero"`
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

// Error 是 Kiro 的失败。
//
// Gone 表示凭证本身永久失效，应立即停号：refresh 返回 400 + invalid_grant，或根本没法刷新（没有 refresh token、
// 未知登录方式、不允许的 token 地址）。refresh 的其它 400 / 401 / 403（防火墙页、短暂拒绝……）不是 Gone：
// 号池累计失败次数、冷却，连续多次才停。
//
// Reregister 表示 IdC 的 client 注册已过期（clientSecretExpiresAt）：需要重新注册 / 重新登录，
// 不是 refresh token 失效，不直接停号。
type Error struct {
	Status     int
	Message    string
	Body       string
	Gone       bool
	Reregister bool
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("kiro: %s (%d)", e.Message, e.Status)
	}
	return "kiro: " + e.Message
}

// IsGone 报告 err 是否意味着凭证永久失效、账号需要重新登录。
func IsGone(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Gone
}

// IsReregister 报告 err 是否是 IdC client 注册过期。
func IsReregister(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Reregister
}

// invalidGrant 报告一个 refresh 失败是不是「refresh token 已被撤销 / 过期」：400 且返回体里有 invalid_grant。
// 参考 hank9999/kiro.rs（MIT）src/kiro/token_manager.rs 的判定，未移植代码。
func invalidGrant(e *Error) bool {
	return e.Status == http.StatusBadRequest && strings.Contains(e.Body, "invalid_grant")
}

// clientExpired 报告 IdC 的 refresh 失败是不是 client 注册过期。
func clientExpired(cred Cred, e *Error) bool {
	if cred.Method != MethodIDC || e.Status != http.StatusBadRequest {
		return false
	}
	if !cred.ClientSecretExpiresAt.IsZero() && !time.Now().Before(cred.ClientSecretExpiresAt) {
		return true
	}
	return strings.Contains(e.Body, "InvalidClientException") || strings.Contains(e.Body, "invalid_client")
}

// Client 访问 Kiro 的 auth / runtime / management。端点可换，测试用本地服务替代。
type Client struct {
	HTTP          *http.Client
	RuntimeURL    func(region string) string
	ManagementURL func(region string) string
	RefreshURL    func(region string) string
	OIDCURL       func(region string) string
	AuthService   string
	// Identity 是客户端身份；零值用 DefaultIdentity。
	Identity Identity
	// IdPHosts 是 external-idp 允许的 token 端点主机（及其子域名）。空 = DefaultIdPHosts。
	// refresh token 会发到凭证里写的 TokenURL：不校验的话，导入一份恶意凭证就能把它发到任意地址。
	IdPHosts []string
}

// DefaultIdPHosts 是 external-idp 默认允许的 token 端点：Microsoft Entra ID（各云）。
var DefaultIdPHosts = []string{
	"login.microsoftonline.com", "login.microsoftonline.us", "login.microsoft.com",
	"login.partner.microsoftonline.cn", "login.chinacloudapi.cn",
}

// CheckTokenURL 校验 external-idp 的 token 端点：必须是 https、不带用户信息、主机在允许列表里（含子域名）。
func CheckTokenURL(raw string, hosts []string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("token URL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Host == "" {
		return fmt.Errorf("token URL must be https without credentials: %q", raw)
	}
	if len(hosts) == 0 {
		hosts = DefaultIdPHosts
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, a := range hosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && (h == a || strings.HasSuffix(h, "."+a)) {
			return nil
		}
	}
	return fmt.Errorf("token URL host %q is not an allowed identity provider (allowed: %s)", h, strings.Join(hosts, ", "))
}

// IdleConnTimeout 是到上游的空闲连接保留时长。实测（2026-10-07）runtime 的 HTTP/2 连接空闲 100s 仍可复用、
// 150s 已被对端关闭；取 60s 留足余量，少拿已被关掉的连接去发请求。
const IdleConnTimeout = 60 * time.Second

// NewClient 用生产端点建客户端。h 为 nil 时用不带总超时的客户端（流式响应可能很长）。
func NewClient(h *http.Client) *Client {
	if h == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.IdleConnTimeout = IdleConnTimeout
		h = &http.Client{Transport: tr}
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
			map[string]string{"User-Agent": c.identity().DesktopUA}, &out)
	case MethodIDC:
		err = c.postJSON(ctx, c.OIDCURL(region)+"/token", map[string]string{
			"clientId": cred.ClientID, "clientSecret": cred.ClientSecret,
			"refreshToken": cred.RefreshToken, "grantType": "refresh_token",
		}, map[string]string{"User-Agent": c.cliUA(cred.MachineID), "x-amz-user-agent": c.cliUA(cred.MachineID)}, &out)
	case MethodExternalIdP:
		if cred.TokenURL == "" {
			return cred, &Error{Message: "external IdP has no token URL", Gone: true}
		}
		if err := CheckTokenURL(cred.TokenURL, c.IdPHosts); err != nil {
			return cred, &Error{Message: err.Error(), Gone: true} // 不把 refresh token 发到未知地址
		}
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {cred.ClientID}, "refresh_token": {cred.RefreshToken}}
		if cred.ClientSecret != "" {
			form.Set("client_secret", cred.ClientSecret) // 机密客户端：标准 OAuth 要求一起发
		}
		err = c.do(ctx, http.MethodPost, cred.TokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil, &out)
	default:
		return cred, &Error{Message: "unknown sign-in method " + string(cred.Method), Gone: true}
	}
	if err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			switch {
			case clientExpired(cred, e):
				e.Reregister = true // 不是 refresh token 失效：需要重新注册 client
			case invalidGrant(e):
				e.Gone = true
			}
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
		c.cliHeaders(h, cred.MachineID)
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
		c.cliHeaders(h, cred.MachineID)
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
	// Multiplier 是相对 Auto 的 credit 倍率（Kiro rateMultiplier）；0 表示上游没报。
	Multiplier  float64 `json:"multiplier,omitzero"`
	RateUnit    string  `json:"rate_unit,omitzero"`
	Description string  `json:"description,omitzero"`
	// PromptCaching 是上游声明的缓存规则（断点数 / 最小段长）。
	PromptCaching *PromptCaching `json:"prompt_caching,omitzero"`
	Default       bool           `json:"default,omitzero"`
}

// PromptCaching 是 List-Available-Models 里的 promptCaching。
type PromptCaching struct {
	Supported      bool `json:"supported"`
	MaxCheckpoints int  `json:"max_checkpoints,omitzero"`
	MinTokens      int  `json:"min_tokens,omitzero"`
}

// ListModels 是 List-Available-Models，默认模型排第一。
func (c *Client) ListModels(ctx context.Context, cred Cred) ([]Model, error) {
	var out struct {
		Models []struct {
			ModelID     string `json:"modelId"`
			ModelName   string `json:"modelName"`
			Description string `json:"description"`
			TokenLimits struct {
				MaxInput  int `json:"maxInputTokens"`
				MaxOutput int `json:"maxOutputTokens"`
			} `json:"tokenLimits"`
			SupportedInputTypes []string `json:"supportedInputTypes"`
			RateMultiplier      float64  `json:"rateMultiplier"`
			RateUnit            string   `json:"rateUnit"`
			PromptCaching       *struct {
				MaxCheckpoints int  `json:"maximumCacheCheckpointsPerRequest"`
				MinTokens      int  `json:"minimumTokensPerCacheCheckpoint"`
				Supported      bool `json:"supportsPromptCaching"`
			} `json:"promptCaching"`
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
		model := Model{
			ID: m.ModelID, Name: cmp.Or(m.ModelName, m.ModelID), Context: m.TokenLimits.MaxInput, Output: m.TokenLimits.MaxOutput,
			Multiplier: m.RateMultiplier, RateUnit: m.RateUnit, Description: m.Description,
			Default: m.ModelID == out.DefaultModel.ModelID,
		}
		if pc := m.PromptCaching; pc != nil {
			model.PromptCaching = &PromptCaching{Supported: pc.Supported, MaxCheckpoints: pc.MaxCheckpoints, MinTokens: pc.MinTokens}
		}
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

// Limits 是 Get-Usage-Limits 的摘要。Used / Limit 是所有有效额度之和：套餐、有效的试用与奖励。
// 超额（overage）单独记：它不占额度，但也是号上实际扣的 credits，对账要算上。
type Limits struct {
	Email   string    `json:"email,omitzero"`
	Plan    string    `json:"plan,omitzero"`
	Used    float64   `json:"used"`
	Limit   float64   `json:"limit"`
	ResetAt time.Time `json:"reset_at,omitzero"`
	// Bonus 是 Used/Limit 里有效奖励额度的部分
	BonusUsed  float64 `json:"bonus_used,omitzero"`
	BonusLimit float64 `json:"bonus_limit,omitzero"`
	// Overage 是超出额度后按量计费的已用 credits（开启超额时才有）
	Overage float64 `json:"overage,omitzero"`
}

// Consumed 是号上本周期实际扣掉的 credits：额度内已用 + 超额。对账用。
func (l Limits) Consumed() float64 { return l.Used + l.Overage }

// Remaining 是剩余额度比例 [0,1]；没有上限信息时返回 1。
func (l Limits) Remaining() float64 {
	if l.Limit <= 0 {
		return 1
	}
	return max(0, 1-l.Used/l.Limit)
}

// UsageLimits 是 Get-Usage-Limits：邮箱、套餐、credits。
func (c *Client) UsageLimits(ctx context.Context, cred Cred) (Limits, error) {
	l, _, err := c.UsageLimitsRaw(ctx, cred)
	return l, err
}

// UsageLimitsRaw 同 UsageLimits，另外返回原始响应（探测用）。
func (c *Client) UsageLimitsRaw(ctx context.Context, cred Cred) (Limits, []byte, error) {
	var out struct {
		NextDateReset      float64 `json:"nextDateReset"`
		UsageBreakdownList []struct {
			usageAmount
			NextDateReset float64 `json:"nextDateReset"`
			FreeTrialInfo *struct {
				usageAmount
				Status string  `json:"freeTrialStatus"`
				Expiry float64 `json:"freeTrialExpiry"`
			} `json:"freeTrialInfo"`
			Bonuses []struct {
				usageAmount
				Status    string  `json:"status"`
				ExpiresAt float64 `json:"expiresAt"`
			} `json:"bonuses"`
			Overage *struct {
				usageAmount
				Charges float64 `json:"currentOverages"`
			} `json:"overageInfo"`
			Overages     *float64 `json:"currentOverages"`
			OveragesPrec *float64 `json:"currentOveragesWithPrecision"`
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
	var raw json.RawMessage
	if err := c.management(ctx, cred, "Get-Usage-Limits", q, &raw); err != nil {
		return Limits{}, nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Limits{}, raw, err
	}
	l := Limits{Email: out.UserInfo.Email, Plan: planName(out.SubscriptionInfo.Title)}
	reset := out.NextDateReset
	now := time.Now()
	live := func(status string, until float64) bool {
		if strings.EqualFold(status, "EXPIRED") || strings.EqualFold(status, "INACTIVE") {
			return false
		}
		return until <= 0 || time.Unix(int64(until), 0).After(now)
	}
	for _, u := range out.UsageBreakdownList {
		if t := u.FreeTrialInfo; t != nil && (strings.EqualFold(t.Status, "ACTIVE") || t.Status == "" && live("", t.Expiry)) && t.limit() > 0 {
			l.Used += t.used()
			l.Limit += t.limit()
		}
		for _, b := range u.Bonuses {
			if live(b.Status, b.ExpiresAt) && b.limit() > 0 {
				l.Used += b.used()
				l.Limit += b.limit()
				l.BonusUsed += b.used()
				l.BonusLimit += b.limit()
			}
		}
		if u.limit() > 0 {
			l.Used += u.used()
			l.Limit += u.limit()
			reset = cmp.Or(u.NextDateReset, reset)
		}
		switch {
		case u.OveragesPrec != nil:
			l.Overage += *u.OveragesPrec
		case u.Overages != nil:
			l.Overage += *u.Overages
		case u.Overage != nil:
			l.Overage += max(u.Overage.used(), u.Overage.Charges)
		}
	}
	if reset > 0 {
		l.ResetAt = time.Unix(int64(reset), 0)
	}
	return l, raw, nil
}

// usageAmount 是额度条目的用量字段：带精度的优先，否则用整数版。
type usageAmount struct {
	UsedPrec  *float64 `json:"currentUsageWithPrecision"`
	UsedInt   *float64 `json:"currentUsage"`
	LimitPrec *float64 `json:"usageLimitWithPrecision"`
	LimitInt  *float64 `json:"usageLimit"`
}

func (a usageAmount) used() float64  { return firstOf(a.UsedPrec, a.UsedInt) }
func (a usageAmount) limit() float64 { return firstOf(a.LimitPrec, a.LimitInt) }

func firstOf(vs ...*float64) float64 {
	for _, v := range vs {
		if v != nil {
			return *v
		}
	}
	return 0
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
// 还没收到任何响应字节就断了连接（多半是复用的空闲连接已被对端关掉）时，用新连接重试一次：
// 上游没处理这个请求，不算号的失败。
func (c *Client) Generate(ctx context.Context, cred Cred, payload []byte) (*http.Response, error) {
	var answered atomic.Bool // 收到过响应字节就不重试：上游可能已经处理了这个请求
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() { answered.Store(true) }})
	res, err := c.generate(traced, cred, payload)
	if err != nil && ctx.Err() == nil && !answered.Load() && ConnDropped(err) {
		c.HTTP.CloseIdleConnections()
		res, err = c.generate(ctx, cred, payload)
	}
	return res, err
}

// ConnDropped 报告 err 是不是连接被对端关闭 / 重置（EOF、connection reset、broken pipe）。
func ConnDropped(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// Windows 的 WSAECONNRESET 不等于 syscall.ECONNRESET，只能看文字
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "forcibly closed by the remote host")
}

func (c *Client) generate(ctx context.Context, cred Cred, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RuntimeURL(cred.APIRegion())+"/generateAssistantResponse", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	cred.setAuth(req.Header)
	c.cliHeaders(req.Header, cred.MachineID)
	for k, v := range map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/vnd.amazon.eventstream",
		"x-amzn-codewhisperer-optout": "true",
		"amz-sdk-invocation-id":       NewUUID(),
		"amz-sdk-request":             "attempt=1; max=1",
		"x-amzn-kiro-agent-mode":      "vibe",
	} {
		req.Header.Set(k, v)
	}
	return c.HTTP.Do(req)
}

func (c *Client) management(ctx context.Context, cred Cred, method string, q url.Values, out any) error {
	h := http.Header{}
	cred.setAuth(h)
	c.cliHeaders(h, cred.MachineID)
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
