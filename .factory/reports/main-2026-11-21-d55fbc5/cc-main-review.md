# go-bb-browser main 全库只读代码审查报告

- **对象**：`coinpost/go-bb-browser` main，detached HEAD `d55fbc55162db73a43c01e1a6b782750437cfb5a`
- **性质**：main 全库审查（不是 factory/FT1 分支 diff 审查）。只读：未修改生产代码、未提交、未 push/fetch、未运行 gh、未切换分支、未启动任何 Chrome。
- **结论**：确认 **21 个 bug**（P0 0 / P1 2 / P2 8 / P3 11），每个为可独立修复的单一根因；另列 17 项待确认风险（不建 bug 任务）。
- **配置测试**：`gofmt` / `go vet` / `go test -race ./...` 在 main 上全部通过（无现有测试失败）。
- **机器可读清单**：[findings.json](findings.json)（21 项，字段 id/severity/title/file/line/trigger/actual/expected/impact/root_cause/fix_direction/acceptance/evidence）。

> 所有 bug 均在 main@d55fbc5 当前代码上独立验证；未引用 FT1 分支的代码或结论，FT1 分支才引入的回归不在本报告内。

## 1. 覆盖表（按系统边界）

| 边界 / 模块 | 文件 | 审查内容 | 方式 | 结果 |
|---|---|---|---|---|
| daemon 入口与配置 | `cmd/bb-daemon/main.go`、`internal/daemon/config.go` | flag/env 解析、默认值、退出码、Validate | 全文阅读 + 构建二进制实跑（不可写 state dir） | 无独立 bug；见 B09、B12（内存模式回退） |
| HTTP / JSON-RPC 分发 | `internal/daemon/v1.go`、`server.go`、`rpc_observation.go`、`audit_ctx.go` | 请求校验、方法分发、审计、超时、关停顺序 | 全文阅读 + fake conn 测试 | B01、B21 |
| 鉴权与安全边界 | `internal/daemon/auth.go`、`internal/store/client_ip.go` | token 白名单、常量时间比较、loopback 豁免、代理头 | 全文阅读（对照 AGENTS.md 条款） | 未发现缺陷 |
| tab 身份 / 生命周期 / 并发 | `internal/daemon/tab_idle.go`、`rpc_connections.go`、`internal/state/tab_registry.go`、`tab_idle.go` | 短 id、注册表同步、每 tab 锁、空闲回收、重启恢复 | 全文阅读 + barrier / 白盒测试 | B03、B04、B10、B14 |
| CDP 监督 / 重连 / 健康 | `internal/daemon/browser_reconnect.go`、`health.go` | watchdog、fatal 退出、错误分类、探活 | 全文阅读 + fake conn 测试 | B01；R4、R5 |
| browser：会话与操作 | `internal/browser/session.go`、`errors.go`、`fetch.go` | attach 池、取消语义、eval/click/fill/screenshot/fetch | 全文阅读 + 对照 chromedp v0.15.1 源码 | B01；R4、R5、R7 |
| browser：快照 | `internal/browser/snapshot.go` | @ref 编号与选择器 | 全文阅读 + node fake DOM 执行原脚本 | B05 |
| browser：观测 | `internal/browser/observer.go`、`internal/state/obs_store.go`、`ringbuf.go`、`internal/daemon/obs_sink.go` | seq/cursor、丢弃计数、租约过期、INV-5/6 | 全文阅读 + barrier 测试 | B15 |
| browser：网络拦截 | `internal/browser/network_routes.go` | 规则表、worker、Fetch 同步、匹配 | 全文阅读 + 单元测试 | B16；R1–R3、R11 |
| 持久化 / 恢复 / 轮转 | `internal/store/store.go`、`replay.go`、`audit_parse.go`、`compact_body.go` | 追加、replay 截断、轮转快照、seq、权限 | 全文阅读 + 临时目录测试 | B03、B11、B12、B13；R6、R10 |
| 协议 | `pkg/protocol/jsonrpc.go`、`health.go` | 方法表、参数/结果字段与 handler 一致性 | 阅读 + 子代理交叉核对 | 未发现不一致 |
| 客户端库 | `pkg/daemonclient/*.go`、`README.md` | Client.Call、Pool 分配/亲和/故障转移、tab id 解析 | 子代理全文审查 + 本人复核关键代码并重跑复现 | B06、B19、B20；R7、R9、R14 |
| CLI | `cmd/bb-browser/main.go`、`url.go`、`launch*.go` | 子命令参数、退出语义、输出、launch 清理 | 全文阅读 + 进程内执行 cobra 命令 | B02、B07、B18、B21；R8、R17 |
| adapter 运行 | `internal/site/run.go`、`adapters.go` | @meta 解析、参数映射、包装改写 | 全文阅读 + Go 测试 + node 执行 | B17 |
| 打包 / 部署 / 文档 | `Dockerfile`、`.goreleaser.yaml`、`.github/workflows/release.yml`、`deploy/**`、`README.md`、`AGENTS.md`、`skills/**` | 文档声明 vs 实际行为、单元/镜像可用性 | 子代理逐条核对 + 本人复核 | B02、B08、B09、B21；R15、R16 |

`CLAUDE.md` 在仓库中不存在；已阅读 `README.md` 与 `AGENTS.md`。

## 2. Bug 统计

| 级别 | 数量 | 编号 |
|---|---|---|
| P0 | 0 | — |
| P1 | 2 | B01、B02 |
| P2 | 8 | B03–B10 |
| P3 | 11 | B11–B21 |

验证强度：18 个由 Go 诊断测试 FAIL 证明（overlay，不落入仓库），B05/B17 另有 node 执行证据，B08/B09/B21 为静态确认（B09 的代码侧另有本地二进制实跑）。各条“验证方式”中已注明未能用真实 Chrome / Docker / systemd 验证的部分。

## 3. 已确认 Bug

### B01 [P1] 页面 JS 异常文本含 websocket/eof 等字样时 daemon 误判 CDP 断开并整体退出

- **位置**：`internal/daemon/browser_reconnect.go:46`
- **触发条件**：任一客户端调用 eval / run（adapter）/ snapshot --scope，页面侧抛出的异常描述里含 "websocket"、"eof"、"broken pipe"、"connection reset"、"channel closed"、"could not dial"、"use of closed network connection" 任一子串（大小写不敏感）。例：脚本抛 `Failed to execute 'send' on 'WebSocket': Still in CONNECTING state.`；`snapshot` 的 selector_scope 为 `div[data-kind=websocket`（非法选择器，异常文本会回显选择器）。
- **实际行为**：Session.EvalAwaitContext（internal/browser/session.go:816-818）与 SnapshotContext（internal/browser/snapshot.go:161-164）把 chromedp 返回的 *runtime.ExceptionDetails 原样上抛；handler 调 cdpHint（internal/daemon/v1.go:1261）→ isCDPSessionLost 按错误字符串子串匹配（browser_reconnect.go:43-58，internal/browser/errors.go:79-87）判为 ErrorTransport → fatalCDPLost：置 sessionFailed、取消 runCtx、ListenAndServe 返回错误，进程以 1 退出。
- **期望行为**：页面级 JS 异常只应作为该次 RPC 的 `eval failed` / `snapshot failed` 错误返回；daemon 保持运行，CDP 会话状态不变。只有真正的传输层错误（websocket 读写失败、连接关闭）才触发 fatal。
- **影响**：任何有 RPC 权限的调用方（或被访问页面的脚本内容，经 adapter eval 透传）一次请求即可让 daemon 退出；所有客户端在途请求失败，观测缓冲、网络拦截规则、焦点等内存状态全部丢失，依赖 supervisor 重启。对爬虫类调用方表现为随机整机重启。
- **根因**：传输错误识别基于 err.Error() 子串，而页面可控文本（异常 description、选择器回显）会进入同一条错误链；eval/snapshot/fetch 路径没有把页面异常包装成 Kind=ErrorRequest 的 CDPError。
- **最小修复方向**：在 Session 的 eval/snapshot/fetch 路径用 errors.As 识别 *runtime.ExceptionDetails（及 *cdproto.Error 协议级错误）并包装为 Kind=ErrorRequest 的 CDPError；isCDPSessionLost 只信任类型化的 ErrorTransport（或仅对非 CDP 应答类错误做子串兜底）。
- **回归验收**：新增 daemon 测试：fake conn 的 Eval/Snapshot 返回 *runtime.ExceptionDetails（description 分别含 websocket、EOF、非法选择器回显），RPC 返回 JSON-RPC error，且 fatalCh 无值、ensureBrowserSession 仍为 nil；browser 包测试断言 ErrorKindOf(*runtime.ExceptionDetails) != ErrorTransport；原有真实传输错误（io.EOF、"use of closed network connection"）仍判定为丢失。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_PageExceptionTextMustNotKillDaemon，3 个子用例 FAIL）；[repro/daemon_audit_test.go](repro/daemon_audit_test.go)
- **验证方式**：本地 fake conn 复现（错误类型与 chromedp.Evaluate 一致）；未用真实 Chrome。

