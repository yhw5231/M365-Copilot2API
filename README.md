# M365 Copilot2API

<p align="center">
  <img src="https://img.shields.io/github/license/yhw5231/M365-Copilot2API" alt="License">
  <img src="https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go" alt="Go Version">
  <img src="https://img.shields.io/badge/API-OpenAI%20Compatible-412991?logo=openai" alt="OpenAI Compatible">
  <img src="https://img.shields.io/badge/API-Anthropic%20Compatible-FF6B6B?logo=anthropic" alt="Anthropic Compatible">
</p>

<p align="center">
  <strong>Microsoft 365 Copilot → OpenAI / Anthropic 兼容 API 网关</strong>
</p>

M365 Copilot2API 是一个用 Go 编写的自托管网关，把微软 365 Copilot 商业订阅背后的 **ChatHub 私有协议**（WebSocket）翻译成标准的 **OpenAI / Anthropic 兼容 API**。Claude Code、OpenCode、Cursor 以及任何 OpenAI 客户端都可以直接用熟悉的格式调用 M365 Copilot。

工作原理概括：**ChatHub 私有协议 ⇄ OpenAI / Anthropic 兼容 API**。连接握手、心跳保活、事件流解析、工具调用转换全部封装在 `internal/chathub` 层，对外只暴露 `/v1/chat/completions`、`/v1/messages` 等标准端点。

项目自带完整 Web 管理控制台，覆盖账号授权（OAuth/PKCE）、API Key 管理、代理池、云端对话管理、用量统计与模型测试，适合个人自部署、自托管使用。

> ⚠️ **免责声明（请务必阅读）**
>
> - 本项目**不是微软官方产品**，与 Microsoft、OpenAI、Anthropic 及其关联公司**均无任何从属或合作关系**。
> - 使用第三方账号池、代理转发等方式接入 M365 服务**可能违反服务商服务条款**，由此产生的一切后果由使用者自行承担。
> - 请遵守当地法律法规与目标平台的服务条款（ToS）。
> - 本项目**仅供个人学习与研究**，**禁止用于商业转售或规模化运营**。
> - 账号被封禁、数据丢失等任何损失，本项目维护者与贡献者**概不负责**。

## 界面预览

<p align="center"><img src="docs/screenshots/02-dashboard.png" alt="仪表盘" style="max-width:860px;border-radius:12px;box-shadow:0 8px 32px rgba(0,0,0,.18)"></p>

<table>
  <tr>
    <td align="center" width="33%"><img src="docs/screenshots/01-login.png" alt="登录页" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>登录</b></sub></td>
    <td align="center" width="33%"><img src="docs/screenshots/03-usage.png" alt="用量统计" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>用量统计</b></sub></td>
    <td align="center" width="33%"><img src="docs/screenshots/04-accounts.png" alt="账号管理" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>账号管理</b></sub></td>
  </tr>
  <tr>
    <td align="center" width="33%"><img src="docs/screenshots/05-apikeys.png" alt="API Keys" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>API Keys</b></sub></td>
    <td align="center" width="33%"><img src="docs/screenshots/06-conversations.png" alt="对话管理" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>对话管理</b></sub></td>
    <td align="center" width="33%"><img src="docs/screenshots/07-proxies.png" alt="代理池" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>代理池</b></sub></td>
  </tr>
  <tr>
    <td align="center" width="33%"><img src="docs/screenshots/08-modeltest.png" alt="模型测试" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>模型测试</b></sub></td>
    <td align="center" width="33%"><img src="docs/screenshots/09-settings.png" alt="设置" style="border-radius:10px;box-shadow:0 4px 16px rgba(0,0,0,.12)"><br><sub><b>设置</b></sub></td>
    <td align="center" width="33%"><sub><i>更多功能，等你发现</i></sub></td>
  </tr>
</table>

## 功能特性

| 功能 | 说明 |
|------|------|
| OpenAI 兼容 `/v1/chat/completions` | 支持流式输出与 function calling |
| OpenAI Responses `/v1/responses` | 兼容 Responses 协议（Codex 等客户端） |
| Anthropic 兼容 `/v1/messages` | Claude Code / Cursor 直连 |
| SSE 流式输出 | 逐字实时返回，`stream: true`；预检期/推理静默期自动心跳保活（5s keep-alive），并携带 `X-M365-Execution-Environment` 环境标识头 |
| 工具调用转换 | OpenAI function calling ⇄ M365 工具协议，`router` / `native` 两种规划模式；本地执行类工具（exec_command 等）仅对 Codex 类客户端开放 |
| 会话显式续用 | 仅凭显式会话 ID 续用云端对话（无 ID 请求一律新建对话，绝不按聊天记录相似度复用）；命中时只发送增量消息 |
| 会话显式绑定 | `session_id` / `x-session-id` 请求头精确指定要继续的会话（标准 OpenAI 兼容客户端默认发送，如 DSH / pi-ai） |
| 自动清理 | 按闲置时间（默认 2h）或保留数量回收云端对话 |
| 多账号管理 | PKCE 授权 + 账号轮询 + 故障自动转移 |
| API Key 管理 | 控制台创建 / 撤销 / 回读 |
| 代理池 | HTTP / HTTPS / SOCKS5 代理轮换、健康检查、失败冷却 |
| 用量统计 | 按 key / 账号 / 模型 / 端点聚合（`usage.jsonl`） |
| 缓存命中统计 | 命中率、节省 token 仪表盘 |
| 多模态输入 | 支持图片等附件（base64 data URL / https URL），自动完成 M365 上传与消息注解注入 |
| 图像生成 | `/v1/images/generations`，支持 `url` / `b64_json` 输出，Designer 图片自动下载转存本机 |
| 图像编辑 | `/v1/images/edits`，multipart 上传原图 + 指令，返回编辑后的图片 |
| Web 控制台 | 账号、密钥、代理池、模型、对话、日志一屏管理 |

