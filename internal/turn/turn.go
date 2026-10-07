// Package turn 是一次回复的协议无关形状：上游解码出的事件流写进 Sink，
// 各下游协议（Anthropic Messages、OpenAI Chat Completions、OpenAI Responses）各自实现 Sink。
package turn

import "encoding/json"

// Stop 原因，取 Anthropic 的命名；各协议自己映射。
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
)

// Usage 是返回给下游的记账。Kiro 不报 token 数：本地估算分出各部分，再按上游上下文占用校正总量。
//
// 输入侧三项互不重叠：PromptTokens = Input + CacheRead + CacheWrite。
type Usage struct {
	Input      int // 未命中、也不写入缓存的输入
	CacheRead  int // 命中缓存的前缀
	CacheWrite int // 本次写入缓存的输入
	// CacheWrite1h 是 CacheWrite 里按 1 小时 TTL 写入的部分（2 倍输入价）；其余是 5 分钟写入。
	CacheWrite1h int
	Output       int
	Reasoning    int // Output 中 thinking 的部分

	Credits float64 // Kiro 实际计量的 credits
	CostUSD float64 // 内部记账（key 预算 LimitUSD）；不下发给下游
}

// PromptTokens 是全部输入 token。
func (u Usage) PromptTokens() int { return u.Input + u.CacheRead + u.CacheWrite }

// CacheWrite5m 是按 5 分钟 TTL 写入的部分。
func (u Usage) CacheWrite5m() int { return u.CacheWrite - min(max(u.CacheWrite1h, 0), u.CacheWrite) }

// HitRate 是缓存命中占全部输入的比例。
func (u Usage) HitRate() float64 {
	if p := u.PromptTokens(); p > 0 {
		return float64(u.CacheRead) / float64(p)
	}
	return 0
}

// Sink 接收一次回复。调用顺序：Begin，然后任意个内容事件 / Ping，最后 End 或 Fail 恰好一次。
// 内容事件：Text / Thinking / Signature / ToolStart / ToolArgs。ToolArgs 属于最近一次 ToolStart。
// Signature 属于最近的 thinking 段。
type Sink interface {
	// Begin 在第一个内容事件前调用一次；u 只有输入侧（Input / CacheRead / CacheWrite）。
	Begin(u Usage)
	Text(s string)
	Thinking(s string)
	Signature(s string)
	ToolStart(id, name string)
	ToolArgs(partial string)
	Ping()
	// End 是正常结束；u 是完整记账。
	End(stop string, u Usage)
	// Fail 是中途失败；status 是 HTTP 语义的状态码。
	Fail(status int, message string)
}

// Block 是收集后的内容块。
type Block struct {
	Kind      string // "text" | "thinking" | "tool_use"
	Text      string // text / thinking
	Signature string // thinking
	ID        string // tool_use
	Name      string // tool_use
	Input     json.RawMessage
}

// Turn 是一条完整回复，给非流式响应用。
type Turn struct {
	ID     string
	Model  string // 下游请求的模型名，原样回显
	Blocks []Block
	Stop   string
	Usage  Usage
}

// Text 拼出所有 text 块。
func (t *Turn) Text() string {
	var s string
	for _, b := range t.Blocks {
		if b.Kind == "text" {
			s += b.Text
		}
	}
	return s
}

// Collector 是收集 Turn 的 Sink。Fail 后 Failed 非零。
type Collector struct {
	Turn   Turn
	Status int
	Err    string
	args   []string
	done   bool
}

var _ Sink = (*Collector)(nil)

func (c *Collector) last(kind string) *Block {
	if n := len(c.Turn.Blocks); n > 0 && c.Turn.Blocks[n-1].Kind == kind {
		return &c.Turn.Blocks[n-1]
	}
	return nil
}

func (c *Collector) add(b Block) {
	c.Turn.Blocks = append(c.Turn.Blocks, b)
	c.args = append(c.args, "")
}

// Begin 记下输入侧用量。
func (c *Collector) Begin(u Usage) { c.Turn.Usage = u }

// Text 追加文字。
func (c *Collector) Text(s string) {
	if b := c.last("text"); b != nil {
		b.Text += s
		return
	}
	c.add(Block{Kind: "text", Text: s})
}

// Thinking 追加思考。
func (c *Collector) Thinking(s string) {
	if b := c.last("thinking"); b != nil {
		b.Text += s
		return
	}
	c.add(Block{Kind: "thinking", Text: s})
}

// Signature 追加到最近的 thinking 块。
func (c *Collector) Signature(s string) {
	if b := c.last("thinking"); b != nil {
		b.Signature += s
	}
}

// ToolStart 开一个工具调用。
func (c *Collector) ToolStart(id, name string) { c.add(Block{Kind: "tool_use", ID: id, Name: name}) }

// ToolArgs 追加工具参数片段。
func (c *Collector) ToolArgs(s string) {
	if c.last("tool_use") != nil {
		c.args[len(c.args)-1] += s
	}
}

// Ping 无操作。
func (c *Collector) Ping() {}

// End 收尾：工具参数解析为 JSON，非法时为 {}。
func (c *Collector) End(stop string, u Usage) {
	for i := range c.Turn.Blocks {
		if c.Turn.Blocks[i].Kind != "tool_use" {
			continue
		}
		c.Turn.Blocks[i].Input = json.RawMessage("{}")
		if a := c.args[i]; a != "" && json.Valid([]byte(a)) {
			c.Turn.Blocks[i].Input = json.RawMessage(a)
		}
	}
	if c.Turn.Blocks == nil {
		c.Turn.Blocks = []Block{}
	}
	c.Turn.Stop, c.Turn.Usage, c.done = stop, u, true
}

// Fail 记下失败；已收到的内容作废。
func (c *Collector) Fail(status int, message string) {
	c.Status, c.Err = status, message
	c.Turn.Blocks = nil
}

// Done 报告是否正常结束。
func (c *Collector) Done() bool { return c.done }
