// Package kiro 说 Kiro 的话：把 Anthropic Messages 请求转成 generateAssistantResponse 的
// conversationState，把 AWS event stream 解码回事件，并处理鉴权与管理 API。
// 形状对齐 vendor/opencode-kiro-auth 与 Magpie internal/gateway/kiro.go。
package kiro

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"kiro-proxy/internal/anthropic"
)

const (
	// proceed 是空消息的占位：Kiro 不收空 content。
	proceed = "Please proceed with the task."
	// noResult 回答没有结果的工具调用。
	noResult = "Tool use was interrupted and did not produce a result."
	// resultLimit 是单个工具结果的上限，和 Kiro 自己的 agent 一致。
	resultLimit = 250_000
	// toolDescriptionLimit 是上游工具描述的 UTF-8 字节上限。
	toolDescriptionLimit = 10240
)

var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

type entry struct {
	User *userMsg `json:"userInputMessage,omitzero"`
	Asst *asstMsg `json:"assistantResponseMessage,omitzero"`
}

type userMsg struct {
	Content    string      `json:"content"`
	ModelID    string      `json:"modelId"`
	Origin     string      `json:"origin"`
	Images     []image     `json:"images,omitzero"`
	Context    *userCtx    `json:"userInputMessageContext,omitzero"`
	CachePoint *cachePoint `json:"cachePoint,omitzero"`
}

// cachePoint 是 Kiro 的显式缓存断点。SDK（@aws/codewhisperer-streaming-client）里只有 type="default"，没有 TTL。
type cachePoint struct {
	Type string `json:"type"`
}

type userCtx struct {
	Tools       []tool       `json:"tools,omitzero"`
	ToolResults []toolResult `json:"toolResults,omitzero"`
}

type image struct {
	Format string      `json:"format"`
	Source imageSource `json:"source"`
}

type imageSource struct {
	Bytes string `json:"bytes"`
}

// tool 是 tools 数组的一项：工具声明，或一个 cachePoint 成员（SDK 的 Tool 联合类型）。
type tool struct {
	Spec       *toolSpec   `json:"toolSpecification,omitzero"`
	CachePoint *cachePoint `json:"cachePoint,omitzero"`
}

type toolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type textPart struct {
	Text string `json:"text"`
}

type toolResult struct {
	ToolUseID string     `json:"toolUseId"`
	Status    string     `json:"status"`
	Content   []textPart `json:"content"`
}

type asstMsg struct {
	Content    string      `json:"content"`
	ToolUses   []toolUse   `json:"toolUses,omitzero"`
	CachePoint *cachePoint `json:"cachePoint,omitzero"`
}

type toolUse struct {
	Name      string          `json:"name"`
	ToolUseID string          `json:"toolUseId"`
	Input     json.RawMessage `json:"input"`
}

type conversationState struct {
	ChatTriggerType string `json:"chatTriggerType"`
	AgentTaskType   string `json:"agentTaskType"`
	ConversationID  string `json:"conversationId"`
	// AgentContinuationID 只在探测里发（BuildOptions.AgentContinuation）；主流程默认不发
	AgentContinuationID string         `json:"agentContinuationId,omitzero"`
	CurrentMessage      currentMessage `json:"currentMessage"`
	History             []entry        `json:"history,omitzero"`
}

type currentMessage struct {
	User *userMsg `json:"userInputMessage"`
}

type body struct {
	State      conversationState   `json:"conversationState"`
	AgentMode  string              `json:"agentMode"`
	ProfileArn string              `json:"profileArn,omitzero"`
	Fields     *ModelRequestFields `json:"additionalModelRequestFields,omitzero"`
}

// BuildOptions 是构造请求需要的上游侧参数。
type BuildOptions struct {
	Model          string
	ProfileArn     string
	ConversationID string              // 稳定的上游会话 ID；空则每次随机
	ThinkingBudget int                 // 旧模型 >0 时在首条消息前加 thinking 标签
	RequestSchema  *ModelRequestSchema // 模型声明了原生参数时优先用它，预算标签不再注入
	// AgentContinuation 是探测开关：发一个由 conversationId 用 sha256 派生的固定 agentContinuationId。
	// 主流程不设置，效果未知。
	AgentContinuation bool
	// CachePoints 是实验开关（默认关）：在这些位置发 cachePoint{type:"default"}。有没有用由探测决定。
	CachePoints CachePoints
}

