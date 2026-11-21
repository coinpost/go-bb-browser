# [P3] rpc.jsonl 尾部不是换行时继续追加会粘行，下次启动 replay 将该行及其后的全部记录截断丢弃

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B11；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/store/store.go:266`（基线行号，实施前重新定位）。

触发条件：日志最后一行不是以 \n 结束：进程在 JSON 已落盘但换行未写入时被杀/断电，或运行中 Write 因磁盘满只写入了部分字节后空间恢复、后续追加成功。

实际行为：Open 的 replayLog 接受无换行的合法末行并把文件定位到末尾；AppendRPC（store.go:266）直接写 `记录+\n`，与上一行粘成 `{…}{…}`。下一次启动时该行解析失败，replayLog 在 goodEnd 处 Truncate（store.go:164-174），粘行以及其后的所有记录被删除。复现：两个受管 tab 重启后全部丢失，rpc.jsonl 被截为 0 字节。

期望行为：追加前保证位于行边界（必要时先补换行）；单行损坏只丢该行，不应连带丢弃其后的合法记录与最近的受管 tab 状态。

影响：受管 tab 的恢复信息丢失 → 重启后这些 daemon 创建的 tab 不再被空闲回收（标签页泄漏）；审计日志被静默截断。

根因：追加路径假设文件总以换行结束；replay 遇到坏行采取“截断其后全部”的策略且无逐行恢复。

修复方向（建议，非指定实现）：Open 时若文件非空且末字节不是 \n，先补写换行；AppendRPC 写入失败/短写后标记“需要先写换行”；replay 对中间坏行改为跳过并保留后续合法行（仅对最后一行做截断）。

回归验收：store 测试：预置无结尾换行的合法记录 → Open → AppendRPC(tab_new) → Close → 再 Open，两个 tab 均在 ReplayManagedTabActivity 中且文件每行可独立解析；预置“坏行+合法行”时合法行不丢；既有“截断的最后一行被丢弃”测试保持通过。

证据：repro-go.log（TestAudit_AppendAfterUnterminatedTailMustNotLoseRecords FAIL）；repro/store_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

去重边界：承接 backlog/FT4 的“完整 JSON 尾行缺换行”问题；FT4 其余轮转和可写探测问题仍属 backlog，不在本任务顺带实施。仅追加换行不能掩盖不完整 JSON；尾部修复和中间坏行恢复策略应符合仓库已有契约，不强制照搬报告建议。
