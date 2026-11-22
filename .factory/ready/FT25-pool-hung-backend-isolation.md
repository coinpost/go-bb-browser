---
branch: factory/FT25-pool-hung-backend-isolation
round: 3
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

## factory · review · 2026-11-22 08:46
head: 82cd6f7f46b1a232ed2decf3d53d66d0f4093092
verdict: fix
fix:
- [P1] pkg/daemonclient/pool.go:423: TabFocus 提前返回后，后台 goroutine 仍可读取公开 Client 配置；调用方在调用已返回后顺序修改另一后端的 Client.HTTP，-race 复现读写竞争，基线同一探针通过。启动 goroutine 前快照每个请求所需 HTTP 客户端、URL 和请求头，并保留原客户端原子请求 ID 计数器。
- [P1] pkg/daemonclient/pool.go:445: 主动取消与响应同时就绪时，select 可选响应并提前返回成功，绕过取消检查；探针先 cancel 再成功响应仍返回 nil 并写入绑定。提交成功结果和绑定前统一检查主动取消，取消时丢弃整批结果。基线 TabFocus 亦可复现，但本任务计划明确要求取消不写绑定，尚未满足。
- [P1] pkg/daemonclient/pool.go:459: 截止排空通道及普通接收未区分预算内与截止后完成的响应；唯一成功后端等待请求 context 截止后才返回时，TabList/TabFocus 仍成功并建立绑定。记录每个请求完成时间，并在普通接收和截止排空时统一过滤超预算成功响应。基线 TabFocus 亦接受迟到响应，TabList 返回截止错误却留下绑定；本任务预算内成功语义未满足。
notes:
- Codex 只读审查 origin/main...HEAD；未修改文件、未提交，分支 HEAD 未变，工作树干净。与 origin/main 可干净合并，无需冲突修复。
- HEAD 和 origin/main 均执行完整 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，退出 0，各 10 个测试包通过、2 个包无测试。FT25 定向 -race -count=3 通过；两个 500ms 挂起后端回归在基线按预期失败，健康端请求数为 0。
- 额外只读探针复现上述三项失败；已返回后才释放的迟到响应不改结果/绑定、inFlight 最终归零，以及首个非空 tab/focus 与最大 seq 检查通过。探针使用 Go overlay，未向代码分支写入测试文件。诊断与日志保留在本机 review run evidence/FT25-82cd6f7/。
- 使用现有 Go 1.27.4、隔离 HOME、TMPDIR 与离线缓存；无安装。未找到 review-code skill。git diff --check 通过。
- 未验证真实非 headless Chrome/CDP、Docker、systemd、真实网络黑洞；未操作用户浏览器。无关联 issue/PR，未发 GitHub 评论。
- 第 1/3 轮发现 P1，review/ → new/，下次 build 在原分支修复；未合并 main、未发布。


## factory · plan · 2026-11-22 12:30
第 2 轮补充计划：
- Behaviour：并发请求启动前快照每个后端 Client 的 HTTP 指针、URL 和深拷贝 headers，ID 仍取自原 Client 原子序列；主动取消在交付结果前优先，丢弃整批且不绑定；只采纳预算内完成解码的成功回复，普通接收与截止排空相同。
- Scope：review 三项 P1、最小 Client 内部请求 helper、永久回归和相关文档；不处理其他工厂任务。
- Decisions：不复制 atomic Client；以解码完成时刻严格早于有效截止为准；取消发生在最后检查之后不能保证生效。保留构造顺序、部分成功、2s 默认预算和 inFlight 最终释放。
- Checking：门控 context/进程内 transport 确定性覆盖取消、超预算、返回后重配竞争；覆盖 TabList/TabFocus、泛型 Call、结果/绑定无污染、共享 ID 序列、快照隔离；先红后绿、重复 race 和全量检查。

