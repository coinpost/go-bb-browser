---
branch: factory/FT10-persist-managed-target-retirement
round: 1
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
