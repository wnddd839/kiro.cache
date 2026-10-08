# kiro-proxy v0.2.0

第二个公开版本：把上游前缀稳定性做扎实，并按模型能力使用原生思考参数。

## 新增

- **原生思考参数**：首次请求读取模型目录的 `additionalModelRequestFieldsSchema`，按声明的 `output_config.effort` 或 `reasoning.effort` 在请求根部发送强度，不再把预算标签写进 prompt。无 schema 的旧模型保留原有标签路径。
- **前缀逐字节诊断**：`debug_requests_dir` 开启后保存实际发送的上游请求与相邻比较，报告消息是否延续、首个不同字节，以及账号、会话、模型参数、工具是否变化。默认关闭，文件含完整对话与图片，调试后请清理。
- **工具描述截断**：限制为 10240 UTF-8 字节，保留完整字符；本地计量与上游请求共用截断后的描述。

## 修复

- **部分 usage 事件**：按字段保留最后值，缺失不覆盖、显式 0 覆盖，不再因只报 outputTokens 清空输入侧。
- **首条内容前失败**：上游已报 credits / tokenUsage 时单独记上游成本，不假定整段输入已处理。
- **历史图片**：全部历史图片随原消息重发，新增图片不再删除旧图。
- **JSON 键顺序**：规范化工具 schema 与 tool-use input 的对象键顺序，数组顺序与数值精度保留。
- **历史工具占位**：占位声明按名字排序、内容固定。
- **旧思考块**：不转发，也不参与会话指纹，思考文本变化不再无故轮换 conversationId。
- **并发首请求**：首次答复前共享同一临时账号，避免并发首请求落到不同号。
- **换号策略**：会话钉住的账号忙时排队，超时返回 429 + Retry-After，不借号破坏前缀；仅账号不可用时换号。

## 变更

- `session_ttl` 默认 45m → 24h：活跃会话续期，无统一结束信号时按空闲过期（容量上限 4096）。
- 项目更名 kiro.cache；发布包、启动命令与配置文件仍沿用 kiro-proxy 名称。

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
kiro-proxy login        # 浏览器登录并加入号池
kiro-proxy list         # 查看号池
kiro-proxy              # 启动，默认 127.0.0.1:8787
```

客户端：

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787     # Claude Code 等
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1     # OpenAI SDK / Cursor / Codex 等
```

管理台：`http://127.0.0.1:8787/ui`，在「接入」页一键生成 API Key。

## 说明

- 仅用于个人学习、研究与自用；使用前请确认符合 Kiro / AWS 的服务条款。
- 详细配置、计费口径与失败处理见 [README](https://github.com/wnddd839/kiro.cache#readme)；上游前缀稳定性与 Sonnet 5.5 实测见 [notes/prefix-stability.md](https://github.com/wnddd839/kiro.cache/blob/main/notes/prefix-stability.md)。