### B02 [P1] `bb-browser run SCRIPT.js --name value` 被 cobra 当作未知 flag 拒绝，文档化的命名参数形式全部不可用

- **位置**：`cmd/bb-browser/main.go:945`
- **触发条件**：按 README.md:333、skills/bb-browser/SKILL.md:50、references/script-system.md:40 的写法执行 `bb-browser run ./x.js hello --count 20` 或 `--count=20`。
- **实际行为**：cobra 在 RunE 之前解析 flag，返回 `unknown flag: --count`，退出码 1；main.go:958-977 手写的 `--name value` / `--name=value` 解析永远到不了。只有未文档化的 `run x.js -- hello --count 20` 能通过。
- **期望行为**：`--name value` / `--name=value` 透传给 adapter，成为 args.name；全局 flag（--json/--tab/--url/--api-token）仍可用。
- **影响**：adapter 的命名参数（两份 skill 文档和 README 的主要用法、所有带 flag 的示例）完全不可用；按文档操作的 agent/用户只会得到 unknown flag。
- **根因**：run 子命令只设置了 Args: cobra.MinimumNArgs(1)，没有 DisableFlagParsing 或 FParseErrWhitelist.UnknownFlags，未知 flag 在进入 RunE 前即报错。
- **最小修复方向**：给 run 命令设置 FParseErrWhitelist{UnknownFlags:true} 并从 os.Args/cmd.Flags().Args() 之外取回原始 token，或 DisableFlagParsing=true 后手工处理全局 flag；保持 `--` 分隔写法兼容。
- **回归验收**：CLI 测试：fake daemon 记录 eval 脚本；`run x.js hello --count 20` 与 `--count=20` 均成功且脚本参数 JSON 含 "count":"20"；`run x.js --json hello`、`--tab t1` 仍按全局 flag 生效；缺值的 `--count` 仍报 `flag --count needs a value`。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_RunAcceptsDocumentedNamedArgs FAIL）；[repro/cli_audit_test.go](repro/cli_audit_test.go)
- **验证方式**：进程内执行 newRootCmd() + httptest fake daemon 复现。

### B03 [P2] 空闲回收/浏览器侧关闭的受管 tab 永不从持久化 managed 集合移除，重启后同短 id 的用户 tab 被当成受管 tab 关闭

- **位置**：`internal/daemon/tab_idle.go:215`
- **触发条件**：（1）daemon 用 tab_new 创建 tab，随后该 tab 被空闲回收关闭，或被用户在 Chrome 里手动关闭/崩溃（没有 tab_close RPC）。（2）daemon 重启（或运行期 runTargetSync 重新 reconcile），浏览器中存在 CDP target id 末尾 hex 与旧 tab 相同（→ 相同短 id）的用户 tab。
- **实际行为**：store.managed（internal/store/replay.go:31-39）只在成功的 tab_close 审计记录时删除条目；closeExpiredTabs → closeTabByShortLocked（tab_idle.go:215）与 removeTargetState（tab_idle.go:58-64）不写任何记录。条目永久留在内存与 rpc.jsonl，并在每次轮转时被 writeManagedSnapshotLocked 重新写入。重启后 reconcileIdleFromLog（tab_idle.go:101-114）按短 id 查到用户 tab 并 MarkManagedIfAbsentAt，空闲超时后把用户 tab 关闭。复现中用户 tab CCCC…1234 被关闭。
- **期望行为**：tab 被空闲回收或被确认从浏览器消失后，应从持久化 managed 集合移除；只有 daemon 创建且仍存活的同一 target 才能在重启后恢复为受管。
- **影响**：（a）daemon 可能关闭用户自己打开的标签页（数据丢失：未保存表单、登录流程）；残留条目越多概率越高（每条残留对任意新 tab 的命中率约 1/65536，长期运行的爬虫 daemon 可累积上万条）。（b）managed 集合、rpc.jsonl 快照无界增长，轮转后文件被陈旧快照占满。
- **根因**：持久化 managed 状态只由“成功的 tab_close RPC 审计行”驱动，空闲关闭/目标销毁路径没有对应的持久化事件；且恢复时仅按 4 位起的短 id 匹配，不校验 target 身份。
- **最小修复方向**：在 removeTargetState/closeExpiredTabs 成功后向 store 追加一条合成 tab_close（或提供 Store.ForgetManaged）；可选增强：managed 记录里保存完整 target id，reconcile 时校验一致才恢复。
- **回归验收**：集成测试（真实 state dir + fake conn）：tab_new → 强制空闲过期 → closeExpiredTabs 后 store.ReplayManagedTabActivity() 不含该短 id；重开 Server 并注入同短 id 的用户 tab，reconcileIdleFromLog 后 tabIdle.Snapshot() 不含该用户 tab，closeExpiredTabs 不会关闭它；target 销毁事件路径同样断言；轮转后新 rpc.jsonl 不含已关闭 tab 的合成 tab_new。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_IdleClosedTabMustLeavePersistedManagedSet FAIL，含用户 tab 被关闭的后果断言）；[repro/daemon_audit_test.go](repro/daemon_audit_test.go)
- **验证方式**：真实 state dir + fake conn 复现，启动序列按 connectBrowserLocked 手工重放。

### B04 [P2] 过期的 Target.getTargets 快照晚于 tab_new 应用时，会把刚创建的受管 tab 从注册表和空闲跟踪中删除

