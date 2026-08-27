# M365 Gateway 修复项对比与本地实现说明

本文档记录本地项目 M365-Copilot2API（Go 中继）对照上游参考实现
[`vipamess/M365-Gateway-Cloudflare`](https://github.com/vipamess/M365-Gateway-Cloudflare)
（Cloudflare Workers + Durable Objects + KV 的 TypeScript 中继）完成的 9 项
修复，包含每一项的参考来源、本地实现位置与验证方式。

## 背景

上游项目在 4 个 commit 中修复了中继场景下的关键可靠性问题：

| commit | 内容 |
| --- | --- |
| `16adad4` | fail closed：本地工具不可用时拒绝托管执行替换 |
| `08da778` | Codex 客户端工具路由稳定（compatibilityOnly 过滤） |
| `27b0a94` | SSE 预检期保活桥接（keep-alive 注释帧） |
| `5426afd` | 长任务完成证据保留（completion evidence） |

本地 Go 项目此前缺少这些保护，逐项适配如下。

---

## 1. `X-M365-Execution-Environment` 响应头

**参考**：上游 `src/openai.ts` `streamHeaders()` 设置
`X-M365-Execution-Environment: cloudflare-worker-relay`。

**本地实现**：`internal/web/sse_common.go`

- 常量 `executionEnvironmentHeader` / `executionEnvironmentValue`
  （`m365-copilot2api-relay`）。
- `setSSEHeaders(w)` 统一设置 SSE 标准头 + 该环境头，替换了
  `internal/web/server.go`（两处流式路径）、`stream.go`、
  `protocol_response.go`、`tool_response.go` 中手写的头。

**作用**：客户端可识别部署边界，区分"本机执行通道"与"托管/云端回退结果"。

## 2. exec_command / write_stdin / view_image 从 native 插件过滤

**参考**：上游 `src/chathub.ts` `clientPlugins` 中
`compatibilityOnly = new Set(["exec_command", "write_stdin", "view_image"])`。

**本地实现**：`internal/chathub/tools.go`

- `clientPlugins` 过滤 `compatibilityOnlyTools`（上述三个工具）。
- 新增导出函数 `IsCompatibilityOnlyTool(name string) bool`。

**作用**：这些工具只适用于 Codex 本地执行模型；对 native 插件注入会
导致工具声明与执行通道不一致。

## 3. SSE 心跳保活

**参考**：上游 `src/openai.ts` `bridgePendingResponsesStream` 的预检期保活。

**本地实现**：`internal/web/sse_common.go`

- `sseKeepalive` 结构体：互斥锁保护的 `lockedWrite` / `lockedWriteCtx`，
  默认 5 秒间隔写入 `: keep-alive` 注释帧，`stop()` 幂等。
- `writeDeadline` 提供 30 秒写超时 + flush。
- 已接入 `server.go` 的 `chatWithAccountEvents` 与
  `chatWithAccountReasoning` 两条流式路径；所有数据帧改走
  `keepalive.lockedWrite(Ctx)`，与心跳 goroutine 共享互斥锁，
  消除 `http.ResponseWriter` 并发写导致的帧交错。

**作用**：上游推理期或背压造成的静默期不再触发客户端/代理空闲超时。

## 4. canonical JSON 指纹标准化

**参考**：上游 `canonicalJSONText` 用于重复调用签名。

**本地实现**：`internal/web/agent_ledger.go`

- `buildAgentLedger` 中重复签名与失败指纹改用
  `canonicalToolArguments(e.Arguments)`（key 排序、忽略空白）。

**作用**：相同参数的不同 JSON 表示（key 顺序、空白差异）不再被误判为
不同调用，重复调用检测与轮次上限更准确。

## 5. 无状态 Responses 续接

**参考**：上游 `statelessToolContinuation` / `lease.pendingCallId`。

**本地实现**：`internal/web/pending_tools.go`

- `recordPendingToolCalls` / `consumePendingToolCall` 记录与消费跨轮次
  未完成的工具调用。
- `restoreStatelessToolCalls` 在 `/v1/responses` 缺少
  `previous_response_id` 时，从请求消息中恢复缺失的 function_call。
- 接入 `internal/web/protocol_handlers.go` 的 `responses()` 处理函数与
  `streamResponsesAdapter`。

**作用**：无状态客户端（如 Codex）的多轮工具对话可正确续接，不再因
缺少 previous_response_id 而丢失工具调用。

## 6. 工具轮次上限优雅终止

**参考**：上游 `toolRecoveryTermination`。

**本地实现**：`internal/web/server.go`（约 1965 行）

- `activeLedger.CanContinue(maxToolRounds())` 达到上限时，注入 system
  消息 "Tool round limit reached. You must now summarize." 引导优雅总结，
  其余错误保留 409。

**作用**：避免工具循环卡死时客户端收到硬错误，改为引导模型收尾。

## 7. 跨轮次 pending call_id 追踪

**参考**：上游 `lease.pendingCallId` 生命周期管理。

**本地实现**：`internal/web/pending_tools.go` + `protocol_handlers.go`

- 流式与非流式路径在工具调用循环中调用 `recordPendingToolCalls`，
  未消费的调用跨轮次保留，供续接复用。

**作用**：与第 5 项配合，保证长任务中的工具调用证据跨轮次不丢失。

## 8. 托管执行替换检测（最小化文本匹配）

**参考**：上游 `isHostedExecutionSubstitution`（协议级门控 +
`ledger.completed.length > 0` 短路 + 窄正则）。

**本地实现**：`internal/web/toolloop.go`

- `needsWorkspaceToolMisjudgmentCorrection(text, toolMaps, userPrompt, ledger)`
  协议级门控：
  1. 调用方必须声明了执行工具（shell/exec）或文件工具；
  2. 本轮无已完成的工具调用（`len(ledger.Completed) == 0`）；
  3. 用户提问未先引入工作区词汇（`userPromptMentionsWorkspace`，
     覆盖 linux / container / 容器 / /mnt/data / sandbox / 沙箱 /
     无法访问工作区 等）。
- 全部通过后才运行原有分层文本检测器
  `isWorkspaceToolMisjudgmentForTools`。

**与旧实现的差异**：旧实现直接用 100+ 短语/正则/语义共现检测，用户对话
内容本身含"Linux 容器 / /mnt/data / 无法访问工作区"等词时可能被误判。
新实现先以协议上下文（声明工具 + 无完成调用 + 用户回声）过滤，文本匹配
仅作为最后一道闸，显著降低误判率。

**验证**：`internal/web/misjudgment_gate_test.go` 覆盖用户回声场景、
无工具声明场景、已完成调用场景、空文本场景。

## 9. 工具操作分类（action hints / completion evidence）

**参考**：上游 `src/completion-evidence.ts` `classifyCompletionActions`
与 `evaluateCompletionEvidence`。

**本地实现**：`internal/web/completion_evidence.go`

- `classifyToolOperation(name, arguments)`：按工具名+参数分类为
  deploy / fix / install / verify / upload / delete / create / configure /
  start（只读工具恒为 verify，分类不读工具输出）。
- `completionClaims(answer)`：从最终回答提取完成声明，过滤否定/计划/
  假设上下文。
- `completionEvidenceAllowsUpgraded(answer, ledger)`：校验每个声明都有
  匹配的、未失败的工具证据；接入 `server.go`（约 3067 行）完成声明守卫，
  无证据时降级为诚实的"无法确认完成"。

**作用**：防止模型在无实际工具证据时宣称"部署完成/已安装/已验证"。

---

## 验证记录

- 单元测试：`go test ./internal/web/ ./internal/chathub/ -count=1` 全部通过
  （含新增 `completion_evidence_test.go`、`misjudgment_gate_test.go`）。
- 静态检查：`go vet ./internal/web/ ./internal/chathub/` 无警告。
- 构建：`go build -o m365-copilot2api.exe ./cmd/server` 成功；
  `docker build -t m365-copilot2api:local-test .` 成功。
- 容器验证（真实 M365 账号）：
  - 非流式真实对话返回正常回答；
  - 流式响应头含 `X-M365-Execution-Environment: m365-copilot2api-relay`、
    `: connected` 初始帧，长生成（618 帧 / 132KB）期间出现 10 次
    `: keep-alive` 帧；
  - 用户提问含"Linux 容器 / /mnt/data / 无法访问工作区"时，模型解释性
    回复未被误判修正（日志无 `[workspace-tool-eject]`）；
  - `tool_choice=auto` 下 `exec_command` 工具调用与结果往返正常，
    无错误修正触发。
