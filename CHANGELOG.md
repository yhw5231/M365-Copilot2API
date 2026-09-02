# Changelog

本项目所有用户可见的功能变更、修复与可靠性改进按时间倒序列出。格式基于
[Keep a Changelog](https://keepachangelog.com/zh-CN/1.0.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)。

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