## factory · 2026-11-22 12:30
第 2/3 轮构建完成。d1711aa 合并 origin/main@7223e5a（无冲突，未 rebase）；79dc763e733ef841c5fa86462e5165677502ade2 修复三项 P1。未合并其他 factory 分支。
- 启动任何 worker 前同步快照各 Client 配置，后台不再读公开字段；共享原 Client 请求 ID 计数器。主动取消丢弃已有成功响应；解码完成时间不早于截止的回复统一判超时。
- 永久回归：CancelBeatsReadyAnswers 四子项修复前均返回错误成功；RejectsSuccessCompletedAfterBudget 四子项修复前均错误成功；KeepsInBudgetNextToOverBudget 两方法错误含超预算 0:ta；DoesNotReadClientConfigAfterReturn 修复前 race 五处。修复后全部通过。RequestIDsShareClientCounter 原先已通过，不算红证据；两个新 helper 单测无旧实现红证据。真实 deadline 排空探针旧实现 80 次均通过，未确定性复现该路径，红证据来自门控 context 的普通接收路径。
- coder 定向 -race -count=15 通过（141s），完整配置通过。manager 独立隔离 HOME/GOCACHE/TMPDIR、GOPROXY=off 运行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全通过，Go 1.27.4，10 个测试包通过、2 个无测试包；diff --check 通过，保护路径无变化，工作树干净。证据保留在 build run evidence/FT25-79dc763/ 的 red.log、green.log、manager-full.log。
- 安装：无；go.mod/go.sum 未变，使用既有 pin 模块缓存。
- 未验：真实 Chrome/CDP、Docker、systemd、真实网络黑洞、Go 1.26；无 UI 改动，未启动 Chrome 或操作用户窗口。快照不保证并发修改 Client 配置本身安全，也不深拷贝 *http.Client 内部；长期不响应取消的自定义 Transport 仍可能保留 goroutine，已覆盖最终释放路径。
状态：代码已推送，new/ → review/，待独立审查。本轮仅 build，未合并 main、未发布。

## factory · review · 2026-11-22 12:50
head: cd08b54d30a197f7b409284235434a937f89a068
verdict: fix
fix:
- [P1] pkg/daemonclient/pool.go:508: 主动取消的最后检查仍在 fanout 内，后续前缀转换、等待 affinity 锁、绑定和复制结果均不再检查；公开 TabList 探针三次均在 cancel 返回后仍无首个绑定，随后约 184ms 返回 30000 个 tab、nil 错误并写入绑定，锁控探针对 TabFocus 也复现。先暂存结果和整批绑定，在池锁内即将提交前重新检查主动取消，取消则不绑定、不覆盖 result；保留 deadline 到达时的预算内部分成功。
notes:
- [P2] pkg/daemonclient/pool.go:375: 后端 A 的旧 TabList 回复已完成、B 仍挂起时，成功 TabClose 删除 A 绑定；B 返回后旧列表又恢复已关闭 ID，后续 Eval 被发到 daemon。HEAD 三次失败、origin/main 三次通过。关闭/forget 时使该 ID 的在途旧观察失效，提交绑定时检查，允许新的有效观察重建。
- [P2] pkg/daemonclient/pool.go:458: worker 在 target.call 的延迟 Body.Close 返回后才记录完成时间/发布回复；自定义 transport 已在 150ms 预算开始约 1ms 时完成解码，仅阻塞 Close，两种聚合均错误丢弃成功，三次复现。解码完成即记录并发布，之后完成 Close/inFlight 清理。
- 审查前因 main 前进发生文档冲突，Codex 合并 origin/main@e63a15c，保留 FT13 分发与 FT25 并发语义；merge cd08b54 已由 manager 推送。未 rebase，未改保护路径。随后 Codex 只读审查 origin/main...HEAD，无文件修改/提交，工作树干净，head 未变。
- 配置完整命令 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 在 HEAD 与 origin/main 均通过（11 个测试包、1 个无测试包）；manager 再执行完整命令通过（部分测试命中缓存）。定向 race 三次、上一轮探针均通过；原始 500ms 挂起后端场景 HEAD 两方法三次通过，基线三次失败且健康后端零请求。上述新诊断 FAIL 是缺陷证据，不是配置测试失败。
- 证据：本机 review run evidence/FT25-79dc763/ 下 review-findings.md、review-public-cancel.log、review-head-diagnostics.log、review-baseline-diagnostics.log、review-probe-sources.tgz、review-full.log、review-baseline-full.log、manager-full.log。探针使用 overlay，不写入代码分支。
- 使用既有 Go 1.27.4、隔离 HOME/TMPDIR/缓存、GOPROXY=off，无安装；离线可选 go mod download 缺 Windows mousetrap 缓存，不影响 macOS 必需测试。未找到 review-code skill。
- 未验真实 Chrome/CDP、Docker、systemd、真实网络黑洞、其他操作系统、Go 1.26；无 UI 变动，未操作用户窗口。无关联 issue/PR，不发 GitHub 评论。
- 第 2/3 轮发现 P1，review/ → new/，下轮在原分支修复。未合并 main、未发布。


