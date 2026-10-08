package meter

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-proxy/internal/anthropic"
)

// 缓存规则取 Anthropic API：前缀顺序 tools → system → messages；
// 在断点处把「到这里为止的前缀」写入缓存；之后的请求前缀相同即命中。
const (
	// MinCacheable 是可缓存前缀最小 token 数的默认值（Sonnet / Opus 为 1024）。
	// 上游模型目录报了 minimumTokensPerCacheCheckpoint 时以它为准，见 Prompt.MinTokens。
	MinCacheable = 1024
	defaultTTL   = 5 * time.Minute
	longTTL      = time.Hour
	// lookback 是一个断点向前查找命中的最多块数。
	lookback = 20
	// maxEntriesPerScope 限制每个号的缓存条目，丢了只影响计量。
	maxEntriesPerScope = 4096
)

// Mode 决定断点怎么取。
const (
	// ModeProtocol：按下游协议选。Anthropic Messages 用 explicit（认 cache_control，下游可按官方规则复核），
	// OpenAI Chat / Responses 用 auto（这两个协议没有 cache_control，官方是自动前缀缓存）。
	ModeProtocol = "protocol"
	// ModeAuto：Kiro 自动前缀缓存。客户端 cache_control 被上游丢弃，本地也不认；
	// 在 system 末尾与最后一块自动加 5 分钟滑动 TTL 断点（命中刷新）。
	ModeAuto = "auto"
	// ModeExplicit：只认 cache_control，与 Anthropic API 完全一致（含 1h TTL）。
	// 仅当下游明确按 Anthropic 规则记账时用；Kiro 实际上不会按这些断点缓存。
	ModeExplicit = "explicit"
	// ModeOff：不模拟缓存，全部计为普通输入。上游 promptCaching.supported=false 的模型走这里。
	ModeOff = "off"
)

// Prompt 是请求前缀按块切开后的形状：每块累计 token 与累计指纹。
type Prompt struct {
	Tokens int // 全部输入 token
	// ToolSystem 是 Tokens 里 Anthropic 工具调用 system 的部分（有工具时才有）。
	ToolSystem int
	cum        []int
	hash       []uint64
	marks      []mark // 断点，按位置升序
	sysEnd     int    // tools+system 的块数
	// MinTokens 是可写缓存断点的最小累计 token 数；0 用 MinCacheable。
	// 对下游计费用 Anthropic 官方分模型表（AnthropicMinCacheable），与 Kiro 的成本侧最小长度无关。
	MinTokens int
	// Warnings 是不合 Anthropic 规则的断点（超过 4 个、1h 排在 5m 后面）。
	// 官方会回 400；这里只记下并按不多扣的方式计费：多出的断点忽略，5m 之后的 1h 降成 5m。
	Warnings []string
}

// AnthropicMinCacheable 是 Anthropic 官方分模型的最小可缓存长度（用于对下游计费）。
// 模型名按 Kiro 写法；未知模型返回 MinCacheable。
// 来源：https://docs.claude.com/en/docs/build-with-claude/prompt-caching（2026 年版本）。
func AnthropicMinCacheable(model string) int {
	m := CanonModel(model)
	for _, r := range anthropicMin {
		if m == r.prefix || strings.HasPrefix(m, r.prefix+"-") {
			return r.min
		}
	}
	return MinCacheable
}

// anthropicMin 按前缀匹配，具体版本在前。
var anthropicMin = []struct {
	prefix string
	min    int
}{
	{"claude-fable-5.1", 512}, {"claude-mythos-5.1", 512}, {"claude-opus-5.5", 512}, {"claude-sonnet-5.5", 512},
	{"claude-fable-5", 512}, {"claude-mythos-5", 512}, {"claude-opus-5", 512},
	{"claude-mythos-preview", 2048}, {"claude-opus-4.7", 2048}, {"claude-haiku-3.5", 2048}, {"claude-3-5-haiku", 2048},
	{"claude-opus-4.6", 4096}, {"claude-opus-4.5", 4096}, {"claude-haiku-4.5", 4096},
	{"claude-opus-4.8", 1024}, {"claude-sonnet-5", 1024}, {"claude-sonnet-4.6", 1024}, {"claude-sonnet-4.5", 1024},
	{"claude-opus-4.1", 1024}, {"claude-opus-4", 1024}, {"claude-sonnet-4", 1024},
}

func (p *Prompt) minTokens() int {
	if p.MinTokens > 0 {
		return p.MinTokens
	}
	return MinCacheable
}

type mark struct {
	at  int // 块下标（含）
	ttl time.Duration
	// deep 表示自动断点：向前查到开头，不受 20 块限制（对应 OpenAI 的自动前缀缓存）。
	deep bool
}