- **位置**：`internal/daemon/tab_idle.go:38`
- **触发条件**：并发：请求 A（tab_list / tab_focus / tab_select，或任何未知 tab 触发的 resolveTab 回退、runTargetSync 重同步）已取得不含新 tab 的 getTargets 结果但尚未调用 syncTabsFromTargets；请求 B 的 tab_new 在此间隙完成 createTarget 并登记。
- **实际行为**：syncTabsFromTargets（tab_idle.go:38-45）无条件把注册表与快照对齐：SyncPageTargetsDetailed 把新 tab 记为 removed（internal/state/tab_registry.go:54-60），clearRemovedTarget 随即 forgetTabManaged、清观测缓冲、forgetTabLock，并对真实 Session 调 ForgetTarget（取消该 tab 的 chromedp 子 context、清路由）。复现中 tab_new 返回的短 id 4d5e 被移出注册表且不再受管。
- **期望行为**：早于 tab 创建时刻取得的 target 快照不得移除更晚登记的 target；tab_new 返回成功的 tab 必须保持注册与受管状态，直到真正关闭。
- **影响**：该 tab 在本进程内失去受管标记，永不被空闲回收（标签页泄漏，直到 daemon 重启靠 rpc.jsonl 恢复）；已启用的观测缓冲与拦截规则被清空；若该 tab 已建立 tabPool 连接，ForgetTarget 取消活跃 chromedp 子 context 会按其注释所述直接关闭该标签页。多客户端/Pool 并发 tab_new 与 tab_list 时是微秒级但持续存在的竞态。
- **根因**：“读浏览器 target 列表”与“应用到注册表”之间没有版本/时序保护，注册表的增量登记（tab_new、target 事件）与全量对齐互相覆盖。
- **最小修复方向**：给注册表加单调代次（每次 RegisterPageTarget/事件登记递增），getTargets 发起前记录代次，对齐时跳过代次更晚登记的 target；或把 getTargets+对齐与 tab_new 的 create+register 串行化在同一把锁内。
- **回归验收**：barrier 测试：fake conn 的 PageTargets 在取得快照后阻塞，期间完成一次 tab_new，再放行；断言 tabs.Lookup(short) 成功、tabIdle.Snapshot() 含新 target、观测缓冲未被清；并覆盖 resolveTab 回退与 runTargetSync 重同步两条调用路径；-race 下 200 次并发 tab_new/tab_list 后所有 tab_new 返回的 tab 均受管。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_StaleTargetListMustNotUnmanageNewTab FAIL）；[repro/daemon_audit_test.go](repro/daemon_audit_test.go)
- **验证方式**：barrier 确定性复现（fake conn）；未验证真实 Chrome 下的触发频率。

### B05 [P2] snapshot 不清除旧的 __bb_snap_ref 属性，click/fill @ref 可能命中上一次快照遗留的其它元素

- **位置**：`internal/browser/snapshot.go:81`
- **触发条件**：同一页面上先后两次 snapshot，且第二次没有遍历到第一次编号过的某些元素：例如先全页 snapshot 再 `snapshot --scope`；或两次之间有元素被隐藏、被 --compact/--depth 剪掉。随后 `click @N` / `fill @N`。
- **实际行为**：每次 snapshot 从 1 重新编号并 setAttribute(__bb_snap_ref)，但从不移除未遍历元素上的旧值；daemon 把 ref 展开为 `[__bb_snap_ref="N"]`（internal/daemon/v1.go:768-771、1190-1193），按文档序取第一个匹配。复现：第二次快照声明 @1 = “Submit order”，选择器实际解析到上一次遗留 "1" 的 “Delete account” 按钮；隐藏横幅场景解析到已隐藏的旧元素。
- **期望行为**：最新一次 snapshot 返回的每个 @ref 必须唯一指向该次快照中的那个元素；旧快照的 ref 应失效（报错）而不是指向别的元素。
- **影响**：agent 按最新快照操作却点击/填写了别的元素（可能是破坏性按钮或把文本填进错误输入框）；或命中不可见旧元素导致 click 超时 60s。
- **根因**：快照脚本只增量写属性，没有在开始时清理文档内已有的 __bb_snap_ref，也没有快照代次区分。
- **最小修复方向**：在 snapshotStub 开头 `document.querySelectorAll("[__bb_snap_ref]")` 逐个 removeAttribute；或把代次编进属性值/属性名（如 `g3-12`），refs 映射与 click 解析同步使用。
- **回归验收**：用 repro/snapshot_stale_refs.js 同款 fake DOM（或 headless Chrome e2e）断言：全页→scope、元素隐藏、pruneEmpty、maxDepth 四种组合下，refs[N] 选择器在文档内恰好匹配 1 个元素且为快照文本中 @N 对应的元素；旧 ref 不再匹配任何元素。
- **证据**：[repro-node.log](repro-node.log)（snapshot_stale_refs.js 两个场景 FAIL）；[repro/snapshot_stale_refs.js](repro/snapshot_stale_refs.js)
- **验证方式**：用 node 在最小 fake DOM 上执行从 main 源码原样提取的 snapshotStub；未在真实 Chrome 验证（querySelector 文档序语义为标准行为）。

### B06 [P2] Pool 的 tab_new 与 Health/Ready/Live 共用同一个轮询计数器，顺序调用方的 tab 会全部落到同一个 daemon

- **位置**：`pkg/daemonclient/pool.go:428`
- **触发条件**：顺序调用方在每次 pool.TabNew 之间调用与后端数成整数倍关系的其它未绑定操作，例如 2 个后端时每次 TabNew 前先 pool.Health()；3 个后端时 Live+Ready+TabNew。
- **实际行为**：order()（pool.go:426-442）对所有未绑定操作共用 p.rr；“最少在途”只统计进行中的 HTTP 调用，顺序调用时恒为 0，退化为纯轮询，健康检查吃掉了交替名额。复现：2 后端 20 次 TabNew 分布 [0 20]；3 后端 30 次分布 [0 0 30]。
- **期望行为**：AGENTS.md / README 承诺的 even spread：新 tab 在健康后端间均匀分布，不受健康探测等其它调用影响。
- **影响**：多 daemon 池退化为单 daemon：一个 Chrome 承担全部标签页，其余空闲；“先探活再开 tab”正是调用方最常见的写法。
- **根因**：tab_new 的分配游标与健康检查/其它未绑定调用共享，且负载度量是瞬时在途请求数而非各后端已绑定 tab 数。
- **最小修复方向**：为 tab_new 使用独立计数器，或按 p.tabs 中各后端已绑定 tab 数选最少者（并列再轮询）；Health/Ready/Live 不推进该游标。
- **回归验收**：新增 pool 测试：2 后端交替 Health+TabNew 20 轮分布为 [10 10]；3 后端 Live+Ready+TabNew 30 轮为 [10 10 10]；原 TestPool_tabNewEvenDistribution 仍通过；一个后端 503 时其余健康后端差值 ≤1。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_SpreadWithInterleavedHealth / …Health3 FAIL）；[repro/client_audit_test.go](repro/client_audit_test.go)
- **验证方式**：httptest fake daemon 复现。

### B07 [P2] `bb-browser run` 在 adapter 返回 { error } 时退出码为 0

- **位置**：`cmd/bb-browser/main.go:1009`
- **触发条件**：adapter 按文档约定返回 `{ error: "HTTP 401", hint: "…" }`（skills/bb-browser-adapter/SKILL.md:176 要求错误都用该形式返回）。
- **实际行为**：printSiteResult（main.go:1017-1035）只向 stderr 打印 `[error] …` 后返回，RunE 返回 nil，进程退出码 0，stdout 为空。
- **期望行为**：adapter 报告错误时命令以非零退出码结束（stderr 输出保持不变），使脚本/agent 能用退出码判断失败。
- **影响**：`bb-browser run … && 下一步`、CI、上层 agent 都会把失败当成功继续执行，且 stdout 为空时难以与“无数据”区分。
- **根因**：printSiteResult 没有返回值，run 的 RunE 在其后无条件 return nil。
- **最小修复方向**：让 printSiteResult 返回 error（或 bool），adapter 结果含非空 error 字段时 RunE 返回错误（可用 SilenceUsage 避免再打印 usage）；--json 模式保持输出包络，退出码建议同样非零或在文档中明确。
- **回归验收**：CLI 测试：fake daemon 的 eval 结果为 {"error":"HTTP 401","hint":"please log in"} 时 root.Execute() 返回非 nil、stderr 含 `[error] HTTP 401` 与登录提示；结果为普通对象时返回 nil 且 stdout 为缩进 JSON。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_RunAdapterErrorMustFailTheCommand FAIL）；[repro/cli_audit_test.go](repro/cli_audit_test.go)
- **验证方式**：进程内执行 newRootCmd() + httptest fake daemon 复现。

### B08 [P2] systemd unit 没有 [Install] 段，文档里的 `systemctl --user enable` 与开机自启不生效

