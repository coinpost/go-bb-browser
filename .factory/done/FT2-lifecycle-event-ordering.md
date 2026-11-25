---
branch: factory/FT2-lifecycle-event-ordering
round: 1
---
# 生命周期事件溢出重同步与优雅关停次序

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] internal/daemon/browser_reconnect.go:133：队列溢出重同步后继续消费旧created事件，可重新注册已消失页；建立事件与枚举次序边界。
[P2] internal/daemon/server.go:253：优雅关停target同步早于在途HTTP请求退出，期间销毁漏记；RPC排空后收齐生命周期事件再停止同步。

来源：FT1 第3～4轮review；旧枚举删除epoch基础约束仍在FT1第6轮，此处单独处理事件队列与关停协议，避免重复实现。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
按此次 owner 授权记录顺序与收窄范围，本项仍留在 backlog/。须在前置 FT7（ready 后 ship 并成功打 tag）以及 FT4 已交付之后，才移入 new/，不启动本次 build。

收窄后范围：优雅关停次序保留（RPC 排空后收齐生命周期事件再停止同步）。溢出后继续消费旧 created/生命周期事件、可能重新登记已消失页的问题与已交付 FT28 有重叠：先核实已有回归（含 FT28 审查 factory · review · 2026-11-23 14:13 head dab7d41a67aaddb4192d7e4abe02208a4e56ff4d 所记「旧排队事件」永久回归，在已合入 main 的 d408b68c5133cc6c17048940f9518e3428eb2cdd / merge 0d7c792e89667c616c64e6eb343eb92db104189e 上是否仍覆盖原调度）再决定是否实现，不以标题宣称已修好。

依赖顺序：FT7 ready → ship 并 tag → FT4 → 本项 → FT3 → FT6。

## factory · 2026-11-25 10:19
前置 FT4 已合入远端 main（merge 7b8aab43feaf205ecc73269a489d400487323c6b，已审 head 9b2f1c2add0f52f35b93c1a992e137b48e4c3294）。按既有顺序将本项从 backlog/ 移入 new/，等待外部工厂 build。本 run 不启动 FT2 的开发、build 或 review。

保留此前收窄范围：优雅关停次序优先（RPC 排空后收齐生命周期事件再停止同步）。溢出后继续消费旧 created/生命周期事件、可能重新登记已消失页的问题与已交付 FT28 有重叠：须由外部工厂先核验已有回归（含 FT28 审查 factory · review · 2026-11-23 14:13 head dab7d41a67aaddb4192d7e4abe02208a4e56ff4d 所记「旧排队事件」永久回归，在已合入 main 的相关提交上是否仍覆盖原调度）再决定是否实现，不以标题宣称已修好。不移动 FT3/FT6，FT5 保持 stop。

## factory · plan · 2026-11-25 10:48
### Behaviour
1. 正常服务时维持既有事件读出时钟、完整 target ID 和生命周期同步语义。
2. 优雅关停先停止接收新 HTTP 请求，保留在途 RPC 所需的 CDP 会话和生命周期消费；等待在途 HTTP/RPC 排空，再收齐已读生命周期事件、结算必要的最终枚举，随后停止同步、释放会话，最后排空并关闭 audit/store。
3. 空队列、没有浏览器、启动失败、HTTP 服务错误、CDP fatal、排空超时/强制关闭路径均有明确且有界的清理；不丢既有错误，不因为等待事件而无限阻塞。枚举失败不得假称目标已消失。
4. 核验 FT28 已有旧 created/overflow 回归：若已覆盖原调度，只记录证据，不重复实现；有真实空缺才在相同生命周期模型内补永久回归和必要修正。
### Scope
聚焦 server shutdown 次序和 target sync 生命周期，永久并发回归及准确文档；不恢复 FT21，不改完整 ID 契约，不推进 FT3/FT6，不做 ship/tag/release，不修改 never 路径。
### Decisions
当前 runCtx 派生于调用方 ctx，取消会过早停止同步，而 CDP 会话本身 WithoutCancel。分离服务停止信号与后台生命周期存活时间，显式取消并等待所拥有的后台任务；采用最少必要的所有权边界，不引入通用生命周期框架。最终事件边界以连接实际已读通知为准，结合现有有界枚举，不声称能观测断线后从未收到的事件。具体细节由代码与可控回归验证。
### Checking
新增确定性回归：在途 RPC 尚未完成时开始 shutdown，再到达 target destroyed，RPC 完成后才释放 session/store；持久化 retirement 不漏记，可重启回放验证；覆盖空队列、fatal/错误和超时路径，检查无死锁/竞态。查证并运行 FT28 既有排队旧事件/overflow 回归，配置测试 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 必须通过。脚本测试使用隔离 HOME/XDG/cache，不依赖真实外网；需要 Chrome 则只用非 headless 隔离 Chrome。没有 UI 改动不需 UI 验证，真实 Chrome 未运行必须说明。

