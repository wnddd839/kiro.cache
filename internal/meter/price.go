package meter

import (
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"kiro-proxy/internal/turn"
)

// Price 是每百万 token 的美元标价。缓存价为 0 时按 Anthropic 倍率推导：
// 写 5m = 1.25× 输入，读 = 0.1× 输入；写 1h 的推导只对 Claude 做（max(2× 输入, 5m 写入）），
// 其它模型没有 1h 缓存，按 5m 写入价。
// 自动缓存、写入不加价的模型（DeepSeek / GLM 等）把 CacheWrite 显式设为 Input。
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheWrite   float64 `json:"cache_write,omitzero"`
	CacheWrite1h float64 `json:"cache_write_1h,omitzero"`
	CacheRead    float64 `json:"cache_read,omitzero"`
	// LongAbove 与 Long：输入超过 LongAbove token 的请求整单按 Long 计（GPT-5.6 的 272K 两档价）。
	LongAbove int    `json:"long_above,omitzero"`
	Long      *Price `json:"long,omitzero"`
	// Source 是价格来源：builtin | openrouter | config | fallback。只用于展示。
	Source string `json:"source,omitzero"`
}

func (p Price) filled() Price { return p.fill(true) }

// fill 补齐缓存价。claude 决定 1h 写入缺省时是否按 2 倍推导。
func (p Price) fill(claude bool) Price {
	if p.CacheWrite == 0 {
		p.CacheWrite = p.Input * 1.25
	}
	if p.CacheWrite1h == 0 {
		if claude {
			p.CacheWrite1h = max(p.Input*2, p.CacheWrite)
		} else {
			p.CacheWrite1h = p.CacheWrite
		}
	}
	if p.CacheRead == 0 {
		p.CacheRead = p.Input / 10
	}
	if p.Long != nil {
		l := p.Long.fill(claude)
		l.Source = ""
		p.Long = &l
	}
	return p
}

// forPrompt 是输入 prompt 个 token 时实际适用的档位。
func (p Price) forPrompt(prompt int) Price {
	if p.Long != nil && p.LongAbove > 0 && prompt > p.LongAbove {
		return *p.Long
	}
	return p
}

// builtin 是离线兜底价格，键是 Kiro 模型 id（或其前缀）。
// 来源（2026-10-07 核对）：全部取 OpenRouter 挂牌价。启用在线价格后会被最新值覆盖。
var builtin = map[string]Price{
	"claude-fable-5.1":  {Input: 10, Output: 50, CacheWrite: 12.5, CacheWrite1h: 20, CacheRead: 0.25},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1},
	"claude-opus-5.5":   {Input: 4, Output: 20, CacheRead: 0.2},
	"claude-opus-5":     {Input: 5, Output: 25},
	"claude-opus-4.8":   {Input: 5, Output: 25},
	"claude-opus-4.7":   {Input: 5, Output: 25},
	"claude-opus-4.6":   {Input: 5, Output: 25},
	"claude-opus-4.5":   {Input: 5, Output: 25},
	"claude-opus-4.1":   {Input: 15, Output: 75},
	"claude-opus-4":     {Input: 15, Output: 75},
	"claude-opus":       {Input: 5, Output: 25}, // 未知版本按现行 Opus
	"claude-sonnet-5.5": {Input: 2, Output: 10},
	"claude-sonnet-5":   {Input: 2, Output: 10},
	"claude-sonnet-4.6": {Input: 3, Output: 15},
	"claude-sonnet-4.5": {Input: 3, Output: 15},
	"claude-sonnet-4":   {Input: 3, Output: 15},
	"claude-sonnet":     {Input: 3, Output: 15},
	"claude-haiku-4.5":  {Input: 1, Output: 5},
	"claude-haiku-3.5":  {Input: 0.8, Output: 4},
	"claude-3-5-haiku":  {Input: 0.8, Output: 4},
	"claude-haiku":      {Input: 1, Output: 5},
	// Auto 由 Kiro 选模型，倍率 1.0x；按 Sonnet 4.5 标价计。
	"auto": {Input: 3, Output: 15},

	"gpt-5.6-sol": {Input: 2, Output: 10, CacheWrite: 2.5, CacheRead: 0.2,
		LongAbove: 272_000, Long: &Price{Input: 4, Output: 15, CacheWrite: 5, CacheRead: 0.4}},
	"gpt-5.6-terra": {Input: 2, Output: 12, CacheWrite: 2.5, CacheRead: 0.2,
		LongAbove: 272_000, Long: &Price{Input: 4, Output: 18, CacheWrite: 5, CacheRead: 0.4}},
	"gpt-5.6-luna": {Input: 0.2, Output: 1.2, CacheWrite: 0.25, CacheRead: 0.02,
		LongAbove: 272_000, Long: &Price{Input: 0.4, Output: 1.8, CacheWrite: 0.5, CacheRead: 0.04}},

	"deepseek-3.2":     {Input: 0.28, Output: 0.42, CacheWrite: 0.28, CacheRead: 0.028},
	"minimax-m2.5":     {Input: 0.27, Output: 1.08, CacheWrite: 0.27, CacheRead: 0.027},
	"minimax-m2.1":     {Input: 0.3, Output: 1.2, CacheWrite: 0.3, CacheRead: 0.03},
	"glm-5":            {Input: 0.6, Output: 1.92, CacheWrite: 0.6, CacheRead: 0.12},
	"qwen3-coder-next": {Input: 0.12, Output: 0.8, CacheWrite: 0.12, CacheRead: 0.07},
}

// fallback 是未知模型的价格（按 Sonnet）。
var fallback = Price{Input: 3, Output: 15, Source: "fallback"}

