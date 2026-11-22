---
branch: factory/FT13-pool-tab-new-distribution
round: 2
---
# [P2] Pool 的 tab_new 与 Health/Ready/Live 共用同一个轮询计数器，顺序调用方的 tab 会全部落到同一个 daemon

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B06；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`pkg/daemonclient/pool.go:428`（基线行号，实施前重新定位）。

触发条件：顺序调用方在每次 pool.TabNew 之间调用与后端数成整数倍关系的其它未绑定操作，例如 2 个后端时每次 TabNew 前先 pool.Health()；3 个后端时 Live+Ready+TabNew。

实际行为：order()（pool.go:426-442）对所有未绑定操作共用 p.rr；“最少在途”只统计进行中的 HTTP 调用，顺序调用时恒为 0，退化为纯轮询，健康检查吃掉了交替名额。复现：2 后端 20 次 TabNew 分布 [0 20]；3 后端 30 次分布 [0 0 30]。

期望行为：AGENTS.md / README 承诺的 even spread：新 tab 在健康后端间均匀分布，不受健康探测等其它调用影响。

影响：多 daemon 池退化为单 daemon：一个 Chrome 承担全部标签页，其余空闲；“先探活再开 tab”正是调用方最常见的写法。

根因：tab_new 的分配游标与健康检查/其它未绑定调用共享，且负载度量是瞬时在途请求数而非各后端已绑定 tab 数。

修复方向（建议，非指定实现）：为 tab_new 使用独立计数器，或按 p.tabs 中各后端已绑定 tab 数选最少者（并列再轮询）；Health/Ready/Live 不推进该游标。

回归验收：新增 pool 测试：2 后端交替 Health+TabNew 20 轮分布为 [10 10]；3 后端 Live+Ready+TabNew 30 轮为 [10 10 10]；原 TestPool_tabNewEvenDistribution 仍通过；一个后端 503 时其余健康后端差值 ≤1。

证据：repro-go.log（TestAudit_SpreadWithInterleavedHealth / …Health3 FAIL）；repro/client_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。


## factory · plan · 2026-11-22 04:52

### Behaviour
- 首次构造不探活，零 tab 时可创建；新 tab 在健康后端间均摊，不受 Health/Ready/Live 或其它未绑定调用干扰。
- 两后端 Health+TabNew 20 次为 [10,10]；三后端 Live+Ready+TabNew 30 次为 [10,10,10]；一个后端 503 时其余健康后端差值≤1。
- 保持错误 failover、全失败 AllFailedError、取消退出、tab 亲和和并发安全。
### Scope
- 仅 pkg/daemonclient 创建调度、必要说明及永久回归；不处理 hung backend、stale binding 或其它独立任务，不改协议或保护路径。
### Decisions
- 从最新 origin/main 独立构建，不合并 FT1。隔离创建与探活的调度状态，采用最小可证明的实现，不持锁执行 HTTP。
- 单独创建游标加固定环重试会将故障份额偏给下一后端，必须避免；保留在途负载优先语义，必要改变须说明。
### Checking
- 先写正确行为永久回归并在生产修复前验证失败：两种交错探活分布、轮换一个503后健康后端差值≤1、typed TabNew 与 Call(tab_new)。保留既有均摊/并发测试。
- 修复后定向重复 race 测试，以及完整 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。
- 仅本地 httptest；测试 HOME、缓存和临时文件在 TMPDIR 下；不用 headless Chrome。真实 Chrome、Docker、systemd 未检须记录。

## factory · 2026-11-22 04:52

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a；独立构建，未合并 FT1。构建提交：3c24855。

实现：tab_new 独立原子 createRR 游标，不再消耗探活/其它未绑定调用游标；成功 failover 后按跳过后端数推进创建游标，使顺序创建在健康后端间轮转。保留最少在途优先、失败重试、取消、AllFailedError 和 tab 亲和。仅修改 pkg/daemonclient 的实现、测试和 README。

决定：不用已绑定 tab 数作调度依据，避免 daemon 侧 idle 关闭后 stale binding 造成漂移及恢复后补发；不持锁执行 HTTP。长期存在在途请求时原有环邻居偏向没有扩展处理。