- **位置**：`deploy/systemd/bb-daemon.service:1`
- **触发条件**：按 deploy/systemd/README.md:23 执行 `systemctl --user enable --now bb-daemon.service`，并按 :33-39 启用 linger 期望重启后自动运行。
- **实际行为**：unit 文件只有 [Service] 段（无 [Unit]、[Install]、WantedBy=）。enable 不创建任何 wants 链接（systemd 提示 unit 无安装配置），--now 只启动当次；注销/重启后服务不会再起来。
- **期望行为**：enable 后服务随用户会话（配合 linger 则随开机）自动启动，符合 README “Run on boot”。
- **影响**：按官方部署文档操作的机器重启后 daemon 不在运行，依赖它的爬虫/agent 全部失败，且无任何报错提示。
- **根因**：unit 文件缺少 [Install] WantedBy=default.target。
- **最小修复方向**：补充 `[Unit] Description=…`（可加 After=network-online.target）与 `[Install] WantedBy=default.target`。
- **回归验收**：静态检查测试（Go test 读取 unit 文件）断言存在 [Install] 且 WantedBy=default.target；在 Linux 环境用 `systemd-analyze --user verify deploy/systemd/bb-daemon.service` 无 “no installation config” 警告，`systemctl --user is-enabled` 为 enabled。
- **证据**：[static-deploy-docs.log](static-deploy-docs.log)（grep 显示 unit 仅有 [Service]）
- **验证方式**：静态确认（文件内容 + systemd 既定语义）；本机为 macOS，未实机运行 systemd。

### B09 [P2] 官方 compose 示例的 named volume 对镜像内 nonroot 用户不可写，daemon 启动即退出

- **位置**：`deploy/docker-compose.example.yml:25`
- **触发条件**：用仓库 Dockerfile 构建镜像，按 deploy/docker-compose.example.yml（或 README.md:124-140 的同款示例）启动：BB_BROWSER_STATE_DIR=/var/lib/bb-daemon + `bb-state:/var/lib/bb-daemon`。
- **实际行为**：镜像基于 distroless/static:nonroot、USER nonroot（Dockerfile:1,4），镜像内不存在 /var/lib/bb-daemon；Docker 在该路径新建的 named volume 归 root:root 0755，uid 65532 无法写入。store.Open 的 MkdirAll/dirWritable 失败（internal/store/store.go:105-110），NewServer 返回错误，进程打印 `server: rpc log store: state dir … is not writable` 并以 1 退出。README.md:143 还明确写“或使用 named volume”即可。
- **期望行为**：示例 compose 开箱可启动，状态目录可写并持久化。
- **影响**：按官方示例部署的容器反复重启/CrashLoop，无法提供服务。
- **根因**：镜像未预建归 nonroot 所有的状态目录，示例把卷挂在镜像中不存在的路径上。
- **最小修复方向**：在 Dockerfile 中预建并 chown 65532 的状态目录（多阶段 `COPY --chown=65532:65532` 一个空目录到 /var/lib/bb-daemon），或把示例改到 /home/nonroot 下；同步修正 README.md:143。
- **回归验收**：构建镜像后 `docker compose -f deploy/docker-compose.example.yml up`（配一个可达的 Chrome 或仅看启动日志）不出现 `rpc log store` 错误；`docker run --rm -v bb-state:/var/lib/bb-daemon --entrypoint /usr/local/bin/bb-daemon <image> --debugger-url 127.0.0.1:1` 的失败原因是 cdp attach 而不是 state dir；卷内出现 rpc.jsonl 且属主为 65532。
- **证据**：[static-deploy-docs.log](static-deploy-docs.log)（Dockerfile/compose/README 行号 + 本地二进制在不可写 state dir 上的退出输出）
- **验证方式**：代码侧已用本地构建的 bb-daemon 在 0555 目录上实测退出（exit=1）；Docker 卷属主行为依据 Docker 既定语义，未实机运行 Docker。

### B10 [P2] 未知/过期 tab id 的请求在 tabCDPLocks 中留下永不回收的条目（key 由客户端控制）

- **位置**：`internal/daemon/rpc_connections.go:193`
- **触发条件**：任意带 tab 参数的 RPC（goto/eval/click/network/tab_close…）使用不存在或已关闭的 tab id；每个不同的字符串一条。客户端在 tab 被空闲回收后继续用旧 id 重试即会触发。
- **实际行为**：lockTab 在解析 tab 之前按原始字符串建锁条目（rpc_connections.go:193-199）；releaseTabLockRef 只有 forgotten=true 才删除（:219-226），而 forgotten 仅在已注册 target 被移除时设置。未知 id 的条目永久保留：300 次请求后 map 中有 300 条。key 最长可接近 MaxBodyBytes（默认 1 MiB）。
- **期望行为**：锁条目在引用计数归零且对应 tab 未注册时立即回收；未知 tab 的请求不留下任何状态。
- **影响**：长期运行的 daemon 内存随陈旧/错误 tab id 请求单调增长；未配置 token 时任何能访问端口的调用方可用大 key 快速耗尽内存（每请求最多约 1 MiB 常驻）。
- **根因**：锁以客户端传入字符串为 key 且先建后验；回收条件只覆盖“曾注册后被移除”的 tab。
- **最小修复方向**：先用 s.tabs.Lookup（必要时一次同步）确认 tab 存在再建锁；或在 releaseTabLockRef 中当 refs==0 且该短 id 当前未注册时删除条目。
- **回归验收**：daemon 测试：对 300 个不存在的 tab id 发 goto/eval/network/tab_close 后 len(tabCDPLocks)==0；对已关闭 tab 的旧 id 重试后同样为 0；既有 TestLockTabHonorsContextDeadline 与并发串行化测试保持通过。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_UnknownTabRequestsMustNotLeakLockEntries FAIL）；[repro/daemon_audit_test.go](repro/daemon_audit_test.go)
- **验证方式**：fake conn 白盒复现。

### B11 [P3] rpc.jsonl 尾部不是换行时继续追加会粘行，下次启动 replay 将该行及其后的全部记录截断丢弃

- **位置**：`internal/store/store.go:266`
- **触发条件**：日志最后一行不是以 \n 结束：进程在 JSON 已落盘但换行未写入时被杀/断电，或运行中 Write 因磁盘满只写入了部分字节后空间恢复、后续追加成功。
- **实际行为**：Open 的 replayLog 接受无换行的合法末行并把文件定位到末尾；AppendRPC（store.go:266）直接写 `记录+\n`，与上一行粘成 `{…}{…}`。下一次启动时该行解析失败，replayLog 在 goodEnd 处 Truncate（store.go:164-174），粘行以及其后的所有记录被删除。复现：两个受管 tab 重启后全部丢失，rpc.jsonl 被截为 0 字节。
- **期望行为**：追加前保证位于行边界（必要时先补换行）；单行损坏只丢该行，不应连带丢弃其后的合法记录与最近的受管 tab 状态。
- **影响**：受管 tab 的恢复信息丢失 → 重启后这些 daemon 创建的 tab 不再被空闲回收（标签页泄漏）；审计日志被静默截断。
- **根因**：追加路径假设文件总以换行结束；replay 遇到坏行采取“截断其后全部”的策略且无逐行恢复。
- **最小修复方向**：Open 时若文件非空且末字节不是 \n，先补写换行；AppendRPC 写入失败/短写后标记“需要先写换行”；replay 对中间坏行改为跳过并保留后续合法行（仅对最后一行做截断）。
- **回归验收**：store 测试：预置无结尾换行的合法记录 → Open → AppendRPC(tab_new) → Close → 再 Open，两个 tab 均在 ReplayManagedTabActivity 中且文件每行可独立解析；预置“坏行+合法行”时合法行不丢；既有“截断的最后一行被丢弃”测试保持通过。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_AppendAfterUnterminatedTailMustNotLoseRecords FAIL）；[repro/store_audit_test.go](repro/store_audit_test.go)
- **验证方式**：单元级复现；真实崩溃/磁盘满时序未模拟。

