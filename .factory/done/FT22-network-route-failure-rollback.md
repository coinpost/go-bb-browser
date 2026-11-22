---
branch: factory/FT22-network-route-failure-rollback
round: 2
---
# [P3] network_route 同步 CDP 失败后规则仍留在内存表中，没有回滚

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B16；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/browser/network_routes.go:104`（基线行号，实施前重新定位）。

触发条件：network_route 调用期间 CDP 同步失败：tab attach 失败、Fetch.enable 报错/超时、或客户端断开导致请求 context 取消。

实际行为：AppendNetworkRouteContext 先把规则写入 rs.rules（network_routes.go:103-105）再 syncRoutes；失败时 RPC 返回 `network_route failed`，但规则保留：NetworkRouteCount 为 1，客户端重试会得到重复规则，之后任一次成功同步会把这条“失败”的规则一并下发生效。RemoveNetworkRoutesContext（:122-141）同样先改表后同步；规则清空分支（:170-176）即使 Fetch.disable 失败也会 stopListening。

期望行为：RPC 返回错误时路由表保持调用前的状态（或明确标记未生效），routes 计数与浏览器实际拦截配置一致。

影响：调用方看到失败却在之后被静默拦截/mock；routes 计数虚高；删除路径失败时可能留下浏览器侧仍启用 Fetch 而本地已无监听者的状态（请求被挂起，需真实 Chrome 确认）。

根因：先改内存状态后做 CDP 同步，失败路径没有恢复旧值。

修复方向（建议，非指定实现）：syncRoutes 失败时恢复调用前的规则切片（Append/Remove/Set 三处）；规则清空分支仅在 Fetch.disable 成功后才停止监听。

回归验收：browser 测试：用已取消的 context 调 AppendNetworkRouteContext 返回错误后 NetworkRouteCount==0；注入 routeCDP/同步失败时 Remove 后规则数不变；成功路径既有测试保持通过。

证据：repro-go.log（TestAudit_FailedNetworkRouteMustNotLeaveRuleRegistered FAIL）；repro/browser_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

## factory · plan · 2026-11-22 07:15

### Behaviour
- Append/Set/Remove 仅在对应 Fetch 同步成功后提交规则变化；context 已取消、attach 失败、enable/disable 失败时保留调用前规则，重试不重复规则。
- 删除最后规则时，仅在 Fetch.disable 成功后停止监听；失败保持旧规则和有效监听，以继续处理暂停请求。
- 并发路由修改不能互相覆盖或被旧失败回滚；target 清理不得被在途更新复活。首次启用失败的 worker/listener 应安全清理，保留既有成功配置。
### Scope
- browser 路由状态、同步和必要的永久测试；不变更 RPC 协议、不引入新依赖、不合并 stop/FT1，不处理其他条目。
### Decisions
- 基于最新 origin/main@4f86c1c 独立构建。采用最小事务边界：序列化状态更新及同步，或生成候选配置后成功提交；保留清晰的锁顺序和 target generation 保护，避免持有规则表锁跨 CDP 阻塞。
- CDP 超时/断连具有远端是否执行的不确定性，本地至少保留最后确认状态，明确说明不能承诺远端原子回滚，不以注释冒充修复。
### Checking
- 修改生产代码前新增正确行为回归并证实原代码失败：取消 context 的 append 后 count=0；失败的 Set/Remove 保留完整规则；disable 失败不取消监听；成功后重试及并发不丢失规则；清理和在途更新不复活。
- 使用本地协议桩或现有注入方式，无真实外网；执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...。
- 无 UI 改动；真实非 headless Chrome、Docker、systemd 未测试时如实记录，禁止 headless 或用户窗口操作。

## factory · 2026-11-22 07:15

基线：最新 main@4f86c1c；未合并 stop/FT1。构建提交：a0e00e5。第 1 轮，new/ → review/，仅 build，未合并 main、未发布。

实现：Append/Set/Remove 统一生成候选规则，仅 Fetch 同步成功后提交；cdpMu 串行整个更新，规则表锁不跨 CDP；generation 防止 target 清理后的在途写入复活。Fetch.disable 失败保留旧规则与监听。首次 enable 失败补发 disable，确认后释放 worker/listener；补发也失败则保留无规则的监听，放行暂停请求，待后续更新或 target 清理回收。

决定和限制：网络超时/断连无法确认远端是否执行，本地仅保存最后确认规则，不保证远端原子回滚；下一次成功更新下发完整 patterns 对齐。首次启用失败的补偿 disable 不使用已取消请求 context，可能额外持全会话 cdpMu 一个操作超时（60s）；review 请重点检查这一边界、候选规则生效窗口与清理并发。

永久回归：修改生产代码前，TestNetworkRouteCanceledAppendLeavesNoRule 失败（failed append left 1 rule(s) registered）；TestNetworkRouteFailedSyncKeepsRulesAndListener 的 append/set/set_empty/remove_one/remove_all 验证旧实现改变规则，清空路径还取消监听。修复后通过。另新增首次 enable 失败清理和重试、未确认时保留监听、并发修改、在途 target 清理回归；后四类依赖新增注入点，无修复前失败记录。

检查：Claude Code 报告完整配置检查通过，路由相关 -race -count=20 通过。manager 独立以隔离 HOME/GOPATH/GOCACHE/TMPDIR 执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过（10 个测试包，2 个无测试包）；diff --check 通过。仅修改 browser 路由实现与永久测试，无保护路径、依赖文件变更，工作树干净。失败/成功日志保留于本机 build run evidence/FT22-a0e00e5/。

安装：无额外工具或未 pin 依赖。coder 恢复 go.sum 已 pin 模块时写入了默认用户模块缓存，违反本次临时缓存隔离要求；未删除共享缓存。manager 复验的 HOME、Go 缓存与所有临时数据均在本 run tmp 内。

未检查：未启动真实 Chrome，Fetch.disable 失败后保留监听的实际暂停请求行为、disable 成功释放暂停请求未实机验证；Docker、systemd 未验证，无 UI 改动、未操作用户窗口。无链接 issue/PR。

## factory · review · 2026-11-22 07:31
head: a0e00e590abb18e95a9a815522deae62281b7081
verdict: fix
fix:
- [P1] internal/browser/network_routes.go:225: 首次 Fetch.enable 失败后的补偿 Fetch.disable 使用 context.Background()，仍持有全会话 cdpMu，最多再阻塞 60 秒；其他健康 tab 的路由更新等待该互斥锁时无法响应取消，耗尽整个 RPC deadline。隔离每个 target 的更新串行边界，让等待支持 context 取消，并给补偿设置独立短超时；补充 A 补偿阻塞但 B 更新仍可及时完成的回归。
notes:
- [P2] internal/browser/network_routes.go:157: 取得 tab context 后排队，直到取得 cdpMu 才读 generation；若期间 ForgetTarget，旧调用可使用清理后的 generation 和已取消 tab context 创建 worker。enable/补偿均失败时留下已删除 target 的 worker。应在 attach/等锁前记录 generation，取得更新锁后校验，提交时继续二次校验。
- [P2] internal/browser/network_routes.go:186: Fetch.enable 已启用、候选规则尚未提交时 worker 仍按旧表处理暂停事件。临时协议桩用首次 Abort 规则复现发送 Fetch.continueRequest 而非拦截，最终更新却成功。建议监听继续入队，仅暂停该 tab 的 worker 分派，成功后用新表、失败用旧表处理积压事件。
- [P3] internal/browser/network_routes.go:168: 清空规则时 commit 因 target generation 改变返回 false，调用仍返回 nil。应与非空分支一致返回 ErrorTargetGone（操作名 Fetch.disable），增加最后规则删除与清理并发回归。

审查：Codex 对 origin/main@4f86c1c...HEAD 只读审查，分支可干净合并 main，无代码修改或提交。临时 -overlay 与 -race 验证了上述四个时序；原有失败回滚、disable 失败保留监听与同 tab 并发更新验证通过。
测试：隔离 HOME、TMPDIR、GOPATH、GOCACHE、GOMODCACHE 执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过（10 个测试包，2 个无测试包）；git diff --check 通过，工作树干净。
安装：无额外工具或未 pin 依赖，仅恢复 go.sum 锁定模块。未找到 review-code skill，按只读 code review 执行。
未检查：真实非 headless Chrome 的 CDP 事件时序、Docker、systemd；未操作用户窗口。无链接 issue/PR，无需 GitHub 评论。
状态：第 1/3 轮发现 P1，review/ → new/，下一次 build 在原分支修复；P2/P3 为审查备注。未合并 main、未发布。


## factory · plan · 2026-11-22 12:08
第 2 轮补充计划：
- Behaviour：路由更新仅按 target 串行，等待支持 context 取消；首次启用失败补偿使用独立短超时，A 补偿阻塞不拖累 B。generation 在 attach/排队前记录，加锁后及提交时再次校验，避免清理后复活。
- Scope：最新 review 的 P1 及直接相关 P2/P3，限 browser 路由实现与永久回归；不处理其他工厂项。
- Decisions：补偿独立 1s；监听继续入队，仅暂停本 tab 分派，成功后使用新规则、失败使用旧规则；清空提交遇代次变化返回 Fetch.disable 的 ErrorTargetGone。合并最新 main，不 rebase。
- Checking：先新增正确行为 barrier 回归并证实旧代码失败，再修复；定向重复 -race 与配置全量检查，测试 HOME/缓存/TMPDIR 隔离，无真实外网依赖。

## factory · 2026-11-22 12:08
第 2/3 轮完成。合并 main@7223e5a：b40afc7（无冲突）；修复提交 c222eb594dc519e1d409a0979309060ce5258f89。未合并其他 factory 分支。

实现：移除全会话 cdpMu，改为按 target 的可取消更新锁，引用计数回收锁条目；首次 enable 失败补偿 disable 独立 1s deadline。generation 提前捕获并重复校验。更新期间继续接收暂停事件但暂缓该 tab 分派，提交后使用确认规则；其他 tab 不受影响。清空规则提交失去 generation 返回 ErrorTargetGone（Fetch.disable）。

决定与边界：补偿仍未确认时保留无规则监听、放行暂停请求；退役 worker 排空队列。同 tab 的暂停请求可能等待在途 Fetch.enable 到操作超时；远端超时执行与本地确认不能保证原子一致，后续成功更新重新对齐。

永久回归：生产改动前六项正确断言失败，覆盖 A 补偿阻塞 B、等待取消、补偿 deadline、attach 中清理复活、清空时 target 消失、暂停事件错用旧规则。其他 tab 分派守卫原代码已通过，不当作红证据；锁回收测试依赖新字段，修复后新增，无红证据。修复后 21 项路由测试通过，定向 -race -count=20 通过。红绿日志与报告保留于本机 build evidence/FT22-round2/。

检查：Claude Code 执行完整配置命令通过；manager 独立以隔离 HOME/GOPATH/GOCACHE/GOMODCACHE/TMPDIR、GOPROXY=off 运行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...` 通过，10 个测试包、2 个无测试包。Go 1.27.4，未验证目标 Go 1.26。git diff --check 通过，保护路径无变更，代码工作树干净；独有历史仅本项及 main merge。

