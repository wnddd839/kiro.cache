# kiro-proxy v0.1.0

首个公开版本。

## 这是什么

本地 Kiro 反代 + 自有号池分发。对下游说 **Anthropic Messages / OpenAI Chat Completions / OpenAI Responses**，对上游说 Kiro `generateAssistantResponse`。

不做本地补全缓存：把同一段对话认回来 —— 钉号、钉 `conversationId`、让请求前缀字节稳定，让 Kiro 自己的 prompt cache 打中。

## 亮点

- **三种下游协议**，流式 / 非流式、工具调用、reasoning
- **号池**：浏览器 OAuth（Google / GitHub / Builder ID / IdC）、导入 Kiro IDE 凭证、手填 token / API key；单飞 refresh、冷却、额度轮询、全局熔断
- **缓存友好分发**：会话粘号 + 稳定 conversationId + 前缀修复（剥离每次都变的 `x-anthropic-billing-header`、工具排序、thinking 钉住）
- **计量与账单**：token 估算、本地按 Anthropic 规则模拟 cache read/write、credits 折算，对下游收费与上游成本两本账
- **管理台**：号池 / 用量账单 / 接入 / 请求 / 模型；接入页一键生成 API Key

## 安装

下载对应平台二进制：

| 平台 | 文件 |
|---|---|
| Windows x64 | `kiro-proxy-windows-amd64.exe` |
| Linux x64 | `kiro-proxy-linux-amd64` |
| macOS Apple Silicon | `kiro-proxy-darwin-arm64` |

校验：

```sh
sha256sum -c SHA256SUMS.txt
```

或自行构建：`make build`（需要 Go ≥ 1.26）。

## 快速开始

```sh
kiro-proxy login        # 浏览器登录并加入号池
kiro-proxy list         # 查看号池
kiro-proxy              # 启动，默认 127.0.0.1:8787
```

客户端：

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787     # Claude Code 等
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1     # OpenAI SDK / Cursor / Codex 等
```

管理台：`http://127.0.0.1:8787/ui`，接入页可一键生成 API Key。

## 说明

- 仅用于个人学习与自用；使用前请确认符合 Kiro / AWS 服务条款。
- 详细配置、计费口径、失败处理见 [README](https://github.com/wnddd839/kiro-proxy#readme)。
