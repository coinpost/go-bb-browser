---
branch: factory/FT25-pool-hung-backend-isolation
round: 1
---
# [P3] Pool.TabList/TabFocus 顺序轮询后端且无单后端超时，一个挂起的 daemon 使整个调用失败

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B19；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`pkg/daemonclient/pool.go:324`（基线行号，实施前重新定位）。

触发条件：池中靠前的某个后端接受连接但不应答（主机假死、网络黑洞、daemon 卡在长操作），调用方带截止时间调用 pool.TabList / pool.TabFocus。

实际行为：callTabList（pool.go:324-343）与 callTabFocus（:361-）在调用方 context 上依次请求各后端；挂起的后端耗尽全部截止时间后，下一轮循环的 ctx.Err() 直接返回 `context deadline exceeded`，健康后端从未被询问（复现中后端 B 的 tab_list 调用数为 0）。没有截止时间时则无限期阻塞（默认 http.Client 无超时）。

期望行为：AGENTS.md：TabList 查询每个后端，“a down backend is skipped”，仅全部失败才返回 *AllFailedError；一个无响应后端不应阻止返回其余后端的 tab。

影响：单个 daemon 假死即让池级 tab 列表/焦点查询全部超时，依赖它做巡检或选 tab 的调用方整体不可用。

根因：多后端聚合是串行的，并共享调用方的整个超时预算。

修复方向（建议，非指定实现）：并行查询各后端（或为每个后端划分子超时），收集在截止前返回的结果；超时的后端计入 lastErrs 并被跳过。

回归验收：pool 测试：后端 0 永不应答、后端 1 正常，500ms 截止的 TabList 返回后端 1 的 tab 且 err==nil；TabFocus 返回后端 1 的焦点；全部挂起时返回 *AllFailedError 或 ctx 错误；合并顺序仍按构造顺序。

证据：repro-go.log（TestAudit_TabListHungBackend FAIL）；repro/client_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。


## factory · plan · 2026-11-22 08:29

### Behaviour
- TabList 与 TabFocus 并发询问后端；一个接受连接但不应答的后端不能耗尽预算后阻止健康后端被询问。
- TabList 按构造顺序合并成功结果，保持 ID 前缀、绑定、最大 seq 与首个非空 Tab/Focus；TabFocus 选择构造顺序中首个成功后端，而非最快响应者。
- 调用截止时保留此前已完成的成功响应；全部失败返回 AllFailedError 或调用方 context 错误。调用前已取消则不发送请求；主动取消应及时终止。
- 无调用方 deadline 时也必须有有限的聚合查询超时，避免无限等待。空列表是成功，不等同失败；所有请求完成时立即结束，不额外等超时。
### Scope
- 仅 pkg/daemonclient 的 TabList/TabFocus 聚合实现、其永久回归与直接相关文档；不改其他方法的超时策略，不处理其他 factory 项目。
### Decisions
- 从最新 origin/main 独立构建，不合并 stop/FT1。并发收集并按索引消费，避免网络完成顺序改变语义；后台工作不直接写调用者 result 或池绑定，防止返回后迟到副作用。
- 聚合采用内部有限默认超时（建议 2 秒；实现时核对现有约定），以调用方更短 deadline 为上限。仅这两种只读聚合受影响，单 Client/其他 Pool 调用不变；文档明确超时和部分成功策略。
- 使用支持取消的 HTTP context、每请求独立结果与有界结果通道，不引入 worker pool、外部依赖或多余公共配置层。维持 in-flight 计数平衡。
### Checking
- 先添加正确行为的永久回归并验证修复前失败：后端 0 挂起、后端 1 健康、500ms deadline，TabList/TabFocus 返回后端 1 成功数据。
- 覆盖全部挂起、无 deadline 的有限超时、预先取消/中途取消、反向完成顺序仍按构造顺序、部分失败、空列表、泛型 Call、绑定与 in-flight 清理，无外网依赖。
- 执行配置全量 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；重复定向 -race。
- 记录未检查的真实 Chrome/CDP、Docker、systemd。无需 UI 变更或真实 Chrome；若确需 Chrome，只允许独立非 headless CDP 实例。

## factory · 2026-11-22 08:29

基线 origin/main@4f86c1c588facd643e4596a40643b88a69061982；提交 82cd6f7。未合并 stop/FT1。

实现：TabList/TabFocus 并发查询并按构造顺序消费结果；仅调用方线程绑定 tab 和写 result，参数预序列化，迟到响应不产生副作用。TabList 保留首个非空 tab/focus、最大 seq 和有序列表；TabFocus 确认构造顺序首个成功者后可提前返回。

决定：内部默认聚合上限 2s，取其与调用方 deadline 较小者；deadline 到达保留已完成成功结果，全部失败返回 AllFailedError 或调用方 context 错误；主动取消返回 context.Canceled，不写绑定。无新增公共 API。慢但健康的后端超过 2s 也会被跳过；TabFocus 现在会并发请求全部后端。只改变这两个只读聚合，单 Client 和其他 Pool 方法不变。inFlight 随每个请求退出释放，不等待异常的不响应取消的自定义 Transport。

回归：coder 先加永久测试并在生产代码未改时确认两个 500ms 挂起后端用例失败，健康端请求数为 0；无 deadline 用例超过 6s 仍阻塞。修复后全部通过。覆盖构造顺序、全部挂起、主动取消、空列表、泛型 Call、部分失败、迟到响应、绑定与计数收敛；coder 定向 -race -count=15 通过。

测试：coder 全量通过；manager 独立执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... 全部通过（10 个测试包、2 个无测试包，Go 1.27.4）。git diff --check 通过，保护路径无变更，工作树干净。测试使用隔离 HOME/缓存/TMPDIR、GOPROXY=off。修复前失败日志和 manager 检查日志已保留于 build run evidence/FT25-82cd6f7/。

安装：无；go.mod/go.sum 不变。

未检查：真实非 headless Chrome/CDP、Docker、systemd、真实 SYN 网络黑洞、自定义不响应 context 取消的 Transport；只用本地 httptest 假 daemon，未启动 Chrome、无 UI 变更、未操作用户窗口。

状态：分支已推送，new/ → review/，第 1 轮。本次仅 build，未做 review，未合并 main，未发布。
