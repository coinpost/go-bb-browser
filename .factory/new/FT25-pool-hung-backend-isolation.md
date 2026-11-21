# [P3] Pool.TabList/TabFocus 顺序轮询后端且无单后端超时，一个挂起的 daemon 使整个调用失败

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B19；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`pkg/daemonclient/pool.go:324`（基线行号，实施前重新定位）。

触发条件：池中靠前的某个后端接受连接但不应答（主机假死、网络黑洞、daemon 卡在长操作），调用方带截止时间调用 pool.TabList / pool.TabFocus。

实际行为：callTabList（pool.go:324-343）与 callTabFocus（:361-）在调用方 context 上依次请求各后端；挂起的后端耗尽全部截止时间后，下一轮循环的 ctx.Err() 直接返回 `context deadline exceeded`，健康后端从未被询问（复现中后端 B 的 tab_list 调用数为 0）。没有截止时间时则无限期阻塞（默认 http.Client 无超时）。

期望行为：AGENTS.md：TabList 查询每个后端，“a down backend is skipped”，仅全部失败才返回 *AllFailedError；一个无响应后端不应阻止返回其余后端的 tab。

影响：单个 daemon 假死即让池级 tab 列表/焦点查询全部超时，依赖它做巡检或选 tab 的调用方整体不可用。

根因：多后端聚合是串行的，并共享调用方的整个超时预算。

修复方向（建议，非指定实现）：并行查询各后端（或为每个后端划分子超时），收集在截止前返回的结果；超时的后端计入 lastErrs 并被跳过。

回归验收：pool 测试：后端 0 永不应答、后端 1 正常，500ms 截止的 TabList 返回后端 1 的 tab 且 err==nil；TabFocus 返回后端 1 的焦点；全部挂起时返回 *AllFailedError 或 ctx 错误；合并顺序仍按构造顺序。

证据：repro-go.log（TestAudit_TabListHungBackend FAIL）；repro/client_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