永久回归：两后端 Health+TabNew、三后端 Live+Ready+TabNew（均覆盖 typed/Call）、任一下标后端503及交错探活、故障恢复、并发创建。coder 报告修复前正确断言失败分布为 [0 20]、[0 0 30]；无探活的故障分布为 [0 20 10]/[10 0 20]/[20 10 0]。修复前失败日志保留于本 run evidence/FT13-3c24855/prefix-fail.log；恢复/并发防护测试修复前已通过，不当作修复证据。

测试：coder 定向 -race -count=30 通过；配置完整 gofmt、go vet ./...、go test -race ./... 通过。manager 用独立全新 GOCACHE 再执行同一完整配置命令通过，8个有测试包通过、2个无测试包；git diff --check 通过，保护路径无修改，工作树干净。测试使用隔离 HOME、TMPDIR 和本地 httptest。

安装：无；已有 Go 1.27.4 和 pinned 模块恢复，go.mod/go.sum 未变。

未检查：真实 Chrome/CDP、Docker、systemd 均未验证，未启动浏览器或改变用户窗口，无 UI 改动。并发故障测试仅断言成功及健康节点均有分配，不保证并发分布差值≤1；严格均摊验收覆盖顺序调用，review 注意并发与在途负载的边界。

状态：分支已推送，new/ → review/，第1轮；仅 build，未执行独立 review、未合并 main、未发布。


## factory · review · 2026-11-22 05:16
head: 3c2485527155c3ac81f141a4307c939b3e13e7a6
verdict: fix
fix:
- [P1] pkg/daemonclient/pool.go:295: failover 成功后按 skipped 追加推进共享 createRR，会越过另一并发请求尚未使用的健康后端，导致健康后端持续饥饿。三个本地 httptest 后端中，A 在后端0等待503，B在后端1成功后再释放A；A重试后端1成功并将游标从2推进到3，后续每对请求重复该顺序。连续10对在 HEAD（含 -race）分布为 [0,20,0]，基线同一复现为 [0,14,6]。修正并发故障转移的轮次预留/推进，保留最低在途优先、单请求后端去重和无锁HTTP，并将此交错转换为永久回归，验证健康后端计数差≤1。
notes:
- Codex 独立只读审查 origin/main...HEAD；无文件修改、无提交。与 origin/main 合并检查无冲突。
- 配置命令 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 退出0；两种入口的串行探活分布、逐后端503、恢复及既有均摊测试均通过。上述新并发诊断复现了 P1，不能因现有全量测试通过而放行。
- 本项回归是新增 skipped 推进在并发时将不均衡扩大为健康后端完全饥饿，并非要求修复所有既有在途负载偏向。修复时保留串行故障均摊验收。
- 安装：无；使用已有 Go 1.27.4，无清单或锁文件变更。测试 HOME、缓存及临时文件均隔离在本 run tmp，使用本地 httptest。
- 未检查：真实 Chrome/CDP、Docker、systemd；未启动浏览器或操作用户窗口。无UI改动。
- 状态：第1/3轮，review/ → new/，下一轮修复以上 P1；未合并 main、未发布。


## factory · plan · 2026-11-22 11:32

第2轮补充：
- Behaviour：故障转移不得追加跨过其它并发请求的健康轮次；通道控制的 A等待后端0的503、B在后端1成功、再释放A，连续10对健康端差≤1。保留首轮探活交错、顺序故障均摊、恢复及亲和语义。
- Scope：仅创建调度、相应文档和永久回归，不处理独立 hung/stale binding 任务。
- Decisions：每次尝试选最低在途且本请求未试过的后端，并列从独立创建游标起选取；选取与在途预留用短锁，HTTP不持锁，不串行化整个请求。按实际选取后端推进一次，不做完成后的 skipped 补偿。
- Checking：修复前永久回归必须FAIL；typed/Call均覆盖，修复后重复race及完整配置测试。真实环境未验如实记录。

## factory · 2026-11-22 11:32

第2轮构建：5793946；先以 d9c4dd8 合并 origin/main@7223e5a，无冲突。原分支保留首轮提交，仅修改 pkg/daemonclient 下实现、测试、README，无保护路径变更。

