# [P3] network_route 同步 CDP 失败后规则仍留在内存表中，没有回滚

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B16；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/browser/network_routes.go:104`（基线行号，实施前重新定位）。

触发条件：network_route 调用期间 CDP 同步失败：tab attach 失败、Fetch.enable 报错/超时、或客户端断开导致请求 context 取消。

实际行为：AppendNetworkRouteContext 先把规则写入 rs.rules（network_routes.go:103-105）再 syncRoutes；失败时 RPC 返回 `network_route failed`，但规则保留：NetworkRouteCount 为 1，客户端重试会得到重复规则，之后任一次成功同步会把这条“失败”的规则一并下发生效。RemoveNetworkRoutesContext（:122-141）同样先改表后同步；规则清空分支（:170-176）即使 Fetch.disable 失败也会 stopListening。

期望行为：RPC 返回错误时路由表保持调用前的状态（或明确标记未生效），routes 计数与浏览器实际拦截配置一致。

影响：调用方看到失败却在之后被静默拦截/mock；routes 计数虚高；删除路径失败时可能留下浏览器侧仍启用 Fetch 而本地已无监听者的状态（请求被挂起，需真实 Chrome 确认）。

根因：先改内存状态后做 CDP 同步，失败路径没有恢复旧值。

修复方向（建议，非指定实现）：syncRoutes 失败时恢复调用前的规则切片（Append/Remove/Set 三处）；规则清空分支仅在 Fetch.disable 成功后才停止监听。

回归验收：browser 测试：用已取消的 context 调 AppendNetworkRouteContext 返回错误后 NetworkRouteCount==0；注入 routeCDP/同步失败时 Remove 后规则数不变；成功路径既有测试保持通过。

证据：repro-go.log（TestAudit_FailedNetworkRouteMustNotLeaveRuleRegistered FAIL）；repro/browser_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
