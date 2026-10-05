package kiro

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    int
		class   Class
		msgPart string
	}{
		{"content length", 400, `{"message":"Input too big","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`, 400, ClassFatal, "input is too long for the model's context: Input too big (CONTENT_LENGTH_EXCEEDS_THRESHOLD)"},
		{"input is too long text", 500, `Input is too long for requested model`, 400, ClassFatal, "input is too long"},
		{"capacity", 429, `{"message":"busy","reason":"INSUFFICIENT_MODEL_CAPACITY"}`, 503, ClassTransient, "busy (INSUFFICIENT_MODEL_CAPACITY)"},
		{"usage limit", 402, `{"message":"You have reached the limit","reason":"USAGE_LIMIT"}`, 429, ClassQuota, "usage limit reached: "},
		{"monthly count", 429, `MONTHLY_REQUEST_COUNT exceeded`, 429, ClassQuota, "usage limit reached: MONTHLY_REQUEST_COUNT"},
		{"401", 401, `{"message":"The bearer token included in the request is invalid."}`, 401, ClassAuth, "bearer token"},
		{"403", 403, `{"message":"forbidden"}`, 403, ClassAuth, "forbidden"},
		{"429 plain", 429, `{"message":"Too many requests, please wait"}`, 429, ClassThrottle, "Too many requests"},
		{"429 quota words", 429, `{"message":"monthly quota exhausted"}`, 429, ClassQuota, "monthly quota"},
		{"throttling at 400", 400, `{"message":"ThrottlingException: slow down"}`, 429, ClassThrottle, "Throttling"},
		{"throttled plain at 400", 400, `{"message":"request throttled"}`, 429, ClassThrottle, "throttled"},
		{"500", 500, `{"message":"internal"}`, 500, ClassTransient, "internal"},
		{"502 empty body", 502, ``, 502, ClassTransient, "Bad Gateway"},
		{"408", 408, `timeout`, 408, ClassTransient, "timeout"},
		{"400 other", 400, `{"message":"Improperly formed request."}`, 400, ClassFatal, "Improperly formed request."},
		{"404", 404, `not here`, 404, ClassFatal, "not here"},
		{"oauth error", 400, `{"error":"invalid_grant","error_description":"expired"}`, 400, ClassFatal, "expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Classify(tt.status, []byte(tt.body))
			if f.Status != tt.want || f.Class != tt.class {
				t.Errorf("Classify = {%d %q %d}, want status %d class %d", f.Status, f.Message, f.Class, tt.want, tt.class)
			}
			if !strings.Contains(f.Message, tt.msgPart) {
				t.Errorf("message %q does not contain %q", f.Message, tt.msgPart)
			}
		})
	}
}

func TestClassifyStream(t *testing.T) {
	tests := []struct {
		text   string
		status int
		class  Class
	}{
		{"ValidationException: x (CONTENT_LENGTH_EXCEEDS_THRESHOLD)", 400, ClassFatal},
		{"Input is too long", 400, ClassFatal},
		{"x: limit (USAGE_LIMIT)", 429, ClassQuota},
		{"MONTHLY_REQUEST_COUNT", 429, ClassQuota},
		{"rate limited: slow down", 429, ClassThrottle},
		{"ThrottlingException: too fast", 429, ClassThrottle},
		{"validationError: bad", 400, ClassFatal},
		{"the reply broke off: unexpected EOF", 502, ClassTransient},
		{"internalServerException: boom", 502, ClassTransient},
		{"", 502, ClassTransient},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			f := ClassifyStream(tt.text)
			if f.Status != tt.status || f.Class != tt.class || f.Message != tt.text {
				t.Errorf("ClassifyStream(%q) = %+v, want status %d class %d", tt.text, f, tt.status, tt.class)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	for _, tt := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"message":"m","reason":"R"}`, "m (R)"},
		{400, `{"error":"invalid_grant"}`, "invalid_grant"},
		{400, `{"error_description":"d","error":"e"}`, "d"},
		{400, `{"other":1}`, `{"other":1}`},
		{503, `  `, "Service Unavailable"},
		{500, ` plain `, "plain"},
	} {
		if got := errorMessage(tt.status, []byte(tt.body)); got != tt.want {
			t.Errorf("errorMessage(%d, %q) = %q, want %q", tt.status, tt.body, got, tt.want)
		}
	}
}
