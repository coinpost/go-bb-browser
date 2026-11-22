---
branch: factory/FT10-persist-managed-target-retirement
round: 2
---
# [P2] 空闲回收/浏览器侧关闭的受管 tab 永不从持久化 managed 集合移除，重启后同短 id 的用户 tab 被当成受管 tab 关闭

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B03；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`internal/daemon/tab_idle.go:215`（基线行号，实施前重新定位）。

触发条件：（1）daemon 用 tab_new 创建 tab，随后该 tab 被空闲回收关闭，或被用户在 Chrome 里手动关闭/崩溃（没有 tab_close RPC）。（2）daemon 重启（或运行期 runTargetSync 重新 reconcile），浏览器中存在 CDP target id 末尾 hex 与旧 tab 相同（→ 相同短 id）的用户 tab。

实际行为：store.managed（internal/store/replay.go:31-39）只在成功的 tab_close 审计记录时删除条目；closeExpiredTabs → closeTabByShortLocked（tab_idle.go:215）与 removeTargetState（tab_idle.go:58-64）不写任何记录。条目永久留在内存与 rpc.jsonl，并在每次轮转时被 writeManagedSnapshotLocked 重新写入。重启后 reconcileIdleFromLog（tab_idle.go:101-114）按短 id 查到用户 tab 并 MarkManagedIfAbsentAt，空闲超时后把用户 tab 关闭。复现中用户 tab CCCC…1234 被关闭。

期望行为：tab 被空闲回收或被确认从浏览器消失后，应从持久化 managed 集合移除；只有 daemon 创建且仍存活的同一 target 才能在重启后恢复为受管。

影响：（a）daemon 可能关闭用户自己打开的标签页（数据丢失：未保存表单、登录流程）；残留条目越多概率越高（每条残留对任意新 tab 的命中率约 1/65536，长期运行的爬虫 daemon 可累积上万条）。（b）managed 集合、rpc.jsonl 快照无界增长，轮转后文件被陈旧快照占满。

根因：持久化 managed 状态只由“成功的 tab_close RPC 审计行”驱动，空闲关闭/目标销毁路径没有对应的持久化事件；且恢复时仅按 4 位起的短 id 匹配，不校验 target 身份。

修复方向（建议，非指定实现）：在 removeTargetState/closeExpiredTabs 成功后向 store 追加一条合成 tab_close（或提供 Store.ForgetManaged）；可选增强：managed 记录里保存完整 target id，reconcile 时校验一致才恢复。

回归验收：集成测试（真实 state dir + fake conn）：tab_new → 强制空闲过期 → closeExpiredTabs 后 store.ReplayManagedTabActivity() 不含该短 id；重开 Server 并注入同短 id 的用户 tab，reconcileIdleFromLog 后 tabIdle.Snapshot() 不含该用户 tab，closeExpiredTabs 不会关闭它；target 销毁事件路径同样断言；轮转后新 rpc.jsonl 不含已关闭 tab 的合成 tab_new。

证据：repro-go.log（TestAudit_IdleClosedTabMustLeavePersistedManagedSet FAIL，含用户 tab 被关闭的后果断言）；repro/daemon_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。


## factory · plan · 2026-11-22 03:58

### Behaviour
- tab_new 同步登记完整 target 身份；显式关闭、空闲关闭、销毁或成功枚举确认消失后持久化退役；重启仅恢复仍存活且完整身份一致的受管 tab，保留启动 grace。
- 首次/空库安全；旧日志仅短别名不能证明身份，不得据此接管用户 tab。瞬时关闭/枚举错误不退役存活 tab。
- 延迟审计不得在退役后复活成员，或误伤复用别名的新 target；轮转仅保留存活受管身份与活动时间。持久化失败明确报告。
### Scope
- 仅 daemon/store 的 managed 生命周期、身份安全恢复、相关文档与永久回归；不合并 stop/FT1，不改短别名算法、陈旧枚举算法、观测 writer 或其他审查项。
### Decisions
- 采用最小显式 store 生命周期 API，完整 target 为身份、短 id 为别名；异步 audit 仅更新已存在且身份匹配的活动时间，不控制成员增删，不引入无界墓碑。
- 无法校验身份的旧记录保守不恢复并说明兼容代价；写盘失败日志告警，内存继续保持安全状态。
### Checking
- 先加入正确行为永久回归并证明修复前失败；真实 state dir+fake conn 覆盖空闲/显式/销毁/枚举退役、重启别名碰撞、精确身份恢复、旧日志、轮转、瞬时错误、延迟审计与别名复用；定向 race 重复与配置全量检查。
- 全量：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。测试隔离 HOME/缓存/临时目录，不依赖真实外网。真实 Chrome/Docker/systemd 未验必须记录。

