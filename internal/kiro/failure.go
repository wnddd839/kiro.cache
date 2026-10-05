package kiro

import (
	"regexp"
	"strings"
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
)

// Failure 是上游非 2xx 响应翻译成的下游状态、消息与分类。
type Failure struct {
	Status  int
	Message string
	Class   Class
}

var quotaWords = regexp.MustCompile(`(?i)quota|insufficient|balance|credit|billing|exceeded|usage.?limit|limit.?reached|monthly`)

// Classify 把 generateAssistantResponse 的失败响应映射成下游语义。
func Classify(status int, body []byte) Failure {
	msg := errorMessage(status, body)
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "CONTENT_LENGTH_EXCEEDS_THRESHOLD") || strings.Contains(lower, "input is too long"):
		return Failure{400, "input is too long for the model's context: " + msg, ClassFatal}
	case strings.Contains(msg, "INSUFFICIENT_MODEL_CAPACITY"):
		return Failure{503, msg, ClassTransient}
	case strings.Contains(msg, "MONTHLY_REQUEST_COUNT") || strings.Contains(msg, "USAGE_LIMIT"):
		return Failure{429, "usage limit reached: " + msg, ClassQuota}
	case status == 401 || status == 403:
		return Failure{status, msg, ClassAuth}
	case status == 429 || strings.Contains(lower, "throttl"):
		if quotaWords.MatchString(msg) {
			return Failure{429, msg, ClassQuota}
		}
		return Failure{429, msg, ClassThrottle}
	case status >= 500 || status == 408:
		return Failure{status, msg, ClassTransient}
	}
	return Failure{status, msg, ClassFatal}
}

// ClassifyStream 是流内异常与中途断流的分类。
func ClassifyStream(text string) Failure {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(text, "CONTENT_LENGTH_EXCEEDS_THRESHOLD") || strings.Contains(lower, "input is too long"):
		return Failure{400, text, ClassFatal}
	case strings.Contains(text, "USAGE_LIMIT") || strings.Contains(text, "MONTHLY_REQUEST_COUNT"):
		return Failure{429, text, ClassQuota}
	case strings.HasPrefix(lower, "rate limited") || strings.Contains(lower, "throttl"):
		return Failure{429, text, ClassThrottle}
	case strings.Contains(lower, "validation"):
		return Failure{400, text, ClassFatal}
	}
	return Failure{502, text, ClassTransient}
}
