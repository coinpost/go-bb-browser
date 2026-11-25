---
branch: factory/FT4-store-recovery-durability
round: 5
---
# 日志尾行、轮转恢复与安全可写探测

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] internal/store/store.go:218：完整JSON尾行缺换行时，后续追加与尾行拼接，下次启动截断丢失归属；补分隔并检查错误。
[P2] internal/store/store.go:171：启动覆盖并删除既有.write-test普通文件；使用独占临时文件或实际打开日志检测。
[P2] internal/store/store.go:331：轮转改名后快照发布前中断，恢复集合丢失；完整写临时快照、原子发布并保留可恢复上一代。
[P2] internal/store/store.go:322：轮转重开失败后句柄永久nil，触发追加仍成功；保留旧可写句柄至切换成功，传播错误并可重试。

来源：FT1 第3～5轮review；尾行和.write-test在HEAD与基线均10/10复现。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## factory · 2026-11-22 01:11
本次 main review B11 的合法尾行缺换行缺陷已单独建立 new/FT18-jsonl-tail-append-recovery.md，FT4 后续只处理其余独立轮转/探测问题，不重复修复该尾行问题。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
按此次 owner 授权记录顺序与收窄范围，本项仍留在 backlog/，在 FT7 已 ready、完成 ship、且版本 tag 成功之前不移入 new/，不启动 build。全局 factory.toml 的 ship 与排序配置不改；FT7 成功交付后的那一次注释版本 tag 是此次 owner 单独授权，不是修改配置。

收窄后范围：去掉已被 FT18 覆盖的「完整 JSON 尾行缺换行」（FT18 审查 2026-11-22 06:30 head a311559db7af14ecca3bb952207b29c6a9c6cd7e，merge 42d72b1ab00cabaf49854d005f1ff58273e72fe1；FT18 自身写明其余轮转/探测仍属 FT4）。保留：（1）轮转原子性：完整写临时快照、原子发布并保留可恢复上一代；（2）轮转失败恢复/吞错：重开失败时保留旧可写句柄至切换成功，传播错误并可重试；（3）可写探测误删：启动不得覆盖并删除已有 .write-test 普通文件。

依赖顺序：FT7 ready → ship FT7 → 打一个注释版本 tag → 本项 backlog → new → 其后再 FT2 → FT3 → FT6。不得提前移动下一项。

## Guoyin · 2026-11-24 23:36
FT7 的 Codex 复审已通过，审查提交 9eeb0d1，已从 review → ready 并推送看板。

先 ship FT7，然后发新版本。

最后按建议去处理 backlog 里面的 5 项任务

## factory · 2026-11-24 23:36
前置 FT7 已交付（merge e8ec86aaa0c6333c8cac35c715ed48ed431a0879，审查 head 9eeb0d1123b34c04663252013bbe7642ceb0b039），且新版本 v0.1.1 发布成功（tag 指向 origin/main@fb2c535b1273627cb7082bc59a9bb24c9e04b8ff，Release https://github.com/coinpost/go-bb-browser/releases/tag/v0.1.1 ，Actions https://github.com/coinpost/go-bb-browser/actions/runs/45426997200 conclusion=success）。按 owner 顺序将本项 backlog → new，本 run 不启动 build。

收窄范围保持不变：去掉已被 FT18 覆盖的「完整 JSON 尾行缺换行」；保留轮转原子性、轮转失败恢复/吞错、可写探测误删。其后顺序仍为本项交付后才将 FT2 → new，再 FT3，再 FT6。t25u 要求全部四项都 ship 后再发一次补丁，不在本次发布。

## factory · plan · 2026-11-25 00:00
### Behaviour
启动探测只操作独占创建的临时文件；轮转先完成私有快照，再原子发布并保留上一代。准备/写入/发布失败保留旧日志与写句柄，向追加方传错且可重试；重启恢复受管集合与活动时间。空集合和首次启动保持明确定义。
### Scope
只处理轮转原子性、失败恢复/吞错及探测误删，增加永久回归和必要文档；FT18 尾行修复不重复，FT21 不恢复，完整 target ID 不变。
### Decisions
采用同目录快照和硬链接上一代、rename 发布，临时句柄直接接管，消除发布后重开失败窗口。支持硬链接为必要条件；不支持则轮转报错、旧日志继续可写。普通追加不新增 fsync 承诺。
### Checking
覆盖快照创建/写入/短写/fsync、备份顺移/链接、发布失败、构造中断状态重启、空集合、retired/activity、多目标、探测文件保留及重试；执行配置完整测试，保留 FT18/权限回归。

## factory · 2026-11-25 00:00
构建完成，new/ → review/。branch: factory/FT4-store-recovery-durability；head: 7f7cbf5。

实现独占临时可写探测；完整快照写入并 fsync 后硬链接上一代、原子 rename 发布，原句柄保留到发布成功。轮转失败以 ErrRotation 返回（触发记录已写入），后续追加重试；启动在活动日志缺失而 .1 存在时链接恢复，忽略未发布临时快照。README/AGENTS/systemd 文档同步。

测试：coder 完整配置命令通过；经理独立复跑 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过。另 store race count=3 和 realchrome-tag vet 通过。无新增安装。起点 origin/main@fb2c535，仅一个本项提交，无 never 路径变更；未合并 main、未发 tag、未发布、未启用任何任务。

限制/审查重点：硬链接文件系统兼容性；目录 fsync 失败当前仅 warn（发布已发生），普通追加不 fsync；旧实现已留下空/半截 active 加完整 .1 的歧义状态未自动恢复。未做实际断电/kill -9、Linux/Windows/无硬链接文件系统及真实 Chrome 验收。ACL 导致不可写目录测试 skip；既有 daemon 测试新增实际创建探测后的 ACL skip，需审查该测试调整。

