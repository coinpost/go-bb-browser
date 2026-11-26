---
branch: factory/FT29-bb-daemon-debugger-url-default
round: 1
---
# bb-daemon --debugger-url 默认值 127.0.0.1:9222

## Guoyin · 2026-11-26 08:51
工厂 new：bb-daemon flag --debugger-url 设置默认值 127.0.0.1:9222

## factory · 2026-11-26 08:53
将 `bb-daemon` 的 `--debugger-url` flag 默认值设为 `127.0.0.1:9222`。

范围仅限该默认值：未显式传入 `--debugger-url` 时使用 `127.0.0.1:9222`；调用方显式传入时仍使用其给定值。不扩大 flag 设计，不改其他参数、连接逻辑或启动流程。

## factory · plan · 2026-11-26 09:09

### Behaviour
- 未传 --debugger-url 且 BB_BROWSER_DEBUGGER_URL 未设置或为空时，flag 默认使用 127.0.0.1:9222。
- 非空环境变量仍覆盖内置默认值，显式 flag 覆盖环境变量；显式空 flag 继续走既有校验失败，不偷偷回退。
- Chrome 未运行时沿用既有连接失败处理，不启动浏览器、不改连接逻辑或启动流程。
### Scope
- 仅修改 bb-daemon flag 的默认值及其帮助文字、必要的针对性回归测试，并同步 README/AGENTS 中“必填”的过时说明。
- 不改变 daemon.Config 的校验/默认值、其他 flag、CDP 连接实现，不恢复 FT21 或短 ID。
### Decisions
- 复用现有 envOrDefault 的优先级与空环境变量语义，不为一个默认值引入解析层或配置抽象。
- 测试隔离 flag 全局状态，优先复用现有 cmd/bb-daemon 测试模式；不依赖真实外网或真实 Chrome。
### Checking
- 验证无环境变量、空环境变量、环境变量覆盖、显式 flag 覆盖、显式空 flag 的行为与帮助默认值。
- 执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。
- 无 UI 改动，不要求启动真实 Chrome；浏览器连接流程保持不变。

## factory · 2026-11-26 09:09

已完成首轮构建，提交 `7006079`（基于 `80a2665`），代码分支已推送。

- `--debugger-url` 默认值改为 `127.0.0.1:9222`；继续保持显式 flag > 非空环境变量 > 内置默认，空环境变量使用默认、显式空 flag 仍由原有校验拒绝。
- 只抽出该 flag 的注册函数以供独立 FlagSet 测试，不改其他 flag、Config 校验、连接逻辑和启动流程；README 与 AGENTS 的必填说明同步更新。
- 新增 7 个针对性用例，覆盖未设置/空环境变量、环境变量与 flag 覆盖、显式空值两种写法、帮助中的默认值。
- coder 和 manager 均通过完整检查：`test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`。manager 在一次性 HOME、独立 XDG/Go 缓存、关闭模块联网的环境复验通过。
- 无新增工具安装、无依赖清单变化、无 worktree/TMPDIR 之外写入。
- 未启动真实 Chrome，未验证实际默认端点附加及 Chrome 未运行时的连接失败路径；这些流程未改动。无 UI 改动。
- FT28 已在 done/，按普通规则选择唯一 new/ 项 FT29；FT21 保持 stop/，未恢复短 ID；本次不合并 main、不打 tag、不发布、不启用任何任务。

移至 `review/`，等待代码审查。

## factory · review · 2026-11-26 09:15
head: 700607909a5ab856caa56b0a8861e5d4e3062af1
verdict: pass
notes:
- Codex 对 `origin/main...HEAD` 完成只读审查，未发现 P0/P1/P2/P3 问题；与 main（`80a2665`）无合并冲突。
- 默认值仅在 bb-daemon 的 flag 注册处设置；显式 flag > 非空环境变量 > 默认，空环境变量回退默认，显式空 flag 仍被原有校验拒绝。未改变 Config、连接逻辑、启动流程或短 ID 约束。
- 完整检查通过：`test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`；一次性 HOME、独立 XDG/Go 缓存，离线执行，退出码 0。
- 新增 7 个子用例单独复跑通过；实际构建的二进制确认 help 默认值，以及环境变量非空时显式空 flag 校验失败、退出码 2。
- 无新增安装；工作树干净，HEAD 未变。coder 首次 mktemp 曾在系统临时目录创建一个空目录，已立即删除；随后所有测试临时内容均在指定 TMPDIR，无剩余目录外产物。
- 未启动真实 Chrome，未实测默认端点实际附加或 Chrome 缺席时的连接失败路径；这些流程未修改，无 UI 改动。
- 移至 ready/，供 owner 确认；本次未合并 main、未打 tag、未发布。
