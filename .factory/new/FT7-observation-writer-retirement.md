---
branch: factory/FT7-observation-writer-retirement
round: 5
---
# target释放与观测writer退役同步

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] internal/daemon/tab_idle.go:183：释放清空观测环后，已取走事件的writer仍可Push重建旧完整ID环，导致旧数据和内存滞留；停止并等待writer或原子验证注册代次。未观察到接替页读取旧事件。

来源：FT1 第5轮review，5/5复现。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B15；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/daemon/tab_idle.go:49`（基线行号，实施前重新定位）。

触发条件：对某 tab 启用过 network/console/errors 观测，tab 关闭（tab_close、空闲回收、用户关闭）时 observer 队列中仍有事件或写协程正在写入——关闭带有在途请求的页面时必然产生一批 loadingFailed 事件。

实际行为：clearRemovedTarget 先 obsStore.ClearTarget（tab_idle.go:49）后才 ForgetTarget 取消 observer（:54）；runWriter（internal/browser/observer.go:267-287）在两步之间或与取消并发时继续调用 Record*，TabObsStore.Push* 通过 getOrCreateLocked（internal/state/obs_store.go:47-58）为已关闭的 target 重新建 ring。该 target 已不在注册表中，后续同步不会再清它。

期望行为：INV-6：tab 关闭后其缓冲被清空且不再接受写入。

影响：每个这样关闭的 tab 残留一组 ring（最多 500+200+100 条事件）直到进程退出；长期运行、频繁开关 tab 的 daemon 内存缓慢增长。

根因：清理顺序是先清缓冲后停生产者，且 store 的 Push 对未知 target 自动建桶，没有“已关闭”墓碑。

修复方向（建议，非指定实现）：调整为先 ForgetTarget（停止 observer 并等待/丢弃其队列）再 ClearTarget；并让 Push* 仅在 target 已登记（EnsureObservation 时显式创建）时写入，ClearTarget 后拒绝写入。

回归验收：barrier 测试：writer 已取出事件并停在 Record 之前，执行与 clearRemovedTarget 相同的清理序列后放行，QueryNetwork/Console/Errors 对该 target 返回 0 条且 store 内无该 target 的桶；-race 下反复开关带事件流的 tab 后 targets map 大小回到 0。

证据：repro-go.log（TestAudit_ObservationRingMustStayClearedAfterTabClose FAIL）；repro/browser_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

复用原 FT7，不另建重复任务。历史等级 P2 保留；本次 CC 标为 P3 仅是本次审查意见，不覆盖原历史证据。按较高 P2 安排，不因新报告降级。

## factory · plan · 2026-11-22 01:27

### Behaviour
- 首次查询一个有效 target 时显式启用其观测存储；未启用或已退役 target 的迟到事件不创建缓冲。
- tab_close、空闲回收和用户关闭导致 target 释放时，取消 observer 并与在途 writer 同步，再清空全部三类缓冲；清理后不得由旧 writer 重建。
- 无事件、重复清理、初始化失败或与关闭并发时保持安全且不死锁；network/console/errors 单域清空仍允许存活 target 后续观测。
### Scope
- 仅 observer 生命周期、观测存储注册/退役、daemon 对应调用及永久回归；不合并 stop/FT1，不处理其他审查项，不改 RPC 接口、部署或保护路径。
### Decisions
- 从最新 origin/main 独立构建。优先明确的 writer 完成同步和存储显式注册，不用永久墓碑表；兼顾初始化中被取消、队列积压和重复退役。
- 由 coder 检查并选择最小可证明安全的同步实现，覆盖重注册时旧 writer 不污染新生命周期；现有 Push* 调用和测试应显式建立所需生命周期，不保留隐式重建漏洞。
### Checking
- 先新增并运行会在修复前失败的永久回归；barrier 卡住已取出待 Record 事件，执行清理/退役，再放行并验证三类查询为空且 targets 桶数为零。
- 覆盖队列积压、各关闭路径、重复退役和并发启停；-race 下反复测试，确认活 target 新事件与单域清空正常。
- 配置全量检查：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。
- 不依赖真实外网。未实测真实 Chrome、Docker、systemd 必须写明；若用 Chrome，只能独立非 headless CDP 实例，不触碰用户窗口。

## factory · 2026-11-22 01:27

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a；未合并 stop/FT1。构建提交：4288fc7b6de83061bc0d43fee2744a6658e32acb。

实现：observer 用独立 stop 信号及 Record 退役闸门，ForgetTarget 等待在途 Record 完成；daemon 先退役 observer 再 ClearTarget；TabObsStore 仅 RegisterTarget 建桶，未注册/退役后的 Push 返回 false。查询显式注册并在附加后复核 target 存在；单域 clear 保持生命周期。补充生命周期说明和永久回归。

决定：不用永久墓碑或代次表；旧 observer 闸门阻止退役后写入。Session.Close 同样退役 writer。通过可选 ForgetTarget 接口测试 daemon 关闭路径，不改 RPC 协议。

回归证据：coder 在修改生产代码前执行 TestForgetTargetSynchronizesWithInFlightWriter 和 TestObsStoreRejectsPushWithoutRegistration，均按正确行为断言失败：释放提前返回，旧 writer 重建 network 2 / console 1 / errors 1 条；未注册 target 建桶。修复后补全显式注册及桶数断言。新增 barrier、同 id 重注册、初始化期释放、Session.Close、并发事件流反复启停、三域单独清空、五种 target 释放路径回归。coder 报告定向 -race -count=30 通过，并用恢复旧顺序/移除闸门的变异验证回归有效。

测试：coder 全量通过，manager 独立重新执行配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过，Go 1.27.4，8 个有测试包全部通过；git diff --check 通过，保护路径无变更，工作树干净。测试使用隔离 HOME、缓存和临时目录，无真实外网依赖。

安装：无；go.mod/go.sum 未变，使用已有工具链与已 pin 模块缓存。

未检查：真实非 headless Chrome/CDP 端到端、真实 loadingFailed 事件流、Docker、systemd 均未验证。daemon 假连接验证退役契约，browser 测试验证真实 Session 闸门与真实 store；无单个贯穿真实 Session + daemon 的测试，也未在真实 EnsureObservation 中途注入释放。供 review 重点检查初始化/关闭竞争及清理顺序。

状态：分支已推送，new/ → review/，第 1 轮；本次仅 build，未 review、未合并 main。

运行提示：Claude Code 五小时额度已到上限，本次调用正常完成；工具返回时距离重置约 2 小时 04 分。


## factory · review · 2026-11-22 01:34
head: 4288fc7b6de83061bc0d43fee2744a6658e32acb
verdict: fix
fix:
- [P1] internal/browser/session.go:438: 真实 EnsureObservation attach 初始化期间收到 targetDestroyed，ForgetTarget 直接取消 child，chromedp attachTarget 写 Context.Target 与取消协程读它发生 data race。基线也复现，但属于本项明确要求的初始化/释放竞争验收。让初始化方在有界 attach 完成后移交清理所有权，退役先禁止写入，再在不持 observer 表锁的情况下等待安全取消；新增真实 attach 路径永久 -race 回归。
- [P1] internal/browser/session.go:434: 首个 ForgetTarget 删表后尚在等待在途 Record，第二个调用查到空项便返回，越过退役屏障。第二次返回后 ClearTarget 并重注册同 ID，再放行旧 Record，新 ring 收到旧事件 seq=73。保留可等待的退役条目/完成信号，使重复释放等待同一屏障，不能只把删表视为退役完成。
- [P1] internal/daemon/tab_idle.go:62: 旧 ForgetTarget 等待 CDP detach 回包时，同 ID 可重新登记并复用旧桶，查询读到旧事件 seq=17；放行 detach 后旧 ClearTarget 又清掉新生命周期桶，新 writer Push 返回 false。该诊断基线通过。让同 ID 的新注册等待旧生命周期退役及清桶全部完成，或统一以生命周期标识校验注册、写入、清理；增加真实 Session+daemon barrier 回归。
- [P1] internal/daemon/tab_idle.go:68: targetDestroyed 先于 closeTarget 回包时，事件处理先删除 registry 并阻塞于退役；tab_close RPC 再次 removeTargetState 因条目已无而返回成功，但此时仍有 1 个桶和旧 network 事件，违反关闭完成后缓冲清空契约。该诊断基线通过。RPC 必须等待已开始的同 target 清理完成后再回成功，不得以 registry 条目不存在代替清理完成。
notes:
- [P2] internal/daemon/rpc_observation.go:116: 首次 attach 失败返回错误后，新增注册桶未回滚，TargetCount=1 且迟到 PushNetwork=true；基线无残留桶。区分本次新建注册与既有有效观测，仅回滚失败且未被接管的注册，不破坏既有域数据。
- [P2] internal/browser/session.go:399: Close 清空 observer 表但无 closed 状态；在途 attach 在关闭后仍报成功，旧 Session 引用还能新 attach；初始化失败时因失去表项而跳过 child 清理，context 仍存活。基线也复现。应在锁下标记关闭，拒绝新 attach，初始化方检查关闭并保留安全清理所有权。
- Codex 只读审查，无改代码/提交/安装。完整配置检查 gofmt、go vet ./...、go test -race ./... 通过；manager 独立再次执行同命令通过（部分包命中 Go cache），Go 1.27.4，8 个包通过、2 个无测试包；diff --check 通过，工作树干净。完整测试通过不覆盖以上新诊断失败。
- 临时 overlay 共 7 个诊断用例复现上述 6 项缺陷，包括真实 Session+daemon 本地回环 CDP 协议桩。证据保留于 review run 的 evidence/FT7-4288fc7/：results.log、reused-id.log、close-attach.log、基线对照日志、browser_test.go、daemon_test.go、baseline-tests/、manager-test.log；overlay.json 仅保留当时映射，重新运行需将映射重定向到保留的测试文件。诊断不是已提交的永久回归。
- 未验证真实非 headless Chrome/CDP、真实 loadingFailed 流、Docker、systemd；未操作用户窗口；无新增未 pin 工具或依赖。无链接 issue/PR，未发 GitHub 评论。
- 第 1/3 轮发现 P1，review/ → new/，下一轮在原分支修复上述 fix 列表；P2 留作审查备注。未合并 main，未发布。


## factory · plan · 2026-11-22 10:20
第 2 轮补充计划（保留既有计划）：
- Behaviour：attach 初始化与 targetDestroyed 并发时先禁写，再等待安全的初始化清理所有权交接；重复释放等待同一完成屏障；同 ID 重注册隔离旧桶与旧清理；tab_close 必须等已开始的清理完成才返回。
- Scope：最新 review 四项 P1、直接相关的失败注册回滚与 Session.Close，以及 browser/daemon 永久回归。不改变公开 RPC、不处理其他工厂项。
- Decisions：不以 registry 缺项替代清理完成，不持全局 observer 表锁等待 CDP/Record；初始化 attach 有界；原分支 merge 最新 main，不 rebase。
- Checking：真实 Session + daemon + 本地回环 CDP 桩重写四个失败场景；先验证正确断言失败，再修复；重复 -race 和配置全量检查，不依赖真实外网。

## factory · 2026-11-22 10:20
第 2 轮构建完成。先以 b58d8e7 合并 main@7223e5aa905a70f2147801625bdece0d8893876d；保留双方意图并让 main 新增枚举测试显式注册观测桶。修复提交：11af2d36e02a8afec65901bb53d39dbae84bfc96。

实现与决定：attachCancellation 在首次 Run 返回前不取消 chromedp child，并以 60s 操作超时约束 attach；observer/tab pool 共享安全取消逻辑。obsRetiring 保留退役完成信号，重复 ForgetTarget 等同一屏障；daemon 在 registry 移除同一临界区登记 targetReleases，新注册等待旧释放清桶完成，tab_close 等待既有释放。首次 attach 失败仅回滚本次新桶，Session.Close 标记关闭并拒绝新 attach；初始化方负责安全清理，避免关闭用户标签。RegisterTarget 返回是否新建并新增 HasTarget；更新生命周期文档。

回归证据（coder 报告）：internal/daemon/obs_lifecycle_stub_test.go 走真实 Session+daemon+本地 CDP 桩。修复前 TestTargetDestroyedRacingObserverAttachReply 三次均 DATA RACE；TestRepeatedForgetTargetWaitsForInFlightRecord 第二次释放提前返回并污染新桶；TestReusedTargetIDWaitsForOldLifecycleRelease 新生命周期读到旧事件且旧清理丢弃新事件；TestTabCloseWaitsForReleaseStartedByTargetDestroyed 返回成功仍有旧桶/事件。四项修复后通过。Runtime.enable barrier 用例修复前也通过，不作为失败证据；browser 假 allocator 确定性 attach barrier 未跑旧代码，仅用移除等待的变异证明有效，CDP 桩也重新报 race。另以变异验证 attach 有界、Close 中 attach 和失败注册回滚。

测试：coder 配置全量通过；生命周期 -race -count=30、browser/daemon/state 整包 -race -count=5 通过。manager 独立隔离 HOME/GOCACHE/TMPDIR、GOPROXY=off 运行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过，Go 1.27.4，10 个有测试包通过、2 个无测试包。git diff --check 通过；保护路径无变化；相对 main 的独有提交仅本项原提交、本轮 merge 与修复；工作树干净。

安装：无。go.mod/go.sum 相对 main 未变。

未检查：真实非 headless Chrome/CDP 端到端、真实 loadingFailed 事件流、Docker、systemd；未启动 Chrome，未操作用户窗口。没有单独测试 Session 父 context 取消、attach 刚成功撞上 Close、枚举路径独立 barrier（枚举复用释放登记实现）。原 review evidence 在当前机器未找到，永久回归按 review 描述独立重写。

状态：分支已推送，new/ → review/，第 2/3 轮；仅 build，未进行 review、未合并 main、未发布。


## factory · review · 2026-11-22 10:35
head: 11af2d36e02a8afec65901bb53d39dbae84bfc96
verdict: fix
fix:
- [P1] internal/browser/session.go:557: 旧 observer 退役等待 detach 回包时，同 ID 接替页面已登记；随后 chromedp child 取消再次按 target ID 发 closeTarget，实际关闭新页面。按旧 CDP session ID detach 并安全终结 child，避免对复用 target ID 关页；增加检查真实页面仍存在的协议桩永久回归。HEAD、基线均 3/3 复现。
- [P1] internal/browser/session.go:484: Session.Close 与 ForgetTarget 未共享 writer 退役屏障；无论谁先摘表，另一方均可在旧 Record 完成前返回，清桶并重注册后放行旧 Record，新桶混入 old.test。Close 摘表时发布共同退役完成信号，并等待已由 ForgetTarget 接管的 writer；两个顺序均需永久回归。HEAD、基线均 3/3 复现。
- [P1] internal/daemon/rpc_observation.go:157: 查询解析短 ID 后若 targetDestroyed 已完成退役，迟到 registerObservation 仍可重建已释放 target 的桶；等待 attach 失败期间 TargetCount 从 0 回到 1。于 obsLifeMu 下核对解析时生命周期仍有效，再注册和 attach，不可仅检查没有进行中的 release。HEAD 3/3 失败，基线相同调度 3/3 通过。
- [P1] internal/browser/session.go:126: 首次 attach 在生产默认 60s 超时后，迟到 attachToTarget 回包中的 session ID 丢失，远端 flat session 未 detach；重试成功后存在两个 session。保留有界调用等待及迟到回包清理所有权，按迟到 session ID detach；增加永久回归。HEAD 失败，基线复用原 attach 对照通过。
- [P1] internal/browser/session.go:98: Close 可漏掉已入表但尚未 Run 的初始化，abort 见 attachDone=nil 未持久记下关闭，Close 返回后仍可开始 attach 并等待整个时限。abort 持久记录关闭，run 发布首次 attach 前检查；覆盖 observer/tab pool 两路径。HEAD、基线均 3/3 复现。
- [P1] internal/browser/observer.go:125: 首次 Run 成功、域初始化停在 Network.disable 时 Session.Close 已退役 writer，放行回包后首次 network RPC 仍返回成功和 cursor。将域激活成功提交与关闭状态同步，退役赢得竞争则返回关闭错误并回滚首次注册。HEAD、基线均 3/3 复现。
notes:
- Codex 只读审查 origin/main@7223e5aa905a70f2147801625bdece0d8893876d...HEAD；未找到 review-code skill，按需求和计划直接复核。未修改工作树、提交、push/fetch 或运行 gh；无安装。四项基线也存在的缺陷与本项生命周期验收直接相关，基线复现已明确标注，不冒称本轮引入。
- coder 在 HEAD 与临时导出的 main 均执行完整配置检查通过；永久生命周期回归 -race -count=10 通过，额外父 context 取消屏障 3/3 通过。manager 独立隔离 HOME/GOCACHE/TMPDIR、GOPROXY=off 执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过，Go 1.27.4，10 个有测试包通过、2 个无测试包；diff --check 通过，工作树干净。现有测试通过不覆盖新诊断失败。
- 新诊断为只读 overlay，使用真实 browser.Session 和本地回环 CDP 协议桩。stale RPC 在成功解析 ID 后插入调度屏障，并以直接调用同一解析/启用方法交叉验证；before-run 构造已入表尚未执行状态；迟到 attach 使用生产默认 60s。证据保留在本机 review run evidence/FT7-11af2d3/：commands.md、overlay/、diagnostics.log、head-stale-rpc.log、baseline-stale-rpc.log、head-late.log、baseline-late.log、head-browser.log、full.log、main-full.log、manager-full.log。commands.md 说明临时目录清理后的复跑方式；诊断尚非永久回归。
- 未验证真实非 headless Chrome/CDP、真实 loadingFailed 流、Docker、systemd；未启动 Chrome、未操作用户窗口。无链接 issue/PR，故不发 GitHub 评论。
- 第 2/3 轮发现 P1，review/ → new/，下一轮在原分支修复以上 fix 列表；本轮未改代码、未合并 main、未发布。


## factory · plan · 2026-11-22 14:44
第 3 轮补充计划：
- Behaviour：按旧 CDP session detach，不关闭复用 ID 的接替页；Close/Forget 两种顺序共享 writer 退役屏障；过期解析不得为已释放 target 重建桶；超时 attach 保留迟到回包清理所有权；run 前 abort 持久生效；域初始化成功提交与关闭同步。
- Scope：最新 review 六项 P1 与直接相关永久回归及生命周期文档，不改公开 RPC、不处理其他工厂项。
- Decisions：明确清理所有权，不持全局表锁跨 CDP/Record；调用方等待有界，迟到 attach 采用有限宽限期；在原分支实施，不 rebase。
- Checking：真实 Session+daemon 本地 CDP 协议桩 barrier、修复前失败对照、重复 race 和配置全量检查；不依赖真实外网。

## factory · 2026-11-22 14:44
第 3/3 轮构建提交：e11ba35；基于 11af2d3，origin/main 未移动，无需新增 merge。

实现：子 context 取消前清空 Target.TargetID，仅按旧 session ID detach，覆盖 observer/tab pool；Close 发布并等待 obsRetiring，与 ForgetTarget 共享退役屏障；registerObservation 在 obsLifeMu 下检查 target 仍在 registry；attach 调用方超时后由初始化方继续等待迟到 session（lateAttachGrace 默认 5 分钟）并 detach，新增 Session.SetAttachTimeout；abort 永久记录并由 run 前检查；EnsureObservation 仅在 observer 仍有效且 Session 未关闭时提交成功，失败由 daemon 回滚首次桶。更新 AGENTS.md。

回归证据（coder 报告）：修复前 11af2d3 导出副本叠加新永久测试，复用 ID 误关接替页、Close/Forget 双顺序旧 Record 污染新桶、过期解析建桶、迟到 attach 残留双 session、Close 后域初始化误报成功、run 前 abort/Close 仍 attach 均失败；迟到 attach 对照使用临时 shim 将旧超时设为 100ms，非生产默认 60s；cancel 子用例原本通过，不计失败证据。修复后生命周期 browser/daemon -race -count=20、browser 整包 -race -count=3 通过。

测试：coder 全量通过；manager 独立隔离 HOME/GOCACHE/TMPDIR、GOPROXY=off 执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全量通过，Go 1.27.4，10 个有测试包、2 个无测试包。git diff --check 通过，保护路径与 go.mod/go.sum 未变；独有历史仅本项提交和 main merge，工作树干净。

安装：无。coder 报告一个编辑脚本误用系统默认临时目录，已自行删除；未由 manager 清理 /tmp。

需 review 重点检查的边界：迟到回包超过 5 分钟宽限期仍可能泄漏 session；Close 不提前结束已超时且已出表的 attach，最多等宽限期，宽限期到期仅假 allocator 验证。注册检查未引入代次，同 ID 已重新登记时旧查询服务当前页面；过期解析回归直接调用 resolveTab/enableObservation，未在 RPC 函数内部注入屏障。Target.TargetID 处理依赖 chromedp v0.15.1 的取消 watcher 实现，升级需复核。

未检查：真实非 headless Chrome/CDP、真实 loadingFailed 流、Docker、systemd；未启动 Chrome、未操作用户窗口。Session 父 context 取消与本轮改动组合未单独做 barrier 测试。

状态：代码分支已推送，new/ → review/，第 3/3 轮；仅 build，尚未 review，未合并 main、未发布。下一步核查六项 P1 与上述边界，不将现有测试通过视为审查通过。

## factory · review · 2026-11-22 15:16
head: 50e450c5ec61e81677d47e8d8ed925720882595f
verdict: fix
fix:
- [P1] internal/daemon/rpc_observation.go:141: 旧首次 network RPC attach 失败、回滚前，同完整 target ID 已以新 short ID 重注册并写入新事件；旧 created=true 随后 ClearTarget 擦掉接替页事件。HEAD 3/3 失败，main 3/3 保留事件。注册返回具体桶/生命周期句柄，仅在该句柄仍拥有当前桶时回滚，不能按 target ID 无条件清理。
- [P1] internal/daemon/rpc_observation.go:165: 真实 network RPC 在解析 c0de 后暂停，旧页释放、c0de 分配给另一 target、原完整 ID 以 0c0de 回归；旧 RPC 返回接替页 network 事件却标记 tab=c0de。HEAD/main 均 3/3 失败。解析时捕获注册身份，在注册、attach 和读 ring 时验证同一生命周期及 alias 映射，否则返回 stale-tab 错误。
- [P1] internal/daemon/tab_idle.go:306: targetDestroyed 已退役旧页且同 ID 接替页已登记后，旧 tab_close 成功回包再次 removeTargetState，删除接替页 registry 及观测事件，真实页面仍存在。HEAD/main 均 3/3 失败，无 short-ID 碰撞变体也复现。close 绑定发出时的注册身份，只完成/等待该次释放，不删除后来注册的资源。
notes:
- [P2] internal/browser/session.go:200: 生产默认 60s 超时及 5 分钟宽限期不变，365s 后释放 attach 回包，再重试成功留下两个远端 CDP session；main 同调度一个。保留迟到确认清理所有权直至取得 session ID 可 detach 或 transport 关闭，调用方等待仍可有界。
- [P2] internal/browser/session.go:687: 旧 observer detach 等待期间，接替页 network_route 已确认一条规则；旧退役完成又清零其规则。HEAD/main 均 3/3。路由清理绑定退役代次，或新安装等待旧清理完成。
- [P2] internal/browser/session.go:612: Session.Close 后立即释放未完成的 attach 成功回包，network/eval 均在存活页面残留远端 session；HEAD/main 各路径均 3/3。关闭后仍须保留已发 attach 的回包处理及按 session ID detach 所有权。
- [P2] internal/browser/session.go:230: 初始化失败后仅一次 detach 被拒，清理丢弃 session ID，重试成功后残留双 session；HEAD/main 均 3/3。非终态 detach 错误保留 ID 并重试清理。
- [P2] internal/browser/observer.go:124: Close 赢得域初始化竞争时 HEAD 已正确返回 RPC 错误，但仍在 Close 返回后发 Network.enable 且 session 残留；HEAD/main 均 3/3。域初始化与退役取消关联，并按 session ID detach 已初始化 child。
- [P2] internal/browser/session.go:635: 并发四次 Close 对 s.cancel 的读写触发 race，HEAD/main 均复现。删除无用字段或同步其终结，仍确保每个 Close 等待 writer 退役屏障。
- 原六项 P1 的原始失败调度本轮全部通过，包括接替页存活、Close/Forget 双顺序、target 仍缺席时拒绝旧 RPC、宽限期内迟到回包 detach、run 前 abort 双路径、域初始化关闭竞争返回错误。独立宽限期内诊断使用生产默认 60s。不是沿用历史 finding；上列为相邻新失败调度，基线存在项已明确标注。
- 审查前 origin/main 已到 e63a15c，与分支 AGENTS.md 冲突。Codex 本地 merge 并保留两侧规则，提交 50e450c；manager 检查保护路径及 diff、独立全量测试后推送，仅代码分支更新，未合并 main。
- Codex 只读 review origin/main...HEAD，无 review-code skill，未改工作树、提交、push/fetch 或运行 gh。HEAD 与导出 main 的配置 gofmt/vet/go test -race ./... 均通过；选定永久回归 -race -count=5 通过。manager 独立配置全量通过（部分包缓存），11 个有测试包、1 个无测试包，Go 1.27.4；另独立重跑上述三项 P1 及无碰撞 close 变体均失败。配置通过不覆盖诊断缺陷。
- 证据保留本机 review run evidence/FT7-50e450c/：REPRODUCE.md、source/、schedules-head/main.log、close-head/main.log、close-same-alias-head/main.log、grace-head/main.log、configured.log、configured-main.log、manager-diagnostics.log、prior-schedules-head.log、within-grace.log、permanent.log。merge 后 manager 全量日志另存 evidence/FT7-e11ba35/manager-merge-test.log。诊断用真实 Session+daemon 与本地回环 CDP 桩、工作树外 overlay，非已提交永久回归。
- 无安装。未验证真实非 headless Chrome/CDP、真实 loadingFailed、Docker、运行中的 systemd；未启动 Chrome、未操作用户窗口。无链接 issue/PR，不发 GitHub 评论。
- 第 3/3 轮发现 P1，达到 max_rounds，留在 review/ 等 owner 决定；如需修复，请添加说明并移至 new/。本轮未发布。

## Guoyin · 2026-11-22 16:54
再来一轮


## factory · plan · 2026-11-22 17:15
第 4 轮补充计划（owner「再来一轮」授权）：
- Behaviour：注册、读取、首次 attach 回滚及关闭完成均绑定解析/发出时的注册身份；完整 target ID 或 short alias 复用时，旧操作不得读取或清除新生命周期；过期观测返回现有 stale/invalid tab 错误。
- Scope：最新 review 三项 P1、必要 state/daemon 身份与桶句柄、永久回归及文档；不扩张处理其余 P2，不改公开 RPC。
- Decisions：不以字符串相等代替生命周期相等；注册身份需稳定于活页重复登记，桶按所有权条件读取/回滚；不持全局锁跨 CDP/Record，原分支 merge 最新 main，不 rebase。
- Checking：真实 RPC + Session + 本地 CDP 协议桩 barrier 验证三项原始调度及无碰撞 close 变体，修复前失败、修复后通过；重复 race、三域正常观测与 clear、配置全量。

## factory · 2026-11-22 17:15
第 4 轮完成：46231d1 合并 main@2be49f8，无冲突；修复提交 9dfe0a4。代码分支已推送，new/ → review/，round=4；owner 明确追加本轮，不修改 max_rounds。

实现与决定：TabRegistration 增加独立、仅新登记递增的身份（现有 targetGen 在活页重复登记时也变，不可充当身份）；ObsBucket 持有具体桶句柄，条件回滚/读取。network/console/errors 在解析、注册、attach 后、读环时校验同一登记，旧失败不清新桶；tab_close/idle close 绑定发出前登记，迟到成功回包只释放原登记。过期查询装上的无主 observer 用 ForgetObservation 单独退役，不动 tab pool/路由。新增生产 nil 的 obsStepHook 及 CDP 扣回包测试缝，确定性复现 RPC 内调度；更新 AGENTS.md。

回归证据（Claude Code 报告）：仅加测试缝、未改逻辑的对照 -race -count=3，TestFailedAttachRollbackKeepsReplacementLifecycleBuffers 3/3 擦掉新事件；TestStaleAliasQueryDoesNotReadReplacementLifecycle 的 resolved/attached 两屏障均返回错标 tab 的接替页事件；TestStaleTabCloseReplyKeepsReplacementLifecycle 的 alias_reassigned/same_alias 均删掉新登记及事件。修复后通过；生命周期 -race -count=20、daemon/state/browser 整包 -race -count=5 通过。新永久回归另覆盖无主迟到 observer（移除清理的变异失败）、活页三域及 clear、身份稳定性和桶句柄隔离。原 review overlay 本机未找到，按描述独立重写。

测试：coder 全量 gofmt/vet/race 通过；manager 独立隔离 HOME/GOCACHE/GOTMPDIR、GOPROXY=off 执行配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全量通过，Go 1.27.4，11 个有测试包、1 个无测试包。git diff --check 通过，工作树干净；相对 main 保护路径未变，独有历史仅本项提交与 main merges。本机 build evidence/FT7-9dfe0a4 保留修复前日志、coder 最终全量日志及 manager-full.log。

安装：无。本轮修复未改 go.mod/go.sum 或保护路径。

未检查/待 review：真实非 headless Chrome/CDP、真实 loadingFailed、Docker、运行中 systemd 均未验证，未启动 Chrome或操作用户窗口。读环复核没有独立 RPC barrier，仅 state 测试和路径覆盖。最新 review 六项 P2 均未处理：超宽限期 attach、旧清理路由、Close 后 attach、detach 错误重试、Close 后域启用、并发 Close race。旧释放仍按 target ID 调用 ForgetTarget/forgetTabManaged，同 ID 接替页在旧释放期间建立的 pool/路由/managed 标记仍有相邻风险；targetDestroyed 仅含 ID，仍按处理时当前登记释放；eval/click/*_clear 等其他 RPC 未绑定登记。重点审查身份/桶/observer 所有权边界，不能把测试通过等同 review 通过。

状态：仅 build，等待独立 review；未合并 main、未发布。


## factory · review · 2026-11-22 17:41
head: 9dfe0a406ec4339e09f9d8fd54c9439267990eac
verdict: fix
fix:
- [P1] internal/daemon/rpc_observation.go:182: 旧请求迟到安装的 observer 未绑定登记/具体桶；B 的 Record 暂停后 B 退役，C 同完整 ID 登记并复用该 observer，放行后 C 的真实 network RPC 读出仅在 B 发送的事件。observer、域租约、recorder 应绑定登记与桶句柄，退役按 observer 实例回收，写入条件验证所有权，不能因同 ID 有新桶就视为接管。HEAD/main 均 3/3。
- [P1] internal/daemon/v1.go:478: 生命周期消费被无关页 detach 阻塞时，A 销毁但通知未处理，tab_new 创建同完整 ID 的新页继承旧登记/缓冲，首次 network 返回 A 的旧事件；迟到旧销毁通知还会删除新页状态。统一创建/销毁/回包排序，新创建隔离旧登记；无法归属的旧销毁先重新枚举核对登记再清理。纯本地协议桩、无生产调度钩子亦复现，HEAD/main 均 3/3。
- [P1] internal/daemon/tab_idle.go:326: 解析原登记后、CDP closeTarget 发出前换为同完整 ID 接替页，旧 tab_close 真正关掉接替页；idle 回收亦能关闭从未受管的新页。回包后条件删除不足；发送前取得并校验原登记专属会话，关页只送原会话，失效即 stale，不对复用裸 ID 重发。HEAD/main 均 3/3。
- [P1] internal/daemon/v1.go:1175: network_clear 解析旧别名后换页，随后无条件按 target ID 清桶，擦除接替页已读到的新事件；console_clear:1101、errors_clear:1138 同样。捕获 TabRegistration，生命周期锁下核对并对具体桶清域，过期返回 unknown tab id；保留活页未建桶空清理行为。HEAD/main 均 3/3。
- [P1] internal/daemon/v1.go:747: 已解析 eval 在执行前换页，按复用完整 ID 首次 attach 接替页执行脚本，却将成功结果标为旧别名。页面操作需取得并验证原登记所属会话句柄，不得按 ID 重新取得新 executor。HEAD/main 均 3/3，相邻 RPC 的基线缺陷。
notes:
- [P2] internal/browser/session.go:996: 旧 close 回包拆掉接替页正在 eval 的 pool 会话并清空其新路由；按原 pool entry/路由代次条件清理，或交原登记释放方。HEAD/main 均复现。
- [P2] internal/daemon/tab_idle.go:160: 旧释放在 forgetTabManaged 前暂停，新 tab_new 已登记受管身份，放行又删去新 managed 成员；成员 open/retire 需共享登记身份并条件删除，或新提交等待旧退役。HEAD/main 均复现。
- [P2] internal/daemon/tab_idle.go:210: A 的旧 close 回包等待 B 的释放屏障，直到 B 结束才应答；释放完成信号应按登记保存，另保留按 ID 的建桶屏障。HEAD/修复前失败，main 3/3 通过。
- [P2] internal/daemon/v1.go:245: 成功读环后二次解析短 ID 将旧 RPC 审计 target 和 idle 续期归给另一受管页；首次解析时固定审计身份，按原 managed 登记续期。HEAD/main 均复现。
- 前轮六项 P2 当前均重测、保持原级：session.go:200 超 5 分钟宽限期 attach 泄漏、:663 旧退役清新路由、:612 Close 后 attach 漏 detach、:230 detach 失败丢清理句柄、observer.go:124 Close 后继续启用域、session.go:635 并发 Close 的 cancel 指针 race。365 秒诊断保留生产 60 秒超时和 5 分钟宽限期，HEAD 重试后两个 session、main 一个；其余两边均复现。修法分别为保留回包所有权至 session ID/断连、路由代次条件清理、Close 后仍处理已发请求回包、detach 非终态失败保留 ID 重试、域动作关联退役取消、同步或移除 cancel 字段写入。

Codex 独立只读审查 origin/main@2be49f8...HEAD，无可用 review-code skill。第4轮原三个 P1 原始调度在 HEAD 各 3/3 通过，修复前对照失败；额外读环屏障 HEAD 3/3 拒绝旧读取、main/修复前失败；选定永久回归 -race -count=5 通过。上述是新遗漏，不是重复已修调度；五项 P1 均在 main 存在，协议桩刻意复用完整 target ID，真实 Chrome 发生频率未验。
HEAD 工作树与导出 main 均通过配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，Go 1.27.4，各11个有测试包、1个无测试包。manager 核对配置日志及诊断源码，独立 -race -count=3 重跑五项 P1 主要场景（包括三种 clear、显式/idle close、旧 writer、无钩子队列、eval）全部按正确断言失败；没有另行重复全量。完整配置通过不覆盖诊断缺陷。
证据保留本机 review run evidence/FT7-9dfe0a4/：REPORT.md、KNOWN-P2.md、source/、reproduce.sh、configured.log、configured-main.log、prior-*.log、adjacent-*.log、writer-*.log、dispatch-*.log、p2-*.log、grace-*.log、manager-diagnostics.log。复跑脚本仅用本地提交和模块缓存；诊断为工作树外 overlay，尚非永久回归。
无安装、依赖清单或工作树改动；隔离 HOME/TMPDIR/缓存，GOPROXY=off、GOSUMDB=off。未验证真实非 headless Chrome/CDP、真实 loadingFailed、实际 ID 复用频率、Docker、运行中 systemd；click/fill 等未逐项做换页注入。未启动 Chrome、未操作用户窗口。无链接 issue/PR，不发 GitHub 评论。
round=4，已超过配置 max_rounds=3；本次追加构建由 owner「再来一轮」授权，不自动追加后续修复。发现 P1，留在 review/ 等 owner 决定；需继续请追加说明并移至 new/。未改代码、未合并 main、未发布。


## factory · 2026-11-22 17:58
owner 本次明确要求：「补充合理边界，把任务移回 new 再来一轮」。本节记录本次授权与验收范围；不修改历史计划、审查或全局 max_rounds。

合理边界（本轮及后续 FT7 验收以此为准）：
- 使用场景为关闭旧 tab 后创建新 tab，完整 CDP target ID 在该 daemon 生命周期内不复用。此为本任务的工作假设，不宣称已通过真实 Chrome 证明其全局唯一性。复用活 tab 导航不等于销毁后复用完整 target ID；短 alias 碰撞也不等于完整 ID 复用。
- 核心契约：tab_close、空闲回收、用户关闭及枚举发现消失后，旧 observer/writer 必须退役；清理完成后 network/console/errors 缓冲为空、store 不再有该 target 的桶，迟到事件、迟到查询/初始化不得重新建桶；持续开关不同完整 ID 的 tab 不得积累旧桶。
- 初始化与关闭并发、重复清理、Close/Forget 交错、在途 Record、关闭完成应答、清理失败及取消路径仍在范围内；不能因完整 ID 不复用而忽略仍能发生的 race、死锁、提前应答或旧桶残留。
- 活页正常观测和单域 clear 应继续有效；不同完整 target ID（含短 alias 冲突）之间不得串数据、误清理或误关页。公开 RPC 行为不扩张。
- 不为人为复用同一完整 target ID 的接替页增加跨所有 RPC 的代次/会话隔离系统。不扩展到 eval/click/fill 等通用页面操作重构。已有可工作的保护和永久回归保留，除非证明不必要且安全，不为收窄范围盲目回滚。

最新审查的处理：
- 2026-11-22 17:41 的五项 P1 原始复现场景均依赖完整 target ID 被复用，且 main 亦复现。本轮将其列为范围外的条件性发现，不再仅凭这些调度阻塞 FT7；保留历史证据，不声称已经修复，也不批量改写历史等级。
- 若其中任一缺陷能在完整 ID 各不相同的正常创建/关闭场景复现，按实际影响重新定级并修复。P2 同样逐项判断，不自动全部升级，也不自动全部排除；特别关注并发 Close race 等不依赖 ID 复用的问题。

第 5 轮补充计划：
- Behaviour/Scope：先核实上述收窄契约是否已满足，只修复边界内缺口；清理无关复杂化须有证据，禁止继续把任务扩大为全系统生命周期重构。
- Decisions：沿用现有 factory/FT7-observation-writer-retirement 分支；如 main 已前进先 merge，不 rebase；无需为获得提交而做无意义代码修改，若只需文档/测试澄清，应如实记录。
- Checking：永久 barrier 覆盖已取出待 Record 的事件与释放并发、三域清空和桶数归零、迟到查询/attach 不重建已释放 ID、重复释放及 Close/Forget；-race 反复创建/关闭不同完整 ID 的 tab，区分相同完整 ID 的范围外压力测试。执行配置完整 gofmt/vet/race，并如实报告未验证的真实 Chrome/CDP、loadingFailed、Docker/systemd。

状态：按 owner 要求 review/ → new/，授权额外一次构建（下一轮为第 5 轮）；header round 保留 4，待实际构建完成再递增。全局 max_rounds=3 不变，不授权无限重试、不自动 ship。


## factory · 2026-11-22 18:19
第 5 轮构建完成，遵守 17:58 收窄边界：daemon 生命周期内完整 CDP target ID 不复用，短 alias 可被不同完整 ID 接替。原分支新增 209ee623974ba0a5df585f4743bb4e21ef273ec5；main@2be49f8 已是祖先，无需 merge。仅 build，不表示 review 通过。

实现与决定：删除 Close 对未同步 cancel 字段的写入，保留共享 writer 屏障；首次 attach 清理 detach 被拒时保留 session ID，让 cancel watcher 再尝试一次（非无限重试）；移除 5 分钟迟到 attach 宽限期，保留回包清理所有权直到回包/取消/连接结束；观测查询固定首次解析的完整 target ID，避免短 alias 易主后错误续期另一页和审计错记。新增 obsStepRead 测试缝并澄清 AGENTS.md 边界。未新增跨 RPC 代次/会话架构，未重构 eval/click/fill，保留已有保护和回归。

永久回归与证据（Claude Code 报告）：9dfe0a4 加新测试及测试缝对照，四次并发 Close 的 race 在两批 -count=3 各捕获一次；detach 拒绝一次的 observer/pool 泄漏、不同完整 ID 复用短 alias 后误续期均 3/3 失败，修复后通过。迟到 attach 对照仅以临时 shim 将旧 5 分钟宽限期缩为 30ms，未等待生产默认 60s+5min，不冒称生产时限实测。不同完整 ID/短 alias 碰撞下的在途 Record 释放，以及 tab_close/用户关闭/枚举消失/idle 四路径反复带事件开关，旧三环与桶归零、迟到 Push 被拒：修复前已通过，属契约证明而非新 bug；跳过 ClearTarget、跳过 ForgetTarget、允许 Push 重建的三种变异均使这些回归失败。初版 churn 用例自身 ID 格式错误的失败不算产品缺陷。

测试：coder 配置全量通过，browser/daemon 定向生命周期 -race -count=20、browser/daemon/state 整包 -race -count=5 通过；manager 独立隔离 HOME/GOCACHE/GOTMPDIR，GOPROXY=off、GOSUMDB=off 执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过，Go 1.27.4，11 个有测试包、1 个无测试包。diff --check、提交范围、保护路径、工作树检查通过。安装：无；go.mod/go.sum 未变。证据保留本机 build evidence/FT7-209ee62/，含 before-*.log、mutations.log、repeat.log、full-final.log、manager-full.log。

审查边界/未修复：17:41 五项 P1 仍保留，原始调度依赖完整 ID 复用，按 owner 边界不作为本轮阻塞，未声称修复，也未逐一改造为不同 ID 诊断。旧路由/pool/managed 清理和等待接替注册释放的 P2 同样依赖完整 ID 复用。并发 Close、超宽限期 attach、detach 拒绝一次、观测审计 alias 误续期已处理；Close 后 attach 漏 detach/继续启用域未修，coder 判断 daemon 退出时连接终结、无 writer/桶影响，需独立 review 核实，不以此直接宣告安全。其它 RPC 的短 alias 审计仍用旧逻辑。

重要权衡：移除宽限期后，不回 attach 的浏览器可导致每次重试留下一条待回包 goroutine/请求直到取消或断连；main 相同等待不超时但复用单个 attach，本分支重试可能积累更多。保留回包所有权避免远端 session 泄漏，但此资源增长风险尚未进一步消除，交 review 明确检查。detach 仅额外重试一次，连续拒绝不保证回收。

未检查：真实非 headless Chrome/CDP、真实 loadingFailed 流、真实完整 ID 复用频率、生产默认超时的迟到回包、Docker、运行中的 systemd；未启动 Chrome、未操作用户窗口；click/fill 等 RPC 未注入短 alias 易主调度。

状态：代码已推送，new/ → review/，round=5。owner 仅授权额外一次构建，max_rounds=3 保持不变，不自动追加修复；等待独立 review，未合并 main、未发布。


## factory · review · 2026-11-22 18:38
head: 209ee623974ba0a5df585f4743bb4e21ef273ec5
verdict: fix
fix:
- [P1] internal/daemon/tab_idle.go:169: 启动首次枚举发现目标消失并执行退役时死锁。ListenAndServe 持 tabMu 写锁，退役经 tabConn 再取同锁读锁，HTTP listener 无法启动，取消也无法解开。不同完整 ID 的两个页面在启动期关闭即可复现，HEAD/main 各 3/3。将已连接 Session 传入初始对账，或缩小发布连接的锁范围，保留先停 writer 后清桶契约。
- [P1] internal/daemon/rpc_observation.go:244: tab_list 的旧快照在用户关闭及 observer/桶清理完成后重新登记死 target；随后 network 查询在失败 attach 回包前重建桶，production sink 的迟到 Record 被接受。无完整 ID 复用；HEAD 3/3 重建，main 同样恢复旧别名但不建桶，错误返回后 HEAD 才回滚桶。internal/state/tab_registry.go:199 的正向枚举登记须受请求开始后删除约束，丢弃或刷新失效旧快照，保证已释放 ID 不被迟到查询重新建桶。
notes:
- [P2] internal/browser/observer.go:164: 超时初始化出表后退役无法找到；同一活页重试 8 次留下 8 个未回 attach/等待协程（main 1 次 attach）。另 4 个不同 ID 在 session 已知、Runtime.enable 挂起时超时并销毁，放行命令回包后 HEAD 仍有 4 个初始化等待，main 为 0。应保留可发现的未决初始化、合并/约束重试，已知 session 且确认销毁后取消剩余初始化。此增长诊断使用 25ms 超时，不冒称生产 60s 时限实测。
- [P2] internal/browser/session.go:613: Close 跨 attach 回包仍可残留远端 session；internal/browser/observer.go:124 的域初始化仍可在 Close 返回后发 Network.enable。HEAD/main 均复现；HEAD 正确返回 RPC 错误并回滚首次桶，daemon 正常退出/连接结束限制影响。需保留回包清理所有权并将域激活关联退役取消。
- [P2] internal/daemon/tab_idle.go:326: 显式 tab_close 收到明确 No target with given id found 后跳过退役，销毁通知缺失/延迟时保留登记、桶和旧事件，HEAD/main 均 3/3；后续枚举/通知可清理。应同 idle 路径在 ErrorTargetGone 时释放原登记，同时保留现有 RPC 错误。

Codex 独立只读审查 origin/main@2be49f8...209ee62，无可用 review-code skill。严格使用 owner 17:58 边界：完整 CDP ID 不复用、允许不同完整 ID 的短 alias 碰撞；未将 17:41 人为同完整 ID 复用的五项 P1 沿用为阻塞。上述两项 P1 均无需完整 ID 复用，属于初始化/枚举/释放/迟到建桶的范围内缺口；main 已有的死锁明确标记，非宣称本轮引入。
HEAD 与干净 main 的配置 gofmt/vet/go test -race ./... 均通过（11 个有测试包、1 个无测试包），Go 1.27.4。browser/daemon/state 整包 -race -count=5 通过，永久回归覆盖在途 writer、Close/Forget、并发 Close、关闭应答、四条不同 ID 关闭路径 churn、活页三域/clear、拒绝一次 detach 和 alias 审计。生产默认 60s 超时、回包实际延迟 365 秒后的重试仅余一个远端 session，HEAD/main 均通过。完整配置通过不覆盖新增诊断缺陷。
manager 核对诊断源码/全量日志，独立运行保留复现脚本，HEAD/main 各 -race -count=3 重现两项 P1 及所列资源/关闭诊断；没有另行重复全量。工作树干净、无代码修改或提交。无工具/依赖安装；隔离 HOME/TMPDIR/缓存，使用已有 pin 模块，GOPROXY=off、GOSUMDB=off，无真实外网。
证据保留本机 review run evidence/FT7-209ee62/：REVIEW.md、source/、reproduce.zsh、configured.log、configured-main.log、repeat.log、barriers.log、late-head/main.log、diagnostics-head/main.log、startup-head/main.log、release-head/main.log。诊断使用真实 Session+daemon 与回环 CDP 桩；startup overlay 仅增加首次枚举前调度屏障，未改生产判断；临时诊断尚非永久回归。
未验：真实非 headless Chrome/CDP、真实 loadingFailed、Chrome 完整 ID 实际复用频率、Docker、运行中 systemd、Go 1.26；未启动浏览器或操作用户窗口。无链接 issue/PR，不发 GitHub 评论。
round=5，已超过 max_rounds=3；owner 仅授权本次额外构建，不自动追加。发现 P1，留在 review/ 等 owner 决定；若继续修复请追加说明并移至 new/。仅 review，未合并 main、未发布。


## factory · 2026-11-23 11:39
owner 明确要求：「把 go-bb-browser FT7 移回 new 并开始用 cc build」。review/ → new/，授权第 6 轮使用 Claude Code；round 完成后递增，max_rounds 不变，不自动 ship。沿用 2026-11-22 17:58 的合理边界，不以同完整 target ID 人为复用扩张范围。

## factory · plan · 2026-11-23 11:39
第 6 轮补充计划：
- Behaviour：启动首次对账遇到消失 target 不死锁，取消可结束；已经退役的 target 不被迟到枚举快照重新登记，迟到查询/初始化不得重建桶；活页枚举和观测保持正常。
- Scope：最新 review 两项 P1，相关初始化/枚举/释放永久回归。P2 仅在同一清理所有权路径必须处理时做最小修复并说明，不扩展通用页面操作或全系统代次架构。
- Decisions：复用原分支，先检查最新 main 是否需 merge；不 rebase；用连接显式传递或缩小锁范围修复启动锁递归；以请求开始后的移除约束/快照有效性阻止陈旧正向登记，具体最小实现由代码核实。
- Checking：真实 Session+daemon 本地回环 CDP 桩复现启动死锁和 tab_list 旧快照恢复死 target，两项正确断言修复前失败、修复后通过；重复 -race，配置完整 gofmt/vet/race；记录真实 Chrome/CDP、Docker/systemd 未检查部分。