## factory · 2026-11-22 03:58

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a；复用上次遗留的干净 FT10 工作树，无未提交改动或额外提交；未合并 stop/FT1。Claude Code 构建提交：f708b1b。

实现：Store 以完整 target id 为 key，专用同步 managed_tab_open/managed_tab_retire 控制成员；daemon 的创建、关闭、空闲、销毁/成功枚举清理接入。异步 RPC 审计带 target，仅精确刷新已有成员的单调活动时间，不能增删成员；轮转快照保存完整身份。reconcile 仅恢复仍存活的相同 target，并退役已消失成员；启动枚举失败不执行空集合对账。README/AGENTS 更新。

决定：旧日志只有短 id 的记录一律不恢复并 warn；升级前仍开着的旧受管 tab 将不再自动空闲回收，可手动关闭。写盘错误返回并 warn，内存仍登记/退役；未落盘条目可能在重启前残留，但身份校验防止接管不同 target。无永久墓碑。ReplayManagedTabActivity 保留签名，新增 ManagedTabs 用于完整身份对账。

测试：coder 报告修复前永久回归覆盖的 7 类缺陷失败，含空闲后残留、重启接管/误关同别名用户 tab、销毁/枚举/显式关闭、身份/旧记录/轮转及延迟审计；修复后通过。coder 定向 -race -count=30 通过，并加入真实异步 audit 并发（8×20轮）、轮转和写盘失败 store 回归。manager 独立再次执行配置全量 gofmt/vet/race 通过（多数包命中缓存），Go 1.27.4，8 个测试包通过、2 个无测试包；git diff --check 通过，保护路径无变化，工作树干净，提交范围仅本项。

安装：无。使用已有 mise Go 1.27.4，模块按 go.sum 恢复，go.mod/go.sum 未变。测试 HOME、Go 缓存与所有临时文件均隔离在 run tmp 下，运行结束清理。

未检查：真实非 headless Chrome/CDP、Docker、systemd 均未验证；全部 offline fake conn。真实 connectBrowserLocked 枚举失败分支未直接单测；daemon 写盘失败 warn 无专门断言（store 错误返回有测）；旧版本降级读取新记录仅代码推断未实测。依赖新 API/字段的追加回归无修复前运行记录。

已知范围边界：FT11 的陈旧枚举与 tab_new 竞争仍可把新建 tab 判为消失，失去自动回收；原来已丢内存登记，本次也会退役持久成员。未在本分支修复，review 需关注同步登记/退役之间的生命周期竞争和写盘失败语义。

状态：代码分支已推送；new/ → review/，第 1/3 轮。本次仅 build，未 review、未合并 main、未发布。

## factory · review · 2026-11-22 04:08
head: f708b1b2debcd711f8f172d0f90938a35f830267
verdict: fix
fix:
- [P1] internal/daemon/tab_idle.go:160: reconcileIdleFromLog 以早先枚举结果核对稍后读取的 ManagedTabs；注册表同步结束后、读取受管集合前成功的 tab_new 仍存活于 Chrome/注册表，却被删除 idle 跟踪并持久化退役，重启也无法恢复。本变更扩大 FT11 竞争影响；相同时序探针基线通过、HEAD 失败。应在发起枚举前固定本轮可退役的完整 target 候选集合，仅对这些候选判定缺席，加入永久时序回归。
notes:
- [P2] internal/daemon/v1.go:485: targetCreated/targetDestroyed 均先于 tab_new 处理创建结果时，销毁因尚无 store 成员未能退役，迟到登记又持久化已销毁 target；应协调在途创建与销毁，使确认销毁优先于迟到登记。
- [P2] internal/daemon/v1.go:245: 成功响应阶段重新解析短别名可将 A 的 goto 归到复用别名的 B；tab_focus 也可能因多余 params.tab 刷新错误成员。应在实际解析目标时保存完整身份，审计与 idle 共用，focus 使用返回目标。
- [P2] internal/daemon/tab_idle.go:102: 重启回放后注册表尚未填充时，销毁事件因 RemoveTarget 未命中直接返回；应始终按完整 target 幂等退役，仅别名清理依赖注册表命中。
- [P2] internal/daemon/tab_idle.go:196: 显式关闭得到 No target with given id found 后未退役已确认消失成员；共用路径应识别 ErrorTargetGone 并退役完整身份，可保留 RPC 原错误。
- [P2] internal/store/store.go:374: 同步登记吞掉轮转失败；旧日志已移至 .1、创建新日志失败时 RegisterManagedTab 仍返回 nil，重开丢失存活身份。应先准备可恢复快照再替换，并将失败返回生命周期调用者。
- [P3] internal/daemon/tab_idle.go:72: 生命周期写入 ErrClosed 仅 debug；应按约定 warn，或关闭 store 前等待生命周期写入结束。