安装：无工具或未 pin 依赖安装；go.mod/go.sum 未变，锁定模块从本机缓存离线恢复。一次 go mod download 提示缓存缺 mousetrap v1.1.0 的 .mod；当前平台构建、vet、测试未需要它，均通过。

未检查：真实非 headless Chrome/CDP 时序、分派暂停及 Fetch.disable 后 Chrome 实际恢复行为，仅本地协议桩；Docker、systemd。无 UI 变更，未启动 Chrome，未操作用户窗口。无关联 issue/PR。

状态：代码已推送，new/ → review/，第 2 轮；本次仅 build，未做独立 review、未合并 main、未发布。

## factory · review · 2026-11-22 12:19
head: c222eb594dc519e1d409a0979309060ce5258f89
verdict: pass
notes:
- [P2] internal/browser/network_routes.go:497: 同 tab 连续更新时，积压的暂停事件在出队时重新读取最新规则，可能跳过事件入队期间已成功提交的 Abort 规则。本地 WebSocket CDP 桩：worker 阻塞于较早请求，更新 A 加入 Abort 并接收匹配暂停事件，A 成功后更新 B 删除该规则且保留另一规则，释放 worker 后 3/3 次发送 continueRequest 而非 failRequest。建议为更新期间入队的事件绑定该次更新最终确认的不可变规则快照，补充连续更新积压回归。
- [P3] internal/browser/network_routes.go:188: 清空规则在等待同 target 更新锁时取消，及时退出但 CDPError.Op 错报 Fetch.enable。先确认排队再取消的探针 5/5 次复现；建议使用独立路由更新操作名，或清空操作标记 Fetch.disable。