已修复最新 review 的 P1：pickCreate 按尝试选择、推进创建游标并原子预留在途计数，各请求每后端最多一次；移除成功后 skipped 追加推进。Health/Ready/Live 等不消耗创建游标。直接影响是故障重试同样遵守最低在途优先，不再固定环重试；长期在途时健康端轮转也会改善，是同一实现的结果，未加入冷却或调度抽象。

永久回归 TestPool_tabNewConcurrentFailoverKeepsHealthySpread 覆盖 TabNew/Call。生产修复前两入口均复现 [0,20,0]；修复后为 [0,10,10]。定向 -race -count=50 通过，首轮探活交错、逐后端503、恢复及已有并发测试保留。证据位于本run evidence/FT13-round2/，未提交机器路径或日志。

测试：coder 完整配置首次通过。manager首次复验遇到 TestShutdownWaitsForHandlerBeforeClosingAudit：server_test.go:413: shutdown timed out waiting for handler。额外定位证实 main@7223e5a 同断言在负载下复现：24进程×60轮，main失败16/1440，本分支19/1440；单独300轮两边均通过，确认为既有flaky，不在本分支修无关问题。coder 随后完整配置连续3次通过；manager再以隔离新GOCACHE执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过，10个有测试包通过、2个无测试包。git diff --check 通过，工作树干净。

安装：无新增未pin工具或依赖变更；使用已有Go 1.27.4及固定模块恢复，go.mod/go.sum未改。所有测试HOME、缓存、临时文件隔离在run TMPDIR，仅本地httptest。

未检查：真实Chrome/CDP、Docker、systemd；未启动浏览器或操作用户窗口，无UI改动。取消路径未加专门回归，依赖既有全量测试。任意并发负载不保证严格差≤1，严格验收涵盖顺序和上述确定性交错。review 应检查调度边界及已有main关停flaky记录。

状态：分支已推送，new/ → review/，round=2；本次仅build，未执行独立review、未合并main或发布。分支首次push因credential helper缺仓库路径失败，按命令添加credential.useHttpPath=true后重试成功，未修改持久设置。


## factory · review · 2026-11-22 11:45
head: 5793946443b7855f87324a76bfe7a3d8ec2c72af
verdict: pass
notes:
- [P3] pkg/daemonclient/pool_test.go:434: 并发回归中，A阻塞于故障后端时若B提前报错，t.Fatalf跳过释放A，httptest.Server.Close会卡住直到测试超时。工作树外故障注入令B返回context canceled后复现3秒超时；建议handler通道等待加入退出信号，并在关闭测试服务器前触发。仅测试失败清理问题，不阻断本次通过。
- Codex独立只读审查origin/main...HEAD，基线7223e5aa905a70f2147801625bdece0d8893876d；无P0/P1，无修改或提交，工作树干净。与origin/main合并检查无冲突，远端分支head与本次审查一致。
- 上轮P1已修复：每次选择和在途预留在短锁内完成，HTTP在锁外；重试仅选未尝试后端，无完成后的skipped补偿。独立overlay加载旧实现，两入口复现[0,20,0]；HEAD加强逐对断言后均为[0,10,10]。
- 配置完整命令test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...退出0，10个包通过、2个包无测试；已知main关停flaky本次未出现。
- 创建/并发创建定向race重复30轮通过，保留交错探活和逐后端503均摊；独立取消/全失败overlay诊断race重复20轮通过，两入口均覆盖预取消零请求、首次/末次尝试取消、在途预留释放、四端各一次及AllFailedError四项错误。git diff --check通过。
- 安装：无；已有Go 1.27.4及固定依赖，无清单或锁文件变更。测试HOME、缓存和诊断文件隔离于本run TMPDIR，仅本地服务；证据保留于本run evidence/FT13-round2-review。
- 未检查：真实Chrome/CDP、Docker、systemd；未启动浏览器或操作用户窗口，无UI改动。严格并发均摊仅覆盖上述确定性交错，不保证任意并发负载差≤1。
- 状态：第2/3轮，review/ → ready/；owner可查看分支及非阻断P3。未合并main、未发布。
