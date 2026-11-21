---
branch: factory/FT9-run-named-arguments
round: 1
---
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


## factory · plan · 2026-11-22 04:23

### Behaviour
- run 接受位置参数与 --name value / --name=value，保持值与位置顺序；全局 --json/--tab/--url/--api-token 在 run 前后或穿插仍有效。
- 首个 -- 后全部归 adapter，可使用同名参数；缺值、缺脚本、非法全局值在 HTTP 请求前报错。帮助与原有版本行为保持；空值、重复值、负数有明确文档。
### Scope
- 仅 run 局部参数处理、永久回归及相关参数文档；不改 adapter error 退出码、JS 包装、超时或其它任务，不合并 stop/FT1。
### Decisions
- run 使用 DisableFlagParsing，局部解析器保留任意 adapter token；查继承 flags 识别保留名，经 pflag 设置与校验，不读 os.Args，支持 SetArgs。
- 全局保留名优先，-- 后可传同名 adapter 参数；值原样保留，空值合法，第一个等号分割，同名最后一次胜出；单横线 token 为普通值。以 -- 开头的命名值用等号形式。
### Checking
- 先写永久正确行为回归证明修复前失败；fake HTTP daemon 捕获 eval 参数，覆盖两种命名形式、位置、全局 flags 前后穿插、JSON 布尔、token、分隔、错误和帮助；其它子命令仍拒绝未知 flag。
- 配置完整 gofmt/vet/race；测试隔离 HOME/缓存/临时文件，不依赖真实外网。真实 Chrome/Docker/systemd 未验明确记录。

## factory · 2026-11-22 04:23

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a；Claude Code 构建提交 8004cbd723455c73fd843f07ac0f9c5be43f4468，未合并其它 factory 分支。

实现：新增 run_args.go 局部解析器，run 关闭 Cobra flag 解析再分别处理 CLI 全局参数与 adapter 命名参数，不丢弃未知 token；pflag 保留校验与 Changed 标记。新增 run_test.go 集成和表驱动永久回归，README、adapter skill 与 script-system 参数说明同步。

决定：全局选项按继承 flag 集合识别而非硬编码；保留名在首个 -- 前优先。命名参数值原样传递，支持空字符串、等号、负数，同名最后胜出。缺脚本使用明确 usage 错误；空名或第二个裸 -- 报 flag needs a name，不再静默忽略。已知宽松：Cobra 也会把 run 前的未知 token 交给 run，因此 --count 20 run x.js 会作为 adapter 参数被接受，未用测试固定此宽松行为。

回归证据：coder 先只加测试，TestRunArguments 的7个子例失败（6个 unknown flag: --count，1个负数被当 shorthand）；错误路径8个子例失败。修复后新增59个子用例全部通过，帮助/版本原本通过且仍通过。原审计复现未直接运行，正确断言纳入永久回归。

测试：coder 配置全量通过；manager 独立再次执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过（多数包缓存），Go 1.27.4，8个测试包通过、2个无测试包。git diff --check 通过，工作树干净，提交范围仅本项，保护路径无变化。coder 还用构建二进制手查帮助、缺脚本、缺值与 run 后 --url。

安装：无；go.mod/go.sum 未变；使用已有 Go 与已 pin 模块缓存，无新增工具依赖。测试 HOME、Go 缓存、全部临时产物在 run tmp 内，结束清理。

未检查：真实 Chrome/CDP、真实 daemon 端到端、Docker、systemd 均未验证，仅 fake HTTP daemon；DisableFlagParsing 下 shell 补全未验。review 应关注 Cobra 根命令/帮助边界与分隔符语义。

推送说明：第一次代码推送的 credential helper 因未携 repository path 被拒；仅命令级添加 credential.useHttpPath=true 后重试成功，未修改用户 git 设置。

状态：代码分支已推送；new/ → review/，第1/3轮。本次仅 build，未 review、未合并 main、未发布。


## factory · review · 2026-11-22 04:31
head: 8004cbd723455c73fd843f07ac0f9c5be43f4468
verdict: pass
notes:
- [P2] cmd/bb-browser/main.go:954: run 前的未知根选项被当作 adapter 参数；`bb-browser --tba intended run x.js` 原本拒绝，现在会在焦点 tab 执行脚本。建议在根分派时校验 run 前的选项。
- [P2] cmd/bb-browser/run_args.go:73: 需值全局选项会吞掉裸 `--`；`run x.js --tab -- --count 20 --tab inside` 实际在 inside 执行，而非将分隔符后的 tab 传给 adapter。建议对分隔符前的 --tab 报缺值并调整现有单测。
- [P2] cmd/bb-browser/run_args.go:50: `run x.js -h=true --tab t1` 原本只显示帮助，现在在 t1 执行脚本。建议恢复 pflag 对 -h=<bool> 的解析。
- [P2] cmd/bb-browser/run_args.go:55: `run -- --x.js hello` 不再支持以 -- 开头的脚本路径。建议尚无路径时原样接收分隔符后的首个 token。
- [P3] cmd/bb-browser/run_args.go:86: `run x.js --help -- --count` 被 adapter 缺值校验拦截，原本帮助退出0。建议确定帮助后跳过 adapter 参数校验。
- [P3] cmd/bb-browser/run_args.go:64: 第二个裸 -- 从原本忽略改为错误；`run x.js -- -- --count 20` 不再成功。建议兼容原行为并补回归。
- [P3] cmd/bb-browser/main.go:954: Cobra __complete 中 `run --h` 丢失 --help 候选，而 `run x.js -- --j` 错误提示 --json。建议补全遵循同一分隔边界。

Codex 只读审查 origin/main...HEAD；未发现 P0/P1，按规则 review/ → ready/。以上 P2/P3 不阻断，但 owner 应查看错误执行目标、帮助及分隔符兼容性风险。无 review-code skill，使用 diff 和基线/HEAD 双版本 CLI 对照。无需冲突解决，代码未修改、未提交。

验收：两种 --count 写法在基线新增回归失败、HEAD 通过；fake daemon 捕获 count 为字符串20；缺值在 HTTP 请求前报 flag --count needs a value。配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过（Go 1.27.4，8包通过、2包无测试）；git diff --check 通过，manager 确认工作树干净、head 未变。

安装：无新增工具/依赖，manifest 未变。临时 HOME、缓存、复现产物位于本次 run tmp 并清理。未验证真实 Chrome/CDP、真实 daemon 端到端、Docker、systemd；补全仅验证 __complete 接口，未做交互 shell 按键测试。无 linked issue/PR，无需 GitHub 评论。本次未合并 main、未发布。


## factory · 2026-11-22 06:08
owner 授权：“先把 ready 的 ship 掉”。本次固定合并 FT8、FT9、FT11、FT12、FT14、FT15；review head 与所合并远端分支一致。

已合并并推送 main；本项 merge commit：082b535，本批 main 最终提交：8836949。配置 ship=["merge", "push"]，未打 tag、未触发版本发布、未部署。无 linked issue/PR 需关闭，分支按配置保留。

逐项合并后执行完整 gofmt/vet/race 测试，最终累计 main 配置测试再次通过（部分缓存）；git diff --check 通过，保护路径未改，工作树干净。FT12 的 go.mod 冲突及 FT14 的 README/run_test.go 冲突由 CC 仅解决整合冲突，保留双方依赖、文档与全部回归；FT9 与 FT14 测试拆文件并共存通过。无新增未 pin 工具或依赖。

review 中已有 P2/P3 未在本次 ship 修复，继续保留原记录。真实 Chrome/CDP、Docker、systemd 运行时未验证。ready/ → done/。