## 架构

```
┌──────────────┐    OpenAI / Anthropic    ┌──────────────────┐    ChatHub    ┌──────────────┐
│ Claude Code  │ ───────────────────────► │      网关         │ ────────────► │ M365 Copilot │
│ OpenCode     │   /v1/chat/completions   │ (Go, m365-copilot2api) │  WebSocket    │  (云端对话)   │
│ 任意 OpenAI  │   /v1/messages           │  internal/web     │  internal/    │              │
│ 客户端        │   /v1/responses          │                   │  chathub      │              │
└──────────────┘                          └──────────────────┘               └──────────────┘
```

- **协议层（`internal/chathub`）**：封装 M365 Copilot ChatHub 的 WebSocket 私有协议——连接建立、心跳保活、事件流解析（流式 token、工具调用、多模态输入）。对上层只暴露统一的事件接口。
- **会话解析（`internal/web/session_resolver.go`）**：多账号场景下把每个客户端请求稳定解析到固定账号与云端对话，并实现内容键会话复用（见下文原理）。
- **账号轮询与故障转移**：多账号间轮询均衡流量；账号故障（鉴权失效、连接断开等）自动切换到下一个可用账号重试。

## 快速开始

### 环境要求

- Go 1.23+（`go.mod` 声明的最低版本）
- Windows / Linux 均可；Windows 上推荐用仓库自带的 `manage.py` 管理生命周期

### 预编译二进制（推荐）