// CachePoints 是显式 cachePoint 的位置。Kiro 没有单独的 system 字段，system 并在首条 user 里。
type CachePoints struct {
	FirstUser bool // 首条 user（含 system）
	Assistant bool // 历史里最后一条 assistant
	Tools     bool // tools 数组末尾
}

// Any 报告是否开了任何一个位置。
func (c CachePoints) Any() bool { return c.FirstUser || c.Assistant || c.Tools }

// Build 把 Messages 请求转成 generateAssistantResponse 的 body。
// 返回的 names 是「改写后工具名 → 原名」，解码时还原。
// 输出对同一输入是字节级稳定的：上游 cache 靠前缀不变。
func Build(req *anthropic.Request, opts BuildOptions) (payload []byte, names map[string]string, err error) {
	entries := buildEntries(req, opts.Model)
	fields, err := ThinkingFields(req, opts.RequestSchema)
	if err != nil {
		return nil, nil, err
	}

	system := req.System.Text("\n")
	if opts.ThinkingBudget > 0 && opts.RequestSchema == nil {
		tag := fmt.Sprintf("<thinking_mode>enabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", opts.ThinkingBudget)
		system = tag + prefixNonEmpty("\n", system)
	}
	if system != "" {
		entries[0].User.Content = system + "\n\n" + entries[0].User.Content
	}

	current := entries[len(entries)-1].User
	if tools := buildTools(req, entries); len(tools) > 0 {
		if opts.CachePoints.Tools {
			tools = append(tools, tool{CachePoint: &cachePoint{Type: "default"}})
		}
		if current.Context == nil {
			current.Context = &userCtx{}
		}
		current.Context.Tools = tools
	}
	if opts.CachePoints.FirstUser && len(entries) > 1 {
		entries[0].User.CachePoint = &cachePoint{Type: "default"}
	}
	if opts.CachePoints.Assistant {
		for i := len(entries) - 2; i >= 0; i-- {
			if entries[i].Asst != nil {
				entries[i].Asst.CachePoint = &cachePoint{Type: "default"}
				break
			}
		}
	}

	convID := opts.ConversationID
	if convID == "" {
		convID = NewUUID()
	}
	out := body{
		State: conversationState{
			ChatTriggerType: "MANUAL",
			AgentTaskType:   "vibe",
			ConversationID:  convID,
			CurrentMessage:  currentMessage{User: current},
		},
		AgentMode:  "vibe",
		ProfileArn: opts.ProfileArn,
		Fields:     fields,
	}
	if opts.AgentContinuation {
		out.State.AgentContinuationID = ContinuationID(convID)
	}
	if len(entries) > 1 {
		out.State.History = entries[:len(entries)-1]
	}
	payload, err = json.Marshal(out)
	return payload, toolNames(req), err
}