执行偏差：coder 曾在系统临时目录生成两个 HOME 与编辑脚本，并误 source 外部遗留 env.sh，导致一次测试写到旧 FT28 临时目录；coder 报告新建内容均已清理（经理未额外删除外部路径），旧临时目录父目录 mtime 改变。后续验证未 source 该文件；经理独立设置隔离 HOME 重跑全套通过。不得在后续运行复用此类外部环境脚本。

通知失败：按本次 cron 元数据指定 contact_id 与 chat_id 发送 Build started，渠道拒绝（chat_id 与群目标不匹配）；未擅自改路由，Built/Summary 留在运行输出。

## factory · review · 2026-11-25 00:07
head: 7f7cbf5245ea98c6d68b724a64ab311d2874bce2
verdict: fix
fix:
- [P1] internal/store/store.go:546：两个 Store/daemon 共用状态目录时，固定 rpc.jsonl.tmp 可被另一个实例删除并重建，先完成者随后会发布对方尚未写入的快照。受控交错复现：A 追加返回 nil，活动日志 0 字节，上一代 373 字节，重启丢失受管集合。增加跨进程单写者保护，在启动清理临时文件之前取得并持有至 Close，或提供等价的明确拒绝机制；增加双实例永久回归，不能只依赖各 Store 的 mutex。
- [P1] internal/store/store.go:619：活动日志被删除后再次轮转，先将可恢复 .1 顺移到 .2，而 link 活动路径缺失被当作可继续；若随后发布 rename 失败，启动只查 .1，便成功新建空日志。复现：先成功轮转，删除活动日志，再对发布注入 EIO，重启前仅剩含 persisted 的 rpc.jsonl.2，恢复集合为 []。备份顺移前处理活动路径缺失，确保新快照发布成功前仍有启动能恢复的上一代，并增加组合失败回归。
notes:
- [P2] internal/store/store.go:583：轮转发布后的目录 fsync 失败仅 warn，触发追加返回 nil，随后低于阈值的追加也不重试同步。注入 EIO 复现目录同步仅调用一次。建议保留已发布句柄，明确传播“记录已写、发布成功但目录同步失败”，并单独重试目录同步；不扩大普通追加 fsync 承诺。
- [P2] internal/store/store.go:296：旧版将符号链接活动日志移至 .1 后中断，.1 仍可读出受管记录，restorePreviousLog 却将非普通文件当作不存在，成功创建空日志。至少对已存在但不支持的备份类型明确报错。
- [P2] internal/store/store.go:576：Windows 的 os.OpenFile 共享标志不含 FILE_SHARE_DELETE，发布时新旧句柄仍打开，rename 替换预计会持续共享冲突。仅静态核对 Go Windows 实现，未做原生实测；需原生平台验证并采用兼容发布策略。
- [P3] internal/daemon/tab_idle.go:70,169：ErrRotation 表示触发记录已写入，但生命周期告警仍声称注册不会恢复或退役未持久化；实测重新打开的受管数分别为 1、0。建议 errors.Is(err, store.ErrRotation) 区分轮转与追加失败。

Codex 只读审查 origin/main...HEAD（base fb2c535），无代码提交，工作树干净。配置测试 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全部通过；上述两条 P1 由临时测试覆盖层确定性交错/故障注入复现，经理核对复现输出与对应控制流。三代备份已满时连续三次失败后重试保留原 .1/.2；既有 .write-test 保护通过。继承 ACL 允许创建时，两条不可写目录测试的 skip 合理；移除隔离测试目录的继承 ACL 后原测试均执行通过。

第 1/3 轮审查发现 P1，review/ → new/，下一次 build 在同一分支修复 fix 列表。无新增安装；无新增可见 /tmp 路径，TMPDIR 外仅按要求刷新本 run 锁。未做真实断电、kill -9、Linux、Windows、无硬链接文件系统或真实 Chrome 验收。未合并、未发 tag、未发布，未移动后续 backlog 项。无 source 或 owner Closes/Fixes/Resolves 链接，不发表评论。

通知失败：Review started 按 cron 指定 contact_id 与 chat_id 发送，被渠道拒绝（chat_id 与群目标不匹配）；未擅自替换路由，后续结果与 Summary 留在运行输出。

## Guoyin · 2026-11-25 00:17
不会有多进程共用状态目录的问题

## factory · 2026-11-25 00:17
按 owner 此次说明，本项以单进程独占该状态目录为使用前提。factory · review · 2026-11-25 00:07 的第一条 P1（internal/store/store.go:546：两个 Store/daemon 共用状态目录时，固定 rpc.jsonl.tmp 可被另一实例删除并重建，先完成者随后发布对方尚未写入的快照）属于跨进程共享场景，超出当前验收范围，不再作为 FT4 交付阻塞，不要求为该场景新增跨进程锁。此边界不得泛化豁免单进程故障、线程竞争或日志恢复问题。

同一审查的第二条 P1（internal/store/store.go:619：活动日志被删除后再次轮转，先将可恢复 .1 顺移到 .2，活动路径缺失被当作可继续；若随后发布 rename 失败，启动只查 .1 并成功新建空日志，恢复集合丢失）仍为交付阻塞，须继续解决。其他已有问题（该审查 notes 中的 P2/P3，以及此前收窄范围内的轮转原子性、失败恢复/吞错、可写探测误删）仍按原记录处理。

不改先前 review 正文与 verdict: fix；本项仍留在 new/，本 run 不启动 build/review/ship，不改代码。


