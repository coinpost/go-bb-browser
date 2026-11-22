---
branch: factory/FT27-rpc-timeout-contract
round: 1
---
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

## factory · plan · 2026-11-22 09:22

### Behaviour
- 单次 daemon RPC/CDP 工作上限保持 60 秒；CLI 不再为 eval/fetch/snapshot/screenshot/goto/run 等承诺 120/180 秒的有效服务端预算。
- run 可包含多个 RPC，文档区分整条命令预算与单个 RPC；本任务采用统一保守 60 秒命令预算，避免让 adapter 作者以为单次 eval 有 180 秒。
- 空输入、首次使用、正常成功和原有错误语义保持；超时提示明确单次操作上限，较早的调用方取消仍优先生效。
### Scope
- 共享超时常量、CLI 使用、必要的 daemon 超时提示、adapter 文档与一致性/阻塞连接回归。保留现有 daemon/CDP 上限和 HTTP WriteTimeout 的合理 flush 余量。
- 不延长服务端操作，不修改网络超时架构、协议、发布配置或保护路径，不合并 stop/FT1。
### Decisions
- 选择任务明确允许的“文档与 CLI 统一 60s”路线：实际服务能力不变，最小风险；已有更短的健康检查/本地操作期限无需扩大。
- 常量作为 Go 内唯一预算来源；文档通过永久测试校验，不引入生成器或新依赖。
### Checking
- 先增加正确行为断言并证明修复前失败，再修复；fake Eval 连接检查上下文实际截止，与较早取消行为，不让测试真实等待 60 秒。
- 覆盖 CLI/run 与 daemon/CDP 常量一致性、文档秒数、错误提示与 HTTP 写出余量；执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。
- 不访问真实外网；真实 Chrome、Docker、systemd 如未运行如实记录，不使用 headless 或操作用户窗口。

## factory · 2026-11-22 09:26

构建基线：origin/main@4f86c1c；提交：661f9df；分支：factory/FT27-rpc-timeout-contract。未合并 stop/FT1。

实现与决定：采用任务允许的保守方案，daemon/CDP 单 RPC 60s 上限不变。OperationSeconds 为秒数来源，CLI 原 60/120/180s 统一引用 Operation；原 30s 短操作不扩大。run 整条命令含选页/开页等待累计 60s，实际 eval 剩余预算可能更短，文档已明确。CLI 与 daemon 超时提示说明 60s 上限；保留较早取消、HTTP WriteTimeout 及 flush 余量。同步 README、AGENTS、adapter SKILL 与 patterns。

回归：coder 先加不改变行为的秒数常量以使测试可编译，再确认 CLI 多命令截止、run 长预算、超时提示、文档数值断言修复前失败，修复后通过；较早取消、写出余量原已通过，不作为修复证据。新增永久测试通过 -race -count=20。fake Eval 读取真实上下文截止并模拟超时；取消测试实际等待 ctx.Done，不真实等待 60s。

检查：coder 与 manager 分别通过配置全量命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`；manager 部分包命中缓存。git diff --check 通过；保护路径无变更，提交仅含本项，工作树干净。Go 1.27.4，测试 HOME/TMPDIR/缓存隔离，无真实外网依赖。

安装：无；go.mod/go.sum 未变，复用既有已 pin 模块。

未检查：真实非 headless Chrome/CDP、真实 60s 超时及 watchdog 端到端、Docker、systemd 均未验证；browser runCDPWithContext 的原有 60s 层未新增测试。供 review 核对端到端预算及提示准确性。

状态：代码已推送，new/ → review/，round: 1。本次仅 build，未 review、未合并 main、未发布。

## factory · review · 2026-11-22 09:48
head: 661f9df605569304a0c86c9e16b0522f46e715ac
verdict: pass
notes:
- [P2] internal/browser/session.go:160: requestCtx 到期经 context.AfterFunc(cancel) 转成普通取消。真实本地 HTTP/chromedp/CDP 替身诊断中，附加标签页延迟 300ms 后，60.004s 返回 eval failed / context canceled，缺少 60s 提示；应保留到期与主动取消区别，并覆盖 runCDPWithContext。
- [P2] cmd/bb-browser/main.go:1146: run 开页后的 time.Sleep(3s) 不监听期限；虚拟时钟中 tab_new 第59秒返回后到第62秒才报错。第185行 open --wait 65 也可第65秒成功，与整条命令预算表述不符。等待应监听 ctx.Done 并补近期限回归。
- [P2] internal/daemon/v1.go:1243: resolveTab 丢弃枚举错误，Target.getTargets 到期可返回 -32602 unknown tab id；应向上传递枚举错误，避免将超时报成无效ID。
- [P2] skills/bb-browser-adapter/references/patterns.md:225: 文档承诺直接中断页面脚本，但 Runtime.evaluate 超时只停止等待，不保证终止已启动的异步任务；此处及 SKILL.md:18 应明确页面任务可能继续、有重复副作用风险。
- [P2] cmd/bb-browser/main.go:48: 统一60s提示未表达实际期限；network clear 实际30s或更短父期限也提示60s，health GET到期无时长提示。应按实际预算及取消原因提示，覆盖GET和响应体读取。
- [P3] internal/daemon/rpc_connections.go:89: tab锁等待耗尽RPC期限仅提示 another operation is still running for this tab；应保留竞争原因并注明到期预算。
- [P3] internal/timeout/timeout_test.go:29: 文档一致性测试未校验新增的 AGENTS.md:33 60s 声明，应补 Operation budget 段落校验。
- [P3] internal/daemon/operation_timeout_test.go:56: newBudgetServer 未关闭audit writer，goroutine持续等待auditCh；建议注册 t.Cleanup(closeAudit)。

审查：Codex 对 origin/main...HEAD 只读审查；无 P0/P1，依规则通过。审查基线 origin/main@7223e5a，三点差异基点4f86c1c。git merge-tree 验证与当前main无冲突，无需改分支。六个目标命令共用 Operation，短操作保留，HTTP写出覆盖读取/操作/flush余量。以上 P2/P3 均为非阻塞意见，并不表示所有60s契约细节均已正确。

检查：配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全通过（Go 1.27.4，隔离HOME/缓存）。额外 go test -overlay 临时诊断覆盖等待、短操作、枚举与锁，并用本地HTTP/WebSocket进行两轮真实60s等待。manager复核HEAD、工作树干净及diff --check。代码无修改、无新提交；安装：无。

未检查：真实Chrome中的超时后副作用、watchdog端到端恢复、Docker/systemd实机。未访问真实外网，未操作用户窗口。

状态：review/ → ready/；未合并main、未发布。请owner关注上述非阻塞超时语义与文档问题。
