---
branch: factory/FT20-private-rpc-log-permissions
round: 1
---
# [P3] 含完整请求体的 rpc.jsonl 以 0644 创建、状态目录 0755，本机其他用户可读取 fill 文本与 fetch 头中的凭据

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B13；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/store/store.go:112`（基线行号，实施前重新定位）。

触发条件：默认配置运行 daemon（umask 022），客户端调用 fill（密码等）、fetch（headers 含 Authorization）、network_route（mock body）、eval。

实际行为：审计行保存请求体（仅对超过 200 字符的字符串做中段截断，internal/store/compact_body.go），文件用 os.OpenFile(…, 0o644) 创建（store.go:112，轮转 :300 同），目录 MkdirAll 0o755（:105）；轮转备份 rpc.jsonl.1-3 权限相同。复现中日志含 `hunter2-secret` 与 `Bearer sk-live-123`，文件 mode=644、目录 mode=755。

期望行为：状态目录 0700、日志及轮转备份 0600；（可选）对 fill.text、fetch.headers/body 等敏感字段脱敏。

影响：多用户主机/共享容器卷上，其他本地账户可读出通过 daemon 输入的密码、会话令牌和 API 密钥；API token 鉴权保护了 RPC 入口却没有保护落盘副本。

根因：把审计日志当普通状态文件创建，未考虑其内容包含调用方提交的秘密。

修复方向（建议，非指定实现）：MkdirAll 用 0o700（已存在目录不放宽）、OpenFile 用 0o600，轮转后文件同样 0o600；启动时对既有文件做一次 Chmod 收紧。

回归验收：store 测试（umask 022）：新建 state dir 权限为 0700，rpc.jsonl 与轮转产生的 .1 备份为 0600；既有 0644 文件在 Open 后被收紧；写入/replay/轮转既有测试保持通过。

证据：repro-go.log（TestAudit_RPCLogMustNotBeWorldReadable FAIL）；repro/store_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

## factory · plan · 2026-11-22 06:40

### Behaviour
- 首次磁盘启动创建私有状态目录（0700）和 rpc.jsonl（0600）；空目录配置及 '-' 内存模式不变。
- 打开既有状态目录时收紧该目录及现存 rpc.jsonl、rpc.jsonl.1–3 的权限，先保护再回放；权限收紧失败明确报错，不带着泄露风险继续。
- 每次轮转后活动日志与备份保持 0600；已有有效日志仍正常写入、回放与恢复 managed tab。
### Scope
- 仅 store 权限初始化/轮转与永久回归，以及必要文档；不改日志格式，不做可选字段脱敏，不改共享父目录，不合并 stop/FT1，不改保护路径。
### Decisions
- 状态目录是 daemon 专有目录，既有宽权限收紧到 0700，现有备份一并迁移；不递归 chmod 非日志文件和父目录。
- umask 022 的回归通过子进程隔离，避免全局 umask 影响并发测试；覆盖更严格 umask 下安全行为，具体断言采用不放宽已有更严格权限的合理策略并记录。
### Checking
- 先加永久权限回归并确认修改前失败，再实现；覆盖新建、旧目录/活动日志/全部备份迁移、轮转多次、写入与重开回放、内存模式、权限错误。
- 完整执行配置命令：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，测试 HOME、缓存、临时文件均隔离。
- 不依赖真实外网；不启动 Chrome、不操作用户窗口。真实 Chrome/CDP、Docker、systemd 未验需明确记录。

## factory · 2026-11-22 06:40

基线：origin/main@4f86c1c；独立构建，未合并 stop/FT1。Claude Code 构建提交 d738276，分支已推送。

实现：状态目录新建为0700，rpc.jsonl和轮转新日志0600；启动回放前收紧既有目录、活动日志及 .1–.3 普通备份文件，失败报错。保留更严格权限，不更改既有父目录、其他文件及日志格式，不做可选敏感字段脱敏。更新README、AGENTS及systemd文档。

决定及边界：备份位置非普通文件（含符号链接、目录）跳过；活动日志在打开的句柄上Chmod；Windows不执行POSIX权限收紧。组权限共享且非属主的部署可能因chmod失败而拒绝启动，review应检查兼容性与失败处理。生产测试钩子chmodPath用于模拟备份chmod失败。

永久回归：子进程隔离umask 022/000/077；新建权限、既有目录和活动日志及全部备份迁移、多轮轮转、写入与重开回放、更严格权限不放宽、内存模式、权限收紧失败。coder报告修复前新建/迁移/轮转断言失败（state 0755 / log 0644），修复后通过。

测试：coder完整配置命令及定向-count=1回归通过；manager独立执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 退出0（部分包命中缓存），Go 1.27.4；git diff --check通过，工作树干净，保护路径无变更。测试使用隔离HOME/TMPDIR/缓存、GOPROXY=off，无真实外网依赖。coder另报告Linux/Windows的store vet通过，仅交叉编译检查。

安装：无；go.mod/go.sum未改，使用既有Go工具链和模块缓存。

未检查：真实Chrome/CDP、Docker构建/卷迁移、systemd运行、Linux和Windows实机测试，真实非属主EPERM未测（仅钩子注入）。没有UI变更，未操作用户窗口。

状态：new/ → review/，第1轮；未review、未合并main、未发布。owner重点检查共享目录兼容性、备份异常路径及权限失败语义。

运行备注：Claude Code五小时额度已用86%，调用完成时约1小时50分后重置；未重试。恢复阶段清理本build目录内过期临时文件，无未提交改动或本地board提交需恢复。首次auth检查因工作目录无仓库未成功，进入专用clone后auth检查成功。