// Analyze 把请求切成块并算累计 token / 指纹。seed 区分不同模型与 thinking 设置。
// extra 是不在请求里、但会发给上游的前缀 token（如 thinking 标签），计入第一块。
func Analyze(req *anthropic.Request, seed string, extra int, mode string) *Prompt {
	p := &Prompt{}
	h := sha256.Sum256([]byte(seed))
	cur := binary.LittleEndian.Uint64(h[:8])
	total := extra
	add := func(kind string, v any, tokens int, cc *anthropic.CacheControl) {
		b, _ := json.Marshal(v)
		sum := sha256.New()
		_ = binary.Write(sum, binary.LittleEndian, cur)
		sum.Write([]byte(kind))
		sum.Write(b)
		cur = binary.LittleEndian.Uint64(sum.Sum(nil)[:8])
		total += tokens
		p.cum = append(p.cum, total)
		p.hash = append(p.hash, cur)
		// auto：Kiro 丢弃客户端 cache_control，本地也不认，后面统一加 5m 断点。
		if cc != nil && mode == ModeExplicit {
			ttl := defaultTTL
			if cc.TTL == "1h" {
				ttl = longTTL
			}
			p.marks = append(p.marks, mark{at: len(p.cum) - 1, ttl: ttl})
		}
	}

	for i, t := range req.Tools {
		n := Count(t.Name) + Count(t.Description) + Count(string(t.InputSchema)) + toolOverhead
		if i == 0 {
			p.ToolSystem = ToolSystemTokens
			n += ToolSystemTokens
		}
		cc := t.CacheControl
		t.CacheControl = nil // 指纹不含断点：改 TTL / 挪断点不该让整段工具前缀失配
		add("tool", t, n, cc)
	}
	for _, b := range req.System {
		add("system", b.Text, Count(b.Text), b.CacheControl)
	}
	p.sysEnd = len(p.cum)
	for _, m := range req.Messages {
		for i, b := range m.Content {
			n := blockTokens(b)
			if i == 0 {
				n += messageOverhead
			}
			add(m.Role, stable(b), n, b.CacheControl)
		}
	}
	if len(p.cum) == 0 {
		add("empty", "", 0, nil)
	}
	p.Tokens = total
	if mode == ModeExplicit {
		if cc := req.CacheControl; cc != nil {
			p.autoMark(cc)
		}
		p.normalizeMarks()
	}

	if mode == ModeAuto {
		if p.sysEnd > 0 && p.sysEnd < len(p.cum) {
			p.marks = append(p.marks, mark{at: p.sysEnd - 1, ttl: defaultTTL, deep: true})
		}
		p.marks = append(p.marks, mark{at: len(p.cum) - 1, ttl: defaultTTL, deep: true})
	}
	return p
}

// MaxBreakpoints 是 Anthropic 一个请求里的 cache_control 断点上限。
const MaxBreakpoints = 4

// autoMark 是 Anthropic 的顶层 cache_control（自动缓存）：断点放在最后一块，占一个断点名额。
// 最后一块已有同 TTL 的显式断点时什么也不做；TTL 不同官方回 400，这里保留显式断点并记警告。
func (p *Prompt) autoMark(cc *anthropic.CacheControl) {
	ttl := defaultTTL
	if cc.TTL == "1h" {
		ttl = longTTL
	}
	last := len(p.cum) - 1
	if n := len(p.marks); n > 0 && p.marks[n-1].at == last {
		if p.marks[n-1].ttl != ttl {
			p.Warnings = append(p.Warnings, "top-level cache_control TTL differs from the last block's; using the block's")
		}
		return
	}
	p.marks = append(p.marks, mark{at: last, ttl: ttl})
}

// CapTTL 把长于 ttl 的断点降到 ttl（cache_ttl=5m：客户端声明的 1h 一律按 5m 计）。
func (p *Prompt) CapTTL(ttl time.Duration) {
	for i := range p.marks {
		p.marks[i].ttl = min(p.marks[i].ttl, ttl)
	}
}

// normalizeMarks 把客户端断点整理成 Anthropic 接受的形状。官方对这些情况回 400，
// 分发层不拒请求，只按对下游最便宜的解释计费并留警告：
//   - 超过 4 个：保留前 4 个，其余忽略（写入不会变长）；
//   - 1h 排在 5m 后面：那个 1h 降成 5m（1h 写入是 2 倍，5m 是 1.25 倍）。
func (p *Prompt) normalizeMarks() {
	if len(p.marks) > MaxBreakpoints {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%d cache_control breakpoints; only the first %d are billed", len(p.marks), MaxBreakpoints))
		p.marks = p.marks[:MaxBreakpoints]
	}
	seen5m := false
	for i := range p.marks {
		switch {
		case p.marks[i].ttl != longTTL:
			seen5m = true
		case seen5m:
			p.marks[i].ttl = defaultTTL
			p.Warnings = append(p.Warnings, "1h cache_control after a 5m breakpoint; billed as 5m")
		}
	}
}