### B12 [P3] --state-dir - 内存模式下每条 RPC 记录被永久保存在 memLogs，内存无界增长

- **位置**：`internal/store/store.go:260`
- **触发条件**：以 `--state-dir -`（BB_BROWSER_STATE_DIR=-）运行 daemon 并持续接收 RPC（含 network/console 轮询）；HOME 不可得导致默认目录解析为空时也会静默进入该模式（internal/daemon/config.go:147-150）。
- **实际行为**：AppendRPC 在 inMemory 分支把每条 LogRecord 追加到 s.memLogs（store.go:259-262）；生产代码没有任何读取或裁剪 memLogs 的地方（全仓 grep 仅此一处写入）。5000 次调用后保留 5000 条。
- **期望行为**：内存模式不保留历史记录（managed 映射已单独维护），或只保留有上限的环形缓冲。
- **影响**：长期运行的内存模式 daemon RSS 持续上涨直至 OOM（每条记录含压缩后的请求体，数百字节；1 RPC/s 约每天数十 MB）。
- **根因**：遗留的只写调试缓冲，没有容量上限也没有读者。
- **最小修复方向**：删除 memLogs（测试需要时改用带上限的 ring 或测试专用 hook）。
- **回归验收**：store 测试：内存模式连续 AppendRPC 10 万条后保留记录数 ≤ 固定上限（或为 0），ReplayManagedTabActivity 行为不变；用 testing.AllocsPerRun/堆快照确认不随调用数线性增长。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_InMemoryModeMustNotRetainEveryRecord FAIL）；[repro/store_audit_test.go](repro/store_audit_test.go)
- **验证方式**：单元级复现。

### B13 [P3] 含完整请求体的 rpc.jsonl 以 0644 创建、状态目录 0755，本机其他用户可读取 fill 文本与 fetch 头中的凭据

- **位置**：`internal/store/store.go:112`
- **触发条件**：默认配置运行 daemon（umask 022），客户端调用 fill（密码等）、fetch（headers 含 Authorization）、network_route（mock body）、eval。
- **实际行为**：审计行保存请求体（仅对超过 200 字符的字符串做中段截断，internal/store/compact_body.go），文件用 os.OpenFile(…, 0o644) 创建（store.go:112，轮转 :300 同），目录 MkdirAll 0o755（:105）；轮转备份 rpc.jsonl.1-3 权限相同。复现中日志含 `hunter2-secret` 与 `Bearer sk-live-123`，文件 mode=644、目录 mode=755。
- **期望行为**：状态目录 0700、日志及轮转备份 0600；（可选）对 fill.text、fetch.headers/body 等敏感字段脱敏。
- **影响**：多用户主机/共享容器卷上，其他本地账户可读出通过 daemon 输入的密码、会话令牌和 API 密钥；API token 鉴权保护了 RPC 入口却没有保护落盘副本。
- **根因**：把审计日志当普通状态文件创建，未考虑其内容包含调用方提交的秘密。
- **最小修复方向**：MkdirAll 用 0o700（已存在目录不放宽）、OpenFile 用 0o600，轮转后文件同样 0o600；启动时对既有文件做一次 Chmod 收紧。
- **回归验收**：store 测试（umask 022）：新建 state dir 权限为 0700，rpc.jsonl 与轮转产生的 .1 备份为 0600；既有 0644 文件在 Open 后被收紧；写入/replay/轮转既有测试保持通过。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_RPCLogMustNotBeWorldReadable FAIL）；[repro/store_audit_test.go](repro/store_audit_test.go)
- **验证方式**：单元级复现（darwin，umask 022）。

### B14 [P3] 短 tab id 在后缀碰撞时取决于登记顺序，重启后同一短 id 会指向另一个 tab

- **位置**：`internal/state/tab_registry.go:172`
- **触发条件**：同时存在两个 CDP target id 末 4 位 hex 相同的 page target（50 个 tab 时约 1.9%，200 个 tab 时约 26%），daemon 重启（或注册表经全量同步重建）。
- **实际行为**：assignShortLocked 取“最短的未被占用后缀”（tab_registry.go:172-179），结果依赖登记先后：运行期按创建顺序 A=abcd、B=1abcd；重启后 SyncPageTargetsDetailed 按 target id 排序登记，得到 A=0abcd、B=abcd。旧短 id abcd 现在解析到 B。
- **期望行为**：AGENTS.md 声明短 id 由 CDP target id 确定性派生、跨重启稳定：同一 target 的短 id 不应随登记顺序变化，旧短 id 不应解析到别的 target。
- **影响**：客户端/Pool 持有的短 id 在重启后静默指向另一个标签页（操作串台）；rpc.jsonl 重放把受管状态与最后活动时间记到错误的 tab 上，可能误关未受管的 tab、漏关受管 tab。
- **根因**：短 id 不是 target id 的纯函数，碰撞消解依赖当前已占用集合。
- **最小修复方向**：让短 id 成为 target id 的纯函数（例如固定取更长的后缀/完整 id 的哈希前缀，碰撞时对所有碰撞方统一加长），或在持久化记录里同时保存完整 target id 并在恢复时校验。
- **回归验收**：state 测试：对两个末 4 位相同的 target，以两种登记顺序（逐个 Register 与排序后的 Sync）得到的短 id 完全一致；重建注册表后 Lookup(旧短 id) 仍返回原 target；无碰撞时短 id 仍为 4 位。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_ShortIDsMustBeStableAcrossRestartOnSuffixCollision FAIL）；[repro/state_audit_test.go](repro/state_audit_test.go)
- **验证方式**：单元级确定性复现。

### B15 [P3] tab 关闭后 observer 写协程仍可把已出队事件写回，观测 ring 被重建且永不清理（违反 INV-6）

- **位置**：`internal/daemon/tab_idle.go:49`
- **触发条件**：对某 tab 启用过 network/console/errors 观测，tab 关闭（tab_close、空闲回收、用户关闭）时 observer 队列中仍有事件或写协程正在写入——关闭带有在途请求的页面时必然产生一批 loadingFailed 事件。
- **实际行为**：clearRemovedTarget 先 obsStore.ClearTarget（tab_idle.go:49）后才 ForgetTarget 取消 observer（:54）；runWriter（internal/browser/observer.go:267-287）在两步之间或与取消并发时继续调用 Record*，TabObsStore.Push* 通过 getOrCreateLocked（internal/state/obs_store.go:47-58）为已关闭的 target 重新建 ring。该 target 已不在注册表中，后续同步不会再清它。
- **期望行为**：INV-6：tab 关闭后其缓冲被清空且不再接受写入。
- **影响**：每个这样关闭的 tab 残留一组 ring（最多 500+200+100 条事件）直到进程退出；长期运行、频繁开关 tab 的 daemon 内存缓慢增长。
- **根因**：清理顺序是先清缓冲后停生产者，且 store 的 Push 对未知 target 自动建桶，没有“已关闭”墓碑。
- **最小修复方向**：调整为先 ForgetTarget（停止 observer 并等待/丢弃其队列）再 ClearTarget；并让 Push* 仅在 target 已登记（EnsureObservation 时显式创建）时写入，ClearTarget 后拒绝写入。
- **回归验收**：barrier 测试：writer 已取出事件并停在 Record 之前，执行与 clearRemovedTarget 相同的清理序列后放行，QueryNetwork/Console/Errors 对该 target 返回 0 条且 store 内无该 target 的桶；-race 下反复开关带事件流的 tab 后 targets map 大小回到 0。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_ObservationRingMustStayClearedAfterTabClose FAIL）；[repro/browser_audit_test.go](repro/browser_audit_test.go)
- **验证方式**：barrier 复现（手工构造 tabObserver + 真实 TabObsStore，按 tab_idle.go:49→54 的顺序调用）；未用真实 Chrome。

