package meter

import (
	"math"

	"kiro-proxy/internal/turn"
)

// CreditRate 是按 token 估 Kiro credits 的系数：每百万 token 的 credits。
// Context 是未命中缓存的上下文（下游 prompt + Kiro 自带的隐藏 token）；命中缓存的部分按 Context × ReadFactor。
type CreditRate struct {
	Context float64 `json:"context"`
	Output  float64 `json:"output"`
	// ReadFactor 是缓存读相对未缓存上下文的单价比例；0 = DefaultReadFactor。
	ReadFactor float64 `json:"read_factor,omitzero"`
}

// DefaultReadFactor 是缓存读的 credits 折扣。
//
// 来源：2026-10-07 `kiro-proxy probe credits / cachepoint / usage / continuation / longctx`，
// claude-sonnet-4.5、同一号，28 次调用拟合得热 / 冷上下文单价比 0.531（平均误差 1.7%，最大 4.3%）。
// 注意这与 Anthropic 的 0.1 倍读价差很多：Kiro 对缓存读只打约五折。
const DefaultReadFactor = 0.53

// defaultCreditRates 用于上游没来得及报 meteringEvent 的请求（客户端中断、中途断流）。
//
// claude：上述探测拟合，未缓存上下文 4.108 / 百万、输出 109 / 百万（输出字符 / 4 计 token）。
// 只测了 sonnet-4.5；其它 Claude 模型的倍率不同，用 credit_rates 覆盖。
// qwen3-coder-next：2026-10 本机账本 7 笔拟合（无缓存读）。
var defaultCreditRates = map[string]CreditRate{
	"claude":           {Context: 4.108, Output: 109, ReadFactor: DefaultReadFactor},
	"auto":             {Context: 4.108, Output: 109, ReadFactor: DefaultReadFactor},
	"qwen3-coder-next": {Context: 1.35, Output: 6.8, ReadFactor: DefaultReadFactor},
}

// CreditRateOf 是模型的估算系数：配置覆盖 → 内置（前缀）→ Claude 的系数。
func CreditRateOf(model string, overrides map[string]CreditRate) CreditRate {
	m := CanonModel(model)
	for _, table := range []map[string]CreditRate{overrides, defaultCreditRates} {
		best, bestLen := CreditRate{}, -1
		for k, v := range table {
			k = CanonModel(k)
			if (m == k || len(m) > len(k) && m[:len(k)] == k && (m[len(k)] == '-' || k == "claude")) && len(k) > bestLen {
				best, bestLen = v, len(k)
			}
		}
		if bestLen >= 0 {
			return best
		}
	}
	return defaultCreditRates["claude"]
}

// EstimateCredits 按 token 估一次请求扣的 credits。hidden 是 Kiro 自带的上下文 token：
// 命中缓存时它也在缓存里（探测：热请求的整段上下文都按读价），否则按未缓存计。
func EstimateCredits(r CreditRate, u turn.Usage, hidden int) float64 {
	if u.PromptTokens() == 0 && u.Output == 0 {
		return 0
	}
	f := r.ReadFactor
	if f <= 0 {
		f = DefaultReadFactor
	}
	cold, warm := float64(u.Input+u.CacheWrite), float64(u.CacheRead)
	if u.CacheRead > 0 {
		warm += float64(hidden)
	} else {
		cold += float64(hidden)
	}
	c := (cold*r.Context + warm*r.Context*f + float64(u.Output)*r.Output) / 1e6
	return math.Round(c*1e6) / 1e6
}
