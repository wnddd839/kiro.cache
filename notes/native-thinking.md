# 原生思考参数与前缀实测

2026-10-08，在本地账号上直连 Kiro，模型 `claude-sonnet-4.6`。本记录只保存请求形状与用量结果，不含账号、凭据或业务对话。

## 请求参数

先读取 `List-Available-Models` 的 `additionalModelRequestFieldsSchema`。该模型声明：

- `thinking.type`：`adaptive` / `disabled`。
- `thinking.display`：`summarized` / `omitted`。
- `output_config.effort`：`low` / `medium` / `high` / `max`，默认 `high`。

在请求根部发送：

```json
{
  "additionalModelRequestFields": {
    "thinking": {"type": "adaptive", "display": "summarized"},
    "output_config": {"effort": "low"}
  }
}
```

这次探测另外按 schema 设置 `max_tokens: 1024`，限制实验输出；这不是主流程转发 `max_tokens` 的功能变更。目录中 GPT 模型声明的是 `reasoning.effort`，不能复用 Claude 的字段。Sonnet 4.5、Haiku 4.5 和 Auto 在本次目录响应中 schema 为 null，继续使用旧路径。

## 改强度是否保留缓存

同一 conversationId，完全相同的 system / messages，使用约 3500 token 的参考文本，要求只回复 `ok`。按顺序发送 5 次。每次 HTTP 200、正文 `ok`、没有可见 thinking，`contextUsagePercentage` 相同。

| 次序 | 原生 effort | credits |
|---|---|---:|
| 1 | low | 0.0367151450 |
| 2 | low | 0.0196541002 |
| 3 | high | 0.0367151450 |
| 4 | high | 0.0196541002 |
| 5 | low | 0.0196541002 |

5 次 `conversationState` JSON 的 SHA-256 完全一致：`5d46c197fa226dc052f7c6112ea84a2f71088564c968390b837fc77bbf1cbc86`。只有请求根部原生参数改变。

另用不同的参考前缀与 conversationId 做旧标签对照：

| 次序 | prompt 标签预算 | credits |
|---|---|---:|
| 1 | 10000 | 0.0391237350 |
| 2 | 10000 | 0.0220103022 |
| 3 | 30000 | 0.0391237350 |

旧路径变更预算后，`conversationState` 的 SHA-256 也随之改变。两组参考文本的随机标识不同，不能直接把两组 credits 的差值解释为原生参数的节省。

**结论：原生字段消除了预算标签导致的文本前缀改写，但本样本不支持“不同 effort 可以共用上游缓存”的判断。** 同强度第二次成本降低、首次切换强度成本恢复、切回已用强度成本再次降低，支持按生效参数分别保留缓存状态的解释。主流程因此按原生参数隔离本地缓存，不把新强度直接计为命中。

上游这几次均未报 `tokenUsage`；上述缓存判断来自 credits 的间接证据，不是上游 cache token 计数，也不能推广为所有模型的固定规则。

## 工具描述截断

同一模型另发一次带工具的请求，原始描述为 21000 UTF-8 字节的中文文本，构造请求后为 10239 字节，保留完整字符。上游 HTTP 200、回复 `ok`，credits `0.0532309659`。

本地端到端测试另外覆盖 10240 字节边界、英文溢出、中文切点和 emoji，并确认 `count_tokens` / 账本使用的描述与实际发往上游的描述一致。没有发送超限描述做失败对照，因此不把 10240 字节描述为本轮实验测出的上游硬限制。
