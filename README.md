# kiro-proxy

本地 Kiro 反代 + 自有号池分发。对下游说 Anthropic Messages / OpenAI Chat Completions / OpenAI Responses，对上游说 Kiro `generateAssistantResponse`。
下游按 key 分发（预算 / 周期 / RPM / 模型白名单），每次请求记账，管理台看用量与账单。
目标是把上游 prompt cache 命中率打上去。

**不做本地补全缓存。** 把同一段对话认回来，钉号、钉 `conversationId`、让请求前缀字节稳定，让 Kiro 自己的 cache 打中。详见 `notes/cache-strategy.md`。

## 快速开始

```sh
make build                           # 或 go build -o bin/kiro-proxy.exe ./cmd/kiro-proxy
bin/kiro-proxy.exe login                # 浏览器登录（Google / GitHub / Builder ID / IdC）并加入号池，可重复加多号
bin/kiro-proxy.exe import-ide           # 或：导入 Kiro IDE 当前登录（~/.aws/sso/cache/kiro-auth-token.json）
bin/kiro-proxy.exe list
bin/kiro-proxy.exe                      # 默认 127.0.0.1:8787
bin/kiro-proxy.exe -version
```

`login` 需要 IDE 的回调端口（3128、4649…）空闲，登录前关掉 Kiro IDE 的登录页。公司自建 IdP（external_idp）暂不支持。

> **同一个 Kiro 账号只走一条路。** `login` 加的号自己持有 refresh token，不写回任何文件；`import-ide` 加的号刷新后会写回 IDE 的 token 文件。
> 若同一个账号两种加法的记录都在池里（或 IDE 里仍登录着它），两边各自 refresh 会使对方的 refresh token 失效，
> 表现为该号被自动停用并记 `sign-in expired`。加号前先 `kiro-proxy list` 看池里有没有同一邮箱的号。

## 管理台

浏览器打开 `http://127.0.0.1:8787/`（或 `/ui`）。七个章节：

- **概览**：命中率、号池计数、今日 / 本月 credits、平均 TPS、配置；prompt token + 缓存命中率趋势折线（24 小时按小时 / 7 天 / 30 天按天）。
- **号池**：浏览器登录 / 导入 IDE / 手填凭证；单号刷新、启停、删除；勾选后**批量**测试（换取凭证 + 读额度，不花 credits）、刷新额度、刷新 token、启停、删除；额度与冷却倒计时。
- **用量账单**：按日期 / key / 号 / 协议筛选；credits、命中率、平均 TPS；每日 credits + 命中率折线；按 key / 号 / 模型分组；明细分页；CSV 导出。
- **Key**：新建、改预算 / RPM、启停、换 secret、删除；本周期已用进度条、最近使用。
- **请求**：最近 500 次上游尝试，含号、thread、conversation、cache read/write、credits、TPS、错误。
- **模型**（credit 倍率、上下文、是否支持缓存）、**接入说明**。

页面本身不带数据，全部走 `/admin/*`：设了 `admin_token` 时页面会提示输入，存在浏览器 localStorage。**没设 `admin_token` 时 `/admin` 只接受本机访问**（回环地址连入、Host 为 `localhost` / `127.0.0.1`、且不带 `X-Forwarded-For` / `Forwarded` / `X-Real-IP` 等转发头，挡局域网、DNS rebinding 与反代）。**放在反代后面时必须设 `admin_token`**；监听 `0.0.0.0` 又要从别的机器管理就必须设 `admin_token`。`/admin` 还拒绝跨站写请求（`Sec-Fetch-Site` / `Origin` 校验）。导入 IDE 凭证时自定义路径只接受名为 `kiro-auth-token.json` 的普通文件（它之后会被读取、刷新时写回）。

客户端：

- Claude Code 等：`ANTHROPIC_BASE_URL=http://127.0.0.1:8787`
- OpenAI SDK / Cursor / Codex 等：`OPENAI_BASE_URL=http://127.0.0.1:8787/v1`
- 鉴权：管理台建了 key 或配了 `api_keys` 后，带 `x-api-key` 或 `Authorization: Bearer`。两者都没有时不校验。

