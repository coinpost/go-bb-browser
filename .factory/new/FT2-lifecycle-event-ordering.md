# 生命周期事件溢出重同步与优雅关停次序

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] internal/daemon/browser_reconnect.go:133：队列溢出重同步后继续消费旧created事件，可重新注册已消失页；建立事件与枚举次序边界。
[P2] internal/daemon/server.go:253：优雅关停target同步早于在途HTTP请求退出，期间销毁漏记；RPC排空后收齐生命周期事件再停止同步。

来源：FT1 第3～4轮review；旧枚举删除epoch基础约束仍在FT1第6轮，此处单独处理事件队列与关停协议，避免重复实现。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
按此次 owner 授权记录顺序与收窄范围，本项仍留在 backlog/。须在前置 FT7（ready 后 ship 并成功打 tag）以及 FT4 已交付之后，才移入 new/，不启动本次 build。

收窄后范围：优雅关停次序保留（RPC 排空后收齐生命周期事件再停止同步）。溢出后继续消费旧 created/生命周期事件、可能重新登记已消失页的问题与已交付 FT28 有重叠：先核实已有回归（含 FT28 审查 factory · review · 2026-11-23 14:13 head dab7d41a67aaddb4192d7e4abe02208a4e56ff4d 所记「旧排队事件」永久回归，在已合入 main 的 d408b68c5133cc6c17048940f9518e3428eb2cdd / merge 0d7c792e89667c616c64e6eb343eb92db104189e 上是否仍覆盖原调度）再决定是否实现，不以标题宣称已修好。

依赖顺序：FT7 ready → ship 并 tag → FT4 → 本项 → FT3 → FT6。

## factory · 2026-11-25 10:19
前置 FT4 已合入远端 main（merge 7b8aab43feaf205ecc73269a489d400487323c6b，已审 head 9b2f1c2add0f52f35b93c1a992e137b48e4c3294）。按既有顺序将本项从 backlog/ 移入 new/，等待外部工厂 build。本 run 不启动 FT2 的开发、build 或 review。

保留此前收窄范围：优雅关停次序优先（RPC 排空后收齐生命周期事件再停止同步）。溢出后继续消费旧 created/生命周期事件、可能重新登记已消失页的问题与已交付 FT28 有重叠：须由外部工厂先核验已有回归（含 FT28 审查 factory · review · 2026-11-23 14:13 head dab7d41a67aaddb4192d7e4abe02208a4e56ff4d 所记「旧排队事件」永久回归，在已合入 main 的相关提交上是否仍覆盖原调度）再决定是否实现，不以标题宣称已修好。不移动 FT3/FT6，FT5 保持 stop。
