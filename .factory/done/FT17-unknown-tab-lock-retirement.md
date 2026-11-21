---
branch: factory/FT17-unknown-tab-lock-retirement
round: 1
---
# [P2] 未知/过期 tab id 的请求在 tabCDPLocks 中留下永不回收的条目（key 由客户端控制）

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B10；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`internal/daemon/rpc_connections.go:193`（基线行号，实施前重新定位）。

触发条件：任意带 tab 参数的 RPC（goto/eval/click/network/tab_close…）使用不存在或已关闭的 tab id；每个不同的字符串一条。客户端在 tab 被空闲回收后继续用旧 id 重试即会触发。

实际行为：lockTab 在解析 tab 之前按原始字符串建锁条目（rpc_connections.go:193-199）；releaseTabLockRef 只有 forgotten=true 才删除（:219-226），而 forgotten 仅在已注册 target 被移除时设置。未知 id 的条目永久保留：300 次请求后 map 中有 300 条。key 最长可接近 MaxBodyBytes（默认 1 MiB）。

期望行为：锁条目在引用计数归零且对应 tab 未注册时立即回收；未知 tab 的请求不留下任何状态。

影响：长期运行的 daemon 内存随陈旧/错误 tab id 请求单调增长；未配置 token 时任何能访问端口的调用方可用大 key 快速耗尽内存（每请求最多约 1 MiB 常驻）。

根因：锁以客户端传入字符串为 key 且先建后验；回收条件只覆盖“曾注册后被移除”的 tab。

修复方向（建议，非指定实现）：先用 s.tabs.Lookup（必要时一次同步）确认 tab 存在再建锁；或在 releaseTabLockRef 中当 refs==0 且该短 id 当前未注册时删除条目。

回归验收：daemon 测试：对 300 个不存在的 tab id 发 goto/eval/network/tab_close 后 len(tabCDPLocks)==0；对已关闭 tab 的旧 id 重试后同样为 0；既有 TestLockTabHonorsContextDeadline 与并发串行化测试保持通过。

证据：repro-go.log（TestAudit_UnknownTabRequestsMustNotLeakLockEntries FAIL）；repro/daemon_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。


## factory · plan · 2026-11-22 05:52

### Behaviour
- 空 short 保持 no-op；非空 key 在有 holder 或 waiter 时共用同一 semaphore，最后引用退出即释放 map 条目，不论 tab 是否有效。
- 未知/过期 tab RPC 保持硬错误；取消安全扣减引用；目标移除及同 key 重新请求不得产生同时运行的两代锁。首次 nil map 安全。
### Scope
- 仅 per-tab lock 回收、关联清理调用及 daemon 永久回归；不修改协议、注册表、observer 生命周期或其它任务，不合并 FT1。
### Decisions
- 空闲锁不必缓存，对所有 key 采用引用归零即删；保留条目指针校验，移除冗余 forgotten/forgetTabLock。
- tabMuOps 下不新增 registry 查询或 CDP 调用，不增加锁顺序。新请求只能在旧引用全部退出后创建新条目。
### Checking
- 正确行为永久回归先红后绿：300 个未知 id 的 goto/eval/network/tab_close、已关闭 id 重试、有效/无效 key 等待取消、并发串行化、目标移除后的锁代次共用。
- 定向重复 race 与配置完整 gofmt/vet/race；全部本地 fake conn，隔离 HOME、缓存与临时目录。真实 Chrome/Docker/systemd 未检如实说明。

## factory · 2026-11-22 05:52

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a，独立构建，未合并 FT1。Claude Code 提交：713ab4c。

实现：releaseTabLockRef 对所有 key 在 refs==0 且 map 指针匹配时删除条目；移除 forgotten 字段、forgetTabLock 及 target 清理中的冗余调用/short 参数。所有并发引用仍在 tabMuOps 下取得同一条目，保留取消和串行化行为；空 key 不创建状态。仅 3 个 daemon 文件修改。