也可以 `POST /admin/accounts` 直接给凭证：

```json
{"label":"b","cred":{"method":"social","access_token":"...","refresh_token":"...","region":"us-east-1"}}
{"label":"c","cred":{"method":"idc","refresh_token":"...","client_id":"...","client_secret":"...","region":"us-east-1"}}
{"label":"k","cred":{"access_token":"ksk_..."}}
{"import":"ide","path":"~/backup/kiro-auth-token.json"}
```

## 配置 `kiro-proxy.json`（可省略，缺省即下表）

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | 监听非本机且未设 `api_keys` 会告警 |
| `accounts_file` | `accounts.json` | 号池文件，含 token，0600，已在 .gitignore |
| `api_keys` | 空 | 不记名、不受预算约束的全权 key（自用） |
| `keys_file` | `keys.json` | 管理台建的下游 key（预算 / 周期 / RPM / 模型白名单）。只存 secret 的 sha256；明文只在新建 / 换 secret 时显示一次。旧版明文文件首次载入时自动改成哈希 |
| `usage_file` | `usage.jsonl` | 用量账本；空字符串 = 只记内存 |
| `usage_retention` | `2160h` | 账本保留期（90 天） |
| `cache_mode` | `protocol` | 本地 cache 计量：`protocol`（Anthropic Messages 走 `explicit`，OpenAI Chat / Responses 走 `auto`）、`auto`（自动前缀缓存，5 分钟滑动 TTL，忽略 cache_control）、`explicit`（只认 cache_control / 顶层 cache_control，与 Anthropic 规则一致）、`off` |
| `cache_ttl` | `client` | 客户端声明 `ttl:"1h"` 时：`client` = 按 1h 计（写入 2 倍输入价）；`5m` = 一律按 5m 计 |
| `cache_points` | 空 | 实验：给 Kiro 发显式 `cachePoint{type:"default"}` 的位置，`first-user` / `assistant` / `tools`。探测（2026-10-07）不报错也没效果，保持关闭 |
| `credit_rates` | 探测拟合值 | 按 token 估 credits 的系数（模型前缀 → `{"context","output","read_factor"}`，每百万 token 的 credits），只用于上游来不及报 credits 的中断请求。内置 claude：4.108 / 109 / 0.53（sonnet-4.5 实测） |
| `reported_usage` | `conservative` | 上游若报 tokenUsage 的口径：`conservative`（多条取最后一条、输入扣 Kiro 隐藏 token）/ `raw` / `sum` / `ignore`。上游目前不报 |
| `openai_hosted_tools` | `drop` | OpenAI 内置工具（web_search 等，Codex 默认带）：`drop` 跳过这些工具与它们的历史调用并打 debug 日志；`reject` 返回 400 说明原因 |
| `identity` | CLI 1.28.3 | 对上游的客户端身份：`{"cli_version":"1.28.3","desktop_ua":"Kiro-Desktop/0.2.13 (darwin; arm64)"}`。对话 / OIDC 刷新 / management 都用 CLI 身份，每个号一个固定机器码 |
| `cost_basis` | `api` | 对下游收费口径（key 的 `limit_usd`）：`api` = Claude API 官方价（默认，下游可逐项复核）；`credits` = credits × `credit_usd`。下游 usage 不报美元 |
| `credit_usd` | `0.02` | 一个 Kiro credit 的美元价：账本里的「上游成本」= credits × 它，毛利 / 亏损按它算 |
| `prices` | 内置 | 覆盖价格表：模型 id 前缀 → `{"input","output","cache_write","cache_write_1h","cache_read"}`（USD/百万 token），优先级最高 |
| `price_sync` | `24h` | 在线价格（仅 OpenRouter，过滤为 Kiro 模型）拉取间隔；`0` = 只用内置表 |
| `prices_file` | `prices.json` | 在线价格缓存；空 = 只在内存 |
| `admin_token` | 空 | `/admin` 的 `X-Admin-Token`；空 = 只允许本机访问 `/admin` |
| `session_ttl` | `45m` | 会话→号、→conversationId 的空闲过期 |
| `conversation_mode` | `session` | `random` = 与 Magpie 一致每次随机，用于 A/B 对照命中率 |
| `sort_tools` | `true` | 工具声明按名排序（MCP 加载顺序不定） |
| `pin_thinking` | `true` | 会话内 thinking 预算以首次为准 |
| `system_strip` | 空 | 从 system 删掉的正则；确认打破 cache 再加 |
| `model_aliases` | 空 | 下游模型名 → Kiro 模型 id |
| `max_attempts` | `3` | 一次请求最多试几个号 |
| `max_concurrent` | `3` | 每个号同时进行的请求数默认上限；号上单独设了 `max_concurrent`（加号时 / 号池文件）就以号上的为准。`0` = 不限 |
| `limits_interval` | `10m` | 额度轮询间隔，`0s` 关闭 |
| `breaker_window` | `2m` | 全局熔断窗口：窗口内超过一半的启用号（且至少 2 个）出现同一类失败（刷新失败 / 403 / 网络错误），判定为全局故障：只冷却、不停号、不累计停号次数，打告警并在管理台显示；任一请求成功即解除。`invalid_grant` 与封号文字不受影响，照样立即停号 |
| `log_level` | `info` | |

