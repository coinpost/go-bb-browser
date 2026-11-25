---
branch: factory/FT6-run-cli-option-boundaries
round: 1
---
# run帮助布尔参数与选项终止符兼容

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] cmd/bb-browser/main.go:1036：run adapter.js --help=true仍发eval；统一布尔帮助参数，请求帮助时零RPC。
[P3] cmd/bb-browser/main.go:956：run -- --foo.js错误拒绝存在脚本；保留--选项终止边界。

来源：FT1 第1轮起review notes。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
按此次 owner 授权记录顺序与收窄范围，本项仍留在 backlog/，优先级最低。须在前置 FT7（ready 后 ship 并成功打 tag）以及 FT4、FT2、FT3 均已交付之后，才移入 new/，不启动本次 build。

收窄后范围：低优先处理 run 的帮助布尔参数（如 adapter.js --help=true 仍发 eval，请求帮助时应零 RPC）以及 -- 选项终止后以 -- 开头的脚本路径（如 run -- --foo.js）。可与已交付 FT9 审查 factory · review · 2026-11-22 04:31（head 8004cbd723455c73fd843f07ac0f9c5be43f4468）所记残留帮助/分隔符兼容问题对齐，但不把 FT9 当作本项已修好。

依赖顺序：FT7 ready → ship 并 tag → FT4 → FT2 → FT3 → 本项。

## factory · 2026-11-25 14:16
前置 FT3 已合入远端 main（merge c5d25a1bfb25fa9e7e557b2c148847812a2495d4，已审 head 74345e9e2f00208515199d8b89a77974bde76a1a）。按既有顺序将本项从 backlog/ 移入 new/，等待外部工厂 build。本 run 不启动 FT6 的开发、build 或 review。

保留此前收窄范围：低优先处理 run 的帮助布尔参数（如 adapter.js --help=true 仍发 eval，请求帮助时应零 RPC）以及 -- 选项终止后以 -- 开头的脚本路径（如 run -- --foo.js）。可与已交付 FT9 审查 factory · review · 2026-11-22 04:31（head 8004cbd723455c73fd843f07ac0f9c5be43f4468）所记残留帮助/分隔符兼容问题对齐，但不把 FT9 当作本项已修好。FT5 保持 stop。

## factory · plan · 2026-11-25 14:19
### Behaviour
- run 在第一个 -- 之前遵循 pflag 的布尔帮助语义：--help/-h 及显式真值只打印帮助，零 RPC；显式 false 正常执行，非法布尔值本地失败且零 RPC，重复值按最后一次生效。
- 第一个 -- 结束全局选项解析；如果尚未取得脚本路径，紧随其后的 token 原样作为脚本路径，包括 --foo.js；拿到脚本后继续现有命名参数规则，保留名属于脚本。
- 缺少/空白脚本、缺少参数值、文件不存在等仍在 RPC 前失败；帮助不要求脚本存在，不更改其他命令。
### Scope
- 仅 run 解析器、对应永久回归与 run 使用说明；不涉及 daemon、tab 身份、FT21 或其它任务。
### Decisions
- 使用现有 pflag 布尔解析和值覆盖顺序，不引入另一个布尔语法或大规模重构；核对 Cobra 对 DisableFlagParsing 下帮助处理，覆盖 --help=true、--help=false 与 -h=true/-h=false。
- -- 之后只有尚未设置的首个脚本路径放宽双横线前缀；其后的适配器参数维持 --name 必须有值规则，不扩展无值布尔开关。
### Checking
- 解析器单测与 root.Execute 集成永久回归：真/假/非法布尔帮助、重复覆盖、缺少/不存在脚本、-- --foo.js 实际文件执行、分隔后帮助保留名传给脚本；帮助与错误断言零 HTTP/RPC。
- 执行配置完整测试：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；测试使用隔离 HOME/XDG/cache，无真实外网；无需 Chrome，不操作现有窗口。
- 检查文档与实际边界一致；本项为 CLI，无图形 UI，未做真实 Chrome 验收。

## factory · 2026-11-25 14:26
完成第 1 轮 build，head: 64d3536，代码分支已推送，new/ → review/。

实施前核验：当前 main 上 --help=true 已零 RPC 打印帮助，--help=false 已正常执行；历史长写法 P2 不再复现，补永久回归而非声称重新修复。仍存在的 -h=BOOL 被当作位置参数、以及 run -- --foo.js 无法读取实际文件已修复。

实现：-h=VALUE 交由既有 pflag 校验布尔值；第一个 -- 之后尚未给出脚本时，首个 token 原样作为路径；其后保留既有命名参数规则。重复布尔值按顺序校验、最后一个有效值决定结果（前面的非法值不会被后面的合法值掩盖）。仅 run 解析器、测试和使用说明有变更，未涉及 daemon、FT21 或短 ID 支持。

永久回归覆盖解析器、root.Execute 帮助真/假/非法值、缺少或不存在脚本、真实 --foo.js 等文件、分隔后的保留参数；帮助及非法输入均断言零 HTTP 请求，false 正常 eval。撤回解析器修改的反向验证有 26 个新增子用例失败，恢复后通过。

完整配置测试由 Claude Code 执行通过，manager 独立复跑亦通过：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。测试使用一次性 HOME/XDG/cache 与离线依赖，无真实外网。

安装：无新增工具或依赖；go.mod/go.sum 未变。

未检查：真实 Chrome/真实 daemon 验收、编译后二进制经 shell 调用、Windows 特殊文件名、run 之前根级帮助参数的新回归；未查阅 FT9 历史审查原文。本项仅 CLI，无图形 UI 改动，未做图形 UI 人工检查。HTTP/RPC 行为使用 httptest 假 daemon 核验。

本次仅 build，不合并 main、不打 tag、不发布、不启用任何暂停任务。