Codex 只读审查 origin/main...HEAD；配置完整 gofmt/vet/race 在 HEAD 与基线均通过（各8个测试包、2个无测试包），新增并发生命周期测试重复60次通过。临时 overlay 定向探针复现上述生命周期、审计与轮转问题；P1 已对照基线确认新增影响。git diff --check 通过，工作树干净、保护路径无差异。使用已有 mise Go 1.27.4，仅恢复固定模块，无新增安装。真实 Chrome/CDP、Docker、systemd 未验证；ErrClosed 日志级别为静态核对；未发现可用 review-code skill。

第1/3轮发现P1，review/ → new/，下一 build 按 fix 列表修复；P2/P3为非阻塞审查记录。本次未修改代码、未合并 main、未发布。无 source 或 owner 签名关联 issue/PR，未发 GitHub 评论。


## factory · plan · 2026-11-22 11:09

第2轮补充：在发起会触发受管对账的 CDP 枚举前固定完整 target 退役候选，仅对这些候选判定缺席。启动恢复、dirty 重试共用约束；失败枚举不退役，空库安全，保持完整身份与启动 grace。不持锁跨 CDP，不扩展非阻塞 P2/P3。合并最新 main，保留 FT11 已上线枚举 token 保护及 FT18 日志边界修复。永久回归覆盖枚举在途创建、同步后/对账前创建、正常缺席、失败、重启与轮转，执行定向 race 重复和全量配置检查。

## factory · 2026-11-22 11:09

第2/3轮修复 latest review 唯一 P1。Claude Code 提交：aff9a50 合并 origin/main（7223e5a）；26f38df4dcb57dd16ed375ef2efdd81a90311890 实施修复。合并解决 tab_idle、store、测试与文档冲突，保留双方意图；base 既有其他任务变化不作本项新增工作。

实现：beginManagedReconcile 在 CDP 枚举前固定完整 target 候选，reconcileIdleFromLog 显式接收候选，只退役候选中缺席者；启动与 dirty retry 均接入。恢复仍读取当前 managed 集合，避免重新跟踪已退役条目；nil 候选不退役。无额外后台层或跨 CDP 锁，README/AGENTS 说明同步更新。非阻塞 P2/P3 保持未处理，供后续 review 核对。

测试：coder 在合并后、修复前看到既有 TestDirtyResyncWithStaleSnapshotKeepsNewTab 失败；新增回归依赖新 API，采用临时恢复旧缺席判定的变异验证，两个时序、轮转、空库及真实 Session dirty 重试失败，恢复修复后通过。永久回归使用真实 state dir、fake conn 与本地 CDP stub，覆盖新 target 持久身份/idle/重启恢复及旧候选正常退役。coder 定向 daemon -race -count=40，store/state -race -count=20 通过，全量无缓存 race 通过。manager 独立再执行配置 gofmt/vet/go test -race ./... 通过（10个测试包、2个无测试包）；diff --check 通过，工作树干净，保护路径相对原始基线无改动，提交范围仅本项及 main 合并。合并中间提交单独仍有该 P1，最终 HEAD 已修复。

安装：无新增工具或非固定依赖；已有 Go 1.27.4，仅按 go.sum 恢复模块。main 带入的 manifest/lock 变化保留原样，本轮不另改。测试 HOME、Go 缓存与临时产物隔离在 run tmp，结束清理。

未检查：UI、真实非 headless Chrome/CDP、Docker、systemd 均未验证。connectBrowserLocked 候选接线没有专门阻塞时序测试（启动持有 tabMu 阻止 tab_new 交错），分步测试及既有启动测试通过。真实 Session stub 未注入 getTargets 失败，该分支使用 fake conn/syncInitialTargets 覆盖。

状态：分支已推送；new/ → review/，第2/3轮。仅 build，未进行 review、未合并 main、未发布。

