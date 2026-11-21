# [P3] CLI 与 adapter 文档宣称 run 有约 180s 预算（eval/snapshot/fetch 为 120s），daemon 对每个 RPC 固定 60s 截止

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B21；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/daemon/v1.go:86`（基线行号，实施前重新定位）。

触发条件：adapter 或 eval/fetch/snapshot 单次执行超过 60 秒（翻页抓取、等待慢接口）。

实际行为：CLI 为 run 设 180s、eval/snapshot/fetch/screenshot/goto 设 120s 的客户端超时（cmd/bb-browser/main.go:947、324、293、392 等），skills/bb-browser-adapter/SKILL.md:18、:230 与 references/patterns.md:172、:225 写明“单次 run 超时约 180 秒”。但 daemon 在 v1.go:86 对所有方法套 timeout.Operation=60s（internal/timeout/timeout.go:6），runCDPWithContext 再套一次（internal/browser/session.go:159）。超过 60s 的脚本在约 60s 得到 `eval failed`（context deadline exceeded），并触发一次额外的 watchdog 探测。

期望行为：文档、CLI 超时与 daemon 实际截止一致：要么 daemon 对 eval/fetch 等长操作提供与 CLI 匹配的预算，要么文档与 CLI 统一为 60s。

影响：按文档把任务规划在 60–180s 内的 adapter 必然失败，且错误信息是泛化的 eval failed；CLI 的 120/180s 超时永远不会生效，误导排障。

根因：daemon 端单一全局操作超时与 CLI/文档各自声明的预算没有对齐。

修复方向（建议，非指定实现）：二选一：把文档与 CLI 超时改为 60s（并在超时错误的 hint 中说明上限）；或让 daemon 按方法设置上限（eval/fetch 180s）并同步调整 http.Server WriteTimeout。

回归验收：增加一致性测试：daemon 对 eval 的生效截止 ≥ CLI run 的超时减去余量（或文档/CLI 常量等于 timeout.Operation）；用 fake conn 让 Eval 阻塞到 context 结束，断言返回时间与声明的上限一致；文档中的数字由同一常量来源校验（grep 测试）。

证据：static-deploy-docs.log（“超时相关”一节的行号摘录）。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