### B16 [P3] network_route 同步 CDP 失败后规则仍留在内存表中，没有回滚

- **位置**：`internal/browser/network_routes.go:104`
- **触发条件**：network_route 调用期间 CDP 同步失败：tab attach 失败、Fetch.enable 报错/超时、或客户端断开导致请求 context 取消。
- **实际行为**：AppendNetworkRouteContext 先把规则写入 rs.rules（network_routes.go:103-105）再 syncRoutes；失败时 RPC 返回 `network_route failed`，但规则保留：NetworkRouteCount 为 1，客户端重试会得到重复规则，之后任一次成功同步会把这条“失败”的规则一并下发生效。RemoveNetworkRoutesContext（:122-141）同样先改表后同步；规则清空分支（:170-176）即使 Fetch.disable 失败也会 stopListening。
- **期望行为**：RPC 返回错误时路由表保持调用前的状态（或明确标记未生效），routes 计数与浏览器实际拦截配置一致。
- **影响**：调用方看到失败却在之后被静默拦截/mock；routes 计数虚高；删除路径失败时可能留下浏览器侧仍启用 Fetch 而本地已无监听者的状态（请求被挂起，需真实 Chrome 确认）。
- **根因**：先改内存状态后做 CDP 同步，失败路径没有恢复旧值。
- **最小修复方向**：syncRoutes 失败时恢复调用前的规则切片（Append/Remove/Set 三处）；规则清空分支仅在 Fetch.disable 成功后才停止监听。
- **回归验收**：browser 测试：用已取消的 context 调 AppendNetworkRouteContext 返回错误后 NetworkRouteCount==0；注入 routeCDP/同步失败时 Remove 后规则数不变；成功路径既有测试保持通过。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_FailedNetworkRouteMustNotLeaveRuleRegistered FAIL）；[repro/browser_audit_test.go](repro/browser_audit_test.go)
- **验证方式**：单元级复现（请求 context 取消导致同步失败）；浏览器侧挂起后果未用真实 Chrome 验证。

### B17 [P3] adapter 包装器的括号扫描把正则字面量里的 // 当行注释，合法 adapter 被截断成永不执行的脚本

- **位置**：`internal/site/run.go:85`
- **触发条件**：adapter 采用文档推荐的匿名 `async function(args) {…}` 形式，函数体内含正则字面量且其中出现 `//`（如 `/^https?:\/\//`），并且同一行后面有未在本行闭合的 `{`（最常见的多行 if 写法）。带引号或花括号的正则、含嵌套 `${}` 的模板字符串同理。
- **实际行为**：matchingJSBrace（run.go:84-91、104-120）不识别正则字面量：把 `\/\//` 里的 `//` 当成行注释跳到行尾，漏计该行的 `{`，于是提前匹配到函数体中间的 `}`。rewriteAnonymousAsyncAdapter 在该处插入 `}; return await __bb_run(__args);`，生成的脚本语法合法但外层函数只定义 __bb_run 不调用它：结果为 undefined，fetch 调用 0 次。
- **期望行为**：合法的 adapter 源码被完整包装并执行，返回其结果 {"ok":true}；无法安全改写时应报错而不是生成语义不同的脚本。
- **影响**：含常见 URL 正则的 adapter 静默返回 null/空结果（CLI 退出码 0），极难排查；也可能把函数后半段代码暴露在错误的作用域里执行。
- **根因**：手写的 JS 词法扫描只处理了字符串/注释，没有处理正则字面量与模板插值。
- **最小修复方向**：避免做括号匹配：把 adapter 源码整体作为表达式包装，例如 `const __bb_run = (<源码去掉 export default 与尾部分号>); return await __bb_run(__args);`（匿名 async function/箭头函数均适用），仅在源码不是函数表达式时保留旧路径。
- **回归验收**：site 测试：对含 `/^https?:\/\//` 多行 if、含 `/[{}"]/`、含嵌套模板字符串的匿名 async adapter，RunScript 输出经 `node --check` 通过且在 stub fetch 下执行返回 adapter 的真实结果；现有 adapter 包装测试保持通过。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_AdapterWithRegexLiteralMustWrapWholeFunction FAIL）；[repro-node.log](repro-node.log)（node 执行包装结果：adapter result = undefined）；[repro/site_audit_test.go](repro/site_audit_test.go)；[repro/adapter_wrapped_run.js](repro/adapter_wrapped_run.js)
- **验证方式**：Go 单元测试 + node 执行包装后的脚本；未在真实页面执行。

### B18 [P3] `screenshot --format jpeg` 未指定路径时把 JPEG 数据写入 screenshot-<ts>.png

- **位置**：`cmd/bb-browser/main.go:521`
- **触发条件**：`bb-browser screenshot --format jpeg`（或 jpg），不带位置参数和 --output。
- **实际行为**：默认文件名固定为 `screenshot-%d.png`（main.go:520-522），与实际格式无关；复现中文件 screenshot-<ts>.png 的内容是 JPEG（FF D8 魔数）。
- **期望行为**：默认文件名的扩展名与实际 MIME 一致（.jpg/.jpeg）；可直接使用 RPC 返回的 mime 字段决定。
- **影响**：下游按扩展名判断类型的工具（上传、图像库、agent 的多模态输入）解析失败或误判。
- **根因**：默认文件名硬编码 .png，未参考 --format 或响应的 mime。
- **最小修复方向**：解析响应中的 mime（image/jpeg → .jpg，否则 .png）生成默认文件名。
- **回归验收**：CLI 测试：fake daemon 返回 mime=image/jpeg 时生成的默认文件以 .jpg 结尾且内容为 JPEG；png 时仍为 .png；显式路径/--output 不被改写。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_ScreenshotJPEGDefaultNameMustNotBePNG FAIL）；[repro/cli_audit_test.go](repro/cli_audit_test.go)
- **验证方式**：进程内执行 newRootCmd() + httptest fake daemon 复现。

### B19 [P3] Pool.TabList/TabFocus 顺序轮询后端且无单后端超时，一个挂起的 daemon 使整个调用失败

- **位置**：`pkg/daemonclient/pool.go:324`
- **触发条件**：池中靠前的某个后端接受连接但不应答（主机假死、网络黑洞、daemon 卡在长操作），调用方带截止时间调用 pool.TabList / pool.TabFocus。
- **实际行为**：callTabList（pool.go:324-343）与 callTabFocus（:361-）在调用方 context 上依次请求各后端；挂起的后端耗尽全部截止时间后，下一轮循环的 ctx.Err() 直接返回 `context deadline exceeded`，健康后端从未被询问（复现中后端 B 的 tab_list 调用数为 0）。没有截止时间时则无限期阻塞（默认 http.Client 无超时）。
- **期望行为**：AGENTS.md：TabList 查询每个后端，“a down backend is skipped”，仅全部失败才返回 *AllFailedError；一个无响应后端不应阻止返回其余后端的 tab。
- **影响**：单个 daemon 假死即让池级 tab 列表/焦点查询全部超时，依赖它做巡检或选 tab 的调用方整体不可用。
- **根因**：多后端聚合是串行的，并共享调用方的整个超时预算。
- **最小修复方向**：并行查询各后端（或为每个后端划分子超时），收集在截止前返回的结果；超时的后端计入 lastErrs 并被跳过。
- **回归验收**：pool 测试：后端 0 永不应答、后端 1 正常，500ms 截止的 TabList 返回后端 1 的 tab 且 err==nil；TabFocus 返回后端 1 的焦点；全部挂起时返回 *AllFailedError 或 ctx 错误；合并顺序仍按构造顺序。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_TabListHungBackend FAIL）；[repro/client_audit_test.go](repro/client_audit_test.go)
- **验证方式**：httptest 挂起后端复现。