永久回归与证据：修复前 300 个未知 id 加一个 64KiB key 残留301条、关闭后重试残留3条、最后引用释放与并发回收断言失败；修复后通过。目标移除后同 key 共用旧条目的守护测试修复前已通过，不当作修复证据。coder 锁相关 -race -count=200、300 id 回归 -race -count=20 通过；完整配置检查通过。红绿日志保留于本 run evidence/FT17-713ab4c/。

manager 独立使用全新 GOCACHE、throwaway HOME 再执行配置命令 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，退出0；8个测试包通过、2个无测试包。git diff --check 通过，保护路径无变更，工作树干净，提交范围仅本项。

安装：无；已有 Go 1.27.4，go.mod/go.sum 未变，无新工具或未 pin 依赖。测试临时文件及缓存均在本 run tmp 下，结束清理。

未检查：真实 Chrome/CDP、Docker、systemd 均未验证；全部本地 fake conn，无真实外网依赖，无 UI 改动；未覆盖接近1MiB极限 key，用64KiB代表。

状态：代码分支已推送，new/ → review/，第1/3轮。review 应检查引用退出与新条目创建的并发边界。本次仅 build，未执行独立 review、未合并 main、未发布。

## factory · review · 2026-11-22 06:16
head: 713ab4ced8349fff235b2d999c970f31e50001b7
verdict: pass
notes:
- [P3] internal/daemon/tab_lock_test.go:84: 五个新增测试创建的 Server 未关闭 audit writer，测试进程内每轮遗留五个 goroutine；建议在创建后使用 t.Cleanup 调用 srv.closeAudit() 并检查错误。

Codex 独立审查 origin/main...HEAD，未发现 P0/P1；holder/waiter 引用、取消、先归还 semaphore 再扣引用及 map 指针校验未发现两代锁并行问题。与本次拉取的 origin/main 可干净合并，无冲突修复或代码改动。

测试：在本 run TMPDIR 下的 throwaway HOME/缓存执行配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`，退出0；五个 FT17 定向回归 `go test -race -count=20 -run "^(TestUnknownTabRPCsLeaveNoLockEntries|TestClosedTabRetriesLeaveNoLockEntries|TestLockTabReleasesEntryAfterLastReference|TestLockTabSerializesConcurrentSameKey|TestRemovedTargetLockIsSharedUntilReferencesDrain)$" ./internal/daemon`，退出0。manager 核对 HEAD、干净工作树及 git diff --check。无 review-code skill，采用直接代码审查。

安装：无新增未 pin 依赖或工具，使用已有 Go 1.27.4 和本地模块源。
未检查：未复跑修复前版本；真实 Chrome/CDP、Docker、systemd 未验证，仅 fake conn；无 UI 改动。无关联 issue/PR 需评论。
状态：review/ → ready/；P3 为非阻塞建议，未自动修复。未合并 main、未发布。

## factory · 2026-11-22 06:32

owner 要求：交付 FT17。已将已审查 head 713ab4ced8349fff235b2d999c970f31e50001b7 合入 main 并推送，merge: 4f86c1c588facd643e4596a40643b88a69061982。无合并冲突；与当时 origin/main@9fa35626d7db7d51afd56cdc3ff33f8f5a0acfa9 的 merge-tree 干净。仅 ship FT17，未一并处理其它 ready 项。

测试：在 throwaway HOME 下先跑变更包 `go test -race ./internal/daemon` 通过（ok 2.509s）。累计 main 完整 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 首次因 GOPROXY=off 且模块缓存缺 github.com/dop251/goja 失败，属环境而非代码；`go mod download` 恢复已锁定依赖后（go.mod/go.sum 未改）以 GOPROXY=off -mod=readonly 重跑退出 0。10 个测试包通过、2 个无测试包（internal/timeout、pkg/protocol）。保护路径无变更。工作树干净。

按 ship=[merge,push] 未打 tag、未发布、未部署；无关联 issue/PR 需关闭，未删除代码分支 factory/FT17-unknown-tab-lock-retirement。原 review 的非阻断 P3 保留。ready/ → done/。
