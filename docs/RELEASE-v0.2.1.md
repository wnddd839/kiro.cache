# kiro-proxy v0.2.1

补丁版本：修复在服务器上无法完成浏览器登录的问题。

## 修复

- **服务器上登录**：浏览器登录的回调固定打向 `localhost:<port>`，在服务器上到不了代理进程，之前会一直卡在「等待浏览器完成登录」。现在本机监听与手工提交共用同一套回调处理，支持把浏览器地址栏里的回调 URL 粘贴回来，两种入口都可用：
  - **CLI**：`kiro-proxy login` 会提示粘贴回调 URL（可粘完整地址，也可只粘 `?` 后的查询串）。
  - **管理台**：号池页「开始登录」后出现「粘贴回调地址」输入框。
  - **接口**：`POST /admin/login?callback=<url>`。
- Builder ID / IdC 登录会先返回一个 AWS 登录链接，继续登录后再次粘贴回调即可。
- 贴错或不属于本次登录的 URL 会返回错误，但不会中断登录，可以重贴。

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

也可自行构建，需要 Go ≥ 1.26：

```sh
go build -o bin/kiro-proxy ./cmd/kiro-proxy
```

## 快速开始

```sh
kiro-proxy login        # 浏览器登录并加入号池；服务器上按提示粘贴回调地址
kiro-proxy list         # 查看号池
kiro-proxy              # 启动，默认 127.0.0.1:8787
```

客户端：

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787     # Claude Code 等
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1     # OpenAI SDK / Cursor / Codex 等
```

管理台：`http://127.0.0.1:8787/ui`。

## 说明

- 仅用于个人学习、研究与自用；使用前请确认符合 Kiro / AWS 的服务条款。
- 功能与配置见 [README](https://github.com/wnddd839/kiro.cache#readme)。