## cache 命中做了什么

1. **会话键**：header（`X-Session-Id` 等）→ `metadata.user_id` 里的 Claude Code session → system + 首条 user 指纹。
2. **粘号**：同一会话始终回到同一号；只有该号冷却/停用/满并发才换。新会话挑并发最少、剩余额度最多、最久没用的号。
3. **稳定 conversationId**：按 会话 → 号 → conversationId 钉住。会话在几个号之间来回（并发分流、换号后回退）时每个号保住自己的；某号上历史被回退/编辑/压缩（新消息序列不再以该号上次的序列为前缀）时只轮换该号的。
4. **前缀修复**：剥离 Claude Code 的 `x-anthropic-billing-header`（`cch` 每次变）、工具排序、thinking 钉住、可选 system 正则。
5. **分号统计**：`/admin/stats` 与 `/admin/accounts` 给出 `cache_read / cache_write / input`，切 `conversation_mode` 做 A/B。

## 用量与计费口径

Kiro 的流里一般**不报 token**，只报上下文占用百分比和 credits。所以：

- **input / output**：总量以上游为准。Kiro 报的上下文占用（百分比 × 窗口）是该模型分词器下的真实 token，减去 Kiro 自带的 system（按模型实测，约 3.6k–4k）就是本轮请求加输出。本地估算只用来分配普通输入 / 缓存读 / 缓存写 / 输出的比例；上游报了 `tokenUsage` 则直接用。`count_tokens` 与流式 `message_start` 在上游响应前，只能是本地估算；流式以结尾 `message_delta` 的累计 usage 为准。
- **cache_read / cache_creation**：Anthropic Messages 按 Anthropic 官方规则在本地模拟：只认客户端 `cache_control`（含顶层自动缓存），每个断点往前回看 20 块，最多 4 个断点，最小可缓存长度按官方分模型表（512 / 1024 / 2048 / 4096），命中按条目自己的 TTL 续期。混合 TTL 三段计费：最长命中前缀计读，到最后一个 1h 断点计 1h 写入（2 倍），再到最后一个断点计 5m 写入（1.25 倍）。响应带 `cache_creation.ephemeral_5m_input_tokens / ephemeral_1h_input_tokens`。超过 4 个断点或 1h 排在 5m 后面时官方回 400，这里只记警告，按不多扣的方式计（多出的断点忽略，那个 1h 降成 5m）。OpenAI 协议没有 cache_control，走自动前缀缓存。上游声明不支持 prompt caching 的模型不计缓存。Kiro 上游不分 TTL：1h 只是按客户端声明在本地计费。
- **两本账**：每笔记对下游收费（`cost_usd`，Claude API 官方价）与上游成本（`upstream_usd` = credits × `credit_usd`），管理台按 key / 号 / 模型 / 天显示毛利率、亏损笔数与金额。小请求因 Kiro 自带的约 4k 隐藏 token 会小额亏损，不加最低收费。
- **中断与重试**：客户端中断、中途断流的请求按已消耗的量入账（`aborted`）；上游没来得及报 credits 时按 token 估（`credits_estimated`，系数见 `credit_rates`）。换号重试的失败尝试单独一行（`retried`，同一 `request`）：**不向下游收费**（`cost_usd` = 0）、不扣 key 预算、不计请求数，上游成本全额计入「重试亏损」，管理台与「小请求亏损」分开列。
- **未归属 credits**：每个号 Get-Usage-Limits 的已用增量（含奖励额度与超额）减去同时段账本记的 credits，差额在管理台单列。
- **隐藏 token 监控**：小请求上反推 Kiro 自带上下文（上游上下文 − 本地内容），中位数偏离常数超过 ±100 时日志告警、管理台提示。
- **下游 usage**：只报 token 与 Kiro credits，**不报美元**。流式 `message_start` 的 usage 是保守下界（缓存两项为 0），最终值以 `message_delta` 为准。
- **管理台**：按上游 credits 统计消耗；TPS = 输出 token / 生成秒（总耗时减去首字时延）。
- key 的预算按各自周期（day / week / month / total，本地时区）累计，超限返回 402 + `x-should-retry: false`。准入时为在途请求预留一份估计额度，并行请求不会一起超支。
- **会话状态**：钉号、conversationId、消息指纹、thinking 预算与本地 prompt cache 一起落盘（`cache_file` 与旁边的 `*.sessions.json`），重启后会话接着打中上游缓存；任一缺失就两个都不用。退出时等在途请求跑完（最多 2 分钟）再落盘。

