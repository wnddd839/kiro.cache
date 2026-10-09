# kiro-proxy v0.3.0

本次更新调整默认缓存计量与下游输入收费口径，并修复自审发现的流式和上下文校准问题。已有下游接入方升级前应核对计费规则。

## 计量与收费变化

- **默认自动缓存计量**：`cache_mode` 默认由 `protocol` 改为 `auto`。Anthropic Messages、OpenAI Chat Completions、Responses 都自动模拟 5 分钟前缀缓存，无需客户端提供 `cache_control`；适用于 OpenAI 客户端经 sub2api 转为 Anthropic 的链路。
- **保留显式配置**：已有配置中写明 `protocol`、`explicit`、`off` 的，升级后保持原行为。`auto` 忽略客户端断点与 1h TTL，固定按 5m 模拟；要按客户端断点和 TTL 计量，可选择 `protocol`（Messages）或 `explicit`。
- **Kiro 自带输入计费**：上游未报告输入侧 tokenUsage 时，Kiro 自带 system / 对话模板按现有分模型基线估算，计入下游普通输入。Claude 默认基线 4052 token，DeepSeek 为 3699；其它已测模型使用对应基线，未测模型回退为 Claude 基线。缓存读写不重复包含该基线。
- **保留上游报告**：上游报告输入侧 tokenUsage 时，不再扣除 Kiro 自带输入，也不额外叠加本地基线。缺失字段保留最后报告值，显式 0 保留为 0；`raw` 保留兼容，与 `conservative` 一样采用报告值。
- **计费链路对齐**：三种协议的流式 / 非流式响应、预算预留、`count_tokens` 和账本包含同一输入口径。上游 credits 的内部估算不重复添加基线，仍区分冷暖成本。

## 自审修复

- **流式开头多收费风险**：`message_start` 的 usage 统一报 0，最终累计用量放在 `message_delta`。避免首次收到上游输入为 0 的报告时，网关保留开头的非零估计而多收费。下游必须读取流尾，不能只按开头计费，也不能将两者相加。
- **精确输出不再缩放**：上游只报告 outputTokens 时，保留报告的输出计数，只校准其余本地输入估算。
- **ignore 模式校准**：`reported_usage: "ignore"` 忽略 token 报告后，仍能根据上下文百分比校准本地用量。

## 升级注意

1. 配置省略 `cache_mode` 时会使用新默认 `auto`，这会改变 Anthropic 缓存拆分和费用。保留旧默认行为可显式设置 `"cache_mode": "protocol"`。
2. Kiro 基线收费会增加新请求的普通输入与费用；这是估算收费规则，不是每次上游实测的未缓存输入，应同步到下游计费说明。
3. Anthropic 的普通输入、缓存创建、缓存读取分别计费；缓存创建价已包含处理该输入的费用，不应再对同一份 token 收普通输入费。OpenAI 的总输入字段已包含缓存，不可重复相加。
4. 不迁移或重算历史账本，不删除缓存 / 会话状态。保留配置、账号、key、账本和配套状态文件后，替换对应平台 binary 并重启服务即可。
5. 缓存拆分主要是本地模拟，不保证 Kiro 实际命中或每笔请求盈利。应比较下游实际扣费与真实 credits 成本，并核对下游渠道价格和倍率。
6. nginx 的 API / 管理台鉴权分离及服务级代理环境变量说明已补充至 README；管理台在带路径前缀的反代下生成 Base URL 的限制仍需手动处理。

## 安装

| 平台 | 文件 |
|---|---|
| Windows x64 | `kiro-proxy-windows-amd64.exe` |
| Linux x64 | `kiro-proxy-linux-amd64` |
| macOS Apple Silicon | `kiro-proxy-darwin-arm64` |

校验：

```sh
sha256sum -c SHA256SUMS.txt
```

查看版本：

```sh
./kiro-proxy-linux-amd64 -version
```

自行构建（需要 Go 1.26.4 或更高）：

```sh
go build -trimpath -ldflags "-s -w -X main.version=v0.3.0" -o bin/kiro-proxy ./cmd/kiro-proxy
```

## 说明

- 仅用于个人学习、研究与自用；使用前请确认符合 Kiro / AWS 的服务条款。
- 功能、计费口径和配置见 [README](https://github.com/wnddd839/kiro.cache#readme)。