## factory · 2026-11-25 10:48
构建完成：new/ → review/。起点 7b8aab43feaf205ecc73269a489d400487323c6b，head 429acb3；代码提交 ee04c99、文档提交 429acb3，已推送 factory/FT2-lifecycle-event-ordering，未合入 main、未打 tag、未发布。

## 结论一：溢出后旧 created 事件（原 P2 `browser_reconnect.go:133`）——FT28 已覆盖，未重复实现

原调度：队列溢出 → 重同步枚举（未列出 A）已应用 → 再消费排队中的旧 `created(A)` → 重新登记已消失页。
在 `7b8aab4` 上该调度已被 FT28 的“事件读出时刻”模型拒绝（`RegisterEvent(stamp, id)`：stamp 之后应用过枚举 / 回复删除 / 溢出即拒绝并请求新枚举；公告在其后请求的枚举未列出时即判消失）。证据（本次全部重新运行，`-race` 通过）：

- `internal/state/tab_registry_overflow_test.go`
  - `TestAnyDroppedEventOutdatesQueuedExistenceEvents`：任何溢出都使仍排队的旧“存在”事件失效。
  - `TestDestroyedReadOutdatesQueuedExistenceEvents`：销毁通知（被丢弃或仍排队）之前读到的旧事件不得登记。
  - `TestEventSeenIsItsReadInstantNotItsHandlingInstant`：证据时刻是读出时刻而非处理时刻。
- `internal/state/tab_registry_announced_test.go` `TestAbsentListAppliedBeforeRegistrationProvesAnnouncedTargetGone`；`internal/state/tab_registry_known_target_test.go` `TestRegisterEventKeepsEarliestProofOfKnownTarget`、`TestDestroyedReadIsRememberedOnlyWhileQueued`。
- daemon 级（可控 CDP stub，经真实 `browser.Session` 读线程与 `runTargetSync`）：
  - `internal/daemon/lifecycle_causality_test.go` `TestEventReadBeforeAbsentListDoesNotOutliveIt`（旧 created 在缺席枚举在途时才被消费）、`TestDroppedDestroyIsRecordedBeforeOverflowIsPublished`（溢出发布次序）。
  - `internal/daemon/overflow_evidence_test.go` `TestDroppedDestroyDuringCreate`、`TestDiscoveryRacesOverflowRecovery`；`overflow_reader_evidence_test.go` `TestDiscoveryRacesOverflowRecoveryWithReaderEvidence`。
  - `internal/daemon/lifecycle_announced_test.go` `TestAbsentListAppliedBeforeRegistrationSettlesAnnouncedTarget`（枚举先于排队事件应用，两种先后）。
- 另用一次性探针（未提交，已删除）在 registry 上逐步重放原调度的两个变体（销毁通知被丢弃 / 其它事件被丢弃 → 重同步应用 → 再 `RegisterEvent(旧 stamp)`）：均拒绝登记，`Known`/`Has` 为 false。

判断：无真实空缺，不新增实现也不新增重复回归。

## 结论二：优雅关停次序（原 P2 `server.go:253`）——已修复

根因：`runCtx` 派生自调用方 ctx，停止信号一到，target 同步循环、watchdog、sweeper 立即退出，而 HTTP 在途 RPC 还要排空（最长 15s + 60s）。其间读到的销毁通知只让 id 停止解析，`managed_tab_retire` 不落盘；在途 RPC 请求的枚举也无人发送。