// 协议开销，按 Kiro 实测标定：每条消息约 4，每个工具声明在名字、说明、schema 之外约 24，
// 历史里的 tool_use / tool_result 各约 22。
const (
	messageOverhead = 4
	toolOverhead    = 24
	toolCallWrap    = 22
	// ToolSystemTokens 是 Anthropic API 在请求带工具时加的工具调用 system（Claude 4.x，tool_choice auto）。
	ToolSystemTokens = 346
)

func blockTokens(b anthropic.Block) int {
	switch b.Type {
	case "text":
		return Count(b.Text)
	case "thinking":
		// 历史 thinking 不发给 Kiro，API 上早先轮次的 thinking 也不计入输入
		return 0
	case "tool_use":
		return Count(b.Name) + Count(string(b.Input)) + toolCallWrap
	case "tool_result":
		n := toolCallWrap
		for _, sub := range b.Content {
			n += blockTokens(sub)
		}
		return n
	case "image":
		if b.Source != nil && b.Source.Type == "base64" {
			return Image(b.Source.Data)
		}
		return imageUnknownTokens
	case "document":
		if b.Source == nil {
			return 0
		}
		if b.Source.Type == "text" {
			return Count(b.Source.Data)
		}
		n := 0
		for _, sub := range b.Source.Content {
			n += blockTokens(sub)
		}
		return n
	}
	return Count(b.Text)
}

// stable 是块的指纹内容：去掉 cache_control（它不属于前缀内容）与 thinking（不发给上游）。
func stable(b anthropic.Block) anthropic.Block {
	b.CacheControl = nil
	if b.Type == "thinking" || b.Type == "redacted_thinking" {
		return anthropic.Block{Type: b.Type}
	}
	if len(b.Content) > 0 {
		c := make(anthropic.Content, len(b.Content))
		for i, sub := range b.Content {
			c[i] = stable(sub)
		}
		b.Content = c
	}
	return b
}

// Split 是输入侧的三分：普通输入、缓存读、缓存写。
type Split struct {
	Input      int
	CacheRead  int
	CacheWrite int
	// CacheWrite1h 是 CacheWrite 里按 1 小时 TTL 写入的部分（价格不同）。
	CacheWrite1h int
}

// Cache 是本地 prompt cache 模拟器，按 scope（号）隔离。零值不可用，用 NewCache。
type Cache struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]map[uint64]entry // scope → 前缀指纹 → 条目
}

// entry 是一个缓存条目。ttl 是它自己的寿命：命中时按它续期（1h 条目命中后还是 1h）。
type entry struct {
	exp time.Time
	ttl time.Duration
}

// NewCache 建模拟器。now 为空时用 time.Now。
func NewCache(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{now: now, m: map[string]map[uint64]entry{}}
}

// Peek 只算不写：给预估费用用。
func (c *Cache) Peek(scope string, p *Prompt) Split { return c.apply(scope, p, false) }

// Commit 计算命中并把断点写入缓存。在成功应答或已确认输入被处理的失败尝试后调用。
func (c *Cache) Commit(scope string, p *Prompt) Split { return c.apply(scope, p, true) }