从 [GitHub Releases](https://github.com/HEXUXIU/M365-Copilot2API/releases) 下载对应平台的二进制：

| 平台 | 架构 | 文件 |
|------|------|------|
| Linux | x86_64 / arm64 / i386 | `m365-copilot2api-linux-{amd64,arm64,386}` |
| Windows | x86_64 / arm64 / i386 | `m365-copilot2api-windows-{amd64,arm64,386}.exe` |
| macOS | x86_64 / arm64 | `m365-copilot2api-darwin-{amd64,arm64}` |

```bash
# Linux / macOS 示例
chmod +x m365-copilot2api-linux-amd64
./m365-copilot2api-linux-amd64
```

```powershell
# Windows 示例
.\m365-copilot2api-windows-amd64.exe
```

### 源码编译

```powershell
git clone https://github.com/yhw5231/M365-Copilot2API.git
cd M365-Copilot2API

# 设置管理员密码（必填；项目不再提供默认密码），请使用强密码
$env:M365_ADMIN_PASSWORD = "your_strong_password"

go build -o m365-copilot2api.exe ./cmd/server
```

```bash
# Linux / macOS
export M365_ADMIN_PASSWORD=your_strong_password
go build -o m365-copilot2api ./cmd/server
```

### 启动

Windows 上用 `manage.py` 启动（默认后台上运行，日志写入 `server.log` / `server-error.log`）：

```powershell
python manage.py start    # 后台运行，默认监听 0.0.0.0:9090
python manage.py status   # 查看运行状态
python manage.py logs     # 查看最近日志（可加参数 N 指定行数）
python manage.py err      # 查看错误日志
python manage.py stop     # 停止服务
```

> `manage.py` 内部硬编码了仓库绝对路径（`D:\M365-Copilot2API\m365-copilot2api.exe` 等），克隆到其他目录时请先修改脚本顶部的路径常量，并确保先完成编译。

直接运行二进制则默认只监听内网 `http://127.0.0.1:9090`，可通过环境变量 `M365_LISTEN` 覆盖。

### Docker 部署

仓库自带 `Dockerfile` 与 `docker-compose.yml`：

```bash
cp .env.example .env      # 可选：在 .env 里设置 M365_ADMIN_PASSWORD 等变量
docker compose up -d --build
```

镜像发布在 GitHub Container Registry（`ghcr.io/yhw5231/m365-copilot2api`，支持 linux/amd64、linux/arm64、linux/386），由每次 `vX.Y.Z` 标签自动构建推送；`docker-compose.yml` 默认拉取 `latest` 标签。

**升级（容器版）**

```bash
git pull                      # 或只更新容器镜像
docker compose pull           # 从 GHCR 拉取新镜像
docker compose up -d          # 重建容器；./data 挂载卷保留全部数据
```

- 数据完全持久化在宿主机 `./data`（账号、密钥、会话、配置、密码）；升级/重建容器不会丢失。
- 需要锁定版本或回滚时，在 `.env` 中固定镜像：`M365_IMAGE=ghcr.io/yhw5231/m365-copilot2api:v0.4.1`，再执行 `docker compose pull && docker compose up -d`。
- 本地改动代码想立即生效仍可 `docker compose up -d --build`（本地镜像优先于远程拉取）。
- 不希望自动升级到 `latest` 时，请始终用 `M365_IMAGE` 固定具体版本后只执行 `docker compose pull`。

**管理员密码（推荐从环境变量读取）**

镜像优先读取环境变量 `M365_ADMIN_PASSWORD`（也支持通过 compose 的 `.env` 注入）。第一次登录若尚未修改过密码，仍会强制要求修改并持久化到 `/data/admin-password`。示例：

```bash
# .env
M365_ADMIN_PASSWORD=your_strong_password
```

如需改用文件注入（传统的 Docker Secret 方式），在 `secrets/m365_admin_password` 中写入明文密码（`0400` 权限），容器会把它作为 bootstrap 密码读取。必须通过环境变量或该文件配置管理员密码，项目不再提供内置默认密码。

**首次部署免权限问题**

`data/` 与 `secrets/` 两个空目录已随源码提交（`git clone` 后即存在），因此第一次部署不再需要等容器新建目录后手动改权限、再重新部署。镜像的入口脚本在启动时以 root 修正 `/data` 的属主与权限后，再降权为普通用户 `m365` 运行，确保首次运行即可正常保存密码与配置。

镜像内以非 root 用户运行，端口映射默认只暴露在 `127.0.0.1`，数据目录挂载在 `./data`。

### Nginx 反向代理（推荐）

直接运行二进制默认只监听 `127.0.0.1`。对外提供服务时建议在网关前面加一层 TLS 终止的反向代理，**并显式配置流式相关指令**——否则 Nginx 的默认 `proxy_read_timeout 60s` 会在长响应生成期间掐断连接（表现为请求约 60 秒后失败、消息看似"被打断"），默认的 `proxy_buffering on` 也会吞掉 SSE 数据导致流式输出不及时。

一个可直接使用的 Nginx `server`/`location` 配置示例（`server_name`、TLS 证书路径按实际情况调整）：

```nginx
server {
    listen 443 ssl http2;
    server_name api.example.com;

    ssl_certificate     /etc/nginx/ssl/api.example.com.crt;
    ssl_certificate_key /etc/nginx/ssl/api.example.com.key;

    location ^~ / {
        proxy_pass http://127.0.0.1:9090;

        # 基础透传
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;

        # WebSocket（ChatHub 需要；SSE 也建议显式置空由 Nginx 管理）
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection '';

        # === 流式与长响应关键配置 ===
        proxy_buffering off;        # 关闭缓冲，SSE chunk 立即转发
        proxy_cache off;            # 禁止缓存流式响应
        proxy_read_timeout 300s;    # 上游读超时从默认 60s 提到 300s（按需调整）
        proxy_send_timeout 300s;    # 与 read 对齐，避免写侧意外触发

        # 可选：透传客户端真实地址供网关统计
        proxy_set_header REMOTE-HOST $remote_addr;
    }
}
```

要点说明：

- **`proxy_read_timeout`（默认 60s）是"60 秒后失败"最常见的原因**。它统计的是**两次连续读取之间的间隔**：流式响应下数据持续流动不会触发；非流式长请求（网关要等上游 ChatHub 全部返回后才一次性写回）在生成期间无字节流过，60 秒到期即被 Nginx 掐断。按需调大（如 300s），能同时覆盖流式与非流式长请求。
- **`proxy_buffering off`** 与网关侧的 `X-Accel-Buffering: no` 配套：网关每发一个 SSE chunk 并 `Flush()`，Nginx 立即转发给客户端，而不是攒满缓冲区；缺失时表现为首字延迟或流式体感中断。
- **`proxy_set_header Connection ''`** 让 Nginx 按 HTTP/1.1 标准自行管理连接头；不要透传客户端的 `$http_connection`（不同客户端取值不一致，可能干扰长连接与 WebSocket 升级）。
- **WebSocket 支持**依赖 `proxy_http_version 1.1` + `Upgrade`/`Connection` 头转发（上面的示例已含）。网关与微软 ChatHub 之间的 WebSocket 连接不经过这里的 `location`（那是网关自己的出站连接），但控制台/客户端直连场景需要这些头。
- 若部署在 Docker 中且 Nginx 也在容器里，把 `proxy_pass` 指向网关容器名与服务端口（如 `http://m365-copilot2api:9090`）即可。
- 网关自身超时兜底是 `M365_CHAT_TIMEOUT_SECONDS`（默认 120s，你的部署为 180s），只影响网关自己等待上游的上下文上限，与 Nginx 读超时是两个独立计时器；流式路径上网关的 HTTP `WriteTimeout` 为 0（无限制），因此瓶颈在 Nginx/客户端一侧，请优先调大 `proxy_read_timeout` 与客户端超时。

### 初始化与第一次调用

浏览器打开控制台（默认 `http://127.0.0.1:9090`）：

1. 使用通过 `M365_ADMIN_PASSWORD` 或密码文件配置的管理员密码登录。首次登录仍会**强制要求修改密码**。
2. 在「账号」页点击**开始授权**：
   - 浏览器会弹出新窗口，跳转到 Microsoft 登录页。
   - 用你的 M365 账号完成登录。
   - 登录完成后弹出窗口会显示空白页或错误页——**这是正常的**，因为回调端点不是真正的网站，授权**尚未完成**。
   - 从弹出窗口的**地址栏**复制完整 URL（包含 `code=...&state=...` 参数）。
   - 回到控制台，将 URL 粘贴到「Callback URL」输入框，点击「Confirm and add」。
   - 如果浏览器拦截了弹窗，请允许本站弹窗后重试。
3. 授权成功后，在「API Key」页**创建第一个 API Key**。
4. 用下面的 API 示例验证调用。

> 有多个 M365 账号时可以重复授权，网关会以轮询 + 故障转移的方式自动调度全部账号。

## 配置说明

全部通过环境变量配置，也可以用 `.env.example` 作为起点。应用启动时会优先读取显式设置的环境变量。

### 核心

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `M365_LISTEN` | `127.0.0.1:9090` | 监听地址（`manage.py` 与 Docker 内置为 `0.0.0.0:9090`） |
| `M365_ADMIN_PASSWORD` | 无，必须配置 | 管理员密码。必须通过该环境变量或 `M365_ADMIN_PASSWORD_FILE` 指向的密码文件配置；首次登录仍会强制修改密码 |
| `M365_TOKEN_ENC_KEY` | 空（不加密） | 账号 Token 加密密钥：64 位十六进制（32 字节），启用后 `accounts.json` 中的 accessToken / refreshToken 以 AES-256-GCM 密文落盘（`enc:v1:` 前缀），旧明文数据会自动迁移；不配置则保持明文以兼容旧部署，**请务必配置** |
| `M365_DATA_DIR` | `~/.config/m365-copilot2api` | 数据目录（token、密钥、用量等集中存储；`manage.py` 内置为 `data/`）。**备份该目录等同于备份全部账号凭据，须按敏感数据对待** |
| `M365_CONFIG` | `~/.config/m365-copilot2api/accounts.json` | 账号配置文件路径 |
| `M365_SESSION_TTL_MINUTES` | `120` | 会话绑定存活时间（分钟），过期从 `sessions.json` 清除 |
| `M365_CONTEXT_TTL_MINUTES` | `120` | 上下文指纹复用窗口（分钟） |
| `M365_CONTEXT_SIMILARITY` | `0.6` | 上下文相似度复用阈值（0~1，Jaccard 相似度） |
| `M365_LOG_LEVEL` | `info` | 日志级别 |
| `M365_ACCOUNT_DEFAULT_CONCURRENCY` | `8` | 每个账号同时进行的上游调用上限；其余账号仍可继续接收请求 |
| `M365_GATEWAY_CONCURRENCY` | `0`（不限） | 网关总并发上限，独立于账号并发。达到上限后，新的请求**立即返回 HTTP 503**（不排队），保护服务器不被挤爆；`0` 表示不限 |
| `M365_PUBLIC_IDENTITY_POLICY` | `false` | 公开身份策略总开关；仅在微软反代渠道显式设为 `true` 时启用身份预设及正文、推理、引用和流式清洗 |

### 自动清理

云端对话被视为「缓存条目」：会话命中时自动刷新存活时间，长期闲置或超出数量上限的对话由后台循环回收。

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `M365_AUTO_CLEANUP` | 开启 | 云端对话自动清理开关（设为 `0` / `false` / `no` / `off` 关闭） |
| `M365_AUTO_CLEANUP_INTERVAL_MINUTES` | `30` | 扫描周期（分钟） |
| `M365_AUTO_CLEANUP_MAX_AGE_HOURS` | `2` | 闲置超过即回收（小时） |
| `M365_AUTO_CLEANUP_KEEP_N` | `100` | 最多保留的云端对话数 |

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `M365_CLEANUP_MODE` | `after_response` | 本地对话索引清理模式（`after_response` / `keep_n` / `max_age`） |
| `M365_CLEANUP_KEEP_N` | `5` | `keep_n` 模式的保留量 |
| `M365_CLEANUP_MAX_AGE_HOURS` | `24` | `max_age` 模式的时限 |

### 工具与推理

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `M365_TOOL_PLANNING_MODE` | `router` | 工具规划模式：`router`（网关路由规划）/ `native`（云端原生规划） |
| `M365_MAX_TOOL_CALLS_PER_TURN` | `1` | 单轮最多并行工具调用数（有副作用操作自动降为串行） |
| `M365_MAX_TOOL_ROUNDS` | `0` | 单次请求最大工具轮次（`0` = 不限，默认不限） |
| `M365_CONTEXT_WINDOW` | `128000` | 上下文窗口 |
| `M365_MAX_OUTPUT_TOKENS` | `16384` | 最大输出 Token |
| `M365_CHAT_TIMEOUT_SECONDS` | `120` | 聊天超时（秒） |
| `M365_IMAGE_TIMEOUT_SECONDS` | `150` | 图片处理超时（秒） |

### 代理池与认证

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `M365_PROXY_POOL` | 空 | 代理列表（逗号或换行分隔，支持 http / https / socks5） |
| `M365_PROXY_MODE` | `strict` | 代理模式三选一：`direct`（直连，不走代理池）、`loose`（优先代理，无健康节点时回退直连）、`strict`（配置了代理池就必须走健康节点，否则报错）。旧开关 `M365_ENFORCE_PROXY=1/0` 仍兼容（映射 strict / loose）。也可在 Web 控制台「Proxy pool」页面切换（设置项 `proxyMode`），保存后即时生效 |
| `M365_PROXY_INSECURE_TLS` | — | 信任自签代理证书（`1` / `true`） |
| `M365_PROXY_HEALTH_URL` | 默认探测地址 | 代理健康检查目标 |
| `M365_BROWSER_CLIENT_ID` / `M365_BROWSER_AUTHORITY` / `M365_BROWSER_REDIRECT_URI` / `M365_BROWSER_SCOPE` | 内置 | 浏览器 PKCE 的 OAuth 配置 |
| `M365_DEVICE_CLIENT_ID` / `M365_DEVICE_AUTHORITY` / `M365_DEVICE_SCOPE` | 内置 | Device Code 的 OAuth 配置 |
| `M365_CLIENT_ID` / `M365_AUTHORITY` / `M365_REDIRECT_URI` / `M365_SCOPE` | 内置 | 兼容旧配置；流程专用变量未设置时作为回退 |
| `M365_CONCISE_OUTPUT` | `1`（开启） | 简洁输出策略：在发给上游的请求里注入指令，压制 M365 在正文里输出"本轮已完成…/尚未…/仍待…/Let me…"之类的过程性叙述，让回复聚焦实际工作与最终结论。设 `0` / `false` / `off` 关闭 |

### 数据文件

| 变量 | 说明 |
|------|------|
| `M365_TOKEN_CACHE` | Token 缓存文件（未设置时落到数据目录） |
| `M365_SESSION_CACHE` | 会话绑定缓存文件（默认 `sessions.json`） |
| `M365_CONVERSATION_CACHE` | 本地对话索引（默认 `conversations.json`） |
| `M365_API_KEYS` | API Key 存储文件 |
| `M365_USAGE_LOG` | 用量统计日志（默认 `{data_dir}/usage.jsonl`） |
| `M365_DEBUG_LOG` | 调试日志文件（请求 / 响应元数据） |

## 使用示例

### 基础聊天（OpenAI 格式）

```bash
curl http://127.0.0.1:9090/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.6-sol",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### 流式输出

```bash
curl http://127.0.0.1:9090/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.6-sol",
    "messages": [{"role": "user", "content": "1+1=?"}],
    "stream": true
  }'
