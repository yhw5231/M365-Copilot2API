# M365-Copilot2API 鲁棒性/健壮性审计报告

- 审计时间：2026-08-07
- 审计对象：D:\M365-Copilot2API（M365 Copilot 开源 API 网关，Go）
- 审计范围：`internal/chathub`、`internal/web`、`internal/auth`、`internal/outbound`、`internal/mcp`、`cmd/server`
- 审计方式：只读。全量代码逐行审读 + `go build` / `go vet` / `go test` 实测 + 静态并发分析
- 工具链：D:\Go\bin\go.exe（go1.23.0 windows/amd64）

## 一、实测验证结论（硬事实）

| 检查项 | 结果 |
|---|---|
| `go build ./...` | 全部通过 |
| `go vet ./...` | 仅报 2 处锁拷贝告警（见 L6）；mcp 包测试文件编译失败导致该包 vet 跳过 |
| `go test ./internal/chathub` | 全部 ok |
| `go test ./internal/auth` | 全部 ok |
| `go test ./internal/outbound` | 全部 ok |
| `go test ./internal/web` | **4 个用例失败**（详见 H1、L2） |
| `go test ./internal/mcp` | **编译失败**：`client_test.go:26:12: undefined: StartStdio` |
| `go test -race` | **无法执行**：本机无 gcc/cgo，报 `C compiler "gcc" not found`。本文所有数据竞争均为纯静态分析判定，未经 race 检测实证，建议在带 gcc 的 CI 环境补跑 |

web 包失败用例（已复现，稳定失败）：
- `TestCompletionGuardRejectsPendingAndUnsupportedSuccess`、`TestCompletionGuardRejectsUnsupportedSuccess` —— 确认为代码内一直存在的逻辑 bug（见 H1）
- `TestNamedToolChoiceModeIsStable`、`TestModelToolRouterPromptMarksCompletedResults` —— 工具路由器提示语与测试期望不一致，疑似重构回归（见 L2）

---

## 二、严重 / 高风险

### H1. 完成性守卫失效：未完成的工具调用被无条件放行

- **发现**：`completionEvidenceAllows()` 在「存在 Pending（尚未返回结果的工具调用）」时，所有分支均返回 `true`，包括命中失败关键词、无成功证据、以及回复含"我已完成/已部署"之类自夸措辞的情况。核心分支 `agent_ledger.go:203-211`：`len(l.Pending) > 0` 块内 `if hasFailure { return true }`、`if !unsupportedSuccess.MatchString(answer) { return true }`、兜底 `return true` —— 三个分支恒真，守卫形同虚设。
- **触发条件**：客户端发来带 `tool_calls` 但缺对应 `tool` 结果的请求；或模型回复自称"部署成功/已完成"而未附任何工具结果。
- **影响**：网关把未实际执行的工具动作包装为"已成功"消息返回客户端，正是该守卫旨在防御的越界指令确认攻击面，可造成事实性错误乃至指令欺骗。
- **建议修复**：仅当 Pending 为空且满足以下之一才返回 true：(1) 有非自夸的完成证据；(2) 回复含明确的"无法确认"措辞。Pending 非空时一律返回 false。
- **涉及位置**：`internal/web/agent_ledger.go:190-222`（逻辑核心 203-211）、`internal/web/agent_ledger_test.go`（对应测试用例）

### H2. SSE 长连接写路径无背压、无取消：客户端断开后 goroutine 与上游 WebSocket 悬挂

- **发现**：所有流式写点都是 `fmt.Fprintf(w, ...)` + `Flush()`，返回值 error 被直接丢弃；`main.go:34` 把 `http.Server.WriteTimeout` 设为 0（为流式而设）。这意味着向已断开/停滞的客户端写入可以**无限阻塞**，且没有任何一处检查 `r.Context().Done()`。
- **触发条件**：SSE 客户端在流式中途断开，或停止读取（半关闭、TCP 接收缓冲填满）时恰好有数据要推送。
- **影响**：写阻塞后 `onDelta` 回调卡死 → ChatHub 读取循环卡死 → handler 无法 return → `defer conn.Close()`（`chathub/client.go:179`）不执行，**每个坏客户端悬浮 1 个 goroutine + 1 条上游 WebSocket**。高并发下可累积耗尽 fd/内存，且由于全库无 recover（见 L1），任意扇区 panic 会直接崩溃整个进程。
- **建议修复**：(1) 每次写前后检查 `r.Context().Done()`，完成时提前返回；(2) 给 SSE 连接加独立写超时/心跳踢出机制，抵消 `WriteTimeout: 0` 的无限窗口；(3) 写错误时立即终止 handler。
- **涉及位置**：`internal/web/server.go:952-970`、`internal/web/server.go:1134-1147`、`internal/web/tool_response.go:85-89`、`internal/web/protocol_handlers.go:59-64`、`cmd/server/main.go:28-36`