func (c *Cache) apply(scope string, p *Prompt, write bool) Split {
	if len(p.marks) == 0 {
		return Split{Input: p.Tokens}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	store := c.m[scope]

	// 命中：每个断点各自向前至多 lookback 块，取最长的仍有效的前缀。
	// hits 是各断点查到的命中：都要按各自的 TTL 续期。
	hit := -1
	var hits []int
	for _, mk := range p.marks {
		low := max(0, mk.at-lookback+1)
		if mk.deep {
			low = 0
		}
		for i := mk.at; i >= low; i-- {
			if e, ok := store[p.hash[i]]; ok && now.Before(e.exp) {
				hits = append(hits, i)
				hit = max(hit, i)
				break
			}
		}
	}
	var s Split
	minTok := p.minTokens()
	if hit >= 0 {
		s.CacheRead = p.cum[hit]
	}

	// 写入：命中点之后、够长的断点。最后一个可写断点之前的新内容都算写入。
	wroteTo, wroteLong := s.CacheRead, s.CacheRead
	for _, mk := range p.marks {
		if mk.at <= hit || p.cum[mk.at] < minTok {
			continue
		}
		if p.cum[mk.at] > wroteTo {
			wroteTo = p.cum[mk.at]
		}
		if mk.ttl == longTTL && p.cum[mk.at] > wroteLong {
			wroteLong = p.cum[mk.at]
		}
	}
	s.CacheWrite = wroteTo - s.CacheRead
	s.CacheWrite1h = max(0, wroteLong-s.CacheRead)
	s.Input = p.Tokens - s.CacheRead - s.CacheWrite

	if !write {
		return s
	}
	if store == nil {
		store = map[uint64]entry{}
		c.m[scope] = store
	}
	// 命中续期：每个查到的命中按它自己的 TTL（Anthropic：用到即刷新）
	for _, i := range hits {
		e := store[p.hash[i]]
		if e.ttl <= 0 {
			e.ttl = defaultTTL
		}
		if exp := now.Add(e.ttl); exp.After(e.exp) {
			e.exp = exp
		}
		store[p.hash[i]] = e
	}
	// 新断点写入；同一前缀已有更长的条目就保留长的
	for _, mk := range p.marks {
		if mk.at > hit && p.cum[mk.at] >= minTok {
			h := p.hash[mk.at]
			if e := store[h]; e.exp.Before(now.Add(mk.ttl)) {
				ttl := mk.ttl
				if now.Before(e.exp) {
					ttl = max(ttl, e.ttl) // 还活着的更长条目保留它的 TTL；过期了就是新写入，用这次的
				}
				store[h] = entry{exp: now.Add(mk.ttl), ttl: ttl}
			}
		}
	}
	if len(store) > maxEntriesPerScope {
		for k, e := range store {
			if !now.Before(e.exp) {
				delete(store, k)
			}
		}
		for k := range store { // 仍超限：随机丢（map 遍历顺序随机）
			if len(store) <= maxEntriesPerScope {
				break
			}
			delete(store, k)
		}
	}
	return s
}

// Forget 丢掉一个号的全部缓存（号被删除时）。
func (c *Cache) Forget(scope string) {
	c.mu.Lock()
	delete(c.m, scope)
	c.mu.Unlock()
}

// snapshot 是落盘形状：scope → 前缀指纹（16 进制）→ [过期时间（Unix 毫秒）, TTL（秒）]。
// 旧格式的值是单个过期时间，载入时按剩余寿命推 TTL。
type snapshot map[string]map[string]json.RawMessage

// Save 把仍有效的条目写到 path（先写临时文件再改名）。重启后 Load 回来，
// 进程重启不会让所有会话的下一轮都变成整段缓存写入。
func (c *Cache) Save(path string) error {
	c.mu.Lock()
	now := c.now()
	snap := snapshot{}
	for scope, store := range c.m {
		for h, e := range store {
			if !now.Before(e.exp) {
				continue
			}
			if snap[scope] == nil {
				snap[scope] = map[string]json.RawMessage{}
			}
			v, _ := json.Marshal([2]int64{e.exp.UnixMilli(), int64(e.ttl / time.Second)})
			snap[scope][strconv.FormatUint(h, 16)] = v
		}
	}
	c.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".promptcache-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Load 读回 Save 的文件，跳过已过期条目；文件不存在不算错。返回载入的条目数。
func (c *Cache) Load(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return 0, fmt.Errorf("prompt cache %s: %w", path, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for scope, entries := range snap {
		for hs, raw := range entries {
			h, err := strconv.ParseUint(hs, 16, 64)
			if err != nil {
				continue
			}
			e, ok := decodeEntry(raw, now)
			if !ok || !now.Before(e.exp) {
				continue
			}
			store := c.m[scope]
			if store == nil {
				store = map[uint64]entry{}
				c.m[scope] = store
			}
			if e.exp.After(store[h].exp) {
				store[h] = e
			}
			n++
		}
	}
	return n, nil
}

// decodeEntry 读一个快照值：[过期毫秒, TTL 秒]，或旧格式的单个过期毫秒。
func decodeEntry(raw json.RawMessage, now time.Time) (entry, bool) {
	var pair [2]int64
	if json.Unmarshal(raw, &pair) == nil {
		ttl := time.Duration(pair[1]) * time.Second
		if ttl <= 0 {
			ttl = defaultTTL
		}
		return entry{exp: time.UnixMilli(pair[0]), ttl: ttl}, true
	}
	var ms int64
	if json.Unmarshal(raw, &ms) != nil {
		return entry{}, false
	}
	exp := time.UnixMilli(ms)
	ttl := defaultTTL
	if exp.Sub(now) > defaultTTL {
		ttl = longTTL // 剩余超过 5 分钟的只可能是 1h 条目
	}
	return entry{exp: exp, ttl: ttl}, true
}
