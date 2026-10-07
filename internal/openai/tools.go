package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"kiro-proxy/internal/anthropic"
)

// Responses 工具的新写法（Codex 用）：custom（自由文本输入，如 apply_patch）、namespace 分组、
// additional_tools 输入项。Kiro 只有 JSON schema 的函数工具，这里可逆地映射：
//
//   - custom → 只有一个字符串参数 input 的函数；format（grammar 的 lark / regex 定义）写进说明。
//     模型调用时取出 input 还原成 custom_tool_call；custom_tool_call / _output 历史反向转回去。
//   - namespace → 展开成 「namespace__name」的函数；输出还原成 function_call{namespace, name}。
//   - additional_tools 输入项 → 并入工具列表（上游没有「从某个位置起可用」的语义，全程可用）。
//
// 转不了的（服务端内置工具 web_search 等、tool_search）返回明确的错误，不再悄悄丢掉。

// namespaceSep 是 namespace 与工具名之间的分隔（与 MCP 的 mcp__server__tool 写法一致）。
const namespaceSep = "__"

// customInputSchema 是 custom 工具映射成的函数的参数。
var customInputSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"The raw input for this tool, exactly as the tool expects it (not JSON-encoded)."}},"required":["input"]}`)

// ToolKind 是一个工具在下游的原始写法，写回复时还原用。
type ToolKind struct {
	Custom    bool   // custom 工具：调用还原成 custom_tool_call，参数取 input 字符串
	Namespace string // 所属 namespace；空 = 顶层
	Name      string // 在 namespace 内的短名
}

// ToolKinds 是「上游函数名 → 下游写法」。只含需要还原的工具（custom、namespace 内的）。
type ToolKinds map[string]ToolKind

// Restore 把上游的一次工具调用还原成下游写法：返回输出项类型（function_call / custom_tool_call）、
// namespace、短名、以及参数（function 是 JSON 字符串；custom 是 input 原文）。
func (k ToolKinds) Restore(name, args string) (typ, namespace, short, payload string) {
	kind, ok := k[name]
	if !ok {
		return "function_call", "", name, args
	}
	short = kind.Name
	if !kind.Custom {
		return "function_call", kind.Namespace, short, args
	}
	var in struct {
		Input *string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &in) == nil && in.Input != nil {
		return "custom_tool_call", kind.Namespace, short, *in.Input
	}
	return "custom_tool_call", kind.Namespace, short, args // 模型没按 schema 给：原样交给客户端
}

// respToolDef 是 Responses 的一个工具定义（含 namespace 里的嵌套工具）。
type respToolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Format      *customFormat   `json:"format"`
	Tools       []respToolDef   `json:"tools"` // namespace
}

type customFormat struct {
	Type       string `json:"type"`   // text | grammar
	Syntax     string `json:"syntax"` // lark | regex
	Definition string `json:"definition"`
}

// HostedTools 是转不了的 OpenAI 内置工具（web_search 等）的处理方式。
const (
	HostedReject = "reject" // 返回 400 说明原因
	HostedDrop   = "drop"   // 默认：跳过这些工具与它们的历史调用（Codex 默认带 web_search）
)

// hostedErr 是内置工具转不了的错误。
func hostedErr(at, typ string) error {
	return fmt.Errorf("%s: %q is a hosted OpenAI tool and is not available through Kiro; remove it from the request (or set openai_hosted_tools to \"drop\")", at, typ)
}

// convertTools 把 Responses 的工具定义转成 Anthropic 工具，并记下需要还原的写法。
// drop 模式下跳过的记到 dropped。
func convertTools(defs []respToolDef, where string, kinds ToolKinds, hosted string, dropped *[]string) ([]anthropic.Tool, error) {
	var out []anthropic.Tool
	for i, t := range defs {
		at := fmt.Sprintf("%s[%d]", where, i)
		switch t.Type {
		case "function", "":
			if t.Name == "" {
				return nil, fmt.Errorf("%s: function tool needs a name", at)
			}
			out = append(out, tool(t.Name, t.Description, t.Parameters))
		case "custom":
			if t.Name == "" {
				return nil, fmt.Errorf("%s: custom tool needs a name", at)
			}
			out = append(out, anthropic.Tool{Name: t.Name, Description: customDescription(t), InputSchema: customInputSchema})
			kinds[t.Name] = ToolKind{Custom: true, Name: t.Name}
		case "namespace":
			if t.Name == "" {
				return nil, fmt.Errorf("%s: namespace needs a name", at)
			}
			inner, err := convertTools(t.Tools, at+".tools", ToolKinds{}, hosted, dropped)
			if err != nil {
				return nil, err
			}
			var defs []respToolDef // 与 inner 一一对应（drop 模式下内置工具已跳过）
			for _, d := range t.Tools {
				if d.Type == "" || d.Type == "function" || d.Type == "custom" {
					defs = append(defs, d)
				}
			}
			for j, it := range inner {
				def := defs[j]
				full := t.Name + namespaceSep + it.Name
				desc := it.Description
				if d := strings.TrimSpace(t.Description); d != "" {
					desc = "[" + t.Name + ": " + d + "] " + desc
				}
				out = append(out, anthropic.Tool{Name: full, Description: desc, InputSchema: it.InputSchema})
				kinds[full] = ToolKind{Custom: def.Type == "custom", Namespace: t.Name, Name: it.Name}
			}
		default:
			// web_search / file_search / code_interpreter / tool_search / mcp / computer ……
			if hosted == HostedDrop {
				*dropped = append(*dropped, at+": "+t.Type)
				continue
			}
			return nil, hostedErr(at, t.Type)
		}
	}
	return out, nil
}

// customDescription 把 custom 工具的说明与输入格式合并成函数说明：模型只能从说明里知道输入该长什么样。
func customDescription(t respToolDef) string {
	var b strings.Builder
	b.WriteString(t.Description)
	b.WriteString("\n\nPut the tool's raw input in the `input` string argument.")
	if f := t.Format; f != nil && f.Type == "grammar" && strings.TrimSpace(f.Definition) != "" {
		fmt.Fprintf(&b, " The input must match this %s grammar:\n%s", f.Syntax, f.Definition)
	}
	return strings.TrimSpace(b.String())
}

// additionalTools 解析 additional_tools 输入项。
func additionalTools(raw json.RawMessage) ([]respToolDef, error) {
	var it struct {
		Tools []respToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &it); err != nil {
		return nil, err
	}
	return it.Tools, nil
}

// customCallArgs 把历史里的 custom_tool_call.input（原文）包成映射函数的参数 {"input": "..."}。
func customCallArgs(input string) string {
	b, _ := json.Marshal(map[string]string{"input": input})
	return string(b)
}

// callName 是历史里 function_call / custom_tool_call 在上游的名字：带 namespace 的拼成 namespace__name。
func callName(namespace, name string) string {
	if namespace == "" || strings.HasPrefix(name, namespace+namespaceSep) {
		return name
	}
	return namespace + namespaceSep + name
}

// nonEmpty 报告 raw 是否是非空 JSON。
func nonEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}
