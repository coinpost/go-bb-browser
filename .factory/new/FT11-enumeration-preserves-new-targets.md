# [P2] 过期的 Target.getTargets 快照晚于 tab_new 应用时，会把刚创建的受管 tab 从注册表和空闲跟踪中删除

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B04；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`internal/daemon/tab_idle.go:38`（基线行号，实施前重新定位）。

触发条件：并发：请求 A（tab_list / tab_focus / tab_select，或任何未知 tab 触发的 resolveTab 回退、runTargetSync 重同步）已取得不含新 tab 的 getTargets 结果但尚未调用 syncTabsFromTargets；请求 B 的 tab_new 在此间隙完成 createTarget 并登记。

实际行为：syncTabsFromTargets（tab_idle.go:38-45）无条件把注册表与快照对齐：SyncPageTargetsDetailed 把新 tab 记为 removed（internal/state/tab_registry.go:54-60），clearRemovedTarget 随即 forgetTabManaged、清观测缓冲、forgetTabLock，并对真实 Session 调 ForgetTarget（取消该 tab 的 chromedp 子 context、清路由）。复现中 tab_new 返回的短 id 4d5e 被移出注册表且不再受管。

期望行为：早于 tab 创建时刻取得的 target 快照不得移除更晚登记的 target；tab_new 返回成功的 tab 必须保持注册与受管状态，直到真正关闭。

影响：该 tab 在本进程内失去受管标记，永不被空闲回收（标签页泄漏，直到 daemon 重启靠 rpc.jsonl 恢复）；已启用的观测缓冲与拦截规则被清空；若该 tab 已建立 tabPool 连接，ForgetTarget 取消活跃 chromedp 子 context 会按其注释所述直接关闭该标签页。多客户端/Pool 并发 tab_new 与 tab_list 时是微秒级但持续存在的竞态。

根因：“读浏览器 target 列表”与“应用到注册表”之间没有版本/时序保护，注册表的增量登记（tab_new、target 事件）与全量对齐互相覆盖。

修复方向（建议，非指定实现）：给注册表加单调代次（每次 RegisterPageTarget/事件登记递增），getTargets 发起前记录代次，对齐时跳过代次更晚登记的 target；或把 getTargets+对齐与 tab_new 的 create+register 串行化在同一把锁内。

回归验收：barrier 测试：fake conn 的 PageTargets 在取得快照后阻塞，期间完成一次 tab_new，再放行；断言 tabs.Lookup(short) 成功、tabIdle.Snapshot() 含新 target、观测缓冲未被清；并覆盖 resolveTab 回退与 runTargetSync 重同步两条调用路径；-race 下 200 次并发 tab_new/tab_list 后所有 tab_new 返回的 tab 均受管。

证据：repro-go.log（TestAudit_StaleTargetListMustNotUnmanageNewTab FAIL）；repro/daemon_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
