# Magpie Kiro 调研

## 结论先说

1. **多账号：有，但在 Magpie 层，不在插件层。**
2. **本地 prompt cache：没有自建缓存。** Magpie 只做会话粘滞，指望上游自己命中。
3. **Kiro 上游缓存大概率打不中：** 每次请求都新生成 `conversationId`，整段 history 重发。
4. 分发层要做的核心不是再抄一遍协议，而是：**号池 + 稳定 conversationId + 账号粘滞**。

---

## 代码在哪

两套实现，行为基本对齐：

| 层 | 位置 | 现状 |
|---|---|---|
| 插件（Magpie 现在走这条） | `@magpie-community/opencode-kiro-auth` `index.mjs` | OpenCode 插件，Magpie 用 Bun 跑 |
| 内置 Go（被插件替换） | `internal/provider/kiro*.go` + `internal/gateway/kiro.go` | 登录、token、协议、多账号 home |

插件 README 自己写了缺口：

- 不做 usage/quota（实际代码里已经有 `auth.usage`）
- **不做多账号切换**（OpenCode 一个 provider 一个登录）
- 公司 IdP 浏览器登录不做
- Magpie 给 Kiro 的 web-search 替身不做

Magpie 用 `plugin-auth.json` 的 `id` / `id#slot` + `logins.json` 把多账号补在插件外面（`plugin_accounts.go`）。

实测时：`plugins.json` 装了 `@magpie-community/opencode-kiro-auth@latest`，`plugin-auth.json` 里只有一条 `kiro`，且是 `source: "kiro"` 标记（读 kiro-cli / IDE，不拷 token）。`logins.json` 里这条被 `hidden`。

---

## 多账号怎么管

### Magpie 内置（`kiro_accounts.go`）

- kiro-cli / IDE：**只读，各一个号**。cli 在 kiro-cli 的本地 SQLite `auth_kv`；IDE 在 `~/.aws/sso/cache/kiro-auth-token.json`。
- Magpie 自己登录的号：每个号一个 home，`~/.config/magpie/kiro/<id>/kiro-auth-token.json`。refresh token 只写回自己的 home，互不抢。
- `logins.json` 记顺序、开关、隐藏。CLI/IDE 那个号没有 home，只能藏，不能删凭证。
- 有 provider key（`ksk_…`）时，**只用这个 key，其它号全部不用**。
- `kiroAlsoOn`：第一个号后面还能挂备用号，给 gateway 路由 failover。

### 插件（`index.mjs`）

登录三种：

1. 浏览器 OAuth（Google / GitHub / Builder ID / IAM IDC）
2. 沿用 kiro-cli 或 IDE（不拷贝，每次现读）
3. API key `ksk_…`

插件内部 `account()` 只持有**当前这一份** cred，靠 Magpie 换 `getAuth()` 才换号。OpenCode 单账号；Magpie 多账号是 host 侧切 auth 记录。

迁到插件时（`migrate_kiro.go`）：

- CLI/IDE → `{source:"kiro"}` 标记
- Magpie home 里的 token 整份交给插件，之后由插件 refresh
- provider 上的 API key 变成一条 api 账号并清掉 provider.key

---

## 「本地缓存复用」实际是什么

**没有**把 prompt 缓存在磁盘再命中的层。有的是：

### 1. Magpie affinity（`gateway/affinity.go`）

同一段对话尽量打回上次那个号/key，让**上游** prompt cache 生效：

- `stays=auto`（默认）：同 turn 必粘；跨 turn 要同时满足 cacheRead ≥ 1024 且距上次 < 5 分钟
- `session`：整段会话粘死
- `turn`：只粘 tool 回传
- `off`：不粘

账号休息 / 额度将尽 / 号没了，会放走。记忆 24 小时，落在 `~/.config/magpie/affinity.json`。

路由组还有 `smart/order/rotate/usage`。Kiro 多号时 Magpie 已经能把多个 plugin 账号当成候选。

实测时 affinity 里还没有 Kiro 记录。

### 2. Token / 模型列表的进程内缓存

