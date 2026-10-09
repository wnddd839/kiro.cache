package kiro

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
)

// EventKind 是解码后事件的类别。
type EventKind int

const (
	EvText EventKind = iota + 1
	EvThink
	EvSig
	EvToolStart
	EvToolArgs
	EvStop
	EvError
)

// Usage 是上游报告的记账。Kiro 实际只报 credits 与上下文百分比；token 数是估算或为 0。
// 下游的 token / 缓存计量在 meter 包里本地计算，再用上下文百分比校正总量。
type Usage struct {
	Input      int // tokenUsage 报了就用它，否则是上下文百分比×窗口：本轮全部上下文，含 Kiro 自带部分与本轮输出
	Output     int // tokenUsage 报了就用它，否则按输出字符估算
	CacheRead  int
	CacheWrite int

	Credits    float64 // meteringEvent 的 usage 之和
	ContextPct float64 // 上下文占用百分比（含 Kiro 自己的 system）
	// Reported 表示上游报了输入侧 tokenUsage 字段（uncached / input / cacheRead / cacheWrite）：
	// 显式 0 也是真实计数；只报 outputTokens 不覆盖本地输入拆分与上下文校准。
	Reported bool
	// OutputReported 表示 Output 是上游报的 outputTokens，不是按字符估算的。
	OutputReported bool

	// 以下只用于交叉核对与探测，不参与计费。
	TotalTokens int     // tokenUsage.totalTokens（最后一次报的）
	Normalized  float64 // tokenUsage.normalizedTokenUsage：MPS 按 credit 配置折算的用量
	UsageEvents int     // 带 tokenUsage 的事件个数；>1 时要确认是增量还是累计
	// UsageRaw 是每一条 tokenUsage 的原值（只在多于一条时保留，写调试日志用）
	UsageRaw       *[]string
	MeteringEvents int    // meteringEvent 个数
	MeteringUnit   string // meteringEvent.unit
}

// HiddenTokens 是 Kiro 自己放进上下文、不属于下游请求的 token（它的 system 与对话模板）。
// 实测（2026-10-07）：只发 "Reply with just: ok"、回 "ok" 时的上游上下文 token 减去这两段。
// 没测过的模型按 Claude 的值；基线差几百 token，对长对话的影响不到 1%。
func HiddenTokens(model string) int {
	if n, ok := hiddenTokens[strings.ToLower(model)]; ok {
		return n
	}
	return hiddenClaude
}

const hiddenClaude = 4052 // claude-sonnet-4.5 / claude-haiku-4.5 / claude-sonnet-4 / auto

var hiddenTokens = map[string]int{
	"deepseek-3.2":     3699,
	"minimax-m2.5":     3864,
	"minimax-m2.1":     3640,
	"glm-5":            3883,
	"qwen3-coder-next": 3646,
}

// Event 是一条解码后的回复事件。
type Event struct {
	Kind  EventKind
	Text  string // text / think / sig / toolArgs / error
	ID    string // toolStart
	Name  string // toolStart
	Stop  string // stop: end_turn / tool_use / max_tokens / refusal
	Usage Usage  // stop
}

// frame 是 AWS event stream 的一条消息。
type frame struct {
	headers map[string]string
	payload []byte
}

const maxFrame = 16 << 20

var errMalformed = errors.New("malformed event stream")

// readFrame 读一条消息：总长、头长、prelude CRC、头、payload、整体 CRC。
// CRC 不校验：TLS 已保证完整性，校验只会多一份失败路径。
func readFrame(r *bufio.Reader) (frame, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return frame{}, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	hlen := binary.BigEndian.Uint32(prelude[4:8])
	if total < 16 || total > maxFrame || hlen > total-16 {
		return frame{}, errMalformed
	}
	rest := make([]byte, total-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return frame{}, err
	}
	return frame{headers: parseHeaders(rest[:hlen]), payload: rest[hlen : len(rest)-4]}, nil
}

