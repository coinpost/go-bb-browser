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
