# 快照ref作用域与脱离节点清理

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] internal/browser/snapshot.go:109：清理旧ref早于selector_scope解析，以上轮ref为范围会得到空结果；先解析并保存根节点。
[P2] internal/browser/snapshot.go:108：脱离文档旧标记未清除，插回后ref重复；跟踪清理上轮节点或使用正确的轮次边界。

来源：FT1 第1轮起review notes；非本轮身份修复范围。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## factory · 2026-11-22 01:11
本次 main review B05 已建立 new/FT12-snapshot-reference-uniqueness.md，统一处理 ref 清理及 scope=@ref 兼容；本 backlog 的重叠清理问题委托该任务验收，不重复实现。
本任务列出的 scope=@ref 先解析后清理验收也已委托 FT12；FT5 暂留历史 backlog 供追溯，在 FT12 完成后核查是否仍有独立未覆盖问题，不另行重复构建。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
owner 明确要求归档 FT5。本项移入 stop/ 保留全部既有正文、标题与历史，不是 ship，也不标为 done，不合并 main、不打 tag。

依据既有看板记录而非新的源码审查：本卡 factory · 2026-11-22 01:11 已将原两项问题委托已交付的 FT12 统一验收，并写明 FT12 完成后核查、不另行重复构建。FT12 审查 factory · review · 2026-11-22 05:08（head f76baa44505b3305daf0e1b7f6836cf426e2fe13，verdict: pass）及 ship factory · 2026-11-22 06:08（merge f1b74a7）已记录先保存 selector_scope 再清理旧 ref、脱离后重插不得恢复旧标记，并有离线永久回归先红后绿。FT12 审查遗留的非本卡原范围 P2 不并入本项。