// Pricer 把用量折算成美元。优先级：配置 Overrides → 在线价格 → 内置表 → fallback。
type Pricer struct {
	Overrides map[string]Price // 配置里的价格，键同样是模型 id 前缀
	// CreditUSD 是一个 Kiro credit 的美元价（Pro 套餐 $20 / 1000 credits = 0.02）。
	CreditUSD float64
	// Basis 是对下游报的费用："api"（默认，API 标价等价）或 "credits"（credits × CreditUSD）。
	Basis string

	mu       sync.RWMutex
	online   map[string]Price
	onlineAt time.Time
}

// 费用口径。
const (
	BasisAPI     = "api"
	BasisCredits = "credits"
)

// SetOnline 替换在线价格表（键会规范化）。
func (p *Pricer) SetOnline(prices map[string]Price, at time.Time) {
	m := make(map[string]Price, len(prices))
	for k, v := range prices {
		m[CanonModel(k)] = v
	}
	p.mu.Lock()
	p.online, p.onlineAt = m, at
	p.mu.Unlock()
}

// Online 返回当前在线价格表的副本与拉取时间。
func (p *Pricer) Online() (map[string]Price, time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return maps.Clone(p.online), p.onlineAt
}

// KeepOnline 丢掉 keep 不认的在线条目（旧缓存里的非 Kiro 模型）。
func (p *Pricer) KeepOnline(keep func(id string) bool) {
	if keep == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.online {
		if !keep(k) {
			delete(p.online, k)
		}
	}
}

// PriceOf 返回模型的价格（已补齐缓存价）和是否匹配到了具体条目。
func (p *Pricer) PriceOf(model string) (Price, bool) {
	m := CanonModel(model)
	if v, ok := lookup(p.Overrides, m); ok {
		v.Source = "config"
		return v.fill(isClaude(m)), true
	}
	p.mu.RLock()
	v, ok := lookup(p.online, m)
	p.mu.RUnlock()
	if ok {
		return v.fill(isClaude(m)), true
	}
	if v, ok := lookup(builtin, m); ok {
		v.Source = "builtin"
		return v.fill(isClaude(m)), true
	}
	return fallback.fill(isClaude(m)), false
}

// lookup 先精确匹配，再取最长的「整段」前缀：claude-sonnet-4.5 能匹配 claude-sonnet-4.5-thinking，
// 但 claude-opus-4 不会匹配 claude-opus-4.9（那应走家族条目 claude-opus）。
func lookup(table map[string]Price, model string) (Price, bool) {
	if v, ok := table[model]; ok {
		return v, true
	}
	var best string
	var bestV Price
	for k, v := range table {
		k = CanonModel(k)
		if len(k) > len(best) && len(model) > len(k) && strings.HasPrefix(model, k) && model[len(k)] == '-' {
			best, bestV = k, v
		}
	}
	return bestV, best != ""
}

var versionPrefix = regexp.MustCompile(`-v(\d)`)

// CanonModel 把各家模型名归一到 Kiro 的写法：小写、去掉 vendor/ 前缀、deepseek-v3.2 → deepseek-3.2。
func CanonModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return versionPrefix.ReplaceAllString(s, "-$1")
}

// isClaude 是模型是否是 Claude：只有它有 1h 缓存档。auto 由 Kiro 选 Claude 模型，按 Claude 计。
func isClaude(model string) bool {
	m := CanonModel(model)
	return strings.HasPrefix(m, "claude") || m == "auto"
}

// IsBuiltinID 是 id 是否就是内置价格表的键（Kiro 模型及家族落底，不做前缀模糊）。
func IsBuiltinID(id string) bool {
	_, ok := builtin[CanonModel(id)]
	return ok
}

// APICost 是按 API 标价算的美元费用。u.CacheWrite1h 按 1h 写入价，其余写入按 5m 价。
func (p *Pricer) APICost(model string, u turn.Usage) float64 {
	pr, _ := p.PriceOf(model)
	pr = pr.forPrompt(u.PromptTokens())
	w5 := u.CacheWrite5m()
	usd := float64(u.Input)*pr.Input +
		float64(u.Output)*pr.Output +
		float64(u.CacheRead)*pr.CacheRead +
		float64(w5)*pr.CacheWrite +
		float64(u.CacheWrite-w5)*pr.CacheWrite1h
	return usd / 1e6
}

// Cost 是对下游报的费用：按 Basis 取 API 等价或 credits 折算。
// credits 口径下上游没报 credits（如中途失败）时退回 API 等价。
func (p *Pricer) Cost(model string, u turn.Usage) float64 {
	if p.Basis == BasisCredits && u.Credits > 0 && p.CreditUSD > 0 {
		return u.Credits * p.CreditUSD
	}
	return p.APICost(model, u)
}

// CreditsUSD 是 credits 的美元价值（号池的实际成本）。
func (p *Pricer) CreditsUSD(credits float64) float64 { return credits * p.CreditUSD }

// Table 是内置、在线与覆盖合并后的价格表，给管理台展示。
// 在线条目只保留内置键（精确）或配置覆盖，避免旧缓存里的非 Kiro 模型混进表。
func (p *Pricer) Table() map[string]Price {
	out := make(map[string]Price, len(builtin)+len(p.Overrides))
	for k, v := range builtin {
		v.Source = "builtin"
		out[k] = v.fill(isClaude(k))
	}
	p.mu.RLock()
	for k, v := range p.online {
		if _, ok := builtin[k]; ok {
			out[k] = v.fill(isClaude(k))
		}
	}
	p.mu.RUnlock()
	for k, v := range p.Overrides {
		v.Source = "config"
		k = CanonModel(k)
		out[k] = v.fill(isClaude(k))
	}
	return out
}