## factory · plan · 2026-11-22 15:31
第 3 轮补充计划：
- Behaviour：TabList/TabFocus 在前缀转换与结果准备期间不写绑定或调用者结果；暂存整批绑定与输出，在 affinity 锁内即将提交前重新检查主动取消。取消则返回 context.Canceled、不建立绑定且不覆盖 result；deadline 到达仍保留预算内成功。
- Scope：最新 review 的唯一 P1、最小内部提交 helper、永久回归及相关文档；两项 P2（关闭后旧观察重绑、Body.Close 延迟影响完成时间）本轮不处理。
- Decisions：昂贵准备和用户 decoder 在锁外完成，锁内检查后整批绑定；最终检查后取消不保证生效。结果暂存到新值，成功时整体替换，解码失败不改调用者结果；普通赋值在释放池锁后执行，不运行用户回调。
- Checking：TabList/TabFocus 与泛型 Call 的确定性锁控取消回归、准备期间取消、原绑定与 sentinel 结果不变、无取消成功、deadline 部分成功；先红后绿、重复 race、完整配置检查。

## factory · 2026-11-22 15:31
第 3/3 轮构建完成：提交 5a4d5b0。复用原分支，先快进到 review 合并提交 cd08b54；origin/main@e63a15c 已在分支内，无需再次合并。未合并其他 factory 分支。
- 修复：聚合结果与整批观察绑定先暂存，Pool.commitObserved 在池锁内最终检查主动取消，再批量提交；取消发生在准备/等锁阶段不写绑定、不覆盖 typed/map/any/nil/generic Call 结果。deadline 仍允许预算内成功。
- 边界与兼容性决定：最终锁内检查后的取消视为太晚，普通结果赋值在锁释放后完成。成功结果整体替换，不再保留预填结构中缺省字段或 map 旧键；解码失败不再部分写结果，但保持旧语义：观察到的 tabs 仍绑定后返回解码错误。此行为变更需 review 特别检查。
- 回归红证据：锁控取消 8 子项在旧实现均错误返回 nil；准备阶段取消两方法在旧实现均已污染绑定。修复后新回归 19 子项通过，定向 fanout -race -count=20 通过（210s）；无取消、deadline、custom decoder、解码失败用例为保留性检查，不算红证据。
- 测试：coder 完整配置通过；manager 独立隔离 HOME/GOCACHE/TMPDIR、GOPROXY=off 执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...` 全部通过，11 个测试包、1 个无测试包，Go 1.27.4。diff --check 通过，保护路径未变，工作树干净。证据保留在 build run evidence/FT25-round3/ 的 red.log、green.log、focused-race-x20.log、full.log、manager-full.log。
- 安装：无；go.mod/go.sum 不变，使用既有 pin 模块缓存。coder 报告一次早期 go vet 未套隔离环境、用了默认构建缓存并编译失败；后续所有测试已使用隔离环境。
- 未验：真实 Chrome/CDP、Docker、systemd、真实网络黑洞、其他系统、Go 1.26；未重跑原 30000-tab 计时探针，以确定性锁控和 decoder 控制回归覆盖。无 UI 变更，未启动 Chrome、未操作用户窗口。
- 已知未修 P2：成功 TabClose 后在途旧 TabList 可能恢复绑定；慢 Body.Close 可能令预算内已解码响应被判超时。下次独立 review 应继续评估，不宣称已修。
状态：代码已推送，new/ → review/，等待独立审查。本轮仅 build，未合并 main，未发布。

## factory · review · 2026-11-22 15:49
head: 5a4d5b072e444196b62d33f62289494bd31879ae
verdict: pass
notes:
- [P2] pkg/daemonclient/pool.go:446: 提交时持 p.mu 调用外部 ctx.Err；自定义 Context 若在 Err 中调用 ClientForTab 记录路由会重入锁，阻塞整个池的 tab 操作。两方法 HEAD 各 3/3、main 同探针通过。锁外取得 Done 通道，锁内检查关闭状态，锁外读取错误原因，避免锁内用户回调。
- [P2] pkg/daemonclient/pool.go:483: 泛型 Call 按静态接口类型暂存，丢失接口预置具体指针；有方法接口解码失败，any 内 *T 则变为 map，uint64 9007199254740993 舍入为 9007199254740992。两方法 HEAD 各 3/3、main 通过；按原动态类型创建独立解码目标。
- [P2] pkg/daemonclient/pool.go:487: 自定义 UnmarshalJSON 写入自身 strings.Builder 后，反射赋值复制已使用的 Builder，Call 成功但后续追加 panic。两方法 HEAD 各 3/3、main 通过；为地址敏感结果提供指针交付方式并明确支持边界，避免搬动不可复制的已解码对象。
- [P2] pkg/daemonclient/pool.go:454: 已知旧列表重绑仍在：A 回复后 B 挂起，TabClose 解绑，放行 B 又恢复 A，后续 Eval 发出 RPC。HEAD 3/3、main 保持解绑；提交时按关闭代次过滤旧观察。
- [P2] pkg/daemonclient/pool.go:547: 已知慢 Body.Close 问题仍在：150ms 预算内约 1ms 已解码，仅 Close 阻塞，两方法仍丢弃成功。各 3/3，main 对照保留；解码完成即记时并发布，随后清理。
- [P3] pkg/daemonclient/README.md:105: 主动取消“立即返回”未覆盖同步自定义 decoder；回调阻塞时取消后 200ms 仍等待，释放后正确返回 Canceled 且无绑定/输出污染。main 也等待；文档应说明该边界。
- Codex 独立只读审查 origin/main@e63a15c...HEAD，无 review-code skill。原 P1 触发时序均通过：500ms 挂起首端与健康次端、取消先于成功/已有成功、拒收超预算回复、返回后修改 Client.HTTP、30000 tab 准备阶段取消及两方法等 affinity 锁时取消；各重复 3 次。未沿用已修 finding。
- HEAD 与导出 main 的配置完整命令 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 均退出 0，各 11 个测试包通过、1 个无测试包；HEAD 定向 race 重复 5 次通过。新增诊断失败与配置测试失败分开记录。manager 核对完整测试日志、状态文件、报告、提交实现和完整性记录，未另行重复全量。
- 普通 map 成功整体替换、解码失败保留输出但绑定已观察 tab，符合本轮决定；上述接口/不可复制值为额外兼容性问题。没有 P0/P1，按工厂规则 pass，review/ → ready/，不表示无缺陷；owner 应注意五项 P2 后再决定 ship。
- 证据保留于本机 review run evidence/FT25-5a4d5b0/：review-report.md、sources/、full.log、full-base.log、focused.log、original-head.log、independent-head/base.log、address-head/base.log、known-head.log、latency-head/base.log、integrity.log；overlay 诊断非永久回归。报告所称同名 .log.status 实际为去掉 .log 的 .status。
- 使用既有 Go 1.27.4、离线模块缓存、隔离 HOME/TMPDIR/缓存，GOPROXY/GOSUMDB=off，无安装。工作树干净，HEAD 未变，保护路径无差异。未验证真实 Chrome/CDP、真实网络黑洞、Docker、systemd、其他系统或 Go1.26，未操作用户窗口。
- 无关联 issue/PR，不发 GitHub 评论。第3/3轮，未改代码、未合并 main、未发布。