## factory · plan · 2026-11-25 00:28
第 2 轮补充（保留原计划）：
### Behaviour
按 owner 单进程独占状态目录的前提处理，跨进程 P1 不增锁。活动路径缺失或不再指向打开的日志时，在写快照和备份顺移前拒绝轮转，保留 .1 和旧句柄，返回 ErrRotation；恢复同一活动文件后允许重试。发布后的目录同步失败独立返回 ErrDirSync，接管已发布句柄，后续追加不论大小以及 Close 重试目录同步。启动拒绝不可用的已有 .1，不默默新建空日志。告警区分记录已写和追加失败。
### Scope
修复仍有效的 P1 #2，处理目录 fsync、不可用 .1 和生命周期告警 notes；不恢复 FT21，不改完整 target ID，不处理多进程争用，不为 Windows 引入推测性兼容层。
### Decisions
缺失活动路径选择明确拒绝，而非猜测 .2/.3 或自动修复；普通追加不增加 fsync。Windows 轮转兼容问题以明确不支持和未原生验证记录，供 review 判断，不宣称修复。
### Checking
增加轮转后删除 active、注入发布 EIO 时备份不动且重启恢复、路径恢复后重试、路径替换、发布后目录同步失败与低阈值重试、不可用 .1 和告警永久回归；运行完整配置测试与重复 race 测试。

## factory · 2026-11-25 00:28
第 2 轮构建完成，new/ → review/。branch: factory/FT4-store-recovery-durability；head: 312af9c46d2d9a78a461373d224b2db6b658d9f8。

活动路径预检查在任何快照/备份操作前拒绝缺失或替换；不再把缺失活动路径的 link 失败视作成功。发布后的目录同步失败采用 ErrDirSync 与待同步标记，后续追加及 Close 重试；已有但不可用 .1 明确拒绝；RecordWritten 帮助生命周期/audit 告警区分写入和维护失败。README、AGENTS 与 systemd 文档同步单进程边界、缺失路径处理与 Windows 限制。按 owner 不加跨进程锁。

测试：Claude Code 完整配置命令通过，store race count=10、生命周期告警 race count=5、realchrome-tag vet 通过；经理独立复跑 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全部通过（隔离 HOME/XDG/GOCACHE，离线使用既有只读模块下载缓存）。无新增安装，使用已有 Go 1.27.4。新增回归未在旧提交上跑 red。起点 fb2c535；经理检查仅有本项两次提交，never 路径未变，main 已包含，无需合并。

限制/审查重点：路径缺失期间追加仍进旧的无名句柄，这些新增记录可能随关闭丢失，重启只恢复 .1；预检查之后、link 之前的外部删除仍存在窗口，可能已经顺移备份。发布后 fsync 错误、再追加、再轮转组合与 RecordWritten 的准确语义请复查。Windows 轮转未修复，文档改为不支持，未原生验证。未做实际断电/kill -9、Linux、无硬链接文件系统、真实 Chrome 验收；无 UI 改动。未 ship、未发 tag、未发布、未启用任务、未移动后续 backlog。

执行记录：coder 初期未隔离的 build/test 使用默认共享 Go build cache 与 Go telemetry，后续报告测试使用隔离 HOME；模块缓存使用既有内容。除规定刷新 build 锁外无新增可见 /tmp 路径、无其他报告的外部临时文件。本轮通知发送成功。

## factory · review · 2026-11-25 00:36
head: 312af9c46d2d9a78a461373d224b2db6b658d9f8
verdict: fix
fix:
- [P1] internal/store/store.go:714：先把 .1 顺移到 .2、再 link 新 .1，link 返回 ENOSPC 时会失去启动能发现的上一代。单进程完全串行复现 3/3：首次轮转成功 → 第二次轮转 link 注入 ENOSPC，追加返回 ErrRotation → 再删除 active → 再次追加被预检查拒绝 → Close/Open 成功却恢复 []，原 135 字节受管记录仍在 .2。先将 active 链接到私有暂存名并核验 inode；在能原子替换为新备份之前保留现有 .1 的可恢复名字，再准备更旧代并原子发布 .1，发布 active 前重验路径。增加硬链接失败后删除 active、快照 fsync 后删除/替换 active 的永久回归；仅增加一次 stat 不足以覆盖串行 ENOSPC 场景。
notes:
- [P2] internal/store/store.go:562：待处理目录 fsync 重试 EIO 后，若新快照创建又 ENOSPC，返回值只含 ErrRotation/ENOSPC，ErrDirSync 与 EIO 被掩盖；本次记录写失败又会完全跳过欠下同步。独立重试并保留仍未解决的错误，按本次记录写结果计算 RecordWritten，不能简单合并导致未写记录被误判已写。
- [P2] internal/daemon/tab_idle.go:190：active 删除后写入无名句柄，RecordWritten=true，告警却称 managed tab persisted；实测关闭重启新 target 不恢复。区分写入句柄与重启可恢复，路径异常或目录同步未确认时不要承诺已持久化。
- [P2] internal/daemon/server.go:218：生产入口丢弃 closeAudit 返回值。目录 fsync 持续 EIO，Close 已重试，服务正常取消仍返回 nil 且无关闭告警。保留原服务错误并合并最终关闭错误，或明确记录持久化未确认。
- [P2] internal/store/store.go:317：悬空 active 符号链接加完整 .1 时，Open 创建空链接目标且成功恢复 []，3/3 复现；指向普通文件的 active 链接又会使每次轮转预检查失败。启动与轮转要求统一，明确拒绝非普通入口并保留目标和 .1。

Codex 只读审查 origin/main...HEAD（base fb2c535），无提交且工作树干净。按 owner 单进程独占目录前提，多进程问题不作阻塞；原 P1「进入轮转前 active 已删除」的精确时序通过。新增 P1 不依赖多个 Store、并发文件操作或 Windows；经理核对临时复现源码、3 次输出及备份顺移控制流。快照 fsync 后删除/替换 active 的额外注入也各 3/3 暴露空恢复/错误接管外来文件。

