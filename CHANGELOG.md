# Changelog

本项目所有用户可见的功能变更、修复与可靠性改进按时间倒序列出。格式基于
[Keep a Changelog](https://keepachangelog.com/zh-CN/1.0.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- **内容审查与替换**：后台设置新增"内容审查"页，可配置多条
  `关键词 → 替换文本` 规则（不区分大小写、按顺序匹配）。上游返回内容命中
  关键词时，整段回复替换为该规则的自定义文本（留空则删除内容），后续内容
  不再发送，会话立即终止（会话绑定被移除，下一轮自动开启全新上游会话）。
  流式响应通过"尾部关键词长度保留缓冲 + 开头缓冲（默认 256 字节，可配）"
  保证跨分片关键词必然被检出，且开头缓冲内命中时客户端只会看到替换文本。
  工具调用响应不参与审查，避免破坏代理循环。
- **响应回显实际推理等级**：`/v1/chat/completions` 的非流式响应体与流式
  分块（含结束块）新增 `reasoning_effort` 字段；`/v1/responses` 的响应对象
  （含 `response.created` / `response.completed`）在路由配置了推理等级时
  新增 `reasoning: {"effort": ...}`。下游客户端由此可看到实际生效的推理
  等级（模型路由配置覆盖客户端请求后的值）。

## [v0.5.1] - 2026-09-03

### 变更

- **账号存储改为分文件保存**：已授权账号从单一 `accounts.json` 迁移到
  `accounts/` 目录（默认 `{数据目录}/accounts`，可用 `M365_ACCOUNTS_DIR`
  覆盖）。每个账号保存为以账号名（邮箱）命名的 `<账号名>.json`，包含凭据与
  该账号的调度开关、绑定代理。公用账号调度设置（账号并发、网关总并发、轮询
  规则、会话保留时长、排队超时、限流冷却、切换缓存）从 `settings.json` 拆分
  出来，单独保存在 `account-settings.json`（可用 `M365_ACCOUNT_SETTINGS_FILE`
  覆盖），主设置文件不再承载这些键。首次启动自动导入旧版单文件
  `accounts.json` 并封存为 `accounts.json.migrated`（不删除）；旧环境变量
  `M365_CONFIG` / `M365_TOKEN_CACHE` / `M365_TOKEN_FILE` 仅作为迁移来源识别，
  不再决定存储位置。启用了 `M365_TOKEN_ENC_KEY` 时账号文件同样密文落盘。
- **控制台翻译循环修复**：`translatePage` 自身的 DOM 写入不再触发
  MutationObserver 无限重入（此前会造成控制台白屏卡死）；时区下拉仅在标签
  变化时写回。
- **模型路由推理等级覆盖下游请求**：模型路由在后台配置的推理等级现在优先于
  客户端请求的 `reasoning.effort`（未配置等级的路由沿用客户端请求值），
  保证每条路由始终以调好的等级运行。

### 修复

- **启动阻塞导致 Web 页面打不开**：启动时的全量令牌刷新改为后台执行。此前
  `RefreshExpiredTokens` 在 HTTP 监听之前同步串行刷新所有过期账号（每账号
  一次网络往返），账号较多时耗时数分钟，期间 Web 控制台与 API 全部不可达，
  日志停在 `auto-cleanup enabled` 后没有 `listening on` 行。
- **调试记录（调试页）字段失真**：`/v1/responses` 完成回调不再用原始客户端
  值覆盖推理等级（常为空导致"完成时变空"），也不再覆盖上游真实首字耗时
  （适配器侧测量受推理门控影响接近总耗时，导致"完成时≈总耗时"）。
- **下游缓存值恒为 0**：内部 chat 请求强制携带
  `stream_options.include_usage=true`，恢复 `/v1/responses` 流式链路的内层
  缓存信号（此前内层 usage 分块被门控抑制）；usage 分块恢复默认发送（客户端
  显式 `include_usage=false` 才省略），兼容不声明 `stream_options` 的中转层。
- **`store:false` 时响应 id 为 `<nil>`**：`/v1/responses` 在关闭历史保留时
  回退新生成 `resp_` id，不再返回字面量 `"<nil>"`。
- **Anthropic 端点错误形状**：`/v1/messages` 的 401/503/500/404 改按官方
  `{type:"error",error:{type,message}}` 返回并映射 `error.type`；流式上游失败
  转为流内 `error` 事件，等待期间发 `ping` 保活，防止中间代理空闲断连。
- **`/v1/responses` 流式兼容性**：增加 keep-alive；修复 message/tool 输出项
  `output_index` 冲突与 message 项未闭合；流式期间长推理静默不再断连。
- **chat 流式协议保真**：usage 分块调整为官方顺序（finish 分块后跟空
  `choices` 的 usage 分块）；空完成不再以成功流收尾；流内错误帧补 `type`
  字段；`response.failed` 的 `code` 改字符串。
- **请求校验与状态码**：`/v1/responses`、`/v1/messages` 请求体加 10MiB 上限
  并正确返回 413；405 带 `Allow: POST`；未知 `/v1/*` 路径返回 JSON 404。
- **用量口径**：工具调用非流式 usage 的 prompt/completion 口径修正；
  `/v1/responses` usage 补 `output_tokens_details.reasoning_tokens`（含 chat
  别名），非流式输出估算纳入推理内容；`input` 数组中的字符串项不再被静默
  丢弃；`stop`/`n`/`seed`/`logprobs` 被忽略时在 `m365_ignored_parameters`
  中如实披露。
- **容器构建版本元数据**：`docker compose build` 未传 build-args 时自动从
  git 历史推导版本号/提交/构建时间，`/api/version` 不再显示 `dev/unknown`，
  部署后可直接核对容器内的实际代码版本。

## [Unreleased]

### 新增

- **网关总并发限制（Gateway concurrency）**：新增独立于账号并发的网关级并发
  上限，控制整个网关同时处理的请求数，防止服务器被挤爆。默认 `0`（不限）；
  可通过 `M365_GATEWAY_CONCURRENCY` 环境变量或 Web 控制台「账号调度」页的
  「网关总并发」设置。达到上限后，新的请求**立即返回 HTTP 503**（不排队），
  管理、认证、健康检查与静态资源端点不受限，保证过载时仍可登录控制台调整。
  实现于 `internal/web/gateway_concurrency.go`，由
  `gatewayConcurrencyMiddleware` 统一准入。

- **`X-M365-Execution-Environment` 响应头**：所有 SSE / 流式响应统一携带
  `X-M365-Execution-Environment: m365-copilot2api-relay`，客户端可据此识别
  部署边界，拒绝"托管容器/云端环境"冒充本机的结果。由
  `internal/web/sse_common.go` 的 `setSSEHeaders()` 统一设置，覆盖
  `/v1/chat/completions` 流式路径、`/v1/responses` 流式路径、chathub 流式端点、
  Anthropic 流式路径与工具响应路径。

- **SSE 心跳保活（keep-alive）**：长时静默期（上游推理、背压）默认每 5 秒写入
  `: keep-alive` 注释帧，防止客户端与中间代理的空闲超时断开连接。所有并发
  写入共享同一互斥锁（`sseKeepalive.lockedWrite` / `lockedWriteCtx`），消除
  Go `http.ResponseWriter` 并发写导致的帧交错风险，并带 30 秒写超时保护。

- **工具操作分类（completion evidence）**：新增 `internal/web/completion_evidence.go`，
  将已完成的工具调用按工具名 + 参数分类为 deploy / fix / install / verify /
  upload / delete / create / configure / start 等操作（只读工具恒为 verify，
  分类绝不读取工具输出，防止只读结果文本伪造操作证据）。最终回答中的完成声明
  （"部署完成""已安装"等）会与分类证据校验：无证据或证据不符时降级为诚实的
  "无法确认完成"措辞（`completionEvidenceAllowsUpgraded`）。

- **跨轮次 pending tool_call 追踪**：`internal/web/pending_tools.go` 记录
  未完成的工具调用，跨轮次以 call_id 关联，供无状态 Responses 续接复用。

### 修复

- **exec_command / write_stdin / view_image 从 native 插件过滤**：`internal/chathub/tools.go`
  现在会过滤掉 `compatibilityOnlyTools`（`exec_command`、`write_stdin`、
  `view_image`），避免这些只适用于 Codex 本地执行的工具被注入 native 插件
  上下文；新增导出函数 `IsCompatibilityOnlyTool` 供协议层判断。

- **canonical JSON 指纹标准化**：`internal/web/agent_ledger.go` 中的重复调用
  签名与失败指纹改用 `canonicalToolArguments`（key 排序、忽略空白）生成，
  相同参数的不同 JSON 表示不再被误判为不同调用。

- **本地过载返回 503 而非 429**：项目网关在排队超时或没有可用账户时，HTTP 状态码
  从 `429 Too Many Requests` 改为 `503 Service Unavailable`，错误信息保持不变。
  上游限流仍返回 429。涉及 `internal/web/errors.go`（`upstreamStatus`、
  `writeUpstreamError`）、`internal/web/server.go`（LocalCapacity 错误）及
  `internal/web/account_health.go`（`IsRateLimited` 排除 LocalCapacity）。

- **无状态 Responses 续接**：`/v1/responses` 在缺少 `previous_response_id` 时
  通过 `restoreStatelessToolCalls` 从请求消息中恢复缺失的 function_call，
  使无状态客户端（如 Codex）的多轮工具对话可正确续接。

- **工具轮次上限优雅终止**：达到工具轮次上限（默认 32）时不再返回 409 报错，
  而是注入 system 消息 "Tool round limit reached. You must now summarize."
  引导模型优雅总结，避免客户端看到硬错误。

- **托管执行替换检测（最小化文本匹配）**：`internal/web/toolloop.go` 新增
  `needsWorkspaceToolMisjudgmentCorrection`，用**协议级门控**取代大面积文本
  匹配，只有同时满足以下条件才触发修正：
  1. 调用方实际声明了执行工具（shell / exec）或文件工具；
  2. 本轮尚无任何已完成的工具调用（模型已证明工具可用时不再修正）；
  3. 用户自己的提问**未先引入**工作区/容器相关词汇——例如用户询问
     "Linux 容器 / /mnt/data / 无法访问工作区" 时，模型解释性回复不再被误判。
  只有通过全部门控后才运行窄文本检测，显著降低对话内容本身含匹配文本导致的
  误判。

### 说明

本次改动对标上游分析
[`vipamess/M365-Gateway-Cloudflare`](https://github.com/vipamess/M365-Gateway-Cloudflare)
的 4 个修复 commit（fail-closed 本地工具、Codex 工具路由稳定、SSE 预检保活桥接、
长任务完成证据保留）与本地项目差异，将其适配为 Go 中继实现。详细对比见
`docs/m365-gateway-fixes-comparison.md`。

---

## [v0.5.0] - (前次发布)

<!-- 之前的版本变更记录在此处继续追加 -->
