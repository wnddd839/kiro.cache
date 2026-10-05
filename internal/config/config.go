// Package config 是 kiro-go 的配置文件。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ConversationMode 决定上游 conversationId 怎么取。
const (
	ConversationSession = "session" // 同一会话+同一号复用（默认）
	ConversationRandom  = "random"  // 每次随机，与 Magpie / 插件一致，用于 A/B 对照
)

// Duration 接受 "45m" 这样的字符串。
type Duration time.Duration

// UnmarshalJSON 解析 Go duration 字符串。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON 输出 Go duration 字符串。
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Config 是全部配置。
type Config struct {
	Listen       string `json:"listen"`
	AccountsFile string `json:"accounts_file"`
	// APIKeys 是下游客户端可用的 key；空表示不校验（仅监听本机时可接受）。
	APIKeys []string `json:"api_keys,omitzero"`
	// AdminToken 保护 /admin；空表示不校验。
	AdminToken string `json:"admin_token,omitzero"`

	SessionTTL       Duration `json:"session_ttl"`
	ConversationMode string   `json:"conversation_mode"`
	SortTools        bool     `json:"sort_tools"`
	// PinThinking 让一个会话的 thinking 预算以首次请求为准，中途开关不改首条消息前缀。
	PinThinking bool `json:"pin_thinking"`
	// SystemStrip 是从 system 里删掉的正则（如 cwd / 日期行）。只在确认它们打破 cache 时加。
	SystemStrip []string `json:"system_strip,omitzero"`
	// ModelAliases 把下游模型名映射到 Kiro 模型 id，优先于内置推断。
	ModelAliases map[string]string `json:"model_aliases,omitzero"`

	MaxAttempts    int      `json:"max_attempts"`
	LimitsInterval Duration `json:"limits_interval"`
	LogLevel       string   `json:"log_level"`
}

// Default 是默认配置。
func Default() Config {
	return Config{
		Listen:           "127.0.0.1:8787",
		AccountsFile:     "accounts.json",
		SessionTTL:       Duration(45 * time.Minute),
		ConversationMode: ConversationSession,
		SortTools:        true,
		PinThinking:      true,
		MaxAttempts:      3,
		LimitsInterval:   Duration(10 * time.Minute),
		LogLevel:         "info",
	}
}

// Load 读配置；文件不存在时返回默认值。文件里没写的字段保持默认。
func Load(path string) (Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return c, nil
	case err != nil:
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate 检查取值。
func (c Config) Validate() error {
	switch c.ConversationMode {
	case ConversationSession, ConversationRandom:
	default:
		return fmt.Errorf("conversation_mode must be %q or %q, got %q", ConversationSession, ConversationRandom, c.ConversationMode)
	}
	if c.MaxAttempts < 1 {
		return errors.New("max_attempts must be >= 1")
	}
	if c.Listen == "" || c.AccountsFile == "" {
		return errors.New("listen and accounts_file are required")
	}
	return nil
}
