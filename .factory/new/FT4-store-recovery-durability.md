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
