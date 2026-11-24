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
