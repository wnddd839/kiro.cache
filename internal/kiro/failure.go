package kiro

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Class 是一次失败对号池的含义。
type Class int

const (
	// ClassFatal 是请求本身的问题（太长、格式错），换号无用，直接返回。
	ClassFatal Class = iota
	// ClassAuth 是 token 被拒，刷新后重试同一号。
	ClassAuth
	// ClassQuota 是额度用尽（USAGE_LIMIT / MONTHLY_REQUEST_COUNT），长时间冷却并换号。
	ClassQuota
	// ClassThrottle 是限流，短冷却并换号。
	ClassThrottle
	// ClassTransient 是上游故障（5xx、容量不足、网络），可换号重试。
	ClassTransient
	// ClassForbidden 是不是 token 失效的 403：短冷却并换号，不刷新 token。
	ClassForbidden
	// ClassBanned 是号被封 / 暂停（TemporarilySuspended、423）。刷新 token 救不回来：停用该号并换号。
	ClassBanned
)

// Failure 是上游非 2xx 响应翻译成的下游状态、消息与分类。
type Failure struct {
	Status  int
	Message string
	Class   Class
	// RetryAfter 是上游给的限流窗口（x-amzn-kiro-ratelimit-retry-after / Retry-After）；0 是没给。
	RetryAfter time.Duration
	// Network 表示没拿到 HTTP 响应（连接 / 传输错误）。号池据此判断是不是全局网络故障。
	Network bool
}

// maxRetryAfter 是上游限流窗口的上限：超过也只冷却这么久，免得一个异常值把号冻住。
const maxRetryAfter = 5 * time.Minute

// RetryAfter 读上游的限流窗口。x-amzn-kiro-ratelimit-retry-after 是毫秒；Retry-After 是秒。
func RetryAfter(h http.Header) time.Duration {
	var d time.Duration
	if v := strings.TrimSpace(h.Get("x-amzn-kiro-ratelimit-retry-after")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			d = time.Duration(ms * float64(time.Millisecond))
		}
	} else if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if sec, err := strconv.ParseFloat(v, 64); err == nil && sec > 0 {
			d = time.Duration(sec * float64(time.Second))
		}
	}
	return min(d, maxRetryAfter)
}

// bannedWords 是封号 / 暂停的文字；这些 403 刷新 token 没用。
var bannedWords = regexp.MustCompile(`(?i)temporarily.?(is.?)?suspended|account.?(is.?)?(suspended|disabled|banned|locked)|violation of (the )?terms|terms of service`)

// tokenWords 是「access token 本身失效」的文字：只有这种 403 才值得刷新重试。
// 其它 403（策略、profile、防火墙、额度……）刷新也没用，刷了反而浪费 refresh token。
var tokenWords = regexp.MustCompile(`(?i)bearer token|access ?token|security token|token (is |has )?(invalid|expired|malformed)|(invalid|expired|malformed) (bearer |access |security )?token|ExpiredTokenException|InvalidToken|UnrecognizedClientException|AccessDeniedException: .*token`)

// TokenRejected 报告对话请求的 403 是不是明确的 token 失效（bearer token invalid / expired 一类）。
func TokenRejected(status int, body []byte) bool {
	return status == http.StatusForbidden && !Banned(status, body) && tokenWords.Match(body)
}

// Banned 报告一个失败响应是不是封号。
func Banned(status int, body []byte) bool {
	return status == http.StatusLocked || (status == http.StatusForbidden || status == http.StatusUnauthorized) && bannedWords.Match(body)
}

// quotaWords 是明确的额度用尽文案。Kiro 实际返回的是 402 / 429 + {"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}。
// 不认 "exceeded"：429 "Rate exceeded" 是限流，误判成额度会把号冷却到月度重置。
var quotaWords = regexp.MustCompile(`(?i)MONTHLY_REQUEST_COUNT|USAGE_LIMIT|quota|usage.?limit|reached the limit`)

// Classify 把 generateAssistantResponse 的失败响应映射成下游语义。
func Classify(status int, body []byte) Failure {
	msg := errorMessage(status, body)
	lower := strings.ToLower(msg)
	switch {
	case Banned(status, body):
		return Failure{Status: 403, Message: "account suspended: " + msg, Class: ClassBanned}
	case strings.Contains(msg, "CONTENT_LENGTH_EXCEEDS_THRESHOLD") || strings.Contains(lower, "input is too long"):
		return Failure{Status: 400, Message: "input is too long for the model's context: " + msg, Class: ClassFatal}
	case strings.Contains(msg, "INSUFFICIENT_MODEL_CAPACITY"):
		return Failure{Status: 503, Message: msg, Class: ClassTransient}
	case strings.Contains(msg, "MONTHLY_REQUEST_COUNT") || strings.Contains(msg, "USAGE_LIMIT"):
		return Failure{Status: 429, Message: "usage limit reached: " + msg, Class: ClassQuota}
	case status == http.StatusPaymentRequired:
		// 402：额度用尽，冷却到重置时间并换号
		return Failure{Status: 429, Message: "usage limit reached: " + msg, Class: ClassQuota}
	case status == 401 || TokenRejected(status, body):
		return Failure{Status: status, Message: msg, Class: ClassAuth}
	case status == 403:
		// 不是 token 失效的 403（策略 / profile / 防火墙……）：计一次失败并换号，不刷新
		return Failure{Status: status, Message: msg, Class: ClassForbidden}
	case status == 429 || strings.Contains(lower, "throttl"):
		// 429 默认是限流（Rate exceeded / Too many requests / ThrottlingException），返回体明确写着额度才算额度用尽
		if quotaWords.MatchString(msg) {
			return Failure{Status: 429, Message: msg, Class: ClassQuota}
		}
		return Failure{Status: 429, Message: msg, Class: ClassThrottle}
	case status >= 500 || status == 408:
		return Failure{Status: status, Message: msg, Class: ClassTransient}
	}
	return Failure{Status: status, Message: msg, Class: ClassFatal}
}

// ClassifyStream 是流内异常与中途断流的分类。
func ClassifyStream(text string) Failure {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(text, "CONTENT_LENGTH_EXCEEDS_THRESHOLD") || strings.Contains(lower, "input is too long"):
		return Failure{Status: 400, Message: text, Class: ClassFatal}
	case strings.Contains(text, "USAGE_LIMIT") || strings.Contains(text, "MONTHLY_REQUEST_COUNT"):
		return Failure{Status: 429, Message: text, Class: ClassQuota}
	case strings.HasPrefix(lower, "rate limited") || strings.Contains(lower, "throttl"):
		return Failure{Status: 429, Message: text, Class: ClassThrottle}
	case strings.Contains(lower, "validation"):
		return Failure{Status: 400, Message: text, Class: ClassFatal}
	}
	return Failure{Status: 502, Message: text, Class: ClassTransient}
}
