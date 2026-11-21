# [P1] `bb-browser run SCRIPT.js --name value` 被 cobra 当作未知 flag 拒绝，文档化的命名参数形式全部不可用

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B02；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P1。位置：`cmd/bb-browser/main.go:945`（基线行号，实施前重新定位）。

触发条件：按 README.md:333、skills/bb-browser/SKILL.md:50、references/script-system.md:40 的写法执行 `bb-browser run ./x.js hello --count 20` 或 `--count=20`。

实际行为：cobra 在 RunE 之前解析 flag，返回 `unknown flag: --count`，退出码 1；main.go:958-977 手写的 `--name value` / `--name=value` 解析永远到不了。只有未文档化的 `run x.js -- hello --count 20` 能通过。

期望行为：`--name value` / `--name=value` 透传给 adapter，成为 args.name；全局 flag（--json/--tab/--url/--api-token）仍可用。

影响：adapter 的命名参数（两份 skill 文档和 README 的主要用法、所有带 flag 的示例）完全不可用；按文档操作的 agent/用户只会得到 unknown flag。

根因：run 子命令只设置了 Args: cobra.MinimumNArgs(1)，没有 DisableFlagParsing 或 FParseErrWhitelist.UnknownFlags，未知 flag 在进入 RunE 前即报错。

修复方向（建议，非指定实现）：给 run 命令设置 FParseErrWhitelist{UnknownFlags:true} 并从 os.Args/cmd.Flags().Args() 之外取回原始 token，或 DisableFlagParsing=true 后手工处理全局 flag；保持 `--` 分隔写法兼容。

回归验收：CLI 测试：fake daemon 记录 eval 脚本；`run x.js hello --count 20` 与 `--count=20` 均成功且脚本参数 JSON 含 "count":"20"；`run x.js --json hello`、`--tab t1` 仍按全局 flag 生效；缺值的 `--count` 仍报 `flag --count needs a value`。

证据：repro-go.log（TestAudit_RunAcceptsDocumentedNamedArgs FAIL）；repro/cli_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