### H3. 附件上传无数量/总量上限：单请求内存可达 GB 级，且为服务端请求任意 URL

- **发现**：
  - `uploadAttachments()` 对每个 `type=image` 且非 `data:` 前缀的附件，**逐一从远程 URL 下载**（`chathub/client.go:429-447`，单张上限 10MiB），base64 编码（放大约 1.37 倍）后与整个 multipart 一并驻留内存（`client.go:460-466`）——单张图片峰值驻留约 25MiB，且逐张串行处理。
  - 附件数量无任何上限：`server.go:817` 从 messages 内容提取图片（`internal/web/multimodal.go:20-73`）+ 请求自带 `attachments` 数组，均无 `len()` 约束。
  - `/api/chat`（`server.go:616`）与 `/api/chat/stream`（`stream.go:20`）**连 `http.MaxBytesReader` 都没有**；10MiB body 上限（`server.go:776-777`）只约束 JSON 本体，约束不住 data URL 之外的远程下载总量。
  - URL 直接交给 `http.NewRequestWithContext` 执行 GET（`client.go:430-434`），scheme 不限 → SSRF 面。
- **触发条件**：持有 API key 的调用者发送几十个图片 URL 附件，或指向内网地址的附件。
- **影响**：数十张 × 25MiB ≈ 单请求近 GB 内存，多个并发直接 OOM；任意 URL 可被服务端代为访问，可探测内网服务。
- **建议修复**：(1) 限制附件总数（如 ≤10）与单张/总量字节；(2) URL scheme 白名单仅 http/https，且考虑加下载超时；(3) `/api/*` 统一加 `http.MaxBytesReader`。
- **涉及位置**：`internal/chathub/client.go:421-545`（429-447 远程下载、460-466 内存 multipart）、`internal/web/server.go:616`、`internal/web/stream.go:20`、`internal/web/multimodal.go:20-73`、`internal/web/server.go:776-777`

### H4. token 到期瞬间并发刷新"惊群"：刷新令牌轮换导致大面积 502 与误标过期

- **发现**：`Store.EnsureValid()`（`internal/auth/cache.go:203-251`）在 token 临近过期时**不做 per-account 单飞合并**：每个并发请求各自调用 `Refresh()`（`cache.go:224`）兑换 refresh token；AAD 刷新令牌只能成功兑换一次，失败的请求把账号标为 `expired` 并落盘（`cache.go:226-236`）。
- **触发条件**：某账号 token 刚过 30 秒提前量窗口，同一时刻有多个并发请求命中该账号。
- **影响**：并发刷新中只有一个成功，其余拿到 `invalid_grant` → `502`；且失败者会把账号持久化为 `expired`，而实际上一次成功刷新即可修复。高负载下周期性大面积 502。
- **建议修复**：(1) 按账号 ID 合并 in-flight 刷新（`sync.Map` + 共享结果 channel 或条件变量）；(2) 刷新失败后短暂重试或复用现有有效 token，不要立即落盘 `expired`。
- **涉及位置**：`internal/auth/cache.go:203-251`

---

## 三、中风险

### M1. mcp 包存在数据竞争与 goroutine 泄漏（当前为未挂载的死代码，启用即触发）

- **发现**：`internal/mcp` 的 `HandleSSE`/`HandleMessage`/`NewClient`/`ToolCallQueue` 均无任何调用方（`internal/web` 全目录 grep 无 `mcp/` 引用，Routes 注释声明的 `/v1/mcp/*` 挂载点均未注册）。但包内问题真实存在：
  - `sess.provider` 赋值（`mcp/server.go:226-268`）与读取（`mcp/server.go:194-199`）完全无同步 → 数据竞争。
  - `readSSE` goroutine 退出时直接写 `c.connected = false`（`mcp/client.go:116`），与 `Connect()`（`client.go:94`）、`Close()`（`client.go:253-256`）在互斥锁外的读写竞争；且 `Close()` 只置标志，**无法中断阻塞在 `bufio.Scanner` 读 `body` 上的 goroutine**（`client.go:114-132`），HTTP 连接亦无整体超时，连接永久泄漏。
  - `ToolCallQueue.Dequeue`（`mcp/queue.go:58-76`）持锁期间 `q.cond.Wait()` 由匿名 goroutine 执行：若 `ctx.Done()` 先到，`defer q.mu.Unlock()` 会对**已处于解锁状态的互斥量再次 Unlock → panic（"sync: unlock of unlocked mutex"）**，且等待 goroutine 永久悬挂。
