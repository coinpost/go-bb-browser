# run帮助布尔参数与选项终止符兼容

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] cmd/bb-browser/main.go:1036：run adapter.js --help=true仍发eval；统一布尔帮助参数，请求帮助时零RPC。
[P3] cmd/bb-browser/main.go:956：run -- --foo.js错误拒绝存在脚本；保留--选项终止边界。

来源：FT1 第1轮起review notes。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。

## Guoyin · 2026-11-24 22:29
你盯一下工厂任务，等 FT7 ready 后，ship 掉；然后发一个版本 tag。

接下来，按照你的建议，归档 FT5，FT4 优先，FT2/FT3 其次，FT6 后置。

每个前置任务 ready 之后，就 ship 掉然后把下一个 backlog 任务移到 new。

全部任务 ship 掉之后就结束。

## factory · 2026-11-24 22:29
按此次 owner 授权记录顺序与收窄范围，本项仍留在 backlog/，优先级最低。须在前置 FT7（ready 后 ship 并成功打 tag）以及 FT4、FT2、FT3 均已交付之后，才移入 new/，不启动本次 build。

收窄后范围：低优先处理 run 的帮助布尔参数（如 adapter.js --help=true 仍发 eval，请求帮助时应零 RPC）以及 -- 选项终止后以 -- 开头的脚本路径（如 run -- --foo.js）。可与已交付 FT9 审查 factory · review · 2026-11-22 04:31（head 8004cbd723455c73fd843f07ac0f9c5be43f4468）所记残留帮助/分隔符兼容问题对齐，但不把 FT9 当作本项已修好。

依赖顺序：FT7 ready → ship 并 tag → FT4 → FT2 → FT3 → 本项。

## factory · 2026-11-25 14:16
前置 FT3 已合入远端 main（merge c5d25a1bfb25fa9e7e557b2c148847812a2495d4，已审 head 74345e9e2f00208515199d8b89a77974bde76a1a）。按既有顺序将本项从 backlog/ 移入 new/，等待外部工厂 build。本 run 不启动 FT6 的开发、build 或 review。

保留此前收窄范围：低优先处理 run 的帮助布尔参数（如 adapter.js --help=true 仍发 eval，请求帮助时应零 RPC）以及 -- 选项终止后以 -- 开头的脚本路径（如 run -- --foo.js）。可与已交付 FT9 审查 factory · review · 2026-11-22 04:31（head 8004cbd723455c73fd843f07ac0f9c5be43f4468）所记残留帮助/分隔符兼容问题对齐，但不把 FT9 当作本项已修好。FT5 保持 stop。
