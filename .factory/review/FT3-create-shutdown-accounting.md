---
branch: factory/FT3-create-shutdown-accounting
round: 2
---
# 创建在途关停收尾与唯一释放记账

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] internal/daemon/v1.go:492：强制关停创建已执行但回包在途时，留下未管理页面且审计ok=false；提供有界收尾，按可证明完整身份记账。
[P3] internal/daemon/v1.go:560：显式关闭先于创建回包时可能写tab_close,tab_new,tab_close；协调迟到创建记账，避免重复成功释放。

来源：FT1 第3～4轮review。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
按此次 owner 授权记录顺序与收窄范围，本项仍留在 backlog/。须在前置 FT7（ready 后 ship 并成功打 tag）、FT4 与 FT2 均已交付之后，才移入 new/，不启动本次 build。

收窄后范围：以强制关停时创建已执行、回包仍在途的有界收尾为主（避免留下未管理页面且审计 ok=false）。唯一释放记账（显式关闭早于创建回包时的 tab_close,tab_new,tab_close）与已交付 FT28 的在途创建/迟到 tab_new 工作有重叠，先核验再决定是否实现，不以标题宣称已修好。

依赖顺序：FT7 ready → ship 并 tag → FT4 → FT2 → 本项 → FT6。

## factory · 2026-11-25 11:01
前置 FT2 已合入远端 main（merge 3287688b7ed2bf6c4730de6962630e12f22a1a9d，已审 head 429acb3766d665bc094ebaa0fb5c4955e8bcb13b）。按既有顺序将本项从 backlog/ 移入 new/，等待外部工厂 build。本 run 不启动 FT3 的开发、build 或 review。

保留此前已确认的收窄范围：以强制关停时创建已执行、回包仍在途的有界收尾为主（避免留下未管理页面且审计 ok=false）。唯一释放记账（显式关闭早于创建回包时的 tab_close,tab_new,tab_close）与已交付 FT28 的在途创建/迟到 tab_new 工作有重叠，须由外部工厂先核验已有回归再决定是否实现，不以标题宣称已修好。不移动 FT6，FT5 保持 stop。

## factory · plan · 2026-11-25 11:06
Behaviour：创建前开启身份窗口；关停排空时保持 CDP/生命周期/审计可用。强制关停取消 HTTP 后，对已经发出的创建请求做有界收尾；拿到完整 target id 才能精确提交或回滚、记账，失败/超时/空 id 不猜测身份。不误关其它页面、不虚报成功、无无限等待。迟到创建若已被明确关闭，不恢复受管状态或焦点。

Scope：核验当前 FT28、FT2 的实现及永久回归，重点修复强制关停时创建已执行但回包在途的缺口；增加回归及必要文档。不恢复 FT21，不引入短 ID、身份猜测、所有权协议、跨进程机制或泛化任务框架。不在本 run 交付、打 tag 或发布。

Decisions：优先以小范围、明确所有权的有界创建收尾实现；单纯枚举不能证明页面属于本请求，因此不能靠 URL/列表差集认领。未知身份/连接中断/收尾超时如实报告保证边界。显式关闭早于创建回包的唯一释放问题先用现有回归核验；只有确认仍有缺口才改代码，不重复 FT28。

Checking：新增可控 CDP 调度回归，覆盖创建已执行但回复暂扣、强制 HTTP 取消后回复在收尾界内到达、永不回复/连接断开、无有效 id、已有关闭证据及其它 target 不受影响；核验审计完整身份与成功/失败含义，managed open/retire 和重放不复活，所有窗口/后台工作有界释放。执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；测试离线，throwaway HOME 与隔离 XDG/cache。若无真实非 headless Chrome 验证，明确未检查。

## factory · 2026-11-25 11:23
构建第 1 轮完成，new/ → review/。branch: factory/FT3-create-shutdown-accounting；head: a34fc3b。基于 main 3287688，3 个提交，未合入 main、未打 tag、未发布。

实现：创建请求不再随 HTTP 请求取消立即丢弃回复；请求先结束时至多再等 5s，收到完整 target id 后按该 id 有界回滚（5s），不受管、不选中，审计 ok=false 且携带完整 target。无回复、空 id、错误或断线时不猜身份、不认领/关闭其它页面，明确告警未知身份可能留下未受管页面。关停先等待创建收尾，再停生命周期同步、会话和日志，等待有界（12s，随后终止等回复并再留 7s）。请求提交后的取消仍保留受管状态。

