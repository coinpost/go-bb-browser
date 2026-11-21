# [P2] Pool 的 tab_new 与 Health/Ready/Live 共用同一个轮询计数器，顺序调用方的 tab 会全部落到同一个 daemon

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B06；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`pkg/daemonclient/pool.go:428`（基线行号，实施前重新定位）。

触发条件：顺序调用方在每次 pool.TabNew 之间调用与后端数成整数倍关系的其它未绑定操作，例如 2 个后端时每次 TabNew 前先 pool.Health()；3 个后端时 Live+Ready+TabNew。

实际行为：order()（pool.go:426-442）对所有未绑定操作共用 p.rr；“最少在途”只统计进行中的 HTTP 调用，顺序调用时恒为 0，退化为纯轮询，健康检查吃掉了交替名额。复现：2 后端 20 次 TabNew 分布 [0 20]；3 后端 30 次分布 [0 0 30]。

期望行为：AGENTS.md / README 承诺的 even spread：新 tab 在健康后端间均匀分布，不受健康探测等其它调用影响。

影响：多 daemon 池退化为单 daemon：一个 Chrome 承担全部标签页，其余空闲；“先探活再开 tab”正是调用方最常见的写法。

根因：tab_new 的分配游标与健康检查/其它未绑定调用共享，且负载度量是瞬时在途请求数而非各后端已绑定 tab 数。

修复方向（建议，非指定实现）：为 tab_new 使用独立计数器，或按 p.tabs 中各后端已绑定 tab 数选最少者（并列再轮询）；Health/Ready/Live 不推进该游标。

回归验收：新增 pool 测试：2 后端交替 Health+TabNew 20 轮分布为 [10 10]；3 后端 Live+Ready+TabNew 30 轮为 [10 10 10]；原 TestPool_tabNewEvenDistribution 仍通过；一个后端 503 时其余健康后端差值 ≤1。

证据：repro-go.log（TestAudit_SpreadWithInterleavedHealth / …Health3 FAIL）；repro/client_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
