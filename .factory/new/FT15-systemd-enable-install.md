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
