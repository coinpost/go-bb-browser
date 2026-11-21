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
