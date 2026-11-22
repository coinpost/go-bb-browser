---
branch: factory/FT26-pool-stale-binding-retirement
round: 1
---
# [P3] Pool 通过 tab_list/tab_focus 观察到的 tab 绑定只增不减，已消失的 tab 永久留在映射中

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B20；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`pkg/daemonclient/pool.go:605`（基线行号，实施前重新定位）。

触发条件：长生命周期的 Pool 周期性调用 TabList/TabFocus，而 tab 由池外途径关闭（空闲回收、用户手动关闭、其它客户端 tab_close）。

实际行为：bindObservedTabs（pool.go:605-613）把每次列表中出现的 tab 写入 p.tabs，从不删除该后端本次未再列出的 tab；绑定只会在本池成功 tab_close 或调用方显式 ForgetTab 时移除。复现：5 次列表各只含 1 个不同 tab，映射里留下 5 条，`ClientForTab("0:t0")` 仍为 true。

期望行为：某后端的 tab_list 成功返回即是该后端的权威集合：未再出现的绑定应被移除，之后对这些 id 的调用返回 *UnknownTabError。

影响：映射随浏览器 tab 流转无界增长；陈旧 id 的调用被发往 daemon 才得到 “unknown tab id” 的 RPC 错误，而不是本地的 *UnknownTabError，调用方的错误分支判断失效。

根因：观察式绑定只有添加路径，没有与后端权威列表对账的删除路径。

修复方向（建议，非指定实现）：在单后端 tab_list 成功后，删除该后端下不在本次结果中的绑定；与该后端并发的 tab_new 绑定需要用代次或短锁保护，避免把刚创建的 tab 误删。

回归验收：pool 测试：连续 5 次列表各含 1 个不同 tab 后映射大小为 1，旧 id 的 ClientForTab 为 false、调用返回 *UnknownTabError；并发 TabNew 与 TabList（-race）下新建 tab 不被误删；后端失败的那次列表不触发清理。

证据：repro-go.log（TestAudit_TabListDoesNotPruneStale FAIL）；repro/client_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

## factory · plan · 2026-11-22 09:14

### Behaviour
- 每个后端成功返回 tab_list 后，对该后端绑定与权威 Tabs 集合对账，删除不再存在的旧绑定；空列表清空该后端旧绑定。旧 ID 的 ClientForTab=false，后续操作返回 *UnknownTabError，不发送 RPC。
- 列表失败不清理该后端；部分成功仅对成功后端对账；TabFocus 只提供局部观察，不是全量集合，不能据此删除其他绑定。
- 并发 TabNew/TabFocus 新绑定不被在途旧列表删除；重叠 TabList 不让较旧快照覆盖较新结果；短锁不能跨网络 I/O，不因本项引入全池阻塞。显式 ForgetTab/成功关闭与旧列表重叠时不应复活旧绑定。
### Scope
- 仅 pkg/daemonclient 的绑定生命周期、必要文档与永久回归。不合并 stop/FT1 或其他 factory 分支，不实现 FT25 等其他项。
### Decisions
- 从最新 origin/main@4f86c1c588facd643e4596a40643b88a69061982 独立构建；原报告 d55fbc5 仅作证据。
- 优先有界状态和短临界区，具体并发算法由 coder 依据代码选择；避免无界 tombstone。Tab/Focus 元数据不应保活已不在权威列表内的旧 tab，若需兼容需说明协议依据。
- 保持公共 API、后端隔离、路由与错误类型不变。对并发 snapshot 的无法完全消除的语义限制明确记录。
### Checking
- 先添加正确行为永久回归并在修复前运行，证明失败：5 次各含不同单 tab 的列表最后仅 1 条，旧 ID UnknownTabError；空列表；后端失败和多后端隔离；TabFocus 非权威。
- 用本地 httptest 与 barrier 确定性测试 TabNew 和列表交错、重叠列表乱序、关闭/Forget 与旧快照交错，-race 重复运行，不依赖外网或睡眠猜时序。
- 全量 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；git diff --check。
- 无 UI 变更；真实非 headless Chrome/CDP、Docker、systemd 未检查须明示，不操作用户窗口。

## factory · 2026-11-22 09:14

基线：origin/main@4f86c1c588facd643e4596a40643b88a69061982；未合并 stop/FT1 或其他 factory 分支。构建提交：b0f442e（分支 factory/FT26-pool-stale-binding-retirement）。

实现：成功 tab_list 按后端对账，移除 Tabs 数组不再列出的旧绑定，后续本地返回 *UnknownTabError；失败后端不清理，TabFocus 仅新增观察。仅 Tabs 为权威集合，Tab/Focus 缓存元数据不再建立/保活绑定。添加 10 个永久回归测试及绑定生命周期文档。

并发决定：绑定、观察起点、移除记录使用池内逻辑时刻；只持短锁，不跨 RPC。新 TabNew/TabFocus/并发观察确认的绑定不被在途列表误删；在途旧观察不能复活其开始后被关闭、Forget 或对账移除的 ID。移除记录仅在对应后端存在在途观察时保留，最后一个观察结束即释放；单个长期挂起观察期间仍可能累积移除记录。

语义限制（review 重点）：逻辑时刻不是 daemon 快照顺序；重叠列表中任一次列出的 tab 可能暂时保留，从未绑定过的 ID 也可能被旧快照新增，直到下次不重叠成功列表纠正。未使用 daemon seq 排序，避免重启/时钟倒退影响。本项 manager 原计划希望旧快照不覆盖新结果，当前采取保守并发保护而非完全快照排序；该限制明确交由 review 核验。池外关闭或后端持续列表失败同样需等待后续成功列表纠正。Forget 后新发起的观察可重新绑定仍存活 tab。

回归证据：先在原 pool.go 上运行永久测试，连续五个单 tab 列表得到 got 5 want 1；空列表、按后端清理、旧观察复活、关闭/Forget 交错等用例均失败；防并发误删守卫在原代码已通过，不算修复前失败证据。修复后 go test -race -count=30 -run 'TestPool_' ./pkg/daemonclient/ 通过。使用本地 httptest/barrier，不依赖真实外网。

测试：coder 配置全量检查通过；manager 独立执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过（多数包命中缓存）。git diff --check 通过，保护路径无变化，工作树干净。测试 HOME、模块缓存及临时目录均隔离在本 run TMPDIR。Go 1.27.4。

安装：无未 pin 安装；go.mod/go.sum 未变，已 pin 模块从本机缓存离线恢复。

未检查：真实非 headless Chrome/CDP、真实 daemon 空闲回收/手动关页时序、Docker、systemd；无 UI 变化，未操作用户窗口，未使用 headless Chrome。

运行说明：首次 coder 包装调用 120 秒超时取消，确认无改动且无 coder 进程后直接调用 Claude Code 完成。无链接 issue/PR。new/ → review/，第 1 轮；仅 build，未 review、未合并 main、未发布。
