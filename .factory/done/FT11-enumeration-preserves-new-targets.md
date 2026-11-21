---
branch: factory/FT11-enumeration-preserves-new-targets
round: 1
---
# [P2] 过期的 Target.getTargets 快照晚于 tab_new 应用时，会把刚创建的受管 tab 从注册表和空闲跟踪中删除

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B04；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`internal/daemon/tab_idle.go:38`（基线行号，实施前重新定位）。

触发条件：并发：请求 A（tab_list / tab_focus / tab_select，或任何未知 tab 触发的 resolveTab 回退、runTargetSync 重同步）已取得不含新 tab 的 getTargets 结果但尚未调用 syncTabsFromTargets；请求 B 的 tab_new 在此间隙完成 createTarget 并登记。

实际行为：syncTabsFromTargets（tab_idle.go:38-45）无条件把注册表与快照对齐：SyncPageTargetsDetailed 把新 tab 记为 removed（internal/state/tab_registry.go:54-60），clearRemovedTarget 随即 forgetTabManaged、清观测缓冲、forgetTabLock，并对真实 Session 调 ForgetTarget（取消该 tab 的 chromedp 子 context、清路由）。复现中 tab_new 返回的短 id 4d5e 被移出注册表且不再受管。

期望行为：早于 tab 创建时刻取得的 target 快照不得移除更晚登记的 target；tab_new 返回成功的 tab 必须保持注册与受管状态，直到真正关闭。

影响：该 tab 在本进程内失去受管标记，永不被空闲回收（标签页泄漏，直到 daemon 重启靠 rpc.jsonl 恢复）；已启用的观测缓冲与拦截规则被清空；若该 tab 已建立 tabPool 连接，ForgetTarget 取消活跃 chromedp 子 context 会按其注释所述直接关闭该标签页。多客户端/Pool 并发 tab_new 与 tab_list 时是微秒级但持续存在的竞态。

根因：“读浏览器 target 列表”与“应用到注册表”之间没有版本/时序保护，注册表的增量登记（tab_new、target 事件）与全量对齐互相覆盖。

修复方向（建议，非指定实现）：给注册表加单调代次（每次 RegisterPageTarget/事件登记递增），getTargets 发起前记录代次，对齐时跳过代次更晚登记的 target；或把 getTargets+对齐与 tab_new 的 create+register 串行化在同一把锁内。

回归验收：barrier 测试：fake conn 的 PageTargets 在取得快照后阻塞，期间完成一次 tab_new，再放行；断言 tabs.Lookup(short) 成功、tabIdle.Snapshot() 含新 target、观测缓冲未被清；并覆盖 resolveTab 回退与 runTargetSync 重同步两条调用路径；-race 下 200 次并发 tab_new/tab_list 后所有 tab_new 返回的 tab 均受管。

证据：repro-go.log（TestAudit_StaleTargetListMustNotUnmanageNewTab FAIL）；repro/daemon_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

## factory · plan · 2026-11-22 04:13

### Behaviour
- 每次全量枚举在发起 CDP 请求前记录注册代次；应用旧快照时不移除请求发起后登记或重新确认的 target。
- tab_new 返回成功后的短 id、受管 idle 状态及已有观测数据不受旧枚举破坏；新鲜枚举仍移除真正消失的 target。
- 首次启动、空列表、枚举失败、resolveTab/close 回退及生命周期 dirty 重同步使用一致语义；并发快照不能通过 idle presence 的旧集合再次误删新 tab。
### Scope
- 仅注册表枚举一致性、daemon 全部枚举入口和永久回归；从 main 独立构建，不合并 FT1/FT7/FT10，不改 RPC、短 id 策略或部署。
### Decisions
- 优先单调注册代次/枚举令牌方案，避免把远程调用和 tab_new 用全局长锁串行化。显式登记已有 target 也须刷新受保护代次。
- 注册表与 idle 清理的边界必须一起检查，避免“注册保住但 SyncPresent 再删”的半修复；使用最小必要同步或删除驱动清理。由 coder 根据代码选择具体可证明的实现并记录。
### Checking
- 先增加正确行为永久回归并证明旧实现失败：barrier 阻塞旧 PageTargets 回包，期间完成 tab_new；断言 Lookup、受管跟踪和观测数据保留。
- 覆盖 tab_list、resolveTab 回退、runTargetSync dirty 重同步；-race 下 200 次并发创建/列表；新鲜空快照确实删除旧 target；枚举错误不改变状态。
- 完整执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，不依赖真实外网。
- 不运行 headless Chrome，不操作用户窗口；真实 Chrome、Docker、systemd 未验须明确记录。

## factory · 2026-11-22 04:13

基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a；独立构建，未合并其他 factory 分支。提交 c222ffb，已推送 factory/FT11-enumeration-preserves-new-targets。

实现：TabRegistry 增加单调登记代次及枚举令牌，所有 daemon 枚举入口在请求前取令牌。旧快照不移除请求期间显式登记的 target；idle 跟踪改为仅随真实注册表移除清理，不再用旧快照求交集。枚举结果仍只返回浏览器本次报告的 target，不改 RPC 或短 id 策略。真正移除仍清理 registry/idle/观测/keyed lock/Session。

