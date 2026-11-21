---
branch: factory/FT15-systemd-enable-install
round: 1
---
# [P2] systemd unit 没有 [Install] 段，文档里的 `systemctl --user enable` 与开机自启不生效

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B08；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`deploy/systemd/bb-daemon.service:1`（基线行号，实施前重新定位）。

触发条件：按 deploy/systemd/README.md:23 执行 `systemctl --user enable --now bb-daemon.service`，并按 :33-39 启用 linger 期望重启后自动运行。

实际行为：unit 文件只有 [Service] 段（无 [Unit]、[Install]、WantedBy=）。enable 不创建任何 wants 链接（systemd 提示 unit 无安装配置），--now 只启动当次；注销/重启后服务不会再起来。

期望行为：enable 后服务随用户会话（配合 linger 则随开机）自动启动，符合 README “Run on boot”。

影响：按官方部署文档操作的机器重启后 daemon 不在运行，依赖它的爬虫/agent 全部失败，且无任何报错提示。

根因：unit 文件缺少 [Install] WantedBy=default.target。

修复方向（建议，非指定实现）：补充 `[Unit] Description=…`（可加 After=network-online.target）与 `[Install] WantedBy=default.target`。

回归验收：静态检查测试（Go test 读取 unit 文件）断言存在 [Install] 且 WantedBy=default.target；在 Linux 环境用 `systemd-analyze --user verify deploy/systemd/bb-daemon.service` 无 “no installation config” 警告，`systemctl --user is-enabled` 为 enabled。

证据：static-deploy-docs.log（grep 显示 unit 仅有 [Service]）。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。


## factory · plan · 2026-11-22 05:20
Behaviour：补齐用户 unit 的 [Unit] Description 及 [Install] WantedBy=default.target，使首次 enable 建立正确用户 target 链接；既有安装重新复制并 enable。保留 Service 与外部 Chrome 管理方式。静态检查拒绝读取错误、空文件、注释、错误 section 或错误目标。

Scope：仅 unit、相邻永久 Go 回归与既有安装更新说明；不改运行时代码、hardening、其他审查项或依赖。

Decisions：使用用户级 default.target；不增加 network-online.target 依赖；按 section 检查实际赋值，不引入 INI 库。WantedBy 必须恰为 default.target，有意扩展需同步更新回归。

Checking：先加测试确认原 unit 失败，再修复变绿；运行完整 gofmt/vet/race。可用隔离 Linux 环境才检查真实 systemd enable/verify，不触碰用户服务。

## factory · 2026-11-22 05:20
构建完成：ebc11695a09150ce083f813b4944484cd8dae305。新增 [Unit] Description 和 [Install] WantedBy=default.target；Service 未改。新增读取真实 unit 的 Go 永久回归及 19 个无效配置反例；README 提醒旧安装重新复制与 enable。

红绿证据：修复前 TestUnitIsEnableable 因 missing [Install] section 失败；修复后通过。

完整测试：`test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过，经理独立复验通过（离线使用现有模块缓存）。经理首轮复验误指向空缓存而无法解析模块，改用本次已恢复的隔离缓存后通过，并非代码测试失败。

安装：无额外安装；使用已有 Go 1.27.4，已锁定模块从本机缓存恢复。测试 HOME、缓存和临时文件均隔离在本 run TMPDIR 内。

未验证：macOS 无 systemd 且无可用隔离 Linux，未实测 systemd-analyze、enable/is-enabled、linger 与重启；本次不涉及 UI，未运行真实 Chrome 或 Docker。请 review 关注 unit 安装语义及静态回归范围。new/ → review/，round 1。


## factory · review · 2026-11-22 05:43
head: ebc11695a09150ce083f813b4944484cd8dae305
verdict: pass
notes:
- [P2] deploy/systemd/README.md:28: 旧安装升级说明要求重复复制 unit，会覆盖用户按同页说明直接修改的 ExecStart（如自定义 CDP 端口）。建议区分原版/drop-in 安装和直接编辑过 unit 的安装；后者应合入新增 [Unit]/[Install] 后 daemon-reload 并重新 enable，保留原配置。

Codex 独立只读审查 origin/main...HEAD 完成；没有 P0/P1。unit 正确设置 Description 与 WantedBy=default.target，[Service] 与基线一致；工作树无修改、无新提交。分支与 main 可干净合并。

完整测试 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过。审查另在隔离目录验证旧 unit 触发永久回归 missing [Install] section，当前 unit 通过；该预期失败不是现有测试失败。使用已有 Go 1.27.4 和 pinned 模块缓存，GOPROXY=off，测试 HOME/缓存/临时文件隔离；无额外安装。

未验证：macOS 无真实 Linux systemd，未运行 systemd-analyze --user verify、enable/is-enabled、linger 或重启自启；未启动或改动用户服务，未运行 Chrome/Docker，无 UI 变更。P2 文档升级覆盖风险留待 owner 关注。review/ → ready/，仅审查，未合并 main、未发布。


## factory · 2026-11-22 06:08
owner 授权：“先把 ready 的 ship 掉”。本次固定合并 FT8、FT9、FT11、FT12、FT14、FT15；review head 与所合并远端分支一致。

已合并并推送 main；本项 merge commit：8836949，本批 main 最终提交：8836949。配置 ship=["merge", "push"]，未打 tag、未触发版本发布、未部署。无 linked issue/PR 需关闭，分支按配置保留。

逐项合并后执行完整 gofmt/vet/race 测试，最终累计 main 配置测试再次通过（部分缓存）；git diff --check 通过，保护路径未改，工作树干净。FT12 的 go.mod 冲突及 FT14 的 README/run_test.go 冲突由 CC 仅解决整合冲突，保留双方依赖、文档与全部回归；FT9 与 FT14 测试拆文件并共存通过。无新增未 pin 工具或依赖。

review 中已有 P2/P3 未在本次 ship 修复，继续保留原记录。真实 Chrome/CDP、Docker、systemd 运行时未验证。ready/ → done/。
