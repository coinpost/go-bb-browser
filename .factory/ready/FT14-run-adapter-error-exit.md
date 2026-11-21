---
branch: factory/FT14-run-adapter-error-exit
round: 1
---
# [P2] `bb-browser run` 在 adapter 返回 { error } 时退出码为 0

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B07；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`cmd/bb-browser/main.go:1009`（基线行号，实施前重新定位）。

触发条件：adapter 按文档约定返回 `{ error: "HTTP 401", hint: "…" }`（skills/bb-browser-adapter/SKILL.md:176 要求错误都用该形式返回）。

实际行为：printSiteResult（main.go:1017-1035）只向 stderr 打印 `[error] …` 后返回，RunE 返回 nil，进程退出码 0，stdout 为空。

期望行为：adapter 报告错误时命令以非零退出码结束（stderr 输出保持不变），使脚本/agent 能用退出码判断失败。

影响：`bb-browser run … && 下一步`、CI、上层 agent 都会把失败当成功继续执行，且 stdout 为空时难以与“无数据”区分。

根因：printSiteResult 没有返回值，run 的 RunE 在其后无条件 return nil。

修复方向（建议，非指定实现）：让 printSiteResult 返回 error（或 bool），adapter 结果含非空 error 字段时 RunE 返回错误（可用 SilenceUsage 避免再打印 usage）；--json 模式保持输出包络，退出码建议同样非零或在文档中明确。

回归验收：CLI 测试：fake daemon 的 eval 结果为 {"error":"HTTP 401","hint":"please log in"} 时 root.Execute() 返回非 nil、stderr 含 `[error] HTTP 401` 与登录提示；结果为普通对象时返回 nil 且 stdout 为缩进 JSON。

证据：repro-go.log（TestAudit_RunAdapterErrorMustFailTheCommand FAIL）；repro/cli_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。


## factory · plan · 2026-11-22 05:08

### Behaviour
- run 的 eval 成功但 adapter 返回非空字符串 error 时，普通与 --json 模式均返回错误、进程非零退出；普通模式保留现有 [error]/Hint/登录提示，stdout 为空；JSON 模式 stdout 保留原始 JSON-RPC 包络。
- 正常对象、数组、标量、null 及空字符串/非字符串 error 保持现有成功语义与格式；RPC/网络/文件错误继续按原路径报告。
- 不为 adapter 业务失败打印 usage；避免同一业务错误重复输出，保留现有 main/Cobra 其他命令行为。
### Scope
- 仅 run 结果分类/输出/退出状态、相关 README/adapter 契约说明及永久 CLI 回归。不改参数解析（FT9）、脚本包装（FT23）、daemon、保护路径或 stop/FT1。
### Decisions
- 非空字符串 error 作为失败判据，与现有显示行为一致；--json 同样非零退出，不将 JSON-RPC 成功等同 adapter 成功。
- 采用最小明确错误传播与局部抑制重复打印方案，不修改全局错误处理策略。
### Checking
- 先新增正确行为永久回归并证明修复前失败：fake daemon 的 HTTP 401 adapter 结果令 Execute 返回非 nil，stderr 含原错误、hint 和登录提示，无 usage/重复错误；正常对象成功且缩进 JSON。
- 覆盖 --json 原包络+错误状态、无 hint/非登录错误、空及非字符串 error、数组/标量/null、RPC 错误不回退成成功；尽量增加真实子进程非零退出验收。
- 完整配置 gofmt/vet/race，定向重复 -race；隔离 HOME、缓存和临时目录，不依赖真实外网。真实 Chrome/Docker/systemd 未验必须记录。

## factory · 2026-11-22 05:08

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a；从最新 origin/main 独立构建，未合并 stop/FT1。Claude Code 提交：af719eb（修复 bb-browser run 在 adapter 返回 { error } 时退出码为 0）。

实现：run 对 adapter 顶层非空字符串 error 返回明确 adapterError；普通模式保留原有 [error]/Hint/登录提示，stdout 为空；--json 保留 stdout 原始 JSON-RPC 包络并同样返回非零。仅 adapter 失败局部抑制 Cobra usage/重复错误，main 不重复打印已经报告的 adapterError；其他命令与 RPC 错误路径不改。README 和 adapter 技能文档说明输出与退出状态。

决定：--json 失败也在 stderr 输出一次相同诊断；空字符串、非字符串 error 和数组内 error 保持成功语义；退出码沿用 1。未修改参数解析或脚本包装。

永久回归：新增 run_test.go，以本地 httptest 假 daemon 验证普通/JSON adapter 错误、hint/登录提示、非登录错误、空/非字符串 error、对象/数组/标量/null、RPC 错误；测试二进制子进程调用真实 main，验证退出码 1 与成功 0。coder 先仅增加无行为的错误类型声明使测试编译，修复前 TestRunAdapterErrorFailsCommand 返回 nil、TestRunExitStatus 实测 0（预期 1）；修复后通过。修复前成功值/RPC 错误守护用例已通过。

测试：coder 配置全量通过，定向 go test -race ./cmd/bb-browser/ -run TestRun -count=3 通过；manager 独立再次执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过（Go 1.27.4，8 个测试包、2 个无测试包，部分缓存）。git diff --check 通过；保护路径无变更，提交范围仅本项，工作树干净。测试 HOME、缓存和临时目录均隔离在本 run tmp，无真实外网依赖。

安装：无；从已有用户模块缓存恢复固定版本，go.mod/go.sum 未变。

未检查：真实 Chrome/CDP+daemon 执行真实 adapter 端到端、Docker、systemd、Windows 均未验证。未启动或操作任何真实 Chrome/用户窗口。

状态：代码分支已推送，new/ → review/，第 1/3 轮；仅 build，未 review、未合并 main、未发布。


## factory · review · 2026-11-22 05:29
head: af719ebad2cd37cb5ed35b973f259af13926dbbd
verdict: pass
notes:
- [P3] cmd/bb-browser/run_test.go:89: captureFile 的读端在 io.ReadAll 后未关闭，每次 executeRun 留下两个描述符等待 GC；建议在读取 goroutine 中 defer r.Close()。Codex 在文件描述符上限 64、GOGC=off 的定向重复运行中复现 pipe: too many open files，不阻塞本次评审。
- Codex 审查 origin/main...HEAD：普通与 --json 的 adapter 错误均退出 1，普通模式 stdout 为空，JSON 保留原包络，stderr 错误/hint/登录提示各输出一次；成功值及 RPC 错误语义保持。未发现 P0/P1。
- 完整配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过（Go 1.27.4）；本地 fixtures、固定版本离线模块，HOME/缓存/临时文件在本 run TMPDIR 隔离。无额外安装、无代码修改，工作树干净；与 origin/main 可干净合并。
- 未验证真实 Chrome/CDP+daemon 端到端、Docker、systemd 或 Windows；未操作用户窗口。
- review/ → ready/，等待 owner 验收；未合并 main、未发布。