### 设计决策

1. **所有权**：`Server.bg`（`background`：`sess`、`settle`、`syncDone`、`tasks`）是一次 `ListenAndServe` 调用拥有的全部后台工作；其 context 用 `context.WithoutCancel(ctx)` 与调用方脱钩，只由 `stopRun` 取消。启动阶段用 `context.AfterFunc(ctx, stopRun)` 保留“SIGTERM 可中止首次枚举”，服务起来后解除。没有引入通用生命周期框架。
2. **统一次序**（停止信号 / fatal / HTTP 服务出错三条出口相同）：`shutdownHTTP`（停止接收并排空）→ `stopBackground`（收齐事件、停后台）→ 释放 session → `closeAudit`（最后）。
3. **`stopBackground(settle)`**：
   - `settle`（有序停止且后台 context 仍存活）：关闭 `bg.settle`，同步循环执行 `settleTargets`：清空已排队事件；若仍有枚举请求（`ConsumeTargetDirty`）则发一次以 `CDPWatchdogTimeout` 为限的枚举，最多 `maxSettleEnumerations = 2` 次（请求本身 + 其答案可能留下的核验），然后返回。
   - 取消后台 context，等待循环与周期任务退出；循环退出后由 `ListenAndServe` 所在 goroutine 做一次不发浏览器请求的 `drainTargetEvents`（此时它是队列唯一消费者）。
4. **各路径**：
   - 空队列/无枚举请求：不向浏览器发任何请求，立即返回。
   - 无浏览器（`SkipBrowserAttach`/`tabHook`）：没有同步循环，仅等待周期任务。
   - 启动失败（attach 失败：未启动任何后台；attach 后 listen 失败：不结算，取消并等待后台，释放 session）。
   - HTTP 服务错误：现在同样先 `shutdownHTTP` 排空已接受的请求，再结算；原服务错误原样返回。
   - CDP fatal：`fatalCDPLost` 取消后台 context，不再向连接发请求；已读到的事件仍由最后一次 drain 应用；fatal 错误原样返回。
   - 排空超时/强制关闭：`shutdownHTTP` 的错误照旧返回，随后仍做有界结算。
   - 枚举失败/超时：只告警，不判定任何 tab 消失；遗留受管记录由下次启动的首次成功枚举裁决（既有 `recoverManagedLocked`）。
   - 每次等待以 `settleBudget`（默认 `(2+1)×CDPWatchdogTimeout + 5s`，`Server.settleTimeout` 为测试用覆盖）为限；超时则告警放弃，照常关闭 session 与日志（输掉竞争的写入得到 `store.ErrClosed`，既有处理）。结算失败/超时不改变 `ListenAndServe` 的返回值（服务错误 + `closeAudit` 错误的既有组合保持）。
5. **边界**：以连接读线程**已读到**的通知为准（队列被发现为空的那一刻）；不为“冲刷”通知额外发往返，不声称能观测仍在途或断线后从未到达的通知。
6. **对 plan 的细化**：`runTargetSync` 在 ctx 结束时仍立即返回、不动队列（最后的 drain 归 `stopBackground`），使大量直接驱动该循环的既有回归调度不变；循环体拆出 `drainTargetEvents` / `resyncTargets`，行为不变。idle/observer sweeper 与 watchdog 随后台 context 一直运行到排空结束（此前在停止信号时即停），随后被等待退出。

### 新增永久回归（`internal/daemon/shutdown_lifecycle_test.go`，全部为**可控 CDP stub**，生产入口 `NewServer` + `ListenAndServe`，仅 loopback）

