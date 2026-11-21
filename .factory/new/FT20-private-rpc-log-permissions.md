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
