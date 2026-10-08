package anthropic

import (
	"bytes"
	"encoding/json"
)

// CanonicalJSON 固定对象键顺序，保留数组顺序和数值精度。
// 无效 JSON 保持原值，由原有请求校验或序列化路径处理。
func CanonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil {
		return raw
	}
	out, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return out
}
