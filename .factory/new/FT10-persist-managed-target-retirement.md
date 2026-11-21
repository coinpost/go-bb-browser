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