| 测试 | 覆盖 |
| --- | --- |
| `TestShutdownAppliesDestroyedTabWhileRPCDrains` | 在途 eval 未完成时开始 shutdown，再到达 target destroyed：排空期间即应用（登记/idle/受管记录），不发枚举，session 未释放、`ListenAndServe` 未返回；RPC 完成后才退出；重放 `rpc.jsonl` 确认 retirement 已持久化 |
| `TestShutdownAnswersOpenEnumerationRequest` | 销毁通知丢失 + 排空期间有枚举请求：停止前结算并持久化 |
| `TestShutdownWithNothingToSettleAsksTheBrowserNothing` | 空队列：不发枚举，2s 内返回，存活受管 tab 不被退役 |
| `TestShutdownFinalEnumerationFailureConcludesNothing` | 三个子用例：枚举被拒 / 枚举超时（`CDPWatchdogTimeout`）/ 结算预算超时（`settleTimeout`）：有界、返回 nil、不退役 |
| `TestShutdownOnCDPLossIsBoundedAndKeepsTheError` | fatal：返回 `cdp session lost`，同步循环已退出，连接丢失后不再发枚举，不退役 |
| `TestStopBackgroundAppliesEventsReadAfterTheSyncLoopStopped` | 循环已停后读到的销毁通知由最后一次 drain 应用，不发浏览器请求 |
| `TestListenFailureAfterAttachStopsBackgroundWork` | attach 后 listen 失败：返回 listen 错误，后台已停，session 已释放 |

反向验证：仅把后台 context 改回派生自调用方 ctx 时，`TestShutdownAppliesDestroyedTabWhileRPCDrains` 与 `TestShutdownAnswersOpenEnumerationRequest` 失败（已恢复）。既有 `TestShutdownWaitsForHandlerBeforeClosingAudit`、`TestListenAndServeShutdown`、`TestListenAndServeReportsAuditCloseErrorOnCleanShutdown`、`TestListenAndServeKeepsServiceErrorNextToAuditCloseError` 不变并通过。

## 测试结果

- 完整配置测试 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`：**exit 0**，全部包 `ok`（cmd/bb-browser、cmd/bb-daemon、deploy、deploy/systemd、internal/browser、internal/daemon、internal/site、internal/state、internal/store、internal/timeout、pkg/daemonclient；pkg/protocol 无测试）。日志：TMPDIR 下 `ft2-fulltest.log`。
- 新增与关停相关测试 `-race -count=30` 通过；连同启动/watchdog/生产连接相关用例 `-race -count=15` 通过。
- `go vet -tags realchrome ./internal/daemon` 通过（仅编译/静态检查）。
- 环境：隔离 HOME（TMPDIR 内 `ft2-home.*`），XDG_*、GOCACHE、GOMODCACHE、GOPATH 均在其内，`GOPROXY=off`（模块为已下载固定版本的拷贝，未访问外网）；`after_change=[]`。

## 未验证 / 未检查

- **真实 Chrome 未运行**：`realchrome` 验收（`TestRealChrome*`）本次未执行，关停次序仅由可控 CDP stub 回归验证；stub 不能证明真实 Chrome 在排空期间通知与回复的相对时序。
- 未用真实进程信号（SIGTERM 发给 `bb-daemon` 二进制）做端到端验证；`cmd/bb-daemon` 未改动。
- 结算等待超时后“同步循环仍未退出而被放弃”的分支（需要 `ForgetTarget` 等内部等待挂死）没有专门回归，仅由代码审读与 `settlement budget exceeded` 子用例（循环随取消退出）间接覆盖。
- `shutdownHTTP` 强制 `srv.Close()` 后仍在运行的 handler 与 session/store 关闭的竞争为既有行为，未改动也未新增测试。
- 工具链为本机已有的 Go 1.27.4（mise），非 AGENTS.md 所述 Go 1.26；未在 linux/amd64 上运行。

## 安装 / 依赖

- 无新安装，无新增或未固定的依赖/工具（`go.mod`/`go.sum` 未变）。


经理独立重跑配置完整测试（gofmt + go vet ./... + go test -race ./...）：exit 0。never 路径未修改，工作区干净，提交仅含本项范围。

外部写入：coder 报告本机无参数 mktemp 忽略 TMPDIR，曾产生四个系统 per-user 临时目录，已由 coder 删除；未报告精确路径。经理 /tmp 前后清单无新增。无新安装。Claude Code 5 小时额度已用 71%，返回时约 2 小时 43 分钟后重置；本次无须重试。


## factory · review · 2026-11-25 10:55
head: 429acb3766d665bc094ebaa0fb5c4955e8bcb13b
verdict: pass
notes:
- [P2] internal/daemon/browser_reconnect.go:169: 读线程已为 targetDestroyed(A) 记录时间戳、A 已停止解析但通知尚未入队时，关停两次排空仍可见空队列，重放仍有 A；最终排空前应与读线程建立边界，等待边界前已读通知完成入队或溢出登记。
- [P2] internal/daemon/server.go:299: 队列满导致销毁通知丢弃后若 CDP 断线，最终排空只处理队列，已 doomed 的受管目标仍可能留在持久化记录；应在 lifeMu 下结算注册表中确切销毁证据，不把枚举失败当作消失。
- [P2] internal/daemon/browser_reconnect.go:167: 最终同步 drainTargetEvents 会逐个等待 ForgetTarget，未受收尾预算约束；14 个已附着目标 detach 等待的探针耗时 14.03 秒，超过默认 settleBudget 11 秒。应让最终事件处理与附件释放受期限约束，并优先记录已确认退役。
- [P2] internal/daemon/browser_reconnect.go:362: watchCDPSession 未纳入后台任务等待且只监听 session context；正常返回后断开 stub 仍触发 fatalCDPLost。应监听本次运行停止信号，释放会话前等待 watcher 退出。
- [P2] internal/daemon/server.go:417: Serve 错误路径丢弃 shutdownHTTP 的错误；请求耗尽第二段 60 秒预算被强制断开时只返回 accept 错误。应保留原服务错误并合并排空错误；411 行 fatal 分支同理。

Codex 独立审查 origin/main...HEAD，未发现 P0/P1。coder 因上述五个 P2 建议修改，但工厂规则只以 P0/P1 阻断，故 verdict: pass，review/ → ready/；这些非阻断问题保留供 owner 决定，不宣称无缺陷。

配置完整测试 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` exit 0，经理核验日志及干净工作区。工作树外 overlay 探针复现上述五项，另验证 HTTP listener 出错但在途 RPC 正常完成时 retirement 可落盘。分支与 main 可干净合并，审查未改文件、无提交、无新安装。

