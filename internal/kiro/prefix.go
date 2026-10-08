package kiro

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// PrefixCheck 对比相邻请求的消息序列。Tools 在协议中挂在 current 上，单独比较；
// 从 current 移到 history 的位置不属于正文变化。文件本身保留完整原始请求。
type PrefixCheck struct {
	PreviousAccount    string `json:"previous_account,omitzero"`
	Account            string `json:"account,omitzero"`
	SameAccount        bool   `json:"same_account"`
	SameConversation   bool   `json:"same_conversation"`
	SameModel          bool   `json:"same_model"`
	SameParameters     bool   `json:"same_parameters"`
	SameTools          bool   `json:"same_tools"`
	MessagesExtend     bool   `json:"messages_extend"`
	PreviousBytes      int    `json:"previous_message_bytes"`
	CurrentBytes       int    `json:"current_message_bytes"`
	CommonBytes        int    `json:"common_message_bytes"`
	FirstDifferentByte *int   `json:"first_different_byte,omitzero"`
	PreviousHash       string `json:"previous_message_sha256"`
	CurrentHash        string `json:"current_message_sha256"`
}

func prefixView(payload []byte) (body, []byte, []byte, error) {
	var b body
	if err := json.Unmarshal(payload, &b); err != nil {
		return b, nil, nil, err
	}
	var tools []tool
	if u := b.State.CurrentMessage.User; u != nil && u.Context != nil {
		tools = u.Context.Tools
	}
	entries := append(b.State.History, entry{User: b.State.CurrentMessage.User})
	var messages bytes.Buffer
	for _, e := range entries {
		if e.User != nil && e.User.Context != nil {
			e.User.Context.Tools = nil
			if len(e.User.Context.ToolResults) == 0 {
				e.User.Context = nil
			}
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return b, nil, nil, err
		}
		messages.Write(raw)
		messages.WriteByte('\n')
	}
	rawTools, err := json.Marshal(tools)
	return b, messages.Bytes(), rawTools, err
}

// CheckPrefix 返回消息逐字节延续及上游缓存相关元数据是否改变。
// MessagesExtend 不表示上游必命中：账号、工具、参数及上游 TTL 还需独立核对。
func CheckPrefix(previous, current []byte) (PrefixCheck, error) {
	a, old, oldTools, err := prefixView(previous)
	if err != nil {
		return PrefixCheck{}, err
	}
	b, next, nextTools, err := prefixView(current)
	if err != nil {
		return PrefixCheck{}, err
	}
	oldParams, _ := json.Marshal(a.Fields)
	newParams, _ := json.Marshal(b.Fields)
	common := 0
	for common < min(len(old), len(next)) && old[common] == next[common] {
		common++
	}
	check := PrefixCheck{SameConversation: a.State.ConversationID == b.State.ConversationID,
		SameModel:      a.State.CurrentMessage.User != nil && b.State.CurrentMessage.User != nil && a.State.CurrentMessage.User.ModelID == b.State.CurrentMessage.User.ModelID,
		SameParameters: bytes.Equal(oldParams, newParams), SameTools: bytes.Equal(oldTools, nextTools), MessagesExtend: bytes.HasPrefix(next, old),
		PreviousBytes: len(old), CurrentBytes: len(next), CommonBytes: common,
		PreviousHash: fmt.Sprintf("%x", sha256.Sum256(old)), CurrentHash: fmt.Sprintf("%x", sha256.Sum256(next))}
	if !check.MessagesExtend {
		check.FirstDifferentByte = &common
	}
	return check, nil
}
