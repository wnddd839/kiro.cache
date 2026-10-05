package server

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"kiro-go/internal/kiro"
)

var (
	dateSuffix   = regexp.MustCompile(`-\d{8}$`)
	versionDash  = regexp.MustCompile(`(\d)-(\d)$`)
	latestSuffix = regexp.MustCompile(`-latest$`)
)

// kiroModel 把下游模型名映射到 Kiro 模型 id。
// 顺序：配置别名 → 已知列表里原样存在 → claude-sonnet-4-5-20250929 → claude-sonnet-4.5。
func kiroModel(name string, aliases map[string]string, known func(string) bool) string {
	if v, ok := aliases[name]; ok {
		return v
	}
	if name == "" {
		return "auto"
	}
	if known(name) {
		return name
	}
	m := strings.ToLower(name)
	m = dateSuffix.ReplaceAllString(m, "")
	m = latestSuffix.ReplaceAllString(m, "")
	m = versionDash.ReplaceAllString(m, "$1.$2")
	return m
}

// catalog 缓存各号的模型列表：给 /v1/models 和上下文窗口估算用。
type catalog struct {
	mu     sync.Mutex
	models map[string]kiro.Model // id → model（各号并集）
	at     time.Time
}

const catalogTTL = 30 * time.Minute

func (c *catalog) known(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.models[id]
	return ok
}

func (c *catalog) window(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models[id].Context
}

func (c *catalog) list() []kiro.Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]kiro.Model, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b kiro.Model) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (c *catalog) stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.at) > catalogTTL
}

func (c *catalog) merge(models []kiro.Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models == nil {
		c.models = map[string]kiro.Model{}
	}
	for _, m := range models {
		c.models[m.ID] = m
	}
	c.at = time.Now()
}

// refreshCatalog 从每个启用号拉模型列表并合并。失败只记日志。
func (s *Server) refreshCatalog(ctx context.Context) {
	for _, v := range s.pool.List() {
		if v.Disabled {
			continue
		}
		cred, err := s.pool.Cred(ctx, v.ID, "")
		if err != nil {
			s.log.Warn("list models: credentials", "account", v.ID, "err", err)
			continue
		}
		models, err := s.pool.Client().ListModels(ctx, cred)
		if err != nil {
			s.log.Warn("list models", "account", v.ID, "err", err)
			continue
		}
		s.models.merge(models)
	}
}
