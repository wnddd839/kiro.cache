package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IDETokenPath 是 Kiro IDE 存登录的位置：~/.aws/sso/cache/kiro-auth-token.json。
func IDETokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aws", "sso", "cache", "kiro-auth-token.json"), nil
}

// IDEMachineID 读 Kiro IDE 自己的机器码（各平台的用户配置目录下 Kiro/machineid），读不到返回空。
// 导入 IDE 登录时沿用它：同一个号在上游看来还是那台机器。去掉连字符，与请求里的写法一致。
func IDEMachineID() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Kiro", "machineid"))
	if err != nil {
		return ""
	}
	id := strings.ReplaceAll(strings.TrimSpace(string(raw)), "-", "")
	if len(id) < 16 || len(id) > 64 || strings.Trim(strings.ToLower(id), "0123456789abcdef") != "" {
		return ""
	}
	return strings.ToLower(id)
}

// ideTokenFile 是 IDE token 文件名。
const ideTokenFile = "kiro-auth-token.json"

// CheckIDEPath 校验管理台传来的 IDE token 路径并返回规整后的绝对路径。
// 号池会读这个文件，刷新 token 时还会写回：只接受名为 kiro-auth-token.json 的普通文件（不跟符号链接），
// 否则 /admin 就成了任意文件读写。
func CheckIDEPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("IDE token path must be absolute")
	}
	p = filepath.Clean(p)
	if !strings.EqualFold(filepath.Base(p), ideTokenFile) {
		return "", fmt.Errorf("IDE token path must point to a %s file", ideTokenFile)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("IDE token file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("IDE token path is not a regular file")
	}
	return p, nil
}

type ideToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	Region       string `json:"region"`
	ProfileArn   string `json:"profileArn"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider"`
	ClientIDHash string `json:"clientIdHash"`
}

// ReadIDE 读 Kiro IDE 的登录。IdC 登录还会读同目录下的 client 注册。
func ReadIDE(path string) (Cred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Cred{}, err
	}
	var t ideToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return Cred{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if t.AccessToken == "" {
		return Cred{}, fmt.Errorf("%s has no access token", path)
	}
	c := Cred{
		MachineID:    IDEMachineID(),
		Method:       MethodIDC,
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		Region:       t.Region,
		ProfileArn:   t.ProfileArn,
		BuilderID:    strings.EqualFold(t.Provider, "BuilderId"),
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if ts, err := time.Parse(time.RFC3339, t.ExpiresAt); err == nil {
		c.ExpiresAt = ts
	}
	if t.ClientIDHash != "" {
		if reg, err := os.ReadFile(filepath.Join(filepath.Dir(path), t.ClientIDHash+".json")); err == nil {
			var r struct {
				ClientID     string `json:"clientId"`
				ClientSecret string `json:"clientSecret"`
				ExpiresAt    string `json:"expiresAt"` // IDE 写的是 RFC3339
			}
			if json.Unmarshal(reg, &r) == nil {
				c.ClientID, c.ClientSecret = r.ClientID, r.ClientSecret
				if ts, err := time.Parse(time.RFC3339, r.ExpiresAt); err == nil {
					c.ClientSecretExpiresAt = ts
				}
			}
		}
	}
	if strings.EqualFold(t.AuthMethod, "social") || c.ClientID == "" {
		c.Method = MethodSocial
	}
	return c, nil
}

// WriteBackIDE 把刷新后的 token 写回 IDE 文件。refresh token 可能轮换，
// 不写回的话 IDE 手里的旧 refresh token 会失效。只改三个字段，其余原样保留。
func WriteBackIDE(path string, c Cred) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["accessToken"] = c.AccessToken
	m["refreshToken"] = c.RefreshToken
	m["expiresAt"] = c.ExpiresAt.UTC().Format(time.RFC3339)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, out)
}

// WriteFileAtomic 写临时文件再 rename，避免写一半崩溃损坏凭证。
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 成功后无事可做
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, path)
}
