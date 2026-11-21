# [P2] 未知/过期 tab id 的请求在 tabCDPLocks 中留下永不回收的条目（key 由客户端控制）

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B10；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`internal/daemon/rpc_connections.go:193`（基线行号，实施前重新定位）。

触发条件：任意带 tab 参数的 RPC（goto/eval/click/network/tab_close…）使用不存在或已关闭的 tab id；每个不同的字符串一条。客户端在 tab 被空闲回收后继续用旧 id 重试即会触发。

实际行为：lockTab 在解析 tab 之前按原始字符串建锁条目（rpc_connections.go:193-199）；releaseTabLockRef 只有 forgotten=true 才删除（:219-226），而 forgotten 仅在已注册 target 被移除时设置。未知 id 的条目永久保留：300 次请求后 map 中有 300 条。key 最长可接近 MaxBodyBytes（默认 1 MiB）。

期望行为：锁条目在引用计数归零且对应 tab 未注册时立即回收；未知 tab 的请求不留下任何状态。

影响：长期运行的 daemon 内存随陈旧/错误 tab id 请求单调增长；未配置 token 时任何能访问端口的调用方可用大 key 快速耗尽内存（每请求最多约 1 MiB 常驻）。

根因：锁以客户端传入字符串为 key 且先建后验；回收条件只覆盖“曾注册后被移除”的 tab。

修复方向（建议，非指定实现）：先用 s.tabs.Lookup（必要时一次同步）确认 tab 存在再建锁；或在 releaseTabLockRef 中当 refs==0 且该短 id 当前未注册时删除条目。

回归验收：daemon 测试：对 300 个不存在的 tab id 发 goto/eval/network/tab_close 后 len(tabCDPLocks)==0；对已关闭 tab 的旧 id 重试后同样为 0；既有 TestLockTabHonorsContextDeadline 与并发串行化测试保持通过。

证据：repro-go.log（TestAudit_UnknownTabRequestsMustNotLeakLockEntries FAIL）；repro/daemon_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