- **触发条件**：挂载 `/v1/mcp/*` 路由并接入 MCP 客户端后，客户端断开、ctx 取消等常规场景。
- **影响**：进程级 panic、连接/goroutine 累积泄漏。
- **建议修复**：(1) 启用前逐一修复：provider 加锁或原子指针；(2) readSSE 改为可从外部关闭的读取（`http.Client.CloseIdleConnections` 或 context 化 body）；(3) Dequeue 重写为标准 `cond.Wait` 持锁等待模式，禁止在 goroutine 中 Wait。
- **涉及位置**：`internal/mcp/server.go:108-153`（挂载点，未注册）、`internal/mcp/server.go:194-199` vs `226-268`、`internal/mcp/client.go:114-132`、`internal/mcp/client.go:250-258`、`internal/mcp/queue.go:58-76`

### M2. 所有持久化 store 均采用非原子"直接覆盖写盘"

- **发现**：全部 store 的保存路径都是 `os.WriteFile` 直达目标文件，无临时文件 + `os.Rename` 原子替换：
- **触发条件**：进程在写盘间隙被杀（kill -9）/崩溃、磁盘写满、断电。
- **影响**：accounts.json / sessions.json / api-keys.json 等可能被写坏半截，下次启动 `json.Unmarshal` 失败被静默忽略（部分 store 返回空对象）→ 账号池 / 会话 / API key 全丢，用户被迫重新登录。
- **建议修复**：统一封装"写临时文件 + rename"原子落盘。
- **涉及位置**：`internal/auth/cache.go:96-101`、`internal/web/sessions.go:41-45`、`internal/web/sessions.go:118-122`、`internal/web/session_resolver.go:96-102`、`internal/web/conversation_manager.go:94-101`、`internal/web/keys.go:44-53`、`internal/web/debug.go:95`

### M3. 锁内执行磁盘 I/O：并发请求被磁盘延迟串行钉死

- **发现**：多个持有全局锁/条目锁的临界区里直接做整文件写盘：
- **触发条件**：高并发请求同时命中同一账号/会话（在锁内写盘）、磁盘延迟升高。
- **影响**：把磁盘延迟直接串进请求路径，吞吐被磁盘钉死；多个 store 各自用锁虽无死锁，但整体延迟叠加。
- **建议修复**：落盘移出临界区（内存态即时返回，后台批量/定时 flush；或按条目独立文件）。
- **涉及位置**：`internal/web/session_resolver.go:234`、`268`、`311`、`428`、`463`（`saveLocked`）、`internal/web/conversation_manager.go:114`、`218`、`internal/web/sessions.go:144`、`internal/web/keys.go:125`

### M4. sessionResolver 无容量上限 + 兜底全量相似度扫描

- **发现**：`sessionResolver.sessions` 仅按 2 小时 TTL 淘汰，**无条数上限**（`session_resolver.go:118-125`）；`Resolve` 未命中精确上下文时对**全部 session** 逐一计算 Jaccard 相似度（逐条 tokenize 整个历史，`session_resolver.go:165-187`、`289-324`），且在锁内执行。
- **触发条件**：服务长期运行累积数百至数千条 session；模糊上下文请求。
- **影响**：每个非流式请求 O(会话数 × 历史消息数) 的 CPU 扫描 + 大文件写盘，构成 CPU 型 DoS 面。
- **建议修复**：(1) 限制 session 记录数与总字节；(2) 按 ipFingerprint + 精确上下文指纹建索引，仅在同桶内做相似度计算。
- **涉及位置**：`internal/web/session_resolver.go:118-125`、`165-187`、`289-324`

### M5. conversationCleanup 无锁替换 connectionManager 指针

- **发现**：`conversations.go:49` 通过 `s.connectionManager = openConversationManager()` 直接覆盖字段指针，而并发请求在该字段上无锁读写（如 `server.go:1319`）。
- **触发条件**：存储切换触发重构时恰有并发请求访问。
- **影响**：真实数据竞争（写指针与并发读），偶发读到半状态。
- **建议修复**：改用 `atomic.Value` 或初始化后不再替换。
- **涉及位置**：`internal/web/conversations.go:49`、`internal/web/server.go:1319`

### M6. `/v1/*` debug 中间件记录完整请求/响应：脱敏不彻底且日志无界增长

