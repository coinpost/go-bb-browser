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


## factory · review · 2026-11-25 14:31
head: 64d353672c8d07827461ee7880985f2c883a71a1
verdict: pass
notes:
- [P2] cmd/bb-browser/main.go:996: 分隔符后脚本路径在读取前仍被 TrimSpace，run -- '--foo.js ' 会读取不带尾空格的另一文件或报不存在，未端到端保留原样路径。应仅用 TrimSpace 判断全空白，读取时传原始路径，并用内容不同的两个文件增加回归。
- [P3] README.md:387: “run 前后均可”与帮助重复值“最后一次生效”的组合说明遗漏根级帮助边界；bb-browser --help run --tab t1 --help=false -- --foo.js 实际打印根帮助且零 RPC。README 与脚本说明应明确覆盖保证限于 run 后，或注明根级帮助优先。
- [P3] cmd/bb-browser/run_test.go:707: 缺少 --absent.js / -- 文件的错误仅断言包含文件名，错误地当作选项也可能通过。应断言 errors.Is(err, os.ErrNotExist) 与 *os.PathError.Path，保留零请求断言。

Codex 只读审查 origin/main...HEAD，无 P0/P1；按工厂规则 review/ → ready/，上述 P2/P3 保留给 owner 决定。工作树干净，无代码修改或提交。分支可与当前 main 干净合并，无需冲突修复。

完整配置检查通过：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。使用一次性 HOME、独立 XDG/Go 缓存离线测试，无真实外网。另以编译后二进制及回环假 daemon 验证普通 run -- --foo.js 一次 eval、尾空格文件名问题和根级帮助边界。--help=true 在 main 已正常，本次仅加回归保护，不声称重新修复。

安装：无；使用已有 Go 1.27.4。没有可用的 review-code 技能，按只读代码审查执行。未验证真实 Chrome、真实 daemon、Windows 文件名行为；本项无图形 UI 改动。无工作树和 TMPDIR 外审查产物，/tmp 前后对比无新增。无关联 issue/PR，无需发布审查评论。本轮仅 review，不合并 main、不打 tag、不发布。

## factory · 2026-11-25 15:07
ship 完成。merge: fe1e2aebaf00c39bad7992ab289ab04ef5988407（parents: c5d25a1bfb25fa9e7e557b2c148847812a2495d4 + 64d353672c8d07827461ee7880985f2c883a71a1），已普通推送 main。已审 head 64d353672c8d07827461ee7880985f2c883a71a1 为远端 main 祖先。未打 tag、未发版、未删除代码分支、未改 workflow 或 factory.toml。本段仅记录 FT6 合入；最终小版本将在独立核验 FT7/FT4/FT2/FT3/FT6 均已远端交付且 done、FT5 保持 stop 之后另作记录。

测试命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 退出 0：gofmt 无输出，go vet ./... 通过，go test -race ./... 11 个包通过、1 个无测试（cmd/bb-browser 14.566s，cmd/bb-daemon 1.013s，deploy 1.016s，deploy/systemd 1.027s，internal/browser 6.995s，internal/daemon 77.745s，internal/site 1.913s，internal/state 1.206s，internal/store 14.452s，internal/timeout 1.035s，pkg/daemonclient 12.515s，pkg/protocol [no test files]）。go.mod/go.sum 哈希未变。不修改先前 review 正文与 verdict: pass（factory · review · 2026-11-25 14:31）。保留收窄范围：低优先处理 run 的帮助布尔参数以及 -- 选项终止后以 -- 开头的脚本路径。审查 notes 中的 P2/P3 仍按原记录保留。无 source 链接需关闭。FT5 保持 stop。