核验 FT28：显式关闭早于创建回包已被现有生产代码处理；新增永久审计回归确认恰好一次成功 tab_close、一次失败 tab_new，无 managed open/retire；没有重复实现旧修复，没有触碰 FT21。另将既有等待 handler 关停测试改为单请求单连接，消除疑似空闲 keep-alive 等待；原不稳定原因尚未证实，不宣称已确认属于 base。

检查：coder 新增 11 个场景（含子测试）通过 -race -count=10，完整配置命令最终连续 5 次通过；manager 在隔离 HOME/XDG、本地模块副本和离线环境独立执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过。提交范围、never 路径及 diff --check 均核验通过。新增覆盖真实 Session + CDP stub 的强制关停迟到回复、永不回复、断线、优雅关停，以及空 id/回滚失败/已关闭/入口拒绝等路径。工具链为既有 Go 1.27.4；无安装。

未检查：未运行真实非 headless Chrome、realchrome 套件和 Linux；强制关停用缩短预算而非生产 15s+60s；未固定 5s 收尾之后才回包、进程中途崩溃、提交与应答之间取消的调度。未改变非 tab_new handler 强制关停行为。UI 未改动。

临时文件偏差：coder 报告 macOS mktemp 曾写入用户系统临时目录并自行移除，未提供精确路径；harness 另留临时输出，具体文件未确认。/tmp 前后扫描没有新增顶层条目，不能据此断言已有目录内无新增。审查请重点确认取消/回包/提交边界及收尾时限，不将未知身份情形宣称为保证清理成功。

## factory · review · 2026-11-25 11:33
head: a34fc3b4fdbbb5fcac9a255af3a00dcf7713f379
verdict: fix
fix:
- [P1] internal/daemon/tab_create.go:177: 创建往返虽脱离请求取消，但生产 Session.CreatePageTargetContext 仍以 timeout.Operation（60s）截断它，因此没有承诺的 RPC 超时后额外 5s 收尾。真实 Session + 本地 CDP stub 在第 61s 释放完整 target id，页面仍打开、没有 close 命令，失败审计 target 为空。让创建往返存活到独立的有界收尾期限，同时保持 RPC 的 60s 提交上限；加入使用生产 Session 的永久回归。
notes:
- [P2] internal/daemon/tab_create.go:180: HTTP/1.1 同连接已有下一请求时，强停连接未必及时取消当前 handler ctx。生产 15s+60s 时限诊断在 75.004s 观察到强停后释放创建回复，仍成功提交、audit ok=true、重放受管。强停须显式通知在途创建进入收尾，提交前检查该信号；缩短预算也连续复现 3 次。
- [P2] internal/daemon/v1.go:514: ctx.Err 检查发生在 commitCreatedTab 获取 lifeMu 之前。固定回复到达后等待 lifeMu、此时取消再放锁，仍产生 managed_tab_open 与成功 tab_new。取消判定需与提交边界串行化，在持锁修改 registry 前检查；已提交后取消则保留受管状态。
- [P2] internal/daemon/tab_create.go:173: 入口已取消仍发送创建命令，扣留回复超过 5s 会产生原本不应创建的未受管页；派发前检查取消，只有已发出的请求才进入收尾。
- [P2] internal/daemon/tab_create.go:194: 回复已缓冲且 aborted 就绪时最后 select 随机丢弃已知完整 ID；固定调度 32 次中 15 次误报无回复。放弃等待前优先消费已缓冲回复，保证精确回滚与审计。
- [P3] internal/daemon/v1.go:504: 创建已执行但回复前断线只提示 Target.createTarget: context canceled，未提示结果未知及页面可能未受管；区分明确拒绝与连接丢失，后者说明保证边界。
- [P3] internal/daemon/v1.go:521: 回滚失败 browser busy 时告警正文仍宣称 was rolled back，与 cleanup=not closed 矛盾；正文应反映实际结果。