- **发现**：debug 中间件对每个 `/v1/` 请求缓冲全文（可达 10MiB）并持久化（`debug.go:169-189`）；`redactBody`（`debug.go:46-59`）只替换**顶层**的 `api_key/access_token/authorization` 字段——响应体、工具调用参数、data: URL 等嵌套位置的原生敏感内容照单全收；日志文件（默认 `debug-logs.jsonl`）用 `O_APPEND` 无限追加（`debug.go:95-99`）。
- **触发条件**：开启 debug 并产生 `/v1/*` 流量。
- **影响**：敏感信息（token 引用、对话内容、工具机密）持久化泄露；磁盘无界增长直至写满。
- **建议修复**：(1) 默认关闭；(2) 限定捕获字节数；(3) 深度递归脱敏；(4) 日志文件轮转，仅保留最近 N 条。
- **涉及位置**：`internal/web/debug.go:46-59`、`95-99`、`169-189`

### M7. 无 graceful shutdown

- **发现**：`cmd/server/main.go:28-36` 直接 `log.Fatal(server.ListenAndServe())`，无 `Shutdown`/信号处理；`StartAutoCleanup`（`auto_cleanup.go:47-52`）内部为无穷 `for + sleep`，无停止通道。
- **触发条件**：任何形式的进程退出（Ctrl+C、部署重启、崩溃）。
- **影响**：中断所有长连接、可能写坏半截文件（叠加 M2）、浮动 goroutine。
- **建议修复**：`signal.NotifyContext` + `srv.Shutdown(ctx)`；清理循环增加停止信号。
- **涉及位置**：`cmd/server/main.go:28-36`、`internal/web/auto_cleanup.go:47-52`

---

## 四、低风险 / 提示性

### L1. 全库无任何 recover
- **发现**：整个代码库 grep 不到 `panic(`/`recover()`。任何 handler 内的未预期 panic（如 `protocol_handlers.go:106` 对 `tc["index"].(float64)` 这类类型断言若上游字段缺失）会直接崩溃整个进程，所有在线连接断开。
- **建议修复**：在 `http.ServeMux` 外层加 recover 中间件，转 500 并记录堆栈。
- **涉及位置**：全库；典型高危点 `internal/web/protocol_handlers.go:106`、`internal/web/server.go` 全部 handler

### L2. 工具路由提示语回归：2 个测试稳定失败
- **发现**：`TestNamedToolChoiceModeIsStable`、`TestModelToolRouterPromptMarksCompletedResults` 失败，说明 `model_tool_router` 生成的 prompt 与测试期望不一致。
- **影响**：可能是测试过期，也可能是功能回归——需人工确认；若是回归，工具路由质量下降。
- **建议修复**：对照测试期望与 `model_tool_router.go` 实际输出，确认哪边是真相并统一。
- **涉及位置**：`internal/web/model_tool_router.go` 与对应测试

### L3. settings / deployments 单例无锁初始化
- **发现**：`settings.go:104-114`、`deployments.go:43-58` 的全局单例在"首次并发访问"场景存在理论上的数据竞争；当前生产路径为启动时单线程初始化，规避了实际风险。
- **建议修复**：用 `sync.Once` 或包级初始化。
- **涉及位置**：`internal/web/settings.go:104-114`、`internal/web/deployments.go:43-58`

### L4. 直接使用 http.DefaultClient 做 Cloudflare 部署请求
- **发现**：`deployments.go:170`、`186` 用 `http.DefaultClient` 请求外部部署 API，无超时、无独立 transport。
- **建议修复**：换成带超时与连接池上限的专用 client。
- **涉及位置**：`internal/web/deployments.go:170`、`186`

### L5. ValidateSettings 错误被丢弃
- **发现**：`settings.go:112` `_ = validateSettings(s.v)`——磁盘配置非法时静默退化为默认值，运维无法察觉。
- **建议修复**：启动时打印明确警告或拒绝启动。
- **涉及位置**：`internal/web/settings.go:112`

### L6. GetStats 返回"带锁的拷贝"（go vet 已报）
- **发现**：`go vet` 报告 `return copies lock value`：`cache_stats.go:85`、`conversations.go:105` 在返回值中复制含 `sync.Mutex` 的结构体。
- **影响**：拷贝出的锁与原锁独立，使用方若持锁操作会得到"看似加锁实则未加锁"的错误对象；当前调用方只读字段，实际影响小。
- **建议修复**：返回指针或值拷贝前排除互斥锁字段。
- **涉及位置**：`internal/web/cache_stats.go:82-86`、`internal/web/conversations.go:102-105`

---

## 修复状态（2026-08-07 第二批后）