## 接口

- `POST /v1/chat/completions`、`POST /v1/responses`（流式 / 非流式，含工具调用、reasoning），`usage` 里带 `cached_tokens` 与 `credits`
- `POST /v1/messages`（流式 / 非流式）、`POST /v1/messages/count_tokens`（粗估）、`GET /v1/models`（号池拿不到模型列表时 503，不编造）
- `max_tokens` / `tool_choice` 读了但不转发，`temperature` / `stop_sequences` / `top_p` 没有字段（解 JSON 时丢弃）：Kiro 的接口都不收（与参考插件一致）
- 路径前缀容错：`/messages`、`/v1/v1/messages`、`/anthropic/v1/messages`、尾 `/`、重复 `/` 都落到同一 handler
- `GET/POST /admin/accounts`、`DELETE /admin/accounts/{id}`、`POST /admin/accounts/{id}/{enable|disable|refresh}`、`GET /admin/stats`
- `POST /admin/accounts/batch` `{"action":"test|limits|refresh|enable|disable|delete","ids":[]}`（ids 空 = 全部，delete 必须给 ids）
- `GET /admin/usage?from&to&key&account&model&protocol&status&bucket&page&page_size`、`GET /admin/billing?from&to`（默认本月）、`GET /admin/prices`、`POST /admin/prices/refresh`、`GET /admin/models`、`POST /admin/models/refresh`；时间可写 RFC3339、`YYYY-MM-DD` 或 `24h` / `7d`

## 失败处理

