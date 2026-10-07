// Package meter 在本地计量一次请求：token 估算、prompt cache 命中模拟、按 API 标价折算费用。
//
// Kiro 不返回 token 数，只给上下文百分比和 credits。下游要做预算和分摊，
// 需要的是「同一请求在 Anthropic API 上会怎么计费」：这里用同样的规则在本地算一遍。
package meter

import (
	"bytes"
	"encoding/base64"
	"hash/maphash"
	"image"
	_ "image/gif" // DecodeConfig 识别格式
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"strings"
	"sync"

	"kiro-proxy/internal/tokenizer"
	"kiro-proxy/internal/turn"
)

// tokenScale 把 Claude 2 分词器的计数换算到现行 Claude 模型。
//
// 标定：30 份真实语料（Go / JS / HTML / JSON / YAML / 中英文 Markdown / Claude Code 的 system 与工具说明）
// 发到 Kiro，用 contextUsagePercentage × 窗口减去空请求的基线得到上游真实 token，最小二乘得 1.10。
// 合计偏差 < 0.1%，单份平均 4%；偏差最大的是整段 JSON（约 −13%）。
const tokenScale = 1.10

// Count 估算一段文字在现行 Claude 模型上的 token 数。
func Count(s string) int {
	if s == "" {
		return 0
	}
	return max(1, int(math.Round(float64(rawCount(s))*tokenScale)))
}

// 同一会话每轮重发全部历史：长段按内容记住计数，免得每轮重新分词。
const (
	memoMin     = 2 << 10 // 短于此直接算
	memoEntries = 1 << 14
)

var memo = struct {
	sync.Mutex
	seed maphash.Seed
	m    map[uint64]int
}{seed: maphash.MakeSeed(), m: map[uint64]int{}}

func rawCount(s string) int {
	if len(s) < memoMin {
		return tokenizer.Count(s)
	}
	k := maphash.String(memo.seed, s)
	memo.Lock()
	n, ok := memo.m[k]
	memo.Unlock()
	if ok {
		return n
	}
	n = tokenizer.Count(s)
	memo.Lock()
	if len(memo.m) >= memoEntries {
		clear(memo.m)
	}
	memo.m[k] = n
	memo.Unlock()
	return n
}

// 图片 token：Anthropic 的公式 宽×高/750，长边超过 1568 或超过约 1.15MP 时先缩放，故上限约 1600。
const (
	imageMaxTokens     = 1600
	imageUnknownTokens = 1600 // 读不出尺寸时按上限计
	imageMaxEdge       = 1568
)

// Image 估算一张 base64 图片的 token 数。只解析文件头，不解码像素。
func Image(b64 string) int {
	// 尺寸在文件头；JPEG 的 SOF 可能在 EXIF 之后，取前 256KB 足够
	if len(b64) > 256<<10 {
		b64 = b64[:256<<10]
	}
	b64 = b64[:len(b64)/4*4]
	r := base64.NewDecoder(base64.StdEncoding, strings.NewReader(b64))
	head, _ := io.ReadAll(r)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(head))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return imageUnknownTokens
	}
	w, h := float64(cfg.Width), float64(cfg.Height)
	if edge := max(w, h); edge > imageMaxEdge {
		w, h = w*imageMaxEdge/edge, h*imageMaxEdge/edge
	}
	return min(imageMaxTokens, max(1, int(w*h/750)))
}

// Scale1h 把本地拆出的 1h 写入按同样的占比搬到新的写入总量上（校准后、或上游报了真实 cacheWrite 时）。
// 上游不分 TTL，1h 只能按客户端声明在本地拆。
func Scale1h(local1h, localWrite, write int) int {
	if local1h <= 0 || localWrite <= 0 || write <= 0 {
		return 0
	}
	if local1h >= localWrite {
		return write
	}
	return min(write, int(math.Round(float64(local1h)*float64(write)/float64(localWrite))))
}

// Calibrate 用上游报的上下文 token 校正本地估算。
//
// upstream 是上游这一轮的上下文占用（百分比×窗口：含本轮输出，含 Kiro 自带部分），hidden 是 Kiro 自带部分。
// 两者之差是本轮请求加输出的真实 token。本地估算的误差来自分词器不同（Claude 系约 ±4%，开放模型约多两成），
// 各部分同向，所以输入三分与输出按同一比例缩放，总量对齐上游。
// 比例离谱（窗口或基线不对）时不动，仍用本地估算。
func Calibrate(u turn.Usage, upstream, hidden int) turn.Usage {
	content := upstream - hidden
	local := u.PromptTokens() + u.Output
	if content <= 0 || local <= 0 {
		return u
	}
	k := float64(content) / float64(local)
	if k < 0.5 || k > 2 {
		return u
	}
	scale := func(n int) int { return int(math.Round(float64(n) * k)) }
	out := u
	out.Output = scale(u.Output)
	out.Reasoning = min(scale(u.Reasoning), out.Output)
	prompt := content - out.Output
	out.CacheRead = min(scale(u.CacheRead), prompt)
	out.CacheWrite = min(scale(u.CacheWrite), prompt-out.CacheRead)
	out.CacheWrite1h = Scale1h(u.CacheWrite1h, u.CacheWrite, out.CacheWrite)
	out.Input = prompt - out.CacheRead - out.CacheWrite // 舍入差归到普通输入，总量与上游一致
	return out
}