// parseHeaders 只保留字符串头，其它类型按长度跳过。
func parseHeaders(h []byte) map[string]string {
	out := map[string]string{}
	for len(h) > 0 {
		n := int(h[0])
		if len(h) < 2+n {
			return out
		}
		name, typ := string(h[1:1+n]), h[1+n]
		h = h[2+n:]
		var size int
		switch typ {
		case 0, 1: // bool
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7: // bytes / string：2 字节长度 + 内容
			if len(h) < 2 {
				return out
			}
			l := int(binary.BigEndian.Uint16(h))
			if len(h) < 2+l {
				return out
			}
			if typ == 7 {
				out[name] = string(h[2 : 2+l])
			}
			h = h[2+l:]
			continue
		default:
			return out
		}
		if len(h) < size {
			return out
		}
		h = h[size:]
	}
	return out
}

// Decoder 把 Kiro 的 event stream 转成 Event。
type Decoder struct {
	r        *bufio.Reader
	thinking bool
	window   int               // 模型上下文长度，用于从百分比估算 input
	names    map[string]string // 改写后工具名 → 原名

	think   thinkParser
	queue   []Event
	done    bool
	tool    string
	tools   int
	stop    string
	usage   Usage
	pct     float64
	said    int
	failure string

	lastUsage string           // 上一条 tokenUsage 原值
	tokens    tokenUsageFields // 输入侧各字段最后一次上报的值，用于推算 uncached

	// Mode 是多条 tokenUsage 的合并方式：只有 ReportedSum 累加，其它取各字段最后一次上报的值。
	Mode string

	// Trace 非 nil 时收到每一帧的事件类型与原始 payload（探测工具用）
	Trace func(event string, payload []byte)
}

// NewDecoder 建解码器。thinking 为 true 时把开头的 <thinking>…</thinking> 拆成 think 事件。
func NewDecoder(r io.Reader, thinking bool, window int, names map[string]string) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10), thinking: thinking, window: window, names: names}
}

// tokenUsageFields 是一条 tokenUsage。指针区分「字段缺失」与「值为 0」。
type tokenUsageFields struct {
	Uncached   *int     `json:"uncachedInputTokens"`
	Input      *int     `json:"inputTokens"`
	Output     *int     `json:"outputTokens"`
	CacheRead  *int     `json:"cacheReadInputTokens"`
	CacheWrite *int     `json:"cacheWriteInputTokens"`
	Pct        *float64 `json:"contextUsagePercentage"`
	Total      *int     `json:"totalTokens"`
	Normalized *float64 `json:"normalizedTokenUsage"`
}

