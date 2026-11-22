---
branch: factory/FT19-memory-store-bounded-retention
round: 1
---
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

## factory · plan · 2026-11-22 06:21
Behaviour：内存模式继续校验 action/body、补全时间、维护 managed 活跃/关闭与 seq，不保留 RPC 历史及 body；首次打开 replay 返回空副本，错误输入不改 managed。磁盘追加、轮转与恢复保持不变。

Scope：修改 store 实现、永久回归测试及最小文档说明；不修改 daemon 配置、RPC API、磁盘权限或其他审查项。

Decisions：直接删除无读者的 memLogs，不新增环形缓冲或配置。保留现有压缩与 JSON 校验顺序，不做额外 fast path；堆测量固定 managed tab 数，区分瞬时分配与历史保留。

Checking：先验证正确行为回归在旧实现失败，再修复；10 万次调用分批 GC 测量堆增长，覆盖 managed 创建/更新/关闭、返回副本隔离和错误输入。运行完整 gofmt、vet、race 门禁。

## factory · 2026-11-22 06:21
已构建并推送 fc1cb3e（基于 main@9fa3562），new/ → review/，第 1 轮。

实现：删除 memLogs 字段及追加，内存模式丢弃历史，仅保持 managed 当前状态。保留原校验顺序与错误语义；更新 AGENTS.md 的内存模式说明。

永久回归：10 万条 network RPC、固定两个 managed tab，每 2 万条双 GC 后测量存活堆，相对首批增长上限 2 MiB。修复前普通模式三次及 race 一次均失败，首个检查点增长约 7.8 MB；修复后连续五次及 race 均通过。另覆盖 managed 生命周期、副本隔离、零时间补全，以及内存/磁盘两种模式非法输入不改变 managed。

验证：coder 和 manager 均运行完整 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`，全部通过；提交仅涉及本项，never 路径未变。无新增安装；使用已有 Go 1.27.4（go.mod 最低要求 1.26），仅恢复 go.sum 固定依赖。

未验证：真实 Chrome、Docker、systemd、真实 daemon 长时间 RSS；无 UI 改动。堆阈值仅本机 Go 版本验证，其他平台仍需留意测试稳定性。

## factory · review · 2026-11-22 06:44
head: fc1cb3e1e7e32bad0a17d981483c19187984b239
verdict: pass
notes:
- Codex 对 origin/main...HEAD 完成只读审查，无 P0/P1/P2/P3 发现。内存模式删除历史保留，参数校验、时间补全、managed 生命周期和 seq 保持；磁盘日志路径未改变。
- 完整 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过；10 万条固定 managed 数量堆测量相对首批最多增长 2,288 字节，低于 2 MiB；旧实现对照第 4 万条增长 7,776,768 字节，按预期触发回归断言。
- 使用已有 Go 1.27.4（darwin/arm64）和固定依赖，无新增安装。未找到 review-code skill，直接按请求、计划及配置审查。审查前后 HEAD 不变且工作树干净，manager 复核 diff --check 通过；当前 main 可无冲突合并。
- 未验证真实 daemon 长期 RSS、其他平台堆阈值稳定性、真实 Chrome/CDP、Docker、systemd；无 UI 改动，未启动浏览器或操作用户窗口。测试 HOME、缓存和对照文件均隔离在本 review 的 TMPDIR，无真实外网依赖。
- review/ → ready/，等待 ship；未合并 main，未发布。无关联 GitHub issue/PR 需要评论。


## factory · 2026-11-22 12:37
owner 要求将 ready 项全部 ship；本项已合并并推送 main。

merge: 8c33fe31c359e2801aa5775385e61c894a8aa1fa
main: e63a15c1c8a64c3c1b6a501d965bf17a65025bd3
tag: 无；按 ship=[merge,push]，未打 tag、未触发 tag 发布。

验证：每项合并后均执行完整 gofmt、vet、无缓存 go test -race -count=1 ./...；最终累计 main 再次通过（11个测试包、1个无测试包）。manager 核验审查 head 与远端分支一致，所有批准分支均为最终 main 祖先；保护路径无差异，diff --check 通过、工作树干净。FT19/FT20 冲突由编码代理解决，保留完整 target 生命周期、无 RPC 历史保留、日志换行恢复及私有权限语义，相关测试适配后通过。

关联 issue/PR：无 source 或 owner 明确关联，无关闭对象。分支保留：配置不含 clean。安装：无。测试使用已有 Go1.27.4，HOME/缓存/TMPDIR 隔离、固定模块离线恢复。真实 Chrome/CDP、Docker、systemd 未验证；既有审查非阻塞 notes 未额外修复。

ready/ → done/。FT28 等非 ready 项不在本次范围。
