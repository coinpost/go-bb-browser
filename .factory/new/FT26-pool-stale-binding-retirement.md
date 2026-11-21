# [P3] Pool 通过 tab_list/tab_focus 观察到的 tab 绑定只增不减，已消失的 tab 永久留在映射中

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B20；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`pkg/daemonclient/pool.go:605`（基线行号，实施前重新定位）。

触发条件：长生命周期的 Pool 周期性调用 TabList/TabFocus，而 tab 由池外途径关闭（空闲回收、用户手动关闭、其它客户端 tab_close）。

实际行为：bindObservedTabs（pool.go:605-613）把每次列表中出现的 tab 写入 p.tabs，从不删除该后端本次未再列出的 tab；绑定只会在本池成功 tab_close 或调用方显式 ForgetTab 时移除。复现：5 次列表各只含 1 个不同 tab，映射里留下 5 条，`ClientForTab("0:t0")` 仍为 true。

期望行为：某后端的 tab_list 成功返回即是该后端的权威集合：未再出现的绑定应被移除，之后对这些 id 的调用返回 *UnknownTabError。

影响：映射随浏览器 tab 流转无界增长；陈旧 id 的调用被发往 daemon 才得到 “unknown tab id” 的 RPC 错误，而不是本地的 *UnknownTabError，调用方的错误分支判断失效。

根因：观察式绑定只有添加路径，没有与后端权威列表对账的删除路径。

修复方向（建议，非指定实现）：在单后端 tab_list 成功后，删除该后端下不在本次结果中的绑定；与该后端并发的 tab_new 绑定需要用代次或短锁保护，避免把刚创建的 tab 误删。

回归验收：pool 测试：连续 5 次列表各含 1 个不同 tab 后映射大小为 1，旧 id 的 ClientForTab 为 false、调用返回 *UnknownTabError；并发 TabNew 与 TabList（-race）下新建 tab 不被误删；后端失败的那次列表不触发清理。

证据：repro-go.log（TestAudit_TabListDoesNotPruneStale FAIL）；repro/client_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