func val(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// tokenUsage 记一条 tokenUsage。
//
// 默认不累加，取各字段最后一次上报的值：缺失保留旧值，显式 0 覆盖旧值。
// 上游若发的是累计值，累加会多收；只有确认是增量后才用 ReportedSum。
// 每条原值留在 UsageRaw 里，调用方告警并写调试日志。credits（meteringEvent）照常累加。
//
// uncachedInputTokens 缺失时用 inputTokens − cacheRead − cacheWrite 推算（inputTokens 可能是含缓存的总输入）；
// 字段在但是 0 就是 0。
func (d *Decoder) tokenUsage(raw json.RawMessage) {
	var tu tokenUsageFields
	if json.Unmarshal(raw, &tu) != nil {
		return
	}
	d.usage.UsageEvents++
	if d.usage.UsageEvents == 2 {
		d.usage.UsageRaw = &[]string{d.lastUsage}
	}
	if d.usage.UsageEvents >= 2 {
		*d.usage.UsageRaw = append(*d.usage.UsageRaw, string(raw))
	}
	d.lastUsage = string(raw)
	if tu.Total != nil {
		d.usage.TotalTokens = *tu.Total
	}
	if tu.Normalized != nil {
		d.usage.Normalized = *tu.Normalized
	}
	reported := tu.Uncached != nil || tu.Input != nil || tu.CacheRead != nil || tu.CacheWrite != nil
	if d.Mode != ReportedSum {
		if tu.Uncached != nil {
			d.tokens.Uncached = tu.Uncached
		}
		if tu.Input != nil {
			d.tokens.Input = tu.Input
		}
		if tu.CacheRead != nil {
			d.tokens.CacheRead = tu.CacheRead
		}
		if tu.CacheWrite != nil {
			d.tokens.CacheWrite = tu.CacheWrite
		}
		tu.Uncached, tu.Input = d.tokens.Uncached, d.tokens.Input
		tu.CacheRead, tu.CacheWrite = d.tokens.CacheRead, d.tokens.CacheWrite
	}
	read, write := val(tu.CacheRead), val(tu.CacheWrite)
	var in int
	switch {
	case tu.Uncached != nil:
		in = *tu.Uncached
	case tu.Input != nil:
		in = max(0, *tu.Input-read-write)
	}
	if d.Mode == ReportedSum {
		d.usage.Input += in
		d.usage.CacheRead += read
		d.usage.CacheWrite += write
		d.usage.Output += val(tu.Output)
	} else {
		d.usage.Input, d.usage.CacheRead, d.usage.CacheWrite = in, read, write
		if tu.Output != nil {
			d.usage.Output = *tu.Output
		}
	}
	d.usage.Reported = d.usage.Reported || reported
	d.usage.OutputReported = d.usage.OutputReported || tu.Output != nil
	if tu.Pct != nil && *tu.Pct > 0 {
		d.pct = *tu.Pct
	}
}

// ReportedMode 是上游报了 tokenUsage 时怎么用它。等 `probe usage` 有结论再定口径；
// 默认 ReportedConservative：各字段取最后值、不累加、保留 Kiro 自带输入。
const (
	// ReportedConservative：各字段取最后一次上报的值；输入侧包含 Kiro 自带部分，不扣除。
	ReportedConservative = "conservative"
	// ReportedRaw：兼容旧配置，与 conservative 同样保留上游报告值。
	ReportedRaw = "raw"
	// ReportedSum：多条累加（确认上游发的是增量后再用），不扣 Kiro 自带输入。
	ReportedSum = "sum"
	// ReportedIgnore：不用上游 tokenUsage，只用本地拆分 + 上下文百分比校准。
	ReportedIgnore = "ignore"
)

// Partial 是到目前为止上游已报的记账（credits、tokenUsage），用于中途断开时入账。
// 不能与 Next 并发调用。
func (d *Decoder) Partial() Usage {
	u := d.usage
	u.ContextPct = d.pct
	return u
}

// Next 返回下一条事件。流结束后返回 io.EOF；最后一条总是 EvStop 或 EvError。
func (d *Decoder) Next() (Event, error) {
	for len(d.queue) == 0 {
		if d.done {
			return Event{}, io.EOF
		}
		d.pump()
	}
	ev := d.queue[0]
	d.queue = d.queue[1:]
	return ev, nil
}

func (d *Decoder) pump() {
	f, err := readFrame(d.r)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			d.failure = "the reply broke off: " + err.Error()
		}
		d.finish()
		return
	}
	if d.Trace != nil {
		d.Trace(f.headers[":event-type"], f.payload)
	}
	evs, failed := d.frame(f)
	d.queue = append(d.queue, evs...)
	if failed != "" {
		d.failure = failed
		d.finish()
	}
}

func (d *Decoder) finish() {
	d.done = true
	d.queue = append(d.queue, d.think.end()...)
	if d.failure != "" {
		d.queue = append(d.queue, Event{Kind: EvError, Text: d.failure})
		return
	}
	u := d.usage
	u.ContextPct = d.pct
	if !u.Reported && u.Input == 0 && d.pct > 0 && d.window > 0 {
		u.Input = int(math.Round(d.pct / 100 * float64(d.window)))
	}
	if !u.OutputReported && u.Output == 0 {
		u.Output = (d.said + 3) / 4
	}
	stop := "end_turn"
	switch {
	case d.tools > 0:
		stop = "tool_use"
	case strings.EqualFold(d.stop, "MAX_TOKENS"):
		stop = "max_tokens"
	case strings.EqualFold(d.stop, "CONTENT_FILTERED"):
		stop = "refusal"
	}
	d.queue = append(d.queue, Event{Kind: EvStop, Stop: stop, Usage: u})
}