独立重跑 FT28 溢出/缺席枚举/旧事件回归，并以完整 target ID 重放“旧 created(A) 排队 → 溢出 → 应用不含 A 的枚举 → 消费旧事件”；两种丢弃变体各 5 次，A 未复活，B 仍可解析。原溢出旧事件问题已有覆盖，无须重复实现。

未检查：真实 Chrome、二进制进程级 SIGTERM、Linux；环境 macOS arm64、Go 1.27.4、loopback CDP stub。脚本测试隔离 HOME/XDG/cache。无新安装。/tmp 前后清单无新增；除 run lock 与 git worktree index 元数据外无额外外部写入。未合并 main、未打 tag、未发布。

## factory · 2026-11-25 11:00
ship 完成。merge: 3287688b7ed2bf6c4730de6962630e12f22a1a9d（parents: 7b8aab43feaf205ecc73269a489d400487323c6b + 429acb3766d665bc094ebaa0fb5c4955e8bcb13b），已普通推送 main。已审 head 429acb3766d665bc094ebaa0fb5c4955e8bcb13b 为远端 main 祖先。未打 tag、未发版、未删除代码分支、未改 workflow 或 factory.toml。

测试命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过：11 个有测试包 ok（bb-browser、bb-daemon、deploy、deploy/systemd、internal/browser、internal/daemon、internal/site、internal/state、internal/store、internal/timeout、pkg/daemonclient），pkg/protocol 无测试文件；go.mod/go.sum 未变。

不修改先前 review 正文与 verdict: pass（factory · review · 2026-11-25 10:55）。审查已记录独立重跑 FT28 溢出/缺席枚举/旧事件回归并确认原溢出旧事件问题已有覆盖、无须重复实现；本 ship 不自行调查代码或复审。审查 notes 中的五个 P2 仍按原记录保留，非本 run 处理范围。无 source 或 owner Closes/Fixes/Resolves 链接，无需关闭 issue/PR。按既有顺序，本项合入远端 base 后将安排 FT3 backlog → new；本 run 不启动 FT3 构建或审查，不移动 FT6，FT5 保持 stop。
