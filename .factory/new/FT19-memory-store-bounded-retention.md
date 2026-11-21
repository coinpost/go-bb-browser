# [P3] --state-dir - 内存模式下每条 RPC 记录被永久保存在 memLogs，内存无界增长

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B12；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/store/store.go:260`（基线行号，实施前重新定位）。

触发条件：以 `--state-dir -`（BB_BROWSER_STATE_DIR=-）运行 daemon 并持续接收 RPC（含 network/console 轮询）；HOME 不可得导致默认目录解析为空时也会静默进入该模式（internal/daemon/config.go:147-150）。

实际行为：AppendRPC 在 inMemory 分支把每条 LogRecord 追加到 s.memLogs（store.go:259-262）；生产代码没有任何读取或裁剪 memLogs 的地方（全仓 grep 仅此一处写入）。5000 次调用后保留 5000 条。

期望行为：内存模式不保留历史记录（managed 映射已单独维护），或只保留有上限的环形缓冲。

影响：长期运行的内存模式 daemon RSS 持续上涨直至 OOM（每条记录含压缩后的请求体，数百字节；1 RPC/s 约每天数十 MB）。

根因：遗留的只写调试缓冲，没有容量上限也没有读者。

修复方向（建议，非指定实现）：删除 memLogs（测试需要时改用带上限的 ring 或测试专用 hook）。

回归验收：store 测试：内存模式连续 AppendRPC 10 万条后保留记录数 ≤ 固定上限（或为 0），ReplayManagedTabActivity 行为不变；用 testing.AllocsPerRun/堆快照确认不随调用数线性增长。

证据：repro-go.log（TestAudit_InMemoryModeMustNotRetainEveryRecord FAIL）；repro/store_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