### B20 [P3] Pool 通过 tab_list/tab_focus 观察到的 tab 绑定只增不减，已消失的 tab 永久留在映射中

- **位置**：`pkg/daemonclient/pool.go:605`
- **触发条件**：长生命周期的 Pool 周期性调用 TabList/TabFocus，而 tab 由池外途径关闭（空闲回收、用户手动关闭、其它客户端 tab_close）。
- **实际行为**：bindObservedTabs（pool.go:605-613）把每次列表中出现的 tab 写入 p.tabs，从不删除该后端本次未再列出的 tab；绑定只会在本池成功 tab_close 或调用方显式 ForgetTab 时移除。复现：5 次列表各只含 1 个不同 tab，映射里留下 5 条，`ClientForTab("0:t0")` 仍为 true。
- **期望行为**：某后端的 tab_list 成功返回即是该后端的权威集合：未再出现的绑定应被移除，之后对这些 id 的调用返回 *UnknownTabError。
- **影响**：映射随浏览器 tab 流转无界增长；陈旧 id 的调用被发往 daemon 才得到 “unknown tab id” 的 RPC 错误，而不是本地的 *UnknownTabError，调用方的错误分支判断失效。
- **根因**：观察式绑定只有添加路径，没有与后端权威列表对账的删除路径。
- **最小修复方向**：在单后端 tab_list 成功后，删除该后端下不在本次结果中的绑定；与该后端并发的 tab_new 绑定需要用代次或短锁保护，避免把刚创建的 tab 误删。
- **回归验收**：pool 测试：连续 5 次列表各含 1 个不同 tab 后映射大小为 1，旧 id 的 ClientForTab 为 false、调用返回 *UnknownTabError；并发 TabNew 与 TabList（-race）下新建 tab 不被误删；后端失败的那次列表不触发清理。
- **证据**：[repro-go.log](repro-go.log)（TestAudit_TabListDoesNotPruneStale FAIL）；[repro/client_audit_test.go](repro/client_audit_test.go)
- **验证方式**：httptest fake daemon 复现。

### B21 [P3] CLI 与 adapter 文档宣称 run 有约 180s 预算（eval/snapshot/fetch 为 120s），daemon 对每个 RPC 固定 60s 截止

- **位置**：`internal/daemon/v1.go:86`
- **触发条件**：adapter 或 eval/fetch/snapshot 单次执行超过 60 秒（翻页抓取、等待慢接口）。
- **实际行为**：CLI 为 run 设 180s、eval/snapshot/fetch/screenshot/goto 设 120s 的客户端超时（cmd/bb-browser/main.go:947、324、293、392 等），skills/bb-browser-adapter/SKILL.md:18、:230 与 references/patterns.md:172、:225 写明“单次 run 超时约 180 秒”。但 daemon 在 v1.go:86 对所有方法套 timeout.Operation=60s（internal/timeout/timeout.go:6），runCDPWithContext 再套一次（internal/browser/session.go:159）。超过 60s 的脚本在约 60s 得到 `eval failed`（context deadline exceeded），并触发一次额外的 watchdog 探测。
- **期望行为**：文档、CLI 超时与 daemon 实际截止一致：要么 daemon 对 eval/fetch 等长操作提供与 CLI 匹配的预算，要么文档与 CLI 统一为 60s。
- **影响**：按文档把任务规划在 60–180s 内的 adapter 必然失败，且错误信息是泛化的 eval failed；CLI 的 120/180s 超时永远不会生效，误导排障。
- **根因**：daemon 端单一全局操作超时与 CLI/文档各自声明的预算没有对齐。
- **最小修复方向**：二选一：把文档与 CLI 超时改为 60s（并在超时错误的 hint 中说明上限）；或让 daemon 按方法设置上限（eval/fetch 180s）并同步调整 http.Server WriteTimeout。
- **回归验收**：增加一致性测试：daemon 对 eval 的生效截止 ≥ CLI run 的超时减去余量（或文档/CLI 常量等于 timeout.Operation）；用 fake conn 让 Eval 阻塞到 context 结束，断言返回时间与声明的上限一致；文档中的数字由同一常量来源校验（grep 测试）。
- **证据**：[static-deploy-docs.log](static-deploy-docs.log)（“超时相关”一节的行号摘录）
- **验证方式**：静态确认（常量与调用链）；未实际等待 60s 运行。
## 4. 待确认风险（不建 bug 任务）

以下各项未能在本次约束下证实（多数需要真实 Chrome / 特定故障时序），或属于设计取舍，**明确不作为 bug 任务**；manager 如需跟进应先补验证。

| # | 位置 | 说明 | 未确认原因 |
|---|---|---|---|
| R1 | `internal/browser/network_routes.go:192-201` vs `:398-421` | 规则模式原样传给 `Fetch.enable`，而本地 `urlMatch` 另外支持裸主机 / `host/path` 形式；按 CDP 通配语义裸主机模式不会匹配完整 URL，规则可能永不触发 | 需真实 Chrome 确认 CDP 匹配行为；文档示例只用了 `*…*` 形式 |
| R2 | `internal/browser/network_routes.go:359-380`、`internal/daemon/v1.go:1005` | `status` 未校验；FulfillRequest/FailRequest 失败时错误被丢弃且没有 continue 兜底，被暂停的请求可能永久挂起 | 需真实 Chrome 确认非法状态码的报错行为 |
| R3 | `internal/browser/network_routes.go:170-176` | 清空规则时即使 `Fetch.disable` 失败也停止监听，浏览器侧若仍启用 Fetch 则请求无人处理（与 B16 相邻，修 B16 时一并确认） | 需真实 Chrome |
| R4 | `internal/browser/session.go:642-643`、`observer.go:117` | tab/observer 首次 attach 的 `chromedp.Run` 没有超时；目标无响应时该条目永不 ready，之后对该 tab 的每个请求都等满 60s | 需可卡住 attach 的真实/伪造 CDP 端 |
| R5 | `internal/daemon/browser_reconnect.go:63`、`internal/browser/session.go:281` | 启动期 `Connect` 使用 `WithoutCancel`，attach 卡住时 SIGTERM 被 NotifyContext 吞掉，进程只能被 SIGKILL | 同上 |
| R6 | `internal/store/store.go:97` | `seq`/`cursor` 以纳秒时间戳为种子（≈1.8e18），超过 2^53；JS/jq 等非 Go 客户端回传 `since` 时会丢精度，可能漏读事件 | 设计取舍（Go 客户端不受影响）；需产品确认是否支持非 Go 客户端 |
| R7 | `internal/browser/session.go:678-686`、`pkg/daemonclient/pool.go:259-264` | `tab_new` 在 Chrome 已建 tab 但响应丢失/请求取消时，daemon 侧留下未受管 tab；Pool 会在另一个后端再建一个 | AGENTS.md 明确允许未绑定 `tab_new` 故障转移；是否收紧需决策 |
| R8 | `internal/daemon/v1.go:395-408`、`cmd/bb-browser/main.go:1201-1204` | 无焦点时 `tab_list.tab` 回退为排序后的第一个 tab，`bb-browser close` 等不带 `--tab` 的破坏性命令会作用到任意用户 tab | 行为与 `--tab` 帮助文本的字面描述有出入，但回退是有意设计；需产品确认 |
| R9 | `pkg/daemonclient/client.go:231-244` | `Client.Call` 接受 `{}`、`result:null`、id 不匹配的 200 响应为成功；Pool 的 `tab_close` 会据此解绑 | 仅在中间代理返回非 JSON-RPC 的 200 时出现；README 表述含糊，属健壮性加固 |
| R10 | `internal/store/store.go:300-303`、`:270-274` | 轮转后重新打开失败会使 `logFile` 永久为 nil（此后所有审计写入失败）；受管快照本身超过上限时每次追加都会轮转并冲掉备份 | 需故障注入；与 B03 修复后影响面会变小 |
| R11 | `internal/browser/network_routes.go:423-451` | `wildcardMatch` 递归回溯，对多 `*` 模式与长 URL 呈指数时间，会卡住该 tab 的拦截 worker | 模式由受信调用方提供；未测量 |
| R12 | `internal/browser/fetch.go:51` | `fetch` 的 `headers` 字符串原样拼入页面脚本（可注入 JS） | 调用方本就拥有 `eval` 权限，不构成越权；仅记录 |
| R13 | `deploy/systemd/bb-daemon.service:15`、`internal/daemon/server.go:266-277` | `TimeoutStopSec=10s` 小于优雅关停预算（15s + 60s），有在途长 RPC 时会被 SIGKILL | 未实机验证 |
| R14 | `pkg/daemonclient/pool.go:437-441`、`:259-264`、`:472` | 一个后端宕机时其环上后继承担双倍负载；确定性的应用错误会在所有后端重放；`abandonCreated` 无超时 | 前者与 B06 同一修复方向；后两者属策略选择 |
| R15 | `pkg/daemonclient/README.md:19` 等 | 示例端口 `8080` 与 daemon 默认 `8787` 不一致 | 文档示例，不影响行为 |
| R16 | `cmd/bb-browser/launch_windows.go` | Windows 版 `bb-browser` 未能离线交叉编译（模块缓存缺 `mousetrap`） | 环境限制 |
| R17 | `cmd/bb-browser/main.go:373`、`:425-467` | `fill` 会 TrimSpace 文本（无法填入首尾空白）；`fetch` 在 `ok:false`（含网络错误）时退出码仍为 0 | 语义是否为缺陷需产品确认 |

