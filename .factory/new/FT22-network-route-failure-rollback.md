---
branch: factory/FT22-network-route-failure-rollback
round: 1
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