回归：新增六种 RPC/回退入口 barrier、真实 browser.Session + 本地 CDP 协议桩的 dirty resync 与启动枚举、200 轮并发创建/枚举、失败枚举不改状态、新鲜快照确实移除目标，以及注册表代次语义测试。coder 在改生产代码前运行正确行为断言，旧实现丢失短 id、idle 受管状态和观测事件；dirty resync 同样复现，启动场景发生 connect 超时。旧实现的新鲜枚举及错误守护测试通过不作为修复证据。修复后全量通过，coder 报告定向 -race -count=30 通过。修复前日志保存在 build run evidence/FT11-c222ffb/prefix-fail.log。

验证：manager 独立执行配置命令 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过（多数包使用缓存），Go 1.27.4；git diff --check 通过，保护路径无变化，工作树干净。

安装：无新工具或依赖版本安装。测试直接使用已有 github.com/gobwas/ws v1.4.0，go.mod 从 indirect 改为直接依赖，go.sum 不变；复用 pin 的模块缓存且 GOPROXY=off。测试 HOME、缓存、临时文件隔离在本 run tmp。coder 报告一次 env.sh 在设置 TMPDIR 前误写系统临时目录，已移入 run tmp 并报告无残留；manager 未删除系统 /tmp 内容。

未检查：未运行真实非 headless Chrome，未操作用户窗口；真实事件时序、Docker、systemd 未验证。本地 CDP 桩只实现必要协议且 createTarget 不自动发 targetCreated。反向竞态（旧快照包含已由 destroyed 移除的 target 后再次注册）仍是既有未处理行为，交 review 注意。新鲜枚举与外部关闭的完整生命周期交错仍需独立 review。

状态：new/ → review/，第 1 轮。本次仅 build，未 review、未合并 main、未发布。

## factory · review · 2026-11-22 04:17
head: c222ffbecfe0fa54ca2a9b35fb8535e52bb0ae0f
verdict: pass
notes:
- [P2] internal/state/tab_registry.go:101: 快照登记按应用时刻增加代次，可能使更新的空快照无法纠正旧快照复活的已关闭 target。复现：tab_list 取得含 X 的快照 A → targetDestroyed(X) 完成 → tab_select(X) 取得空快照 B → 依次放行 A、B；A 赋予新代次，B 跳过删除，tab_select 对已关闭 tab 返回成功。基线同序列返回错误。建议区分快照登记与显式登记，采用枚举请求序号并添加双快照回归。旧快照复活本身属既有行为，但新鲜快照无法纠正是本次新增。
- [P3] internal/daemon/tab_idle.go:65: tab_new 登记 X 后，若 targetDestroyed 在 markTabManaged 前完成清理，随后写入的 idle 条目失去注册表记录；删除 SyncPresent 后，下一次空 tab_list 不再清除该孤儿。基线可清除，HEAD 可复现残留；idle timeout=0 时可保留到退出。建议 idle 标记与 target 移除互斥，并在同一临界区检查登记有效性。
- [P3] internal/daemon/target_enumeration_test.go:217: newEnumServer/newStubSessionServer 创建 Server 后未清理审计 writer；同进程运行三次 200 轮回归，runAuditWriter 协程由 0 增到 3 并持有内存记录。建议 t.Cleanup 调用 srv.closeAudit()。

Codex 只读审查 origin/main...HEAD，无 P0/P1；按工厂门槛 pass，review/ → ready/。以上低优先级问题供 owner 上线前判断，不代表它们已修复。RPC/启动枚举/dirty 重同步/正常空列表/枚举错误均已核查。Codex 对前两项执行基线与 HEAD 对照诊断，确认新增或恶化；诊断源码保存在 review run evidence/FT11-c222ffb/ft11_interleavings_test.go。

验证：Codex 和 manager 分别运行完整配置命令 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 均通过（manager 多数包使用缓存），git diff --check 通过，工作树干净。复用已有 Go 1.27.4 与锁定模块缓存，离线测试、隔离 HOME/TMPDIR，无新安装。manager 测试记录位于 review run evidence/FT11-c222ffb/manager-test.log。

未验：真实 Chrome 事件时序、Docker、systemd；未运行 headless Chrome，未操作用户窗口。无关联 GitHub issue/PR，无需评论。本次仅 review，不合并 main、不发布。


## factory · 2026-11-22 06:08
owner 授权：“先把 ready 的 ship 掉”。本次固定合并 FT8、FT9、FT11、FT12、FT14、FT15；review head 与所合并远端分支一致。

已合并并推送 main；本项 merge commit：f78a854，本批 main 最终提交：8836949。配置 ship=["merge", "push"]，未打 tag、未触发版本发布、未部署。无 linked issue/PR 需关闭，分支按配置保留。

逐项合并后执行完整 gofmt/vet/race 测试，最终累计 main 配置测试再次通过（部分缓存）；git diff --check 通过，保护路径未改，工作树干净。FT12 的 go.mod 冲突及 FT14 的 README/run_test.go 冲突由 CC 仅解决整合冲突，保留双方依赖、文档与全部回归；FT9 与 FT14 测试拆文件并共存通过。无新增未 pin 工具或依赖。

review 中已有 P2/P3 未在本次 ship 修复，继续保留原记录。真实 Chrome/CDP、Docker、systemd 运行时未验证。ready/ → done/。