配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 隔离 HOME 全部通过；核心回归 race count=3 通过。诊断 overlay 的预期失败断言确认上述缺陷（非仓库原测试失败）。两条权限测试因继承 ACL skip，临时 overlay 仅清除夹具 ACL 后原断言执行通过；子进程 helper 按设计跳过。无新增安装，使用已有 Go 1.27.4。未做真实断电、kill -9、Linux、无硬链接文件系统或真实 Chrome 验收；Windows 按已声明不支持记录，不计阻塞。

第 2/3 轮审查发现 P1，review/ → new/，下一轮在同分支修复 fix 列表。无 source 或 owner Closes/Fixes/Resolves 链接，不发表评论。未合并、未打 tag、未发布、未启用任务、未移动后续 backlog。

执行偏差：coder 首轮系统 mktemp 将隔离 HOME 建到系统临时目录，报告已将整树移回本 run TMPDIR，核验原路径消失，随后使用新的隔离构建缓存重跑全套通过；未新增可见 /tmp 路径，除此仅刷新授权 review 锁。未复用外部环境脚本。临时诊断及报告按流程在本轮结束清理。


## factory · plan · 2026-11-25 00:53
第 3 轮补充计划：
Behaviour: 在任何备份顺移前，将 active 硬链接到私有暂存名并核对 inode，保证 ENOSPC/link 失败保留旧 .1；以原子替换发布新 .1，发布 active 前再次确认仍是打开的日志。启动明确拒绝非普通 active（含悬空符号链接），不得创建链接目标或丢失 .1。欠下的目录同步独立重试，不因追加/轮转失败掩盖错误；RecordWritten 只反映本次记录写入结果。生命周期告警不承诺可恢复/持久化；生产退出报告最终 closeAudit 错误且保留原服务错误。
Scope: 修复最新 review 的 P1 和四条 P2，保留单进程独占前提，不加跨进程锁。FT18、FT21、完整 target ID、Windows 不支持边界不变。
Decisions: 使用同目录暂存硬链接与 rename 的简单事务，不引入恢复猜测。较旧代备份允许按既有保留策略处理，但当前 .1 可恢复名字在新 .1 可发布前不可失去；重试不得持续挤掉历史。未确认目录同步或路径异常一律保守告警；普通追加仍不承诺 fsync。
Checking: 永久回归 link ENOSPC 后删除 active 并重启；快照 fsync 后删除/替换 active；sync EIO 与追加失败/轮转 ENOSPC 组合、RecordWritten 正反例；生产退出 closeAudit 错误；active 普通/悬空符号链接不触碰目标/.1。完整配置测试，store 重复 race；无实际断电或原生 Linux/Windows 验证，不宣称做过。

## factory · 2026-11-25 00:53
第 3 轮构建完成，new/ → review/。branch: factory/FT4-store-recovery-durability；head: b9f94f6a33b7b5814345e115e57cfe4518ae153b。

Claude Code 修复最新 review 的 P1 和四条 P2：备份顺移前先暂存 active 硬链接并校验 inode，保留旧 .1 至原子替换；发布 active 前复验路径，连续失败重试保留备份。欠下目录同步在写失败时也重试，并保留与写入/轮转错误的组合；RecordWritten 使用本次完整写入的类型标记，不由哨兵推断。启动拒绝非普通 active（含悬空链接）；生命周期/audit 告警不再把写入句柄称为持久化；服务退出合并 closeAudit 错误并保留原错误。README/AGENTS/systemd 文档同步。