## factory · 2026-11-25 15:13
最终小版本发布完成。独立核验 FT7/FT4/FT2/FT3/FT6 均已在远端 main 且位于 done/：FT7 merge e8ec86aaa0c6333c8cac35c715ed48ed431a0879（已审 head 9eeb0d1123b34c04663252013bbe7642ceb0b039）、FT4 merge 7b8aab43feaf205ecc73269a489d400487323c6b（已审 head 9b2f1c2add0f52f35b93c1a992e137b48e4c3294）、FT2 merge 3287688b7ed2bf6c4730de6962630e12f22a1a9d（已审 head 429acb3766d665bc094ebaa0fb5c4955e8bcb13b）、FT3 merge c5d25a1bfb25fa9e7e557b2c148847812a2495d4（已审 head 74345e9e2f00208515199d8b89a77974bde76a1a）、FT6 merge fe1e2aebaf00c39bad7992ab289ab04ef5988407（已审 head 64d353672c8d07827461ee7880985f2c883a71a1）。上述 merge 与已审 head 均为当前 origin/main（fe1e2aebaf00c39bad7992ab289ab04ef5988407）祖先。FT5 保持 stop，未记为 done。backlog/new/review/ready 无既定顺序剩余项。

按 t25u 授权，在最高语义 tag v0.1.1 之上创建注释 tag v0.1.2（tag 对象 2b42d9c57484ad919b7644249a9c95703e047443），指向最终 base fe1e2aebaf00c39bad7992ab289ab04ef5988407，说明为「v0.1.2: FT6 run帮助布尔参数与选项终止符兼容; sequence FT7/FT4/FT2/FT3/FT6 complete」。已普通推送 refs/tags/v0.1.2；远端剥离提交仍为 fe1e2ae。未移动或覆盖 v0.1.0（对象 bf4b7d0c4bc62361073c4b925ba26bc4ed9e8e17，剥离 e8ec86aa）与 v0.1.1（对象 ff189193ea8b3ddd9126c7af83c92f10862d9f86，剥离 fb2c535）。未改 workflow、factory.toml、未删除代码分支、未改默认分支、未部署、未手建 GitHub Release。

发版前预检：go mod tidy 无变更；`goreleaser check` 通过；`goreleaser release --snapshot --clean --skip=publish` 成功，产物为单一 linux/amd64 归档与 checksums，无 Docker。当前 .goreleaser.yaml 仅 linux/amd64 两个 binary（bb-browser、bb-daemon），无 dockers。

自然触发：https://github.com/coinpost/go-bb-browser/actions/runs/45566006833 （workflow release，event=push，head_branch=v0.1.2，head_sha=fe1e2ae，job goreleaser 139040850891）于 2026-11-25 15:10:00 Asia/Shanghai 开始，15:11:06 完成，conclusion=success。残留 Set up QEMU / Docker Buildx / Login to GHCR 步骤执行但未构建或推送镜像（日志无 building docker image；goreleaser 2.20.3 仅构建 linux_amd64 两个二进制并上传归档与 checksums，release succeeded after 36s）。另有 Actions 对 Node.js 20 弃用的警告，未改 workflow。

GitHub Release：https://github.com/coinpost/go-bb-browser/releases/tag/v0.1.2 （id 457763383，name v0.1.2，draft=false，prerelease=false，由 github-actions[bot] 发布于 15:10:55 Asia/Shanghai，target_commitish=main）。资产仅 checksums.txt（sha256 8777bf4462a7925f15b78d427a4aedbeb9e590ff82204201f88eeb3c7640c522）与 go-bb-browser_0.1.2_linux_amd64.tar.gz（sha256 8841e4392a5723622c813609efaee2befa00f4ea6b891267be4dc80cabaaa6f4，7657537 字节）。本地下载后 sha256sum -c 通过，并与 GitHub digest 一致。归档仅含 README.md、AGENTS.md、bb-browser、bb-daemon；二者均为 ELF64 little-endian x86-64，module github.com/coinpost/go-bb-browser v0.1.2，vcs.revision=fe1e2aebaf00c39bad7992ab289ab04ef5988407，GOOS=linux GOARCH=amd64 CGO_ENABLED=0。CI 工具链为 Go 1.26.11。无其他 OS/架构归档，无镜像资产。

不修改先前 review 正文与 verdict: pass（factory · review · 2026-11-25 14:31），不修改 15:07 ship 记录。审查 notes 中的 P2/P3 仍按原记录保留。FT5 保持 stop。
