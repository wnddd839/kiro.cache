package meter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// PriceSource 是在线价格：OpenRouter 挂牌价（含 Claude / GPT / 开放权重）。
const PriceSource = "https://openrouter.ai/api/v1/models"

// PriceSources 给管理台展示当前价格源。
var PriceSources = struct{ OpenRouter string }{OpenRouter: PriceSource}

// FetchPrices 拉 OpenRouter，只保留 want() 命中的模型。want 为 nil 时全留。
func FetchPrices(ctx context.Context, hc *http.Client, want func(id string) bool) (map[string]Price, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := get(ctx, hc, PriceSource)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	m, err := ParseOpenRouter(body)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	if want == nil {
		return m, nil
	}
	out := map[string]Price{}
	for k, v := range m {
		if want(k) {
			out[k] = v
		}
	}
	return out, nil
}

func get(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kiro-proxy (price sync)")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 16<<20))
}

type orPricing struct {
	Prompt       string       `json:"prompt"`
	Completion   string       `json:"completion"`
	CacheRead    string       `json:"input_cache_read"`
	CacheWrite   string       `json:"input_cache_write"`
	CacheWrite1h string       `json:"input_cache_write_1h"`
	Overrides    []orOverride `json:"overrides"`
}

type orOverride struct {
	MinPromptTokens int    `json:"min_prompt_tokens"`
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	CacheRead       string `json:"input_cache_read"`
	CacheWrite      string `json:"input_cache_write"`
	CacheWrite1h    string `json:"input_cache_write_1h"`
}

// ParseOpenRouter 解析 OpenRouter /api/v1/models。价格单位是每 token 美元（字符串）。
// 键规范化为 Kiro 写法（deepseek/deepseek-v3.2 → deepseek-3.2）；带 :batch 等变体后缀的跳过。
// input_cache_write_1h → CacheWrite1h；pricing.overrides[0] → Long / LongAbove。
func ParseOpenRouter(body []byte) (map[string]Price, error) {
	var resp struct {
		Data []struct {
			ID      string    `json:"id"`
			Pricing orPricing `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := map[string]Price{}
	for _, m := range resp.Data {
		if strings.Contains(m.ID, ":") {
			continue
		}
		p := orPrice(m.Pricing)
		if p.Input <= 0 || p.Output <= 0 {
			continue
		}
		p.Source = "openrouter"
		out[CanonModel(m.ID)] = p
	}
	if len(out) == 0 {
		return nil, errors.New("no priced models")
	}
	return out, nil
}

func orPrice(pr orPricing) Price {
	p := Price{
		Input:        perM(pr.Prompt),
		Output:       perM(pr.Completion),
		CacheRead:    perM(pr.CacheRead),
		CacheWrite:   perM(pr.CacheWrite),
		CacheWrite1h: perM(pr.CacheWrite1h),
	}
	if p.CacheWrite == 0 {
		p.CacheWrite = p.Input // 挂牌不报写入价：自动缓存、写入不加价
	}
	if p.CacheRead == 0 {
		p.CacheRead = p.Input
	}
	// 挂牌不报 1h 写入价时不在这里推导：Claude 的由 Price.filled 按 max(2×input, CacheWrite) 补，
	// 其它模型没有 1h 缓存，按 5m 写入价（不多收）
	for _, o := range pr.Overrides {
		if o.MinPromptTokens <= 0 {
			continue
		}
		long := orPrice(orPricing{
			Prompt: o.Prompt, Completion: o.Completion,
			CacheRead: o.CacheRead, CacheWrite: o.CacheWrite, CacheWrite1h: o.CacheWrite1h,
		})
		if long.Input <= 0 || long.Output <= 0 {
			continue
		}
		p.LongAbove = o.MinPromptTokens
		p.Long = &Price{Input: long.Input, Output: long.Output, CacheRead: long.CacheRead, CacheWrite: long.CacheWrite, CacheWrite1h: long.CacheWrite1h}
		break
	}
	return p
}

func perM(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return roundPrice(v * 1e6)
}

// roundPrice 去掉浮点乘法的尾差（"0.00000042" × 1e6 = 0.41999999999999998）。
func roundPrice(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 10, 64), 64)
	return r
}

// onlineFile 是在线价格的落盘格式，重启后无网也能用上次拉到的价。
type onlineFile struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Prices    map[string]Price `json:"prices"`
}

// SaveOnline 把在线价格写到 path（先写临时文件再改名）。
func (p *Pricer) SaveOnline(path string) error {
	prices, at := p.Online()
	if len(prices) == 0 {
		return nil
	}
	b, err := json.MarshalIndent(onlineFile{FetchedAt: at, Prices: prices}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadOnline 读 SaveOnline 写的文件；文件不存在不算错。
func (p *Pricer) LoadOnline(path string) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var f onlineFile
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, err
	}
	p.SetOnline(f.Prices, f.FetchedAt)
	return len(f.Prices), nil
}
