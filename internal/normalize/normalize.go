// Package normalize 把下游请求整理成「同一会话前缀字节稳定」的形状，让上游 prompt cache 能打中。
//
// 只动确定会变且不影响语义的东西：
//   - Claude Code 的 x-anthropic-billing-header 系统块（每次请求 cch 都变）
//   - 工具声明顺序（MCP 工具加载顺序不确定）
//   - 用户配置的 system 正则（cwd / 时间等，默认不开）
//
// 不做答案缓存。
package normalize

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"kiro-proxy/internal/anthropic"
)

// Options 控制整理行为。
type Options struct {
	SortTools bool
	// SystemStrip 是要从 system 里删除的正则（按顺序，全局替换为空）。
	SystemStrip []*regexp.Regexp
}

// Compile 编译配置里的正则。
func Compile(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("system strip pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

const billingPrefix = "x-anthropic-billing-header:"

// Request 就地整理 req。
func Request(req *anthropic.Request, opts Options) {
	req.System = slices.DeleteFunc(req.System, func(b anthropic.Block) bool {
		return b.Type == "text" && strings.HasPrefix(strings.TrimSpace(b.Text), billingPrefix)
	})
	if len(opts.SystemStrip) > 0 {
		for i := range req.System {
			for _, re := range opts.SystemStrip {
				req.System[i].Text = re.ReplaceAllString(req.System[i].Text, "")
			}
		}
	}
	if opts.SortTools {
		slices.SortStableFunc(req.Tools, func(a, b anthropic.Tool) int { return cmp.Compare(a.Name, b.Name) })
	}
}

// sessionIDRe 从 Claude Code 的 metadata.user_id 里取 session：
// 旧格式 user_<hash>_account_<uuid>_session_<uuid>，新格式是 JSON {"session_id":"…"}。
var sessionIDRe = regexp.MustCompile(`(?:_session_|"session_id"\s*:\s*")([0-9A-Za-z-]{8,})`)

// MetadataSession 从 metadata.user_id 取会话 id；取不到返回空串。
func MetadataSession(req *anthropic.Request) string {
	if req.Metadata == nil {
		return ""
	}
	if m := sessionIDRe.FindStringSubmatch(req.Metadata.UserID); m != nil {
		return m[1]
	}
	return ""
}

// PinMessages 把 Anthropic 请求转成 sessionpin.Key 需要的形状：system 作为首条消息，
// 之后只带到首条 user（后面的轮次不参与 key）。应在 Request 整理之后调用。
func PinMessages(req *anthropic.Request) []map[string]any {
	var out []map[string]any
	if s := req.System.Text("\n"); s != "" {
		out = append(out, map[string]any{"role": "system", "content": s})
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		if t := m.Content.Text(" "); t != "" {
			out = append(out, map[string]any{"role": "user", "content": t})
			break
		}
	}
	return out
}
