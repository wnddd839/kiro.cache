package kiro

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
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

// Usage 是上游报告的 token 记账。CacheRead / CacheWrite 是判断 cache 是否打中的依据。
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
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
}

// NewDecoder 建解码器。thinking 为 true 时把开头的 <thinking>…</thinking> 拆成 think 事件。
func NewDecoder(r io.Reader, thinking bool, window int, names map[string]string) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10), thinking: thinking, window: window, names: names}
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
	if u.Input == 0 && d.pct > 0 && d.window > 0 {
		u.Input = int(d.pct / 100 * float64(d.window))
	}
	if u.Output == 0 {
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
		var tu struct {
			Uncached   int     `json:"uncachedInputTokens"`
			Input      int     `json:"inputTokens"`
			Output     int     `json:"outputTokens"`
			CacheRead  int     `json:"cacheReadInputTokens"`
			CacheWrite int     `json:"cacheWriteInputTokens"`
			Pct        float64 `json:"contextUsagePercentage"`
		}
		if raw, ok := m["tokenUsage"]; ok && json.Unmarshal(raw, &tu) == nil {
			in := tu.Uncached
			if in == 0 {
				in = tu.Input
			}
			d.usage.Input += in
			d.usage.Output += tu.Output
			d.usage.CacheRead += tu.CacheRead
			d.usage.CacheWrite += tu.CacheWrite
			if tu.Pct > 0 {
				d.pct = tu.Pct
			}
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
