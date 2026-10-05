# 上游 cache，还是本地再缓一层？

决策：**不自建补全缓存。把劲用在请求管理，让 Kiro 自己的 cache 能打中。**

下游请求会很乱，这正是不要本地 cache 补全的原因。

---

## Kiro 已经有 cache

上游 event stream 会报：

- `cacheReadInputTokens`
- `cacheWriteInputTokens`
- `uncachedInputTokens`

这是 **prompt / 会话级** 的记账，不是完整回答的 KV。Magpie 和插件每次 `conversationId = randomUUID()`，再把整段 history 塞进 body。`conversationId` 排在 JSON 里 `history` 前面。只要上游按前缀或按 conversation 索引，这一个 UUID 就能把命中打成 0。

---

## 为什么不自建补全 cache

分发的请求乱在：不同客户端、不同 system 指纹、工具集合、thinking 开关、中途 tool result。

本地再存一份「同 prompt → 同回答」：

1. **Agent 不是完整命中。** 费用在输入 history，不在输出那几百 token。不发上游才能省钱；一旦跳过上游，工具调用、拒绝、流式都要自己造。
2. **乱请求几乎不会完整相等。** system 里一行 cwd / git status / 时间戳，key 就换了。稍微继精就会串答。
3. **工具轮次不能复用。** 同一句 user，上一轮 `read` 到的文件和这一轮不一样。
4. **Cache 跟账号走。** 上游 cache 绑的是那个 token / profile。换号还去读本地补全，答案可能来自另一个号的上下文。
5. **Buddy Proxy 已经踩过。** `#7` `#11` `#27`：命中率近 0，是没粘会话、没复用上游 conversation id，不是缺本地答案库。

本地 cache 解决的是「相同问题不要再问一遍」。分发层的问题是「同一段对话的下一轮踏上前缀」。两件事。

---

## 作什么：管理请求，不管理答案

```
客户端 /v1/messages  (乱：头、system、工具轮次不一致)
        ↓
  session key（头 / prompt_cache_key / system+首条 user 前缀）
        ↓
  钉号：session → account     不轮询
        ↓
  钉会话：session+account → 稳定 conversationId
        ↓
  形状稳定：thinking 预算、工具顺序、不乱改 system 前缀
        ↓
  runtime.<region>.kiro.dev     仍发全量 history
        ↓
  日志 cacheRead / cacheWrite  没这个数就是猜
```

乱请求的实际对策是 **把「同一段对话」认回来**，不是把回答存盘。

- 有 `X-Session-Id` / `prompt_cache_key` 就用客户端的键
- 没有就用 `system + 首条 user` 的哈希。后面的 tool 轮次不换键
- 两个不同客户端如果首条 user 巧合一样会碰到一起——可接受；比 round-robin 要好
- 429 / USAGE_LIMIT / 换号：作废旧 conversationId，不要拿旧 id 去新号上续

仍发全量 history（跟 Magpie 一样）。先只把 conversationId 稳住，打 `cacheRead`。若仍是 0，再试只发 delta。

---

## 什么时候才考虑本地层

只做 **请求形状正则化**，不做答案库：

- 工具 schema 排序
- thinking 开关不在同一会话里翻来覆去
- 剔掉明显会变的 system 指纹（cwd / 时间）——要很谨慎，只在看到 cacheRead 被这些字打破时动

不做：
- 按 prompt 存模型输出
- 跨号共享答案
- 用向量 / 模糊命中顶替上游