```

### 显式指定会话（会话 ID + 增量发送）

携带同一 `session_id`（或 `x-session-id`）请求头的请求会被绑定到同一条云端对话，命中时网关只把新增历史部分发送给上游。会话 ID 由客户端持有并回传（标准 OpenAI 兼容客户端如 DSH / pi-ai 默认发送 `session_id`）：

```bash
curl http://127.0.0.1:9090/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -H "session_id: my-project-session" \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"继续我们刚才的讨论"}]}'
```

### 多模态图片输入（OpenAI 格式）

客户端用标准的 OpenAI `image_url` 格式传图即可，网关会自动把图片上传到 M365 的 `UploadFile` 端点，并在 ChatHub 消息里注入文件注解（无需客户端感知上游细节）：

```bash
# base64 data URL 方式
curl http://127.0.0.1:9090/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.6-sol",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "这张图里是什么颜色？"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB..."}}
      ]
    }]
  }'
```

也可以直接传 https 图片 URL（仅公网地址，带 SSRF 防护；本地图请用 data URL）。Responses 协议的 `input_image` / `input_file` 同样支持。

### 图像生成（OpenAI 格式）

```bash
curl http://127.0.0.1:9090/v1/images/generations \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"一只戴帽子的柴犬","size":"1024x1024","n":1,"response_format":"url"}'
# → {"created":..., "data":[{"url":"http://127.0.0.1:9090/v1/images/files/<uuid>"}], ...}
```

- `response_format` 支持 `url`（默认）与 `b64_json`。
- 上游返回 Designer 图片时，网关会自动用账号刷新令牌换取 Designer 访问令牌并下载图片，`url` 模式转存本机（`/v1/images/files/{id}`，TTL 15 分钟），`b64_json` 模式直接内联。
- 触发图像生成配额耗尽时返回 `429` + `Retry-After: 86400`。

### 图像编辑（OpenAI 格式）

```bash
curl http://127.0.0.1:9090/v1/images/edits \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "image=@photo.png" \
  -F "prompt=把背景换成海边日落" \
  -F "size=1024x1024"
