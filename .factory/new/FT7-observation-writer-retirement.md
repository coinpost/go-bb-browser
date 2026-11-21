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
