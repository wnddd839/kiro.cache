package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/sessionpin"
)

// 会话状态落盘。
//
// prompt cache 模拟与上游 conversationId 必须一起落盘、一起载入：只留前者，重启后本地会判「命中」，
// 但新的 conversationId 在 Kiro 那边是冷的（若上游按 conversation 分缓存），就会少收钱；
// 只留后者，会把上游命中记成写入，多收钱。所以两者由同一个开关（cache_file）控制：
// 会话状态写在 cache_file 旁边的 <name>.sessions.json，任一个读不出来就两个都丢。

// sessionState 是会话快照。
type sessionState struct {
	Version int                                           `json:"version"`
	Pins    map[string]sessionpin.PinSnapshot             `json:"pins"`
	Conv    map[string]map[string]sessionpin.ConvSnapshot `json:"conversations"`
	Lines   map[string]snapshotTTL[[]uint64]              `json:"lineage"`
	Think   map[string]snapshotTTL[int]                   `json:"thinking"`
}

const sessionStateVersion = 1

// sessionFile 是与 prompt cache 文件配套的会话快照路径。
func sessionFile(cacheFile string) string {
	ext := filepath.Ext(cacheFile)
	return strings.TrimSuffix(cacheFile, ext) + ".sessions.json"
}

// saveState 先写会话快照再写 prompt cache：两个文件各自原子替换。
func (s *Server) saveState() error {
	st := sessionState{
		Version: sessionStateVersion,
		Pins:    s.pool.Pins().Dump(),
		Conv:    s.conv.Dump(),
		Lines:   s.lines.dump(),
		Think:   s.think.dump(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := kiro.WriteFileAtomic(sessionFile(s.cfg.CacheFile), raw); err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	return s.cache.Save(s.cfg.CacheFile)
}

// loadState 载入 prompt cache 与会话快照。任一缺失或损坏就两个都不用：宁可冷启动，也不要半套状态。
func (s *Server) loadState() (cache, sessions int, err error) {
	path := sessionFile(s.cfg.CacheFile)
	raw, rerr := os.ReadFile(path)
	_, cerr := os.Stat(s.cfg.CacheFile)
	switch {
	case errors.Is(rerr, os.ErrNotExist) && errors.Is(cerr, os.ErrNotExist):
		return 0, 0, nil // 首次启动
	case errors.Is(rerr, os.ErrNotExist):
		// 旧版本只落了 prompt cache：没有对应的 conversationId，不能单独用
		return 0, 0, fmt.Errorf("%s missing; prompt cache state discarded (it is only valid together with the conversation ids)", path)
	case errors.Is(cerr, os.ErrNotExist):
		// 会话快照在、prompt cache 不在：上游缓存状态与本地对不上，会话快照也不用
		return 0, 0, fmt.Errorf("%s missing; session state discarded (it is only valid together with the prompt cache state)", s.cfg.CacheFile)
	case rerr != nil:
		return 0, 0, rerr
	}
	var st sessionState
	if err := json.Unmarshal(raw, &st); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", path, err)
	}
	if st.Version != sessionStateVersion {
		return 0, 0, fmt.Errorf("%s: version %d, want %d", path, st.Version, sessionStateVersion)
	}
	n, err := s.cache.Load(s.cfg.CacheFile)
	if err != nil {
		return 0, 0, err
	}
	sessions = s.pool.Pins().Restore(st.Pins) + s.conv.Restore(st.Conv)
	s.lines.restore(st.Lines)
	s.think.restore(st.Think)
	return n, sessions, nil
}
