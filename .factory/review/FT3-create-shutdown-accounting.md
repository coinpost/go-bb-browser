---
branch: factory/FT3-create-shutdown-accounting
round: 1
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