func (d *Decoder) frame(f frame) (evs []Event, failed string) {
	if mt := f.headers[":message-type"]; mt == "exception" || mt == "error" {
		kind := f.headers[":exception-type"]
		if kind == "" {
			kind = f.headers[":error-code"]
		}
		return nil, exception(kind, f.payload)
	}
	kind := f.headers[":event-type"]
	var m map[string]json.RawMessage
	if json.Unmarshal(f.payload, &m) != nil {
		return nil, ""
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	switch kind {
	case "assistantResponseEvent":
		text := str("content")
		d.said += len(text)
		if d.thinking {
			return d.think.feed(text), ""
		}
		if text != "" {
			return []Event{{Kind: EvText, Text: text}}, ""
		}
	case "reasoningContentEvent":
		if t := str("text"); t != "" {
			d.said += len(t)
			return []Event{{Kind: EvThink, Text: t}}, ""
		}
		if s := str("signature"); s != "" {
			return []Event{{Kind: EvSig, Text: s}}, ""
		}
	case "toolUseEvent":
		id, name := str("toolUseId"), str("name")
		if orig, ok := d.names[name]; ok {
			name = orig
		}
		if id != "" && id != d.tool {
			evs = append(evs, d.think.end()...) // 调用前的文字已说完
			d.tool = id
			d.tools++
			evs = append(evs, Event{Kind: EvToolStart, ID: id, Name: name})
		}
		if raw := m["input"]; len(raw) > 0 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				if s != "" {
					evs = append(evs, Event{Kind: EvToolArgs, Text: s})
				}
			} else if v := string(raw); v != "{}" && v != "null" {
				evs = append(evs, Event{Kind: EvToolArgs, Text: v})
			}
			d.said += len(raw)
		}
		return evs, ""
	case "metadataEvent", "messageMetadataEvent":
		if s := str("stopReason"); s != "" {
			d.stop = s
		}
		if raw, ok := m["tokenUsage"]; ok {
			d.tokenUsage(raw)
		}
	case "meteringEvent":
		var credits float64
		d.usage.MeteringEvents++
		if u := str("unit"); u != "" {
			d.usage.MeteringUnit = u
		}
		if json.Unmarshal(m["usage"], &credits) == nil && credits > 0 {
			d.usage.Credits += credits
		}
	case "contextUsageEvent":
		var pct float64
		if json.Unmarshal(m["contextUsagePercentage"], &pct) == nil && pct > 0 {
			d.pct = pct
		}
	case "error", "throttlingError", "validationError", "serviceUnavailableError", "internalServerException":
		return nil, exception(kind, f.payload)
	}
	return nil, ""
}

func exception(kind string, payload []byte) string {
	var e struct {
		Message string `json:"message"`
		Upper   string `json:"Message"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(payload, &e)
	msg := cmp.Or(e.Message, e.Upper, strings.TrimSpace(string(payload)))
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	switch {
	case strings.Contains(strings.ToLower(kind), "throttl"):
		return "rate limited: " + msg
	case kind != "":
		return kind + ": " + msg
	}
	return msg
}

// thinkParser 把开头的 <thinking>…</thinking> 拆出来，无论标签怎么跨分片。
type thinkParser struct {
	state int // 0 未知，1 thinking 中，2 正文
	buf   string
	said  bool
}

const (
	thinkOpen  = "<thinking>"
	thinkClose = "</thinking>"
)

func (p *thinkParser) feed(s string) []Event {
	p.buf += s
	var out []Event
	for {
		switch p.state {
		case 0:
			t := strings.TrimLeft(p.buf, " \t\r\n")
			if rest, ok := strings.CutPrefix(t, thinkOpen); ok {
				p.buf, p.state = rest, 1
				continue
			}
			if len(t) < len(thinkOpen) && strings.HasPrefix(thinkOpen, t) {
				return out // 可能还是标签
			}
			p.state = 2
		case 1:
			if !p.said {
				if p.buf = strings.TrimLeft(p.buf, "\r\n"); p.buf == "" {
					return out
				}
			}
			if before, after, ok := strings.Cut(p.buf, thinkClose); ok {
				if before != "" {
					out = append(out, Event{Kind: EvThink, Text: before})
					p.said = true
				}
				p.buf, p.state = strings.TrimLeft(after, "\r\n"), 2
				continue
			}
			if n := len(p.buf) - partialSuffix(p.buf, thinkClose); n > 0 {
				out = append(out, Event{Kind: EvThink, Text: p.buf[:n]})
				p.buf, p.said = p.buf[n:], true
			}
			return out
		default:
			if p.buf != "" {
				out = append(out, Event{Kind: EvText, Text: p.buf})
				p.buf = ""
			}
			return out
		}
	}
}

func (p *thinkParser) end() []Event {
	if p.buf == "" {
		return nil
	}
	kind := EvText
	if p.state == 1 {
		kind = EvThink
	}
	ev := Event{Kind: kind, Text: p.buf}
	p.buf = ""
	if p.state == 0 {
		p.state = 2
	}
	return []Event{ev}
}

func partialSuffix(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