## 5. 测试命令与结果

环境：Go 1.27.4（本机已有的用户级安装；`go.mod` 要求 1.26，向上兼容），darwin/arm64，`GOPROXY=off`、`GOTOOLCHAIN=local`，`HOME`/`TMPDIR`/`GOCACHE` 均指向审查专用临时目录，只读使用既有模块缓存。

| # | 命令 | 结果 | 分类 | 证据 |
|---|---|---|---|---|
| 1 | `test -z "$(gofmt -l .)"` | 通过（无输出） | 配置测试 | [configured-tests.log](configured-tests.log) |
| 2 | `go vet ./...` | 通过（exit 0） | 配置测试 | 同上 |
| 3 | `go test -race -count=1 ./...` | 全部 `ok`（8 个包；`internal/timeout`、`pkg/protocol` 无测试文件） | 配置测试；**无现有测试失败** | 同上 |
| 4 | `go test -race -count=1 -overlay <tmp>/overlay.json -run TestAudit_ -v ./internal/... ./cmd/bb-browser/ ./pkg/daemonclient/` | 18 个诊断测试全部 **FAIL（预期）** | 新增诊断的预期失败 = 缺陷证据 | [repro-go.log](repro-go.log)、[repro/](repro/) |
| 5 | `node repro/snapshot_stale_refs.js internal/browser/snapshot.go` | 2 个场景 FAIL（预期），exit 1 | 新增诊断的预期失败 | [repro-node.log](repro-node.log) |
| 6 | `node --check <原始 adapter>`；`node repro/adapter_wrapped_run.js <RunScript 输出>` | 输入合法；包装结果 `adapter result = undefined`，FAIL（预期） | 新增诊断的预期失败 | [repro-node.log](repro-node.log) |
| 7 | 本地构建 `bb-daemon` 后以不可写 `--state-dir` 启动；Dockerfile / compose / systemd / 超时常量 grep | `server: rpc log store: …`，exit 1；静态摘录 | 静态 + 实跑确认 | [static-deploy-docs.log](static-deploy-docs.log) |

- 环境失败：无。
- 诊断测试全部是“正确行为断言”，FAIL 才是证据；没有把任何诊断 PASS 当作安全结论。第 4 项中唯一一次 PASS（adapter 用例初版，单行 if 写法不触发）已改为多行写法后 FAIL，初版结果未作为证据使用。
- 复现方法：把 [repro/overlay.template.json](repro/overlay.template.json) 中的相对路径分别补成仓库与 evidence 目录的绝对路径后传给 `-overlay`；诊断源码不进入仓库。

## 6. 未检查项

- **真实 Chrome 端到端**：按约束未启动任何 Chrome。CDP 实际行为（Fetch 通配匹配、非法状态码、attach 卡死、关闭 tab 时的事件时序、`screenshot` 等）均未实测；相关结论见各条“验证方式”与 R1–R5。
- **Docker / systemd 实机**：未运行（B08、B09 为静态确认，B09 代码侧已实跑）。
- **Windows**：`launch_windows.go` 仅阅读；Windows 版 CLI 未能离线交叉编译（R16）。
- **goreleaser / release workflow**：仅静态核对 main 路径与 ldflags 变量存在，未执行发布流程。
- **Tailscale / 反向代理头的真实行为**：仅对照 `auth.go` 与 AGENTS.md 条款做代码审查。
- **性能 / 压测 / fuzz**：未做（R11 未测量）。
- **现有测试用例本身的质量**：只用于了解覆盖与构造方式，未逐个审查。
- **`pkg/daemonclient` 与文档/打包**：由只读子代理先行全文审查，本人复核了报告中引用的代码行并重跑了保留的复现；子代理提出但未纳入 bug 的项目列在 R7、R9、R14、R15。

## 7. 依赖新增情况

- 未新增、未下载任何 Go 模块或工具（`GOPROXY=off`）；`go.mod` / `go.sum` 未变。
- 使用本机已有的用户级 Go 1.27.4 与 Node.js（仅用于执行两个诊断脚本）；未使用 sudo 或系统包管理器。
- Go 构建缓存、测试临时文件、诊断用二进制与 overlay 均位于审查专用临时目录。

## 8. 证据索引

| 文件 | 内容 |
|---|---|
| [findings.json](findings.json) | 21 个 bug 的机器可读清单 |
| [configured-tests.log](configured-tests.log) | gofmt / vet / race 测试输出 |
| [repro-go.log](repro-go.log) | 18 个 Go 诊断测试的完整 `-v` 输出 |
| [repro-node.log](repro-node.log) | 快照 stale ref 与 adapter 包装的 node 输出（含 RunScript 生成的脚本） |
| [static-deploy-docs.log](static-deploy-docs.log) | 部署/文档类问题的静态摘录与不可写 state dir 实跑 |
| [repro/daemon_audit_test.go](repro/daemon_audit_test.go) | B01、B03、B04、B10 |
| [repro/store_audit_test.go](repro/store_audit_test.go)、[repro/store_audit_umask_test.go](repro/store_audit_umask_test.go) | B11、B12、B13 |
| [repro/state_audit_test.go](repro/state_audit_test.go) | B14 |
| [repro/browser_audit_test.go](repro/browser_audit_test.go) | B15、B16 |
| [repro/site_audit_test.go](repro/site_audit_test.go)、[repro/adapter_wrapped_run.js](repro/adapter_wrapped_run.js) | B17 |
| [repro/cli_audit_test.go](repro/cli_audit_test.go) | B02、B07、B18 |
| [repro/client_audit_test.go](repro/client_audit_test.go) | B06、B19、B20 |
| [repro/snapshot_stale_refs.js](repro/snapshot_stale_refs.js) | B05 |
| [repro/overlay.template.json](repro/overlay.template.json) | overlay 映射模板（相对路径） |

## 9. 结束状态

- HEAD 仍为 `d55fbc55162db73a43c01e1a6b782750437cfb5a`（detached），`git status --short` 为空，工作树未改动。
- 未创建任何工厂任务；逐项建任务、去重与 board 更新由 manager 依据 findings.json 完成。
