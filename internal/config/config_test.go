package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 没写的字段用默认：内置工具默认 drop（Codex 默认带 web_search）、每号并发默认 3、熔断窗口 2m。
func TestLoadDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kiro-proxy.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:9000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenAIHostedTools != "drop" || c.MaxConcurrent != 3 || time.Duration(c.BreakerWindow) != 2*time.Minute {
		t.Fatalf("defaults: hosted %q max_concurrent %d breaker %v", c.OpenAIHostedTools, c.MaxConcurrent, time.Duration(c.BreakerWindow))
	}

	if err := os.WriteFile(path, []byte(`{"openai_hosted_tools":"reject","max_concurrent":0,"breaker_window":"5m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if c.OpenAIHostedTools != "reject" || c.MaxConcurrent != 0 || time.Duration(c.BreakerWindow) != 5*time.Minute {
		t.Fatalf("overrides: hosted %q max_concurrent %d breaker %v", c.OpenAIHostedTools, c.MaxConcurrent, time.Duration(c.BreakerWindow))
	}

	for _, bad := range []string{`{"max_concurrent":-1}`, `{"breaker_window":"0s"}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}
