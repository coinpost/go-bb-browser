---
branch: factory/FT7-observation-writer-retirement
round: 2
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