// ContinuationID 是从 conversationId 派生的固定 agentContinuationId（UUID 形状）。
func ContinuationID(convID string) string {
	sum := sha256.Sum256([]byte("agent-continuation:" + convID))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func buildEntries(req *anthropic.Request, model string) []entry {
	newUser := func() *userMsg { return &userMsg{ModelID: model, Origin: "KIRO_CLI"} }
	var entries []entry
	for _, m := range req.Messages {
		var texts []string
		if m.Role == "assistant" {
			a := &asstMsg{}
			for _, p := range m.Content {
				switch p.Type {
				case "text":
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
				case "tool_use":
					a.ToolUses = append(a.ToolUses, toolUse{Name: ToolName(p.Name), ToolUseID: ToolID(p.ID), Input: objectOrEmpty(p.Input)})
				}
			}
			a.Content = strings.Join(texts, "\n\n")
			if a.Content == "" && len(a.ToolUses) == 0 {
				continue // 只有 thinking 的一轮
			}
			if n := len(entries); n > 0 && entries[n-1].Asst != nil {
				prev := entries[n-1].Asst
				prev.Content = joinNonEmpty(prev.Content, a.Content)
				prev.ToolUses = append(prev.ToolUses, a.ToolUses...)
			} else {
				entries = append(entries, entry{Asst: a})
			}
			continue
		}

		u := newUser()
		var results []toolResult
		for _, p := range m.Content {
			switch p.Type {
			case "text":
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			case "document":
				texts = append(texts, documentText(p))
			case "image":
				if img, ok := imageOf(p.Source); ok {
					u.Images = append(u.Images, img)
				}
			case "tool_result":
				out := p.Content.Text("\n\n")
				if len(out) > resultLimit {
					out = out[:resultLimit] + "\n… (cut)"
				}
				if out == "" {
					out = "(no output)"
				}
				status := "success"
				if p.IsError {
					status = "error"
				}
				results = append(results, toolResult{ToolUseID: ToolID(p.ToolUseID), Status: status, Content: []textPart{{Text: out}}})
				// 工具结果里的图片跟消息走：Kiro 的 toolResult 只收文字
				for _, sub := range p.Content {
					if sub.Type == "image" {
						if img, ok := imageOf(sub.Source); ok {
							u.Images = append(u.Images, img)
						}
					}
				}
			}
		}
		u.Content = strings.Join(texts, "\n\n")
		if len(results) > 0 {
			u.Context = &userCtx{ToolResults: results}
		}
		if n := len(entries); n > 0 && entries[n-1].User != nil {
			prev := entries[n-1].User
			prev.Content = joinNonEmpty(prev.Content, u.Content)
			prev.Images = append(prev.Images, u.Images...)
			if len(results) > 0 {
				if prev.Context == nil {
					prev.Context = &userCtx{}
				}
				prev.Context.ToolResults = append(prev.Context.ToolResults, results...)
			}
		} else {
			entries = append(entries, entry{User: u})
		}
	}

	// 对话以用户的话开头：去掉开头的 assistant 和没有调用的工具结果
	for len(entries) > 0 {
		if u := entries[0].User; u != nil {
			if u.Context != nil && u.Content == "" && len(u.Images) == 0 {
				entries = entries[1:]
				continue
			}
			u.Context = nil
			break
		}
		entries = entries[1:]
	}
	// 以用户的话结尾：那是要回答的一条
	if n := len(entries); n == 0 || entries[n-1].User == nil {
		entries = append(entries, entry{User: newUser()})
	}
	// 每个调用在下一条消息里恰好得到一次回答
	for i, e := range entries {
		u := e.User
		if u == nil {
			continue
		}
		var calls []toolUse
		if i > 0 && entries[i-1].Asst != nil {
			calls = entries[i-1].Asst.ToolUses
		}
		var results []toolResult
		if u.Context != nil {
			results = u.Context.ToolResults
		}
		called := make(map[string]bool, len(calls))
		for _, c := range calls {
			called[c.ToolUseID] = true
		}
		answered := map[string]bool{}
		var kept []toolResult
		for _, r := range results {
			if called[r.ToolUseID] && !answered[r.ToolUseID] {
				answered[r.ToolUseID] = true
				kept = append(kept, r)
			}
		}
		for _, c := range calls {
			if !answered[c.ToolUseID] {
				answered[c.ToolUseID] = true
				kept = append(kept, toolResult{ToolUseID: c.ToolUseID, Status: "error", Content: []textPart{{Text: noResult}}})
			}
		}
		u.Context = nil
		if len(kept) > 0 {
			u.Context = &userCtx{ToolResults: kept}
		}
		if u.Content == "" && u.Context == nil {
			u.Content = proceed
		}
	}
	// 历史图片始终随原消息重发；新图不能删除已发送的历史字节。
	return entries
}

// buildTools 是调用方的工具，加上历史里用过但本次没声明的：
// Kiro 拒绝历史里出现未声明的工具。
func buildTools(req *anthropic.Request, entries []entry) []tool {
	var tools []tool
	offered := map[string]bool{}
	for _, t := range req.Tools {
		if t.Name == "" || (t.Type != "" && t.Type != "custom" && len(t.InputSchema) == 0) {
			continue // Kiro 没有的服务端工具
		}
		name := ToolName(t.Name)
		if offered[name] {
			continue
		}
		offered[name] = true
		desc := ToolDescription(t.Description, t.Name)
		schema := t.InputSchema
		if s := bytes.TrimSpace(schema); len(s) == 0 || string(s) == "null" {
			schema = emptySchema
		}
		tools = append(tools, tool{Spec: &toolSpec{Name: name, Description: desc, InputSchema: inputSchema{JSON: anthropic.CanonicalJSON(schema)}}})
	}
	var historical []tool
	for _, e := range entries {
		if e.Asst == nil {
			continue
		}
		for _, c := range e.Asst.ToolUses {
			if !offered[c.Name] {
				offered[c.Name] = true
				historical = append(historical, tool{Spec: &toolSpec{Name: c.Name, Description: "Tool", InputSchema: inputSchema{JSON: emptySchema}}})
			}
		}
	}
	slices.SortFunc(historical, func(a, b tool) int { return strings.Compare(a.Spec.Name, b.Spec.Name) })
	return append(tools, historical...)
}

// ToolDescription 限制工具描述字节数，不切断 UTF-8 字符；空描述使用工具名。
// 计量与请求构造共用这个规则，避免为已截掉的内容计费。
func ToolDescription(description, name string) string {
	if description == "" {
		description = name
	}
	if len(description) <= toolDescriptionLimit {
		return description
	}
	n := toolDescriptionLimit
	for !utf8.RuneStart(description[n]) {
		n--
	}
	return description[:n]
}

var toolIDRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`)

// ToolID 把 Kiro 不收的 tool_use id 改写成稳定的哈希 id。
func ToolID(id string) string {
	if toolIDRe.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "t_" + base64.RawURLEncoding.EncodeToString(sum[:])[:32]
}

var (
	toolNameRe  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	toolNameBad = regexp.MustCompile(`[^a-zA-Z0-9_-]`)
)

// ToolName 把过长或含非法字符的工具名（MCP 前缀）改写成稳定短名。
func ToolName(name string) string {
	if toolNameRe.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	short := toolNameBad.ReplaceAllString(name, "_")
	if len(short) > 55 {
		short = short[:55]
	}
	return fmt.Sprintf("%s_%x", short, sum[:4])
}

// toolNames 是改写过的名字 → 调用方原名。
func toolNames(req *anthropic.Request) map[string]string {
	names := map[string]string{}
	add := func(n string) {
		if k := ToolName(n); k != n {
			names[k] = n
		}
	}
	for _, t := range req.Tools {
		add(t.Name)
	}
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "tool_use" {
				add(p.Name)
			}
		}
	}
	return names
}

func objectOrEmpty(raw json.RawMessage) json.RawMessage {
	if s := bytes.TrimSpace(raw); len(s) > 0 && s[0] == '{' && json.Valid(s) {
		return anthropic.CanonicalJSON(s)
	}
	return json.RawMessage("{}")
}

func imageOf(src *anthropic.Source) (image, bool) {
	if src == nil || src.Type != "base64" || src.Data == "" {
		return image{}, false
	}
	return image{Format: imageFormat(src.MediaType), Source: imageSource{Bytes: src.Data}}, true
}

func imageFormat(mediaType string) string {
	t := strings.ToLower(mediaType)
	f, ok := strings.CutPrefix(t, "image/")
	switch {
	case !ok || f == "":
		return "png"
	case f == "jpg":
		return "jpeg"
	}
	return f
}

func documentText(p anthropic.Block) string {
	var title string
	if p.Title != "" {
		title = p.Title + "\n"
	}
	switch {
	case p.Source == nil:
		return title + "[a document attachment]"
	case p.Source.Type == "text":
		return title + p.Source.Data
	case p.Source.Type == "content":
		return title + p.Source.Content.Text("\n\n")
	}
	kind := p.Source.MediaType
	if kind == "" {
		kind = "document"
	}
	return title + "[a " + kind + " attachment]"
}

func joinNonEmpty(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "\n\n" + b
}

func prefixNonEmpty(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}