| 上游 | 动作 |
|---|---|
| 401 / 明确的 token 失效 403 | 单飞 refresh 后同号重试 |
| 其它 403（策略 / 防火墙……） | 不刷新，短冷却并换号 |
| refresh 返回 400 `invalid_grant` | 立即停用（refresh token 已失效） |
| refresh 的其它 4xx | 冷却并计数，连续 3 次才停用；成功一次清零。IdC client 注册过期标「需要重新登录」，不停号 |
| 封号（403 `TemporarilySuspended` 等 / 423） | 不刷新、直接停用该号，换号 |
| 额度用尽（含 402） | 冷却至额度重置（未知则 1h），换号 |
| 429 / throttling | 冷却：上游给了 `x-amzn-kiro-ratelimit-retry-after`（毫秒，上限 5 分钟）就按它，否则指数退避；换号 |
| 5xx / 网络 | 短冷却，换号 |
| 输入太长 / 校验错 | 直接返回，不换号 |
| 全部不可用 | 429 + `Retry-After` |

- 非流式：整条收完才回写。上游中途报错或断流时丢掉半截内容，记到号上并换号；没有可换的号就回错误状态码，绝不把半句话当 `end_turn`。
- 流式：已开始向下游写 SSE 后不再换号。中途报错 / 断流发 `error` 事件（不发 `message_stop`），同时记到号上（冷却、统计）。
- 钉的号只是并发打满：先排队等它（最多 3 秒），等不到再临时借用别的号，但会话仍钉在原号（原号上的上游缓存还在）。
- external-idp 的 refresh token 只发到 https 的 Microsoft Entra 登录域名；凭证里写了别的地址就拒绝并停用。

## 探测 Kiro 的真实行为

```
kiro-proxy -config kiro-proxy.json probe usage        # 上游报不报 tokenUsage、报几次、是否累计
kiro-proxy -config kiro-proxy.json probe cachepoint   # 显式 cachePoint 开 / 不开
kiro-proxy -config kiro-proxy.json probe credits      # 5k/20k/50k 前缀冷热 + 长输出，拟合 credits 折算
kiro-proxy -config kiro-proxy.json probe ttl          # 缓存存活时间，同 / 新 conversationId 两组（约 70 分钟）
kiro-proxy -config kiro-proxy.json probe analyze -out probe.jsonl
```

用一个号直打上游（会花 credits），每次调用一行 JSONL（`-out`，默认 `probe.jsonl`），每个实验前后各拉一次额度核对。`analyze` 给出 TTL 命中表、cachePoint 对比、可直接贴进配置的 `credit_rates`。

## 开发

`make check`（gofmt + vet + test -race）、`make release`（交叉编译到 `dist/`，版本取自 `git describe`）。CI 见 `.github/workflows/ci.yml`。

## 目录

- `cmd/kiro-proxy` — 入口、`login` / `import-ide` / `list` / `probe` / `-version`
- `internal/probe` — Kiro 缓存 / 计费探测与分析
- `internal/anthropic` — 下游 Messages 类型
- `internal/openai` — Chat Completions / Responses 与 Messages 互转
- `internal/turn` — 协议无关的回复接口（Sink）
- `internal/meter` — token 估算、本地 prompt cache 模拟、价格表
- `internal/keys` — 下游 key 库
- `internal/usage` — 用量账本（明细 + 按小时聚合，JSONL 持久化）
- `internal/kiro` — 请求构造、event stream 解码、thinking 解析、鉴权 / refresh / 管理 API、失败分类、浏览器登录
- `internal/pool` — 号池、单飞 refresh、选号、冷却、额度轮询
- `internal/normalize` — 前缀稳定化
- `internal/server` — HTTP、换号重试、SSE、计量记账、管理 API 与管理台
- `internal/sessionpin` — 会话键、粘号、稳定 conversationId
- `vendor/opencode-kiro-auth` — Magpie 在用的 Kiro 插件，只作协议参考

## 未做

- kiro-cli 登录导入（其 SQLite 存储需要额外依赖；用 `login` 代替）
- 公司自建 IdP 登录
- web search 替代

## 来源

- Magpie: `D:/go/magpie` @ `94c5b54`
- 插件: `@magpie-community/opencode-kiro-auth`（`vendor/` 副本）
- 会话钉号模式来自 Buddy Proxy `internal/sessionpin`