## factory · review · 2026-11-22 11:18
head: 26f38df4dcb57dd16ed375ef2efdd81a90311890
verdict: pass
notes:
- [P2] internal/daemon/tab_idle.go:206: ManagedTabs 快照读取后若 tab_close 完成，恢复循环仍能重新加入已关闭 target 的 idle 跟踪；探针 HEAD 为 browser=0 persisted=0 idle=true，main 为 idle=false。应按完整 target 串行化恢复与退役；磁盘退役仍有效。
- [P2] internal/daemon/tab_idle.go:307: 清扫器按完整 target 检查过期，却在关闭时重新解析短别名；检查后销毁旧目标并复用别名可关闭用户 tab。HEAD 与 main 均复现，属既有窄窗口竞争；应关闭已检查的完整 target。
- [P2] internal/daemon/v1.go:481: 创建与销毁事件均早于 tab_new 返回处理时，迟到登记仍持久化已销毁 target；应使确认销毁优先于在途创建登记。
- [P2] internal/daemon/v1.go:244: 成功响应按请求短别名重新定位审计身份；tab_focus 返回5678但多余 params.tab=1234可记录错误 target，成功 tab_close 审计缺 target。应在实际操作解析时固定完整身份，focus使用返回目标，close在删映射前保存身份。
- [P2] internal/daemon/tab_idle.go:126: 启动回放后注册表未填充时，销毁事件因 RemoveTarget 未命中而不退役持久成员；应总按完整 target 幂等退役。
- [P2] internal/daemon/tab_idle.go:234: 显式关闭收到 No target with given id found 仍保留 managed；应识别 ErrorTargetGone，退役完整身份并保留原RPC错误。
- [P2] internal/store/store.go:385: 轮转快照写入 ENOSPC 仅 warn，RegisterManagedTab 返回nil而重开 managed=0；应先准备可恢复快照再替换，并向生命周期调用者返回失败。
- [P3] internal/daemon/tab_idle.go:74: 生命周期写入 ErrClosed 仍仅debug；应按约定warn或在关闭store前等待生命周期写入完成。

Codex 只读审查 origin/main@7223e5a...HEAD 全部13个变更文件。上轮唯一P1已修复：启动与dirty retry均在CDP枚举前固定可退役完整target候选，保留FT11枚举token。相同时序探针在修复前合并提交aff9a50得到 browser=1 idle=false managed=0，最终HEAD为 browser=1 idle=true managed=1。无P0/P1，以上均为非阻塞项；未因诊断探针预期失败而将配置测试误判为失败。

配置完整 gofmt/vet/go test -race ./... 通过（10个测试包、2个无测试包）；daemon定向race重复30次、state/store定向race重复20次通过，含FT11/FT18回归。诊断问题除ErrClosed静态核对外均有独立探针。manager确认工作树干净、diff --check通过，远端跟踪head未变；分支与main合并干净。未找到review-code skill。

安装：无新增工具/非固定依赖，使用现有Go1.27.4和固定模块；测试HOME、缓存、overlay与基线快照均隔离在本run tmp并清理。未验证真实非headless Chrome/CDP、Docker、systemd。无source或owner签名关联issue/PR，无GitHub评论。

第2/3轮审查通过，review/ → ready/；owner请关注上述生命周期、别名竞争及写盘失败的非阻塞风险。本次未修改代码、未合并main、未发布。


## factory · 2026-11-22 12:37
owner 要求将 ready 项全部 ship；本项已合并并推送 main。

merge: 2141d3b8cf66ecfcaba04e59b914ffa9945c3dc5
main: e63a15c1c8a64c3c1b6a501d965bf17a65025bd3
tag: 无；按 ship=[merge,push]，未打 tag、未触发 tag 发布。

验证：每项合并后均执行完整 gofmt、vet、无缓存 go test -race -count=1 ./...；最终累计 main 再次通过（11个测试包、1个无测试包）。manager 核验审查 head 与远端分支一致，所有批准分支均为最终 main 祖先；保护路径无差异，diff --check 通过、工作树干净。FT19/FT20 冲突由编码代理解决，保留完整 target 生命周期、无 RPC 历史保留、日志换行恢复及私有权限语义，相关测试适配后通过。

关联 issue/PR：无 source 或 owner 明确关联，无关闭对象。分支保留：配置不含 clean。安装：无。测试使用已有 Go1.27.4，HOME/缓存/TMPDIR 隔离、固定模块离线恢复。真实 Chrome/CDP、Docker、systemd 未验证；既有审查非阻塞 notes 未额外修复。

ready/ → done/。FT28 等非 ready 项不在本次范围。
