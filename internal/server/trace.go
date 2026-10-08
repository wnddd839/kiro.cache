package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"kiro-proxy/internal/kiro"
)

type requestTrace struct {
	Account  string            `json:"account"`
	Request  string            `json:"request"`
	Attempt  int               `json:"attempt"`
	Previous string            `json:"previous_request,omitzero"`
	Prefix   *kiro.PrefixCheck `json:"prefix,omitzero"`
}

// traceRequest 保存真正传给 Generate 的字节，不含 Authorization 请求头。
// 完整对话、图片和 profileArn 会落盘，仅在显式打开调试开关时执行。
func (s *Server) traceRequest(c *call, account string, payload []byte) error {
	dir := s.cfg.DebugRequestsDir
	if dir == "" {
		return nil
	}
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	key := c.traceKey
	if key == "" {
		key = c.entry.Request
	}
	stem := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	latest := filepath.Join(dir, stem+".latest.json")
	metadata := filepath.Join(dir, stem+".latest.meta.json")
	trace := requestTrace{Account: account, Attempt: c.entry.Attempts}
	previous, err := os.ReadFile(latest)
	switch {
	case err == nil:
		var old requestTrace
		raw, err := os.ReadFile(metadata)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		check, err := kiro.CheckPrefix(previous, payload)
		if err != nil {
			return err
		}
		check.PreviousAccount, check.Account, check.SameAccount = old.Account, account, old.Account == account
		trace.Previous, trace.Prefix = old.Request, &check
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	file, err := os.CreateTemp(dir, stem+"-*.request.json")
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	trace.Request = filepath.Base(file.Name())
	raw, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return err
	}
	if err := kiro.WriteFileAtomic(file.Name()+".meta.json", raw); err != nil {
		return err
	}
	if err := kiro.WriteFileAtomic(latest, payload); err != nil {
		return err
	}
	return kiro.WriteFileAtomic(metadata, raw)
}