```

支持 `image` / `image[]` 字段（PNG / JPEG / WebP，≤ 20 MiB），可选 `n`、`model`、`response_format`、`accountId` 等字段。

### Anthropic 格式（Claude Code / Cursor）

```bash
curl http://127.0.0.1:9090/v1/messages \
  -H "x-api-key: YOUR_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-sol","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

上游返回的推理内容（ChainOfThought）会映射为 Anthropic `thinking` block，Claude Code 中可正常显示与使用。

## 对接 Claude Code

在 `~/.claude/settings.json` 的 `env` 中指向网关：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:9090",
    "ANTHROPIC_MODEL": "gpt-5.6-sol",
    "ANTHROPIC_API_KEY": "m365_你的密钥"
  }
}
```

其他任何支持 OpenAI / Anthropic `base_url` 配置的客户端（OpenCode、Cursor、Codex 等）同理，把 `BASE_URL` 指向网关即可。

> 作者不针对任何第三方 Agent 框架的兼容性提供适配与排查。如有需要，自行适配。

控制台「API Keys」页的「使用 API 密钥」弹窗可直接生成 Claude Code 的 `settings.json` 配置与终端环境变量，复制即可。

> ⚠️ **认证冲突提醒**：如果系统环境变量残留了 `ANTHROPIC_API_KEY`，或同时配置了 `ANTHROPIC_AUTH_TOKEN`，Claude Code 会告警「认证可能不工作」。请二选一：让 `settings.json` 的 `env` 覆盖系统级变量，或删除系统级 `ANTHROPIC_*`。

## 可用模型

网关默认内置模型映射（可在控制台「设置」页的「模型路由」界面启用/禁用模型与上游映射、增加/删除模型和映射、设置每个模型的默认推理等级与上游映射目标）：

| 模型 | 默认推理级别 | 说明 |
|------|-------------|------|
| `gpt-5.6-sol` | `low` | 默认模型 |
| `gpt-5.6-terra` | `medium` | 推理折中 |
| `gpt-5.6-luna` | `medium` | 推理折中 |

- 模型路由把公开模型名映射到「上游映射」目标（映射携带发送给 ChatHub 的 tone 音色）；控制台「设置 → 模型路由」可添加/删除/禁用模型与上游映射、调整显示名称、上游映射与默认推理等级。
- 被禁用的模型会从 `/v1/models` 目录隐藏，请求时返回 `model_not_found`；删除模型等于移除其路由（内置模型删除后可重新添加回来）。
- 请求未携带 `reasoning_effort` 时，网关采用该模型配置的默认推理等级；请求可随时用 `reasoning_effort` 参数覆盖。
- M365 订阅会上线的新模型名（如 `gpt-5.2`、`gpt-5.4`、`codex` 系）以实际目录为准，可在控制台路由界面添加配置。

## 会话续用原理

会话续用**只认显式会话 ID**，绝不做任何内容相似度匹配（多用户/新对话靠聊天记录猜会话极易串话）。会话 ID 由客户端持有并随请求回传，核心实现在 `internal/web/session_resolver.go`。

`.Resolve()` 只按一个优先级决定续用哪个会话：

1. **显式会话（`session_id` / `x-session-id` 请求头，或 `body.session_id`）**：请求头显式指定的会话 ID 优先级最高，不参与任何身份判定，由调用方主动决定要连接到哪条云端对话。同一 ID 的后续请求绑定到同一条云端对话。
2. **兜底新建**：没有任何显式会话 ID 的请求一律新建云端对话——即使它与之前的请求内容完全相同，也绝不复用历史会话（新开对话、不同用户天然隔离）。

几个特性由此而来：

- **无 ID = 无状态**：不携带会话 ID 的请求每次都是全新对话，`cached_tokens` 为 `0`，多用户之间不可能串话。
- **标准客户端即插即用**：OpenAI 兼容客户端（DSH / pi-ai、Cherry Studio、NextChat 等）默认携带 `session_id` 头；`x-session-id` 作为兼容别名同样识别（同一 ID 值不论用哪个头名都映射到同一条会话）。
- **只发增量**：显式会话命中且消息历史延续时，网关只补发新消息（`HistoryLen` 表示「云端对话已包含的消息条数」），等价于把云端对话当作上下文缓存用。
- **`/v1/responses` 用原生 `previous_response_id`**：Responses 协议的会话续用走标准字段，无需自定义头。
- **线程与清理联动**：会话绑定持久化在 `sessions.json`（0600），过期时间由 `M365_SESSION_TTL_MINUTES` 控制；长期无命中的会话会随自动清理按同一窗口（默认 2 小时）被回收。

### 缓存信息如何返回给下游

网关在 `/v1/chat/completions` 响应（非流式）与流式结尾的 `usage` chunk（`stream_options.include_usage=true` 时）中，以 **OpenAI 标准字段 `usage.prompt_tokens_details.cached_tokens`** 回传本次请求被上游会话复用的输入 token 数：

```json
{
  "usage": {
    "prompt_tokens": 1200,
    "completion_tokens": 80,
    "total_tokens": 1280,
    "prompt_tokens_details": {
      "cached_tokens": 1000,
      "text_tokens": 200
    }
  }
}
```

- `cached_tokens` = 完整逻辑 prompt 中被复用的历史部分（会话续用命中时未重新发送给 ChatHub 的 token 估算，等价于把云端对话当作上下文缓存）。
- `text_tokens` = 本次新提交的增量输入；`prompt_tokens` = `cached_tokens + text_tokens`。
- 续用未命中（新建会话、全量重发）时 `cached_tokens` 为 `0`，字段仍然返回，便于下游统一解析。

下游转发层（如 sub2api / one-api / new-api）和客户端（NextChat、Cherry Studio、LobeChat 等）读取该标准字段即可展示「缓存命中」与节省量，无需额外协议。工具调用响应（`finish_reason: tool_calls`）的 `usage` 同样携带该字段。

## 内容自动清理

云端对话被视作「缓存条目」：**会话命中 = 刷新存活时间；空闲 = 过期**。后台循环默认每 30 分钟回收：

- 空闲超过 `M365_AUTO_CLEANUP_MAX_AGE_HOURS`（默认 2 小时）的云端对话；
- 或超出数量上限 `M365_AUTO_CLEANUP_KEEP_N`（默认 100）的最老对话。

**以下对话永不回收**：白名单对话、有活跃会话绑定正在引用的对话、最近使用过的用户会话。删除云端对话时会联动清理本地索引与会话绑定，杜绝幽灵会话，防止后续请求复用已删除的对话导致串号或报错。详见 `internal/web/auto_cleanup.go`。

## API 端点参考

### 对外兼容端点（`/v1/*`）

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/models` | GET | 模型目录 |
| `/v1/chat/completions` | POST | 聊天补全（流式 / 工具调用） |
| `/v1/responses` | POST | OpenAI Responses 协议 |
| `/v1/messages` | POST | Anthropic Messages（需 `x-api-key` + `anthropic-version`） |
| `/v1/images/generations` | POST | 图像生成（`url` / `b64_json`；Designer 图片自动下载并转存本机访问） |
| `/v1/images/edits` | POST | 图像编辑（multipart：`image` + `prompt` + 可选 `size` / `n` / `response_format`） |
| `/v1/images/files/{id}` | GET | 读取本机转存的生成图片（无需 API Key，TTL 到期自动清理） |
| `/v1/sessions` | GET / POST | 查询会话绑定 / 按 `session_id` 查询或创建 |
| `/v1/sessions/{id}` | DELETE | 解除会话绑定 |

### 管理 API（`/api/*`，需管理员登录态）

| 端点 | 说明 |
|------|------|
| `/api/admin/login` · `/logout` · `/session` | 管理端登录态 |
| `/api/admin/change-password` | 修改管理员密码（首次登录强制） |
| `/api/admin/keys` | API Key 管理（创建 / 撤销 / 回读） |
| `/api/admin/models` · `/models/test` | 模型目录 / 单模型连通测试（不依赖明文 Key） |
| `/api/admin/settings` | 运行时设置查看与修改 |
| `/api/admin/proxy-pool` | 代理池管理 |
| `/api/accounts` · `/refresh` · `/delete` | 账号管理 |
| `/api/auth/start` · `status` · `callback` | PKCE 授权流程（`callback` 立即返回 `{"status":"exchanging"}`，令牌兑换在后台进行，调用方按 `state` 轮询 `/api/auth/status` 直到 `authenticated` 或 `error`；避免慢速/海外网络下被浏览器或反向代理的读超时打断） |
| `/api/conversations` · `/api/m365/conversations` · `/api/m365/conversations/detail` | 本地 / 云端对话列表、删除、清理、白名单；`detail` 返回单条会话完整内容（云端与本地合并视图） |
| `/api/stats` · `/stats/reset` | 缓存命中统计 |
| `/api/usage` · `/usage/logs` | 用量统计仪表盘与明细 |
| `/api/chat` · `/chat/stream` | 控制台内即时对话 |
| `/api/health` · `/api/version` | 健康检查 / 版本 |

## 测试

仓库自带完整单元测试（会话解析、自动清理、工具路由、协议兼容、用量统计、密钥安全迁移、图像编辑/配额、公共身份、工具校验、会话详情等），运行：

```bash
go test ./...
```

例如会验证：默认自动清理闲置窗口为 2 小时（`internal/web/auto_cleanup_test.go`）、内容键前缀命中只发送增量（`session_resolver_test.go`）、Responses / Anthropic 协议事件序列等。

## 目录结构

```
M365-Copilot2API/
├── cmd/server/            # 入口，HTTP 服务启动
├── internal/
│   ├── web/               # HTTP 路由、会话解析器、自动清理、管理 API、用量统计
│   │   ├── web/           # 管理控制台（纯 HTML / JS 单页，go:embed 编译进二进制）
│   │   ├── session_resolver.go   # 内容键会话复用（四重指纹）
│   │   ├── auto_cleanup.go        # 云端对话自动清理
│   │   ├── usage.go               # usage.jsonl 用量统计
│   │   └── ...                    # 工具调用、协议转换、代理池、密钥管理等
│   ├── chathub/           # M365 Copilot ChatHub WebSocket 客户端
│   ├── auth/              # OAuth / PKCE
│   ├── mcp/               # MCP 工具网关（SSE / JSON-RPC）
│   └── outbound/          # HTTP 代理池
├── scripts/               # 运维脚本
│   ├── e2e_test.py        # 端到端测试
│   ├── chathub_probe.py   # ChatHub 协议探针
│   ├── genprobe.py        # 图像生成协议探针（原始帧 dump）
│   ├── multimodal_probe.py # 多模态图片输入探针（上传 + 注解流程）
│   ├── test-recorder.ps1  # Windows 测试录制
│   └── m365-upload-forensic-trace.user.js  # 上传取证脚本
├── docs/screenshots/      # 界面截图
├── manage.py              # start / stop / status / logs / err 进程管理
├── docker-compose.yml · Dockerfile
└── data/                  # 运行数据（由 M365_DATA_DIR 指定）
```

## 安全说明

- **默认仅监听内网**：直接运行二进制默认 `M365_LISTEN=127.0.0.1:9090`；对外提供服务务必通过 TLS 终止的反向代理（Nginx / Caddy，配置示例见上方「Nginx 反向代理」节），并为 SSE 与 WebSocket 开启长连接与 `proxy_buffering off`。
- **强制改密**：必须通过 `M365_ADMIN_PASSWORD` 或 `M365_ADMIN_PASSWORD_FILE` 配置初始管理员密码；首次登录后仍必须立即修改。项目不再提供内置默认密码。
- **Token 加密落盘**：设置 `M365_TOKEN_ENC_KEY`（64 位十六进制）后，`accounts.json` 中的 accessToken / refreshToken 以 AES-256-GCM 密文存储；加载时若密钥缺失或不匹配会直接拒绝启动，绝不静默回退明文。
- **密钥可回读**：API Key 明文与 SHA-256 哈希一并持久化到 `api-keys.json`（`0600`），控制台可随时重新显示并复制完整密钥；旧版明文只在缺少哈希时补写哈希、不再清零。请妥善保护控制台与数据目录权限。
- **数据落盘权限**：账号凭据、Token 缓存、会话绑定、API Key 等数据文件以 `0600` 权限写入，数据目录建议 `0700`。**备份数据目录等同于备份全部账号凭据与 M365 会话**，请按敏感数据处理，并考虑用 `M365_TOKEN_ENC_KEY` 加密后再归档。
- **WebSocket 凭据**：ChatHub WebSocket 的 access token 通过 `Authorization: Bearer` 头传递，绝不进入 URL query，避免进入代理日志、trace 与错误输出。
- **会话与请求边界**：`/v1/sessions` 仅返回脱敏摘要（不包含 contextHistory）；会话按创建方 API key 隔离；图片回拉经过 SSRF 校验（仅 https、拒绝 loopback/私网/metadata/CGNAT、限制重定向）；开放 API 请求体有统一大小上限。

## 常见问题

**Q1：为什么云端对话越来越多？**

后台每 30 分钟自动清理一次：回收闲置超过 2 小时（`M365_AUTO_CLEANUP_MAX_AGE_HOURS`，默认 2）或超出数量上限（`M365_AUTO_CLEANUP_KEEP_N`，默认 100）的云端对话；被活跃会话引用、白名单中的对话永不回收。调低这两个值可以清理得更激进；彻底关闭用 `M365_AUTO_CLEANUP=0`（不推荐，云端对话会无限膨胀，可能触发风控）。

**Q2：如何切换 M365 账号？**

不需要切换。多账号场景下网关自动轮询所有可用账号，单账号故障自动转移到下一个。要增加账号，直接在控制台发起新的 PKCE 授权即可。

**Q3：Claude Code 提示「认证可能不工作」怎么办？**

通常是系统环境变量残留了 `ANTHROPIC_API_KEY`，或同时配置了 `ANTHROPIC_AUTH_TOKEN` 导致两种认证方式冲突。只保留 `~/.claude/settings.json` 中的 `ANTHROPIC_API_KEY`（settings 会覆盖系统级变量），并删除系统级残留或 `AUTH_TOKEN`。

**Q4：session_id / x-session-id 是什么？**

会话续用的唯一依据是客户端携带的显式会话 ID（请求头 `session_id` 或 `x-session-id`，或 `body.session_id`，或 `/v1/responses` 的 `previous_response_id`）。不携带会话 ID 的请求一律新建云端对话——即使内容与之前完全相同也绝不复用，从根本上避免多用户/新对话串话。标准 OpenAI 兼容客户端（DSH / pi-ai 等）默认发送 `session_id` 头，无需额外配置。

**Q5：对话出现串号 / 上下文错乱？**

会话绑定在到期后会自动清除。若本地缓存与云端不同步，可在控制台「对话」页手动删除该云端对话，网关会连同本地绑定一起清理重建。

**Q6：Docker 部署后管理员密码登录不上，输什么都不对，或进入后添加账号提示「请先修改密码」？**

未通过环境变量或密码文件正确配置管理员密码，或首启时 `/data/admin-password` 为空、权限异常，都会导致服务无法正常完成管理员初始化。推荐直接在 compose 环境变量里注入密码：

```bash
# 在仓库根目录的 .env 里设置后执行
M365_ADMIN_PASSWORD=你的强密码
docker compose up -d --build
```

容器会优先使用 `M365_ADMIN_PASSWORD`，也可使用 `M365_ADMIN_PASSWORD_FILE` 指向的密码文件。必须通过其中一种方式配置管理员密码，否则服务无法正常完成管理员初始化。

**Q7：git 拉取后改配置的密码部署不生效，或登录后改配置保存报错？**

根因通常是 `data`/`secrets` 目录在首启后才被创建，且属主是 root，容器内非 root 用户写不进去。本项目已在源码中随附空的 `data/` 与 `secrets/` 目录（`clone` 后即存在），镜像入口脚本也会在启动时以 root 修正 `/data` 权限再降权运行，因此一次部署即可生效；旧的「先部署一次、手动改权限、再部署一次」的流程已不再需要。若仍手动管理，请确保宿主机的 `./data` 目录对容器内用户可写。

## 贡献指南

PRs Welcome！提交前请留意：

1. Fork 仓库并创建独立分支，一个 PR 聚焦一个问题。
2. 切勿提交任何凭据、cookie、账号缓存、日志或构建产物。
3. 改动 Go 文件前先 `gofmt -w`，提交前跑完 `go test ./...`、`go vet ./...` 与 `go build ./...`。
4. 描述行为变化，涉及新逻辑时附上对应测试。

详见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 更新日志

完整的版本变更与修复记录见 [CHANGELOG.md](CHANGELOG.md)；对标上游
M365-Gateway-Cloudflare 的修复项对比与本地实现说明见
[docs/m365-gateway-fixes-comparison.md](docs/m365-gateway-fixes-comparison.md)。

## 许可证

[MIT License](LICENSE)。