审查：Codex 对 origin/main@7223e5a...HEAD 只读审查，首轮 P1 已修复，本轮无 P0/P1。真实回环 WebSocket CDP 桩扣住 A 的补偿 disable 回包，B 三次均在 4ms 内成功，A 约 1s 返回原错误，后续暂停请求仍被放行。分支可干净合并 main。未找到 review-code skill，依完整条目、README、AGENTS 执行审查。
测试：隔离 HOME/GOPATH/GOCACHE/GOMODCACHE/TMPDIR、GOPROXY=off 执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过，10 个包通过、2 个无测试包；路由定向 -race -count=10 通过；临时 overlay 与协议桩验证上述备注。manager 检查逐包日志、head、diff --check、diff 路径及工作树，均符合只读约束。证据保留于本机 review run evidence/FT22-round2-review/（review.md、config-test.log、routes-race.log、wire.log、probe.log）。临时探针随 run 临时目录清理，日志保留。
安装：无工具或未 pin 依赖安装，仅只读复制已有模块缓存至隔离目录。实际 Go 1.27.4 darwin/arm64，未验证目标 Go 1.26。
未检查：真实非 headless Chrome/CDP 时序、Fetch.disable 后实际请求恢复、Docker、systemd；未启动或连接 Chrome、未操作用户窗口。无关联 issue/PR，无需 GitHub 评论。
状态：第 2/3 轮通过阻塞门槛，review/ → ready/；P2/P3 留作 owner 审阅。仅 review，未合并 main、未发布。


## factory · 2026-11-22 12:37
owner 要求将 ready 项全部 ship；本项已合并并推送 main。

merge: dbfef073550dd2bbf3ab528437a1d2df47da5e03
main: e63a15c1c8a64c3c1b6a501d965bf17a65025bd3
tag: 无；按 ship=[merge,push]，未打 tag、未触发 tag 发布。

验证：每项合并后均执行完整 gofmt、vet、无缓存 go test -race -count=1 ./...；最终累计 main 再次通过（11个测试包、1个无测试包）。manager 核验审查 head 与远端分支一致，所有批准分支均为最终 main 祖先；保护路径无差异，diff --check 通过、工作树干净。FT19/FT20 冲突由编码代理解决，保留完整 target 生命周期、无 RPC 历史保留、日志换行恢复及私有权限语义，相关测试适配后通过。

关联 issue/PR：无 source 或 owner 明确关联，无关闭对象。分支保留：配置不含 clean。安装：无。测试使用已有 Go1.27.4，HOME/缓存/TMPDIR 隔离、固定模块离线恢复。真实 Chrome/CDP、Docker、systemd 未验证；既有审查非阻塞 notes 未额外修复。

ready/ → done/。FT28 等非 ready 项不在本次范围。