提交：d362345（store/文档）、b9f94f6（daemon/测试）；前一提交的旧 daemon 断言在后一提交修正，按 head 一起审查。新增事务/组合故障/符号链接/关闭错误永久回归。配置测试 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` coder 通过、经理独立隔离 HOME/XDG/GOCACHE 离线复跑通过；coder store race count=10、daemon 相关 count=5、realchrome-tag vet、Linux 静态 vet、Windows 交叉编译通过。coder 报告五处修复变异检查均使对应回归失败；未在旧提交直接跑 red（新符号无法编译）。无安装，使用既有 Go 1.27.4/只读模块缓存。经理检查起点 main@fb2c535、仅本项四次提交、never 路径未变、工作树干净，origin/main 已为祖先。

审查重点/限制：RecordWritten 裸哨兵现在返回 false，语义收紧；关闭失败使正常停止返回非零，systemd 行为未实测。发布前最终复验与 rename 间仍有外部改名窗口；active 名字丢失时的追加不保证重启恢复。daemon 关闭错误经测试 hook 注入，真实目录同步失败只在 store 层覆盖。权限测试 ACL skip 本轮未逐一核对。未做实际断电、kill -9、原生 Linux/Windows、无硬链接文件系统、真实 Chrome；无 UI 改动。按 owner 不加跨进程锁，不恢复 FT21，不改完整 target ID。FT28 已在 done/，因此按正常排序选择 FT4。本轮未 ship、未打 tag、未发布、未启用任务、未移动后续 backlog。

执行偏差：coder 首次 mktemp 将隔离 HOME 放到系统临时目录并跑过一次 go build，报告已自行删除；随后显式 mktemp -p TMPDIR，全部测试使用本 run 隔离目录。后台测试由执行环境产生一份外部临时输出，已登记 outside；经理不删除 /tmp 内容。未复用外部环境脚本。通知发送成功。

## factory · review · 2026-11-25 01:00
head: b9f94f6a33b7b5814345e115e57cfe4518ae153b
verdict: fix
fix:
- [P1] internal/store/store.go:235：启动在拒绝非普通 active 之前先删除 .tmp/.keep。旧布局 rpc.jsonl 符号链接指向含受管记录的 rpc.jsonl.tmp 或 rpc.jsonl.keep 时，一次 Open 就删除唯一日志，再返回非普通文件错误；真实文件系统无并发复现各 3/3。先验证 active（以及需要恢复时的 .1），再进行暂存清理或备份权限修改；拒绝时保留链接目标，增加重叠文件名永久回归。
notes:
- [P2] internal/store/store.go:820：已有可读符号链接 .1 时，轮转先将其移至 .2，再发布新 .1；对后者注入 EIO，关闭并删除 active 后，启动成功创建 0 字节日志且恢复为空，完整上一代仍可经 .2 读取，3/3 复现。发布成功前保留 .1，或在备份顺移前明确拒绝非普通 .1；增加失败发布→移除 active→重启回归。
- [P3] README.md:139：active 名字丢失后追加是否随关闭丢失取决于 .1 是否已指向打开的日志；发布 .1 后的追加可由它恢复。区分两个事务时点，并修正 store.go:642 最终复验仍称 backups left untouched 的误导错误文本；外来路径保护也须注明最终复验与 rename 并非原子操作。

Codex 只读审查 origin/main...HEAD，base/merge-base fb2c535，无代码改动、提交或安装，工作树和索引干净。经理核对诊断源码、3 次复现输出及对应清理/备份控制流。P1 为单进程、固定目录布局、一次启动即删除唯一日志，不依赖并发、多进程或 Windows；按 owner 不要求跨进程锁，Windows 轮转仍按明确不支持边界处理。

配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 在隔离 HOME/XDG/工具缓存、Go 1.27.4 下完整通过；修正首轮 PATH 遗漏 Node 后清空私有测试结果缓存重跑，adapter 测试启用，11 个有测试包全部通过。store/daemon 核心回归 race count=3 通过；两条因继承 ACL skip 的不可写测试在去除夹具 ACL 的隔离目录中原断言执行通过。临时缺陷诊断的预期正确性断言失败不等同仓库测试失败。真实 store 目录同步 EIO 经生产关闭链路传播的独立集成诊断通过（正常取消报告关闭错误；监听失败保留 EADDRINUSE 与关闭错误）。上一轮常规 .1 的 ENOSPC P1、目录同步组合错误、RecordWritten 正反例、告警及退出错误修复均通过。

第 3/3 轮仍有 P1，达到 max_rounds，本项留在 review/ 等 owner 决定，不自动移回 new/。若继续修复，请 owner 添加说明并移至 new/。无 source 或 owner Closes/Fixes/Resolves 链接，不发表评论。无新增可见 /tmp 路径；coder 报告 TMPDIR 外仅授权锁心跳。未做真实断电、kill -9、原生 Linux/Windows、无硬链接文件系统、真实 Chrome 或 systemd 部署验收；无 UI 改动。未合并、未打 tag、未发布、未启用任务、未移动后续 backlog 项。

## Guoyin · 2026-11-25 08:23
FT4 再排一轮，只移回 new，不启动开发或审查。

## factory · 2026-11-25 08:23
按 owner 此次说明，将本项从 review/ 移回 new/，保留 branch 与 round: 3，不重置轮次。本 run 只做卡片移动与记录，不启动开发、build、review 或 ship，不改代码与配置。

factory · review · 2026-11-25 01:00 的正文与 verdict: fix（head: b9f94f6a33b7b5814345e115e57cfe4518ae153b；P1 启动先删 .tmp/.keep 再验证 active，链接目标日志可能丢失）保持原样，待后续工厂处理。不修改先前 owner 单进程独占状态目录边界（Guoyin / factory · 2026-11-25 00:17）。未移动 FT2/FT3/FT6。

## Guoyin · 2026-11-25 08:27
补充一下日志不会配置成符号链接的文件

## factory · 2026-11-25 08:27
按 owner 此次说明，日志文件不配置成符号链接是本项使用前提。factory · review · 2026-11-25 01:00 中仅因日志为符号链接才成立的发现（含 fix 中的 P1：active 为符号链接并指向 .tmp/.keep 时，启动先删除 .tmp/.keep 再验证 active，可能丢失链接目标日志；以及 notes 中与符号链接备份相关的项）不作为此边界内的交付阻塞。

此边界不得扩大为豁免普通文件日志的恢复问题、线程竞争或磁盘错误。不修改先前 review 正文与 verdict: fix，不自动改为 pass，不将本项移至 ready/。保留先前 owner 单进程独占状态目录边界（Guoyin / factory · 2026-11-25 00:17）以及 08:23 再排一轮、只移回 new、不启动开发或审查的说明。本 run 不改 round/branch，不移动本项，不启动 build/review/ship，不改代码与配置。

## factory · plan · 2026-11-25 08:38
第 4 轮补充：owner 在私聊明确授权“ft4 我移回 new 了，你让 cc 去build”，本轮启动 build，沿用原分支与历史轮次、不重置 round。按已记录的单进程独占、日志不配置为符号链接边界，上一轮仅因符号链接成立的 P1/P2 不作交付阻塞；不引入跨进程锁或 Windows 推测兼容层。维持普通文件启动/恢复与事务轮转，纠正 .1 发布前后名字丢失恢复语义、最终检查与 rename 非原子窗口。小范围调整启动校验顺序，并增加拒绝启动时现有文件不被修改的永久回归；执行完整配置测试。

## factory · 2026-11-25 08:38
第 4 轮 Claude Code 构建完成，new/ → review/。branch: factory/FT4-store-recovery-durability；head: a983fc3a6e79d34d11d623db2b60a0950c457a83。

启动先拒绝非普通 active、执行必要恢复，再清理 .tmp/.keep 与收紧备份权限；新增目录型非法入口回归，拒绝启动时保留暂存内容和备份权限。符号链接重叠目标 P1 随顺序调整一并避免，但不把其作为 owner 验收要求；符号链接 .1 的 P2 未额外扩展实现，按 owner 不配置此类日志的边界不作阻塞。修正轮转拒绝错误的“backups left untouched”过度承诺，README/AGENTS/systemd 文档区分 .1 发布前后的恢复能力，明确最终复验与 rename 并非原子操作。

测试：coder 完整配置测试（含 count=1）、store race count=5 通过；新增 TestRefusedStartupLeavesStateDirAsFound 在旧 store.go 上失败、当前通过。经理独立隔离 HOME/XDG/GOCACHE，离线复跑 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全部通过，Node 在 PATH，adapter 测试启用。无新增安装，使用已有 Go 1.27.4 与只读模块下载缓存。起点 origin/main@fb2c535、仅本项五个提交，无 never 路径修改，工作树和索引干净。代码分支已推送。

未做真实断电/kill -9、原生 Linux/Windows、无硬链接文件系统、真实 Chrome 或 systemd 部署验收；本轮未逐项核对 ACL skip。无 UI 改动。未 ship、未发 tag、未发布、未移动 FT2/FT3/FT6；round 保留累增至 4，后续复审仍需在当前 owner 边界内独立判断，不直接沿用旧 verdict 为 pass。

临时文件均留本 run TMPDIR 并在结束清理；coder 报告一次设置 TMPDIR 前的只读目录枚举，无写入、未 source 外部 env.sh；TMPDIR 外仅授权 build 锁心跳，无新增可见 /tmp 路径。


## factory · review · 2026-11-25 08:52
head: a983fc3a6e79d34d11d623db2b60a0950c457a83
verdict: fix
fix:
- [P1] internal/store/store.go:731：快照 rename 已生效但返回错误时，关闭新句柄并继续写旧 .1，后续轮转永久被路径检查拒绝，重启只读新快照而丢失后续受管记录。用现有 renamePath 注入点实际执行本地改名后返回 ENOENT、立即撤销注入，Codex 5/5、经理独立 1/1 复现 later 仅在 .1、重启只恢复 first。Linux rename(2) 明确记载 NFS 执行成功后重传返回失败的语义，未挂载真实 NFS。发布返回错误后核对 active 与新旧句柄 inode；确认已经发布则接管新句柄并同步目录，确认未发布才按原路径重试，无法判定时保留待核验状态，不得持续写错一代。增加永久回归。
notes:
- [P2] internal/store/store.go:816：备份发布 ENOSPC 后重试忽略 .2 的 lstat(EIO)，再次顺移已处理备份，3/3 追加返回 nil 但 .2/.3 变为同一代、较旧审计历史丢失。非 ENOENT 的检查错误应在顺移前返回并保留原始错误。
- [P2] internal/store/store.go:738：发布后旧日志 Close(EIO) 仅 warn，3/3 追加仍返回 nil；保留已接管的新句柄，将旧句柄关闭错误与目录同步错误合并为维护错误返回，保持 RecordWritten 语义。
- [P2] internal/store/store.go:339：可写探测只创建空文件，不再实际写入；真实子进程 RLIMIT_FSIZE=0 下对照一字节写 EFBIG，Open 却成功、首条记录写失败，3/3。应在独占探测文件写一个字节并检查写入、短写与关闭错误，始终只清理自己的文件。
- [P3] internal/store/store.go:796：.keep 的 lstat(EIO) 被误报为路径替换且原错误丢失；单独以 %w 返回检查错误，仅在取得 inode 后判断身份。
- [P3] internal/store/store.go:238：普通 active 为 0400 时最终 Open 返回 EACCES，但此前已删 .tmp/.keep 并收紧 .2 权限，3/3；应先确认 active 可读写打开，或收窄文档中拒绝启动不改已有文件的保证。

Codex 只读审查 origin/main...HEAD 全部 14 个变更文件，base/merge-base fb2c535，无代码更改或提交，工作树及索引干净。按 owner 单 daemon 独占状态目录、日志不配置成符号链接边界审查，不将跨进程或符号链接布局当阻塞。此前启动顺序 P1 已修复，此次 P1 是独立的发布结果不确定问题；来源核对：https://man7.org/linux/man-pages/man2/rename.2.html BUGS。

完整配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过；另全量 race count=1 与 store/daemon 核心 race count=3 通过。11 个有测试包通过，Node adapter 与两条权限测试实际执行；仅两个子进程 helper 按设计 skip，pkg/protocol 无测试文件。临时 overlay 的预期正确性断言失败用于确认缺陷，不是仓库原测试失败。经理核对 P1 测试源码、日志、控制流，并独立复跑确认。无安装，已有 Go 1.27.4；隔离 HOME/XDG/工具缓存，既有模块缓存只读复制，离线测试。

第 4 轮仍有 P1，round: 4 已达到/超过 max_rounds: 3，本项留在 review/ 等 owner 决定，不自动移回 new/。若继续修复，请 owner 添加说明并移至 new/；本轮不改 branch/round，不移动 FT2/FT3/FT6。无 source 或 owner Closes/Fixes/Resolves 链接，不发表评论。未合并、打 tag、发布或启用任务。

未做真实 NFS 挂载、断电、kill -9、物理磁盘故障、无硬链接文件系统、原生 Linux/Windows、真实 Chrome 或 systemd 部署验收；Windows 轮转仍按不支持边界记录。无 UI 改动。临时诊断与报告按流程结束清理；无新增可见 /tmp 路径，TMPDIR 外主动写入仅授权 review 锁心跳。通知发送成功。

## Guoyin · 2026-11-25 09:35
日志目录只用本机文件系统，不支持 NFS 等网络文件系统；按这个边界收窄 FT4 验收范围。

## factory · 2026-11-25 09:35
按 owner 此次说明，本项仅支持本机文件系统，不支持 NFS 等网络文件系统；仅依赖网络文件系统特殊语义的故障不列为当前验收阻塞。

factory · review · 2026-11-25 08:52 的 P1（internal/store/store.go:731：快照 rename 已生效但返回错误后继续写旧 .1，丢失后续受管记录）在当前报告中由测试注入复现（实际执行本地改名后返回 ENOENT），并引用 Linux rename(2) 对 NFS「执行成功后重传返回失败」的记载，但未做真实 NFS 挂载核验，也未提供该问题在本机文件系统上实际触发的证据。按 owner 本机文件系统边界，该 P1 不作为当前交付阻塞。不宣称本机绝不可能出现同一语义，也不把此次测试注入说成真实 NFS 复现。

此边界不得豁免本机普通文件上的磁盘满、权限错误、进程中断，以及轮转/恢复/线程竞争问题。该审查 notes 中的 P2/P3（备份发布 ENOSPC 后重试忽略检查错误、发布后旧日志 Close 错误仅 warn、可写探测未实际写入、.keep lstat 错误误报、拒绝启动仍改已有文件等）仍按原记录处理。保留先前 owner 单进程独占状态目录边界（Guoyin / factory · 2026-11-25 00:17）与日志不配置成符号链接边界（Guoyin / factory · 2026-11-25 08:27）。

不修改先前 review 正文与 verdict: fix，不自动改为 pass，不将本项移至 ready/ 或 new/，不把此次说明解释为 ship 批准。本 run 不改 round/branch，不启动 build/review/ship，不改代码与配置。

## Guoyin · 2026-11-25 09:46
移到 new 等待 build

## factory · 2026-11-25 09:46
按 owner 此次说明，将本项从 review/ 移回 new/，等待外部工厂 build。本 run 只做卡片移动与记录，不启动开发、build、review 或 ship，不改代码与配置。保留 branch 与 round: 4，不重置轮次或 max_rounds。

不修改先前 review 正文与历史 fix（含 factory · review · 2026-11-25 08:52 的 verdict: fix，head: a983fc3a6e79d34d11d623db2b60a0950c457a83）。保留此前三项 owner 边界：单进程独占状态目录（Guoyin / factory · 2026-11-25 00:17）、日志不配置成符号链接（Guoyin / factory · 2026-11-25 08:27）、本机文件系统且不支持 NFS 等网络文件系统（Guoyin / factory · 2026-11-25 09:35）。未移动 FT2/FT3/FT6。


## factory · plan · 2026-11-25 10:00
第 5 轮补充计划：
### Behaviour
顺移备份前将 .2/.3 检查的非 ENOENT 错误原样传播，.keep 检查失败不误报身份替换。发布后保留新句柄，旧句柄 Close 错误与目录同步错误合并为维护错误返回，RecordWritten 仍只代表本次完整写入。独占可写探测实际写一字节，检查写入、短写、关闭并仅清理自身文件。已有普通 active 先确认可读写打开，再清理暂存和收紧备份权限。
### Scope
修复 08:52 review 三条 P2 与两条 P3，补永久回归和精确文档；沿用单进程独占、非符号链接日志、本机文件系统三项 owner 边界，不新增跨进程锁、NFS 结果核验或 Windows 兼容层，不恢复 FT21。
### Decisions
沿用维护错误和窄测试注入点；发布后旧句柄关闭失败只报告一次，不误标为未发布或目录同步失败。收窄“拒绝启动不改任何文件”保证，明确目录收权限、恢复链接、打开后的清理仍可能发生；普通追加不增加 fsync。
### Checking
永久回归备份检查 EIO 与失败重试不丢历史、.keep 原始错误、发布后 Close/目录同步组合及恢复、探测写/短写/关闭错误和清理、0400 active 拒绝时暂存/备份不动。完整配置测试及 store 重复 race；故障注入不宣称真实磁盘故障。

## factory · 2026-11-25 10:00
第 5 轮 Claude Code 构建完成，new/ → review/。branch: factory/FT4-store-recovery-durability；head: 9b2f1c2（代码 91120bb、文档 9b2f1c2）。round 保留历史累增至 5，未重置 max_rounds。

完成补充计划中的三条 P2、两条 P3：备份槽检查错误先返回而不顺移；.keep 检查保留原始错误；发布后先接管新句柄，旧句柄关闭错误合并返回；可写探测实际写一字节并保留写/短写/关闭/清理错误；普通 active 打开提前于暂存清理与备份权限修改。README/AGENTS/systemd 文档补充本机文件系统、不支持 NFS 和准确启动失败保证。新增六组永久回归；旧代码仅接入三个测试注入点的副本上验证 red（.1 检查子用例原本通过）。不将旧 review 自动改为 pass；其 P1 按 owner 09:35 记录不作为当前阻塞，未新增网络文件系统机制。

测试：coder 完整配置命令通过，store race count=10 通过；经理独立隔离 HOME/XDG/GOCACHE/GOPATH/GOMODCACHE、只读复制既有模块下载缓存、离线复跑 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全部通过，11 个有测试包通过，Node 在 PATH，adapter 启用。coder Linux 静态 vet、Windows 交叉编译/store vet、realchrome-tag daemon vet 通过。store 不可写夹具清除继承 ACL 后实际执行，新增 0400 回归非 root 下不允许 skip；daemon 既有不可写目录测试仍因继承 ACL skip，两个子进程 helper 按设计 skip。无新增安装，使用已有 Go 1.27.4。经理核验仅本项七个提交、origin/main@fb2c535 已为祖先、never 路径未变、工作树干净，分支已推送。

限制/审查重点：首次启动先创建 active，后续备份 chmod 失败可留下空文件，文档明确不承诺所有失败无副作用；旧句柄关闭失败只报一次，不重试。daemon 对新关闭错误沿用通用告警路径，未新增该路径专项测试。未做真实磁盘满/EIO、RLIMIT_FSIZE 子进程、断电/kill -9、NFS、无硬链接文件系统、原生 Linux/Windows、真实 Chrome 或 systemd 部署；无 UI 改动。

FT28 已 done，按正常排序选择唯一 new 项 FT4；FT21 保持 stop，完整 target ID 未改。未 ship、未打 tag、未发布、未启用任务、未移动 FT2/FT3/FT6。临时写入均在工作树与本 run TMPDIR，外部主动写入仅授权锁心跳，无新增可见 /tmp 路径；coder 报告一次 TMPDIR 未传承后的只读系统临时目录枚举，未读取/source 外部 env.sh。按流程结束清理本轮 TMPDIR。通知发送成功。

## factory · review · 2026-11-25 10:09
head: 9b2f1c2add0f52f35b93c1a992e137b48e4c3294
verdict: pass
notes:
- [P2] internal/store/store.go:259：启动忽略删除普通暂存文件的错误。本机不可变 rpc.jsonl.tmp 下 Open 成功且无告警，后续两次超限追加均写入后返回 ErrRotation/EPERM，3/3 复现。应在启动报告路径和清理原始错误，避免无提示地失去轮转能力。
- [P3] internal/daemon/tab_idle.go:194：两条生命周期告警无条件承诺下次追加重试，但发布后旧句柄 Close 错误只报告一次。删除无条件重试措辞，或按错误类型精确说明并覆盖 close-only 告警。
- [P3] README.md:139、AGENTS.md:30：首次轮转预检查拒绝 active 缺失后，后续写入也可能经 .1 恢复（启动刚从 .1 硬链接恢复 active 时两者同 inode），实测 3/3。应按 .1 是否指向当前打开日志描述恢复能力，而非仅按轮转检查时点断言丢失。
- [P3] internal/store/rotation_checks_test.go:199：重试成功后读取 .1 再作为 .1 期望值，无法检出备份内容损坏；应使用重试前 active 内容加触发记录独立构造期望值。

Codex 只读审查 origin/main...HEAD 全部 15 个变更文件及完整线程，base/merge-base fb2c535。按 owner 单进程独占、日志不配置符号链接、本机文件系统（不支持 NFS）边界独立审查，无 P0/P1；旧轮次 review 正文/verdict 保留，不扩大边界豁免本机磁盘错误或恢复问题。经理核对本轮诊断输出、配置测试日志及上述控制流。工作树/索引干净，无代码提交。

配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全部通过，另一次无缓存完整 race 及最新六组回归 race count=5 通过。11 个有测试包通过，Node adapter 实际执行；两个子进程 helper 顶层按设计 skip；daemon 不可写目录因继承 ACL skip，临时 overlay 仅清除夹具 ACL 后原断言通过；store 不可写目录和 0400 active 测试直接执行。pkg/protocol 无测试。Go 1.27.4；隔离 HOME/XDG/工具缓存，既有锁定依赖缓存离线复用，无安装。

本机真实 SIGKILL 覆盖快照 fsync、暂存链接、较旧备份改名/链接、.1 发布、active 发布六个边界，各 3 次共 18 次重启恢复及后续轮转通过；真实 RLIMIT_FSIZE=0 下启动返回 EFBIG 且保留既有 .write-test，3/3 通过。未做物理断电、实际磁盘满/介质 EIO、无硬链接文件系统、原生 Linux、真实 Chrome 或 systemd 部署；Windows 轮转仍不支持，无 UI 改动。

第 5 轮审查通过，review/ → ready/；仅保留以上非阻塞项供 owner 查看。无 source 或 owner Closes/Fixes/Resolves 链接，不发表评论。未合并、打 tag、发布、启用任务或移动 FT2/FT3/FT6，FT21 保持 stop。无新增可见 /tmp 路径，coder 报告工作树/TMPDIR 外仅授权 review 锁心跳；临时诊断与报告按流程结束清理。

## factory · 2026-11-25 10:19
ship 完成。merge: 7b8aab43feaf205ecc73269a489d400487323c6b（parents: fb2c535b1273627cb7082bc59a9bb24c9e04b8ff + 9b2f1c2add0f52f35b93c1a992e137b48e4c3294），已普通推送 main。已审 head 9b2f1c2add0f52f35b93c1a992e137b48e4c3294 为远端 main 祖先。未打 tag、未发版、未删除代码分支、未改 workflow 或 factory.toml。

测试命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过：11 个有测试包 ok（bb-browser、bb-daemon、deploy、deploy/systemd、internal/browser、internal/daemon、internal/site、internal/state、internal/store、internal/timeout、pkg/daemonclient），pkg/protocol 无测试文件；go.mod/go.sum 未变。

不修改先前 review 正文与 verdict: pass（factory · review · 2026-11-25 10:09）。保留三项 owner 验收边界：单进程独占状态目录（00:17）、日志不配置成符号链接（08:27）、本机文件系统且不支持 NFS 等网络文件系统（09:35）。无 source 或 owner Closes/Fixes/Resolves 链接，无需关闭 issue/PR。按既有顺序，本项合入远端 base 后将安排 FT2 backlog → new；本 run 不启动 FT2 构建或审查，不移动 FT3/FT6，FT5 保持 stop。