- **[已修复] H4（token 刷新惊群）**：`internal/auth/cache.go` `refreshInflight` 按账号单飞合并（waiters 阻塞共享 channel 复用胜者结果），不再出现并发兑换同一个一次性 refresh token 导致的惊群。
  - **勘误（2026-10-02）**：原文"失败不再立即落盘 expired"并不成立——合并只消除了 waiters 各自兑换的问题，`refreshInflight` 的错误分支仍对**任何**失败落盘 `status=expired`，包括网络抖动、代理超时与上游 5xx。现已按错误类型区分：只有 `invalid_grant` / `invalid_client` / `interaction_required` 等令牌端点明确判定为永久失效的响应才标记 `expired`；传输类与 5xx 失败保持账号原状态，并进入指数退避（5 分钟起，上限 30 分钟）。
- **[已修复] H2（SSE 写背压/悬挂）**：统一 `writeSSE`/`sseWriteFrame`/`sseDataRaw`/`sseSafeRaw`（`stream.go`/`protocol_response.go`），所有流式写路径写前检查 `r.Context().Err()`、写后检查错误并中止 handler；每次写前设 30s write deadline（抵消 `WriteTimeout: 0`）；server.go emitText/writeChunk/connected/DONE/错误帧全部走安全写。
- **[已修复] H3（附件）**：SSRF 校验（`chathub/ssrf.go`）+ `maxAttachments = 10` + `/api/chat`、`/api/chat/stream` 10MiB body 限制。
- **[已修复] M1（mcp 竞态/泄漏）**：provider 加锁、Dequeue 轮询、stdlib 接口、Close/setConnected 原子化。
- **[已修复] M2（原子落盘）**：`internal/web/atomicfile.go` 临时文件 + rename；auth cache/sessions/session_resolver/conversation_manager/keys/debug 均接入。
- **[已修复] M3（锁内写盘）**：`internal/web/persist.go` 节流落盘（markDirty/flushPending/flushNowBlocking），apiKeys/sessionResolver/conversationManager/sessionStores/usageLog 全部签约，usageLog 批追加保顺序。
- **[已修复] M4（sessionResolver 兜底扫描）**：maxSessions 1000 LRU 上限 + evictLocked。
- **[已修复] M5（conversationCleanup 无锁替换）**：`conversations.go` 改用 `SetMode()` 锁内设置，不再替换 manager 指针。
- **[已修复] M6（debug 中间件）**：递归深度脱敏（16 类敏感键）、请求捕获上限 256KiB、内存 500 条环形。
- **[已修复] M7（无 graceful shutdown）**：`main.go` signal.NotifyContext + `srv.Shutdown` + `StopPersistLoop` + `StartAutoCleanup` 停止通道。
- **[已修复] L1（无 recover）**：`internal/web/recover.go` recoverPanics 中间件 + streaming header 追踪。
- **[已修复] L2（工具路由提示回归）**：`model_tool_router.go` MODE 大写 + 多轮完成证据约束，2 个测试与全套 web 测试全过。
- **[已修复] L3（单例无锁）**：openDeployments/openSettingsStore 改 `sync.OnceValue`。
- **[已修复] L4（DefaultClient）**：`deploymentHTTPClient` 30s 超时独立 client，3 处替换。
- **[已修复] L5（ValidateSettings 错误丢弃）**：启动校验失败输出到日志。
- **[已修复] L6（GetStats 拷贝锁）**：`cache_stats.go` 返回不可变快照，`go vet` 不再报 lock copy。

**验证闭环提示（保留）**：本机无 gcc，`go test -race` 无法执行；建议在带 gcc 的 CI 补跑 `CGO_ENABLED=1 go test -race ./internal/...`。

## 优先处理排序与后续建议

按"可用性 × 被触达概率 × 修复成本"综合排序：

1. **H4** token 并发刷新惊群（周期性大面积 502，改动小，收益最大）
2. **H2** SSE 写路径悬挂（连接/goroutine 泄漏，需写超时与 ctx 检查）
3. **H3** 附件内存/SSRF 放大（安全 + OOM 面，改动集中于 uploadAttachments 与入口限流）
4. **H1** 工具完成守卫失效（已有测试复现，修复有明确验收标准）
5. **M2** 非原子持久化（任意坏文件即清状态）

**验证闭环提示**：本机无 C 编译器，`go test -race` 无法执行，全部数据竞争（H4、M1、M3、M5、L3）均为纯静态判定。建议在带 gcc 的 CI/容器环境补跑：
```
CGO_ENABLED=1 go test -race ./internal/...
```
并先修复 `internal/mcp/client_test.go:26` 的 `StartStdio` 未定义编译错误，使 mcp 包进入可测试状态。
