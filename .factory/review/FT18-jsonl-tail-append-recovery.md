---
branch: factory/FT18-jsonl-tail-append-recovery
round: 1
---
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


## factory · plan · 2026-11-22 06:09

### Behaviour
- 空文件/首次启动照常；合法 JSON 尾行缺换行时完整回放，并在后续追加前恢复行边界，多次重开不丢已有/新增记录。
- 中间损坏行告警并跳过，保留后续独立合法记录；不完整最终 JSON 按原契约截断，不能靠补换行伪装完整记录。
- 运行期追加或快照短写/失败后，下一次写入不得与残片粘连；准确计算已写字节并传播错误。恢复边界失败不得继续追加；保持锁内串行。
### Scope
- 仅 store 的 JSONL 回放/写边界、相关永久回归及必要说明。不处理 FT4 其他轮转原子性/可写探测、managed 身份或其他任务，不改保护路径。
### Decisions
- 从最新 origin/main 独立构建，不合并 FT1 或其它项。以物理换行分隔记录，跳过中间坏行但不尝试猜测粘连 JSON；完整尾记录补分隔，不完整尾记录丢弃。
- 最小共享写入路径维护边界与实际字节数，覆盖 AppendRPC 和轮转快照；缺失错误注入能力时采用最小局部测试 seam，禁止泛化存储层。
### Checking
- 永久正确行为回归先于生产修复运行并记录失败：合法无换行尾部→追加→重开保留两 tab；中间坏行后合法记录保留；不完整最终行仍截断；短写/错误后继续写、分隔修复失败、快照路径以及多次重开。
- 定向 race 重复及配置全量 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。仅本地文件/离线测试，HOME/缓存/临时文件均在 TMPDIR。
- 不启动真实浏览器；真实 Chrome/CDP、Docker、systemd 未验证须明确记录，无UI变更。

## factory · 2026-11-22 06:09

基线：origin/main@d55fbc55162db73a43c01e1a6b782750437cfb5a；独立构建，未合并 FT1 或其它项。Claude Code 构建提交：a311559。

实现：回放按物理换行逐条解析，中间坏行告警跳过但留在文件，独立合法后续行继续恢复；完整无换行尾行回放后标记写边界待修复，不完整最终 JSON 仍截断。AppendRPC 与轮转快照共用 writeLinesLocked：下一次写先补必要换行，失败则拒绝追加；真实字节数更新 logBytes，短写无错误转换 io.ErrShortWrite。README/AGENTS 补充契约。

决定：惰性补换行，Open 只读完整尾行而不立即改写；不猜测拆分已有粘连 JSON。采用一个局部 raw-write 测试 seam，不引入存储抽象。原“中间坏行后全部截断”的测试断言与新契约相反，改为保留不完整最终行截断回归并新增中间恢复验证。

测试：coder 在生产修复前运行永久正确行为回归，7类测试失败，含无换行追加后重开、中间坏行、坏行+不完整尾行、短写、无错误短写、补边界失败及快照短写；修后全部通过。coder 定向 race -count=30 及配置全量 gofmt/vet/race 通过。manager 使用独立全新 GOCACHE、throwaway HOME 再执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`，8个测试包通过、2个无测试包。Go 1.27.4，GOPROXY=off；git diff --check 通过，保护路径无变更，提交范围仅本项，工作树干净。证据保留于本机 build run evidence/FT18-a311559/。

安装：无；使用已有工具链与 pinned 模块，清单/锁文件未变。测试 HOME/缓存/临时文件均在本 run tmp 下，结束后清理。

未检查：真实 Chrome/CDP、Docker、systemd、真实磁盘满或断电未验证；短写/写错误仅 seam 模拟。没有浏览器或 UI 改动。

边界：已粘连记录不抢救，只保留其后的独立合法记录；坏行保留到轮转，重启重复告警。快照短写时未完整落盘的 tab 恢复丢失、轮转失败吞错与写盘失败前 managed 已更新均为既有行为/FT4范围，本项未修复；仅保证继续写入不粘连。

状态：代码分支已推送；new/ → review/，第1/3轮。仅 build，未执行独立 review、未合并 main、未发布。