Codex 只读审查；工作树干净，无提交、无安装。配置检查 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 退出 0（11 个包通过、1 个无测试）。上述失败是额外诊断发现的必需行为验收缺口，不是配置检查失败；P1 来自生产 60s 超时截断收尾，不因诊断测试失败自动提升等级。诊断在隔离副本中执行，使用 throwaway HOME/XDG/cache 和本地锁定依赖，无外网测试。FT28 唯一成功关闭/失败创建审计回归通过。补验提交后响应前取消会保留受管状态；取消后超过 5s 的回复不猜测关闭且窗口释放。

未检查真实非 headless Chrome、Linux、进程中途崩溃。UI 未改动。临时文件均在本 run TMPDIR 内（另触碰本 run 锁和 git 索引），/tmp 顶层前后无新增；不据此保证既有目录无新增。第 1/3 轮，review/ → new/，下一轮修复 fix 列表并保留 notes 供后续检查；未合并 main、未打 tag、未发布。


## factory · plan · 2026-11-25 13:53
第 2 轮沿用现有计划，修复最新 review 的 P1 fix，并补齐同节相关 P2/P3 notes。Behaviour：RPC 60s 仍为提交上限，生产 Session 的创建往返独立保留至 RPC 期限后 5s 收尾加 1s 余量；迟到完整 id 只供精确回滚及失败审计。HTTP 强停显式通知在途创建；lifeMu 内、registry 修改前检查取消；入口已取消不派发，放弃等待前优先读取已缓冲回复。错误明确区分拒绝、身份未知与回滚失败。
Scope/Decisions：只改创建收尾、相关 Session 期限和回归文档；不恢复 FT21/短 ID，不发布。强停通知点为 shutdownHTTP 返回后进入 settleCreations；srv.Close 与通知之间已完成的提交视为已提交。Checking：真实生产 60s Session 回归及固定调度取消/缓冲回归，完整配置检查，离线隔离 HOME/XDG/cache。

## factory · 2026-11-25 13:53
第 2 轮修复完成，new/ → review/。branch: factory/FT3-create-shutdown-accounting；head: 74345e9（代码 f99afa5，文档 74345e9）。未合入 main、未打 tag、未发布；FT28 已 done，FT21 保持 stop。

实现：生产 Session 遵从创建调用方独立期限，不再以 Operation 60s 截断额外收尾；RPC 提交仍不超过 60s。增加强停 gate 通知与提交检查，取消检查移入 lifeMu 临界区，已提交后取消保留受管；已取消入口不派发，abort/timer 时优先消费已缓冲 id，取消往返后最多再等 250ms 获取已知 id；断线未知身份和回滚失败告警如实描述，不猜测认领或误关其它页面。

检查：Claude Code 报告完整配置检查两次通过，创建相关测试 -race -short -count=30 共 1380 PASS；生产 Session 回归真实等待 60s 后再 1s 释放回复，非缩短 Operation，覆盖完整 id 精确关闭、失败审计及无受管。coder 在 TMPDIR 副本还原相关修复验证测试会失败。manager 独立执行完整配置命令通过（部分 Go 缓存命中），并以 -race -count=1 非缓存执行生产60s、pipeline、lifeMu、缓冲回复四项回归通过（62.756s）。测试离线，HOME/XDG/Go cache 在 run TMPDIR 内。提交范围、never 路径、工作树干净及 diff --check 通过。无安装，使用既有 Go 1.27.4。

未检查：真实非 headless Chrome、realchrome、Linux、进程崩溃；pipeline 强停使用缩短预算而非生产15s+60s，其 ctx 未取消仅日志非硬断言；srv.Close 与通知之间提交窗口、入口检查与派发之间取消未固定调度。创建往返自身66s期限的 hint 仍可能含 one RPC is limited to 60s（附未知结果说明）。UI 未改动。请 review 特别核验强停通知/提交边界及期限语义，不将未知身份宣称为保证清理。

范围偏差：coder 写入工作树/TMPDIR 以外的自身 memory 工具链笔记和索引（非项目代码）；定位工具链运行 mise ls/which 是否更新其缓存/状态未确认。manager 未授权该笔记写入且未擅自删除外部文件。/tmp 顶层扫描无新增，不能据此保证已有目录内无新增。