- Go：`kiroAuthCache` 按 key+home 记住最近 cred
- 插件：`account()` 单飞 refresh，避免同一 refresh token 花两次
- 模型 context window 记在内存 `windows` Map，给 usage 估算用

这不是 prompt cache。

### 3. Kiro 协议自己报 cache

插件解码 AWS event stream 的 `tokenUsage`：

- `cacheReadInputTokens`
- `cacheWriteInputTokens`
- `uncachedInputTokens` / `inputTokens`

说明上游**有** cache 计数。问题是请求形状可能让它经常是 0。

---

## 为什么上游 cache 容易 miss

Go 和插件构造请求时都是：

```json
conversationState.conversationId = newUUID()   // ⚠️ 每次新的
conversationState.history        = 前面所有轮
conversationState.currentMessage = 最后一条 user
```

再加上：

- 整段 history 每次重发（没有 “续同一个 conversationId，只发 delta”）
- thinking 开关会改第一条 user 的前缀（`<thinking_mode>`）
- 只有最新一轮图片会留下，历史图片被删
- tool 名过长会被 hash 改写（插件侧没截，Go 侧 `kiroToolName` 会）
- Magpie 粘号是为了 Anthropic/OpenAI 那种 **prefix cache**；Kiro 若按 conversationId 索引，UUID 乱跳等于每次冷启动

所以号池如果 round-robin，cache 更差。必须：

1. **会话 → 账号** 粘滞（Magpie 已有）
2. **会话 → conversationId** 稳定（两边都没做，这是最大缺口）
3. history 字节级前缀稳定（工具顺序、system 前缀、thinking 预算不要来回变）

---

## 协议要点（分发层要复用）

- 入口：`POST https://runtime.<region>.kiro.dev/generateAssistantResponse`
- 鉴权：`Authorization: Bearer <token>`，API key 再加 `tokentype: API_KEY`
- 伪装成 kiro-cli：`agentMode=vibe`，`origin=KIRO_CLI`，AWS event stream
- 管理 API：`https://management.<region>.kiro.dev/`
  - `List-Available-Models`
  - `Get-Usage-Limits`（origin=KIRO_CLI, CREDIT, 可要 email）
  - `List-Available-Profiles`
- 区：us-east-1 / eu-central-1，跟 profile ARN 走
- refresh：social → `prod.<region>.auth.desktop.kiro.dev/refreshToken`；IdC → AWS OIDC `/token`
- 模型：`auto` + 账号自己的 list；Claude / auto 可带 thinking budget（10k/20k/30k/50k）

---

## 分发层建议（下一步，还没写代码）

不要重写 Magpie。做一个薄网关：

```
客户端 /v1/messages
        ↓
   kiro-proxy dispatcher
        ↓ 选号（粘滞优先，其次额度）
   插件协议 或 直接 runtime.kiro.dev
```

必做：

1. **号池文件** `accounts.jsonl`：oauth token / api key / 可选 cli 引用；每号独立 refresh 锁。
2. **会话键** `conversation_id | prompt_cache_key | x-session-id | messages 前缀 hash`。
3. **粘滞表** `session → {accountId, kiroConversationId, lastAt, cacheRead}`，TTL 5–10 分钟可续。
4. **稳定 conversationId**：首次 UUID，之后同会话复用；不要每次 `randomUUID()`。
5. **选号**：新会话按剩余额度 / 冷却；旧会话优先原号；429/USAGE_LIMIT 才换号并作废旧 conversationId。
6. **观察**：把上游 `cacheRead/cacheWrite` 打进日志，没有这个数一切都是猜。

刻意不做：

- 不要本地存模型输出当 “cache”（那是另一套，而且容易串号）
- 不要多号轮询同一段对话
- 不要改 Magpie 安装目录里的插件；改我们自己这份 `vendor/` 或新写 Go

---

## 实测环境

- Magpie 已装 Kiro 插件，但 Kiro 登录是 CLI/IDE 标记，且在 magpie 里被 hidden
- 还没有可用号池。下一步才是找号、落 accounts、打一条请求看 `cacheRead`
