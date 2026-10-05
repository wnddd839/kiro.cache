# kiro-go

本地 Kiro 反代 + 自有号池分发。对下游说 Anthropic Messages，对上游说 Kiro `generateAssistantResponse`。
目标是把上游 prompt cache 命中率打上去。

**不做本地补全缓存。** 把同一段对话认回来，钉号、钉 `conversationId`、让请求前缀字节稳定，让 Kiro 自己的 cache 打中。详见 `notes/cache-strategy.md`。

## 快速开始

```sh
make build                           # 或 go build -o bin/kiro-go.exe ./cmd/kiro-go
bin/kiro-go.exe login                # 浏览器登录（Google / GitHub / Builder ID / IdC）并加入号池，可重复加多号
bin/kiro-go.exe import-ide           # 或：导入 Kiro IDE 当前登录（~/.aws/sso/cache/kiro-auth-token.json）
bin/kiro-go.exe list
bin/kiro-go.exe                      # 默认 127.0.0.1:8787
bin/kiro-go.exe -version
```

`login` 需要 IDE 的回调端口（3128、4649…）空闲，登录前关掉 Kiro IDE 的登录页。公司自建 IdP（external_idp）暂不支持。

客户端：`ANTHROPIC_BASE_URL=http://127.0.0.1:8787`，设了 `api_keys` 就带 `x-api-key` 或 `Authorization: Bearer`。

也可以 `POST /admin/accounts` 直接给凭证：

```json
{"label":"b","cred":{"method":"social","access_token":"...","refresh_token":"...","region":"us-east-1"}}
{"label":"c","cred":{"method":"idc","refresh_token":"...","client_id":"...","client_secret":"...","region":"us-east-1"}}
{"label":"k","cred":{"access_token":"ksk_..."}}
{"import":"ide","path":"~/backup/kiro-auth-token.json"}
```

## 配置 `kiro-go.json`（可省略，缺省即下表）

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | 监听非本机且未设 `api_keys` 会告警 |
| `accounts_file` | `accounts.json` | 号池文件，含 token，0600，已在 .gitignore |
| `api_keys` | 空 | 下游 key；空不校验 |
| `admin_token` | 空 | `/admin` 的 `X-Admin-Token` |
| `session_ttl` | `45m` | 会话→号、→conversationId 的空闲过期 |
| `conversation_mode` | `session` | `random` = 与 Magpie 一致每次随机，用于 A/B 对照命中率 |
| `sort_tools` | `true` | 工具声明按名排序（MCP 加载顺序不定） |
| `pin_thinking` | `true` | 会话内 thinking 预算以首次为准 |
| `system_strip` | 空 | 从 system 删掉的正则；确认打破 cache 再加 |
| `model_aliases` | 空 | 下游模型名 → Kiro 模型 id |
| `max_attempts` | `3` | 一次请求最多试几个号 |
| `limits_interval` | `10m` | 额度轮询间隔，`0s` 关闭 |
| `log_level` | `info` | |

## cache 命中做了什么

1. **会话键**：header（`X-Session-Id` 等）→ `metadata.user_id` 里的 Claude Code session → system + 首条 user 指纹。
2. **粘号**：同一会话始终回到同一号；只有该号冷却/停用/满并发才换。新会话挑并发最少、剩余额度最多、最久没用的号。
3. **稳定 conversationId**：按 会话 → 号 → conversationId 钉住。会话在几个号之间来回（并发分流、换号后回退）时每个号保住自己的；某号上历史被回退/编辑/压缩（新消息序列不再以该号上次的序列为前缀）时只轮换该号的。
4. **前缀修复**：剥离 Claude Code 的 `x-anthropic-billing-header`（`cch` 每次变）、工具排序、thinking 钉住、可选 system 正则。
5. **分号统计**：`/admin/stats` 与 `/admin/accounts` 给出 `cache_read / cache_write / input`，切 `conversation_mode` 做 A/B。

## 接口

- `POST /v1/messages`（流式 / 非流式）、`POST /v1/messages/count_tokens`（粗估）、`GET /v1/models`（号池拿不到模型列表时 503，不编造）
- `max_tokens` / `temperature` / `stop_sequences` / `tool_choice` 解析但不转发：Kiro 的接口不收（与参考插件一致）
- 路径前缀容错：`/messages`、`/v1/v1/messages`、`/anthropic/v1/messages`、尾 `/`、重复 `/` 都落到同一 handler
- `GET/POST /admin/accounts`、`DELETE /admin/accounts/{id}`、`POST /admin/accounts/{id}/{enable|disable|refresh}`、`GET /admin/stats`

## 失败处理

| 上游 | 动作 |
|---|---|
| 401/403 token | 单飞 refresh 后同号重试；refresh 被拒 → 停用并记 note |
| 额度用尽 | 冷却至额度重置（未知则 1h），换号 |
| 429 / throttling | 指数退避冷却，换号 |
| 5xx / 网络 | 短冷却，换号 |
| 输入太长 / 校验错 | 直接返回，不换号 |
| 全部不可用 | 429 + `Retry-After` |

- 非流式：整条收完才回写。上游中途报错或断流时丢掉半截内容，记到号上并换号；没有可换的号就回错误状态码，绝不把半句话当 `end_turn`。
- 流式：已开始向下游写 SSE 后不再换号。中途报错 / 断流发 `error` 事件（不发 `message_stop`），同时记到号上（冷却、统计）。

## 开发

`make check`（gofmt + vet + test -race）、`make release`（交叉编译到 `dist/`，版本取自 `git describe`）。CI 见 `.github/workflows/ci.yml`。

## 目录

- `cmd/kiro-go` — 入口、`login` / `import-ide` / `list` / `-version`
- `internal/anthropic` — 下游 Messages 类型
- `internal/kiro` — 请求构造、event stream 解码、thinking 解析、鉴权 / refresh / 管理 API、失败分类、浏览器登录
- `internal/pool` — 号池、单飞 refresh、选号、冷却、额度轮询
- `internal/normalize` — 前缀稳定化
- `internal/server` — HTTP、换号重试、SSE、管理 API
- `internal/sessionpin` — 会话键、粘号、稳定 conversationId
- `vendor/opencode-kiro-auth` — Magpie 在用的 Kiro 插件，只作协议参考

## 未做

- kiro-cli 登录导入（其 SQLite 存储需要额外依赖；用 `login` 代替）
- 公司自建 IdP 登录
- OpenAI Chat Completions 入口
- web search 替代

## 来源

- Magpie: `D:/go/magpie` @ `94c5b54`
- 插件: `@magpie-community/opencode-kiro-auth`（`vendor/` 副本）
- 会话钉号模式来自 Buddy Proxy `internal/sessionpin`
