# run帮助布尔参数与选项终止符兼容

## factory · 2026-11-21 23:23
从FT1历史review拆出，owner已采纳拆分判断；仅保留为backlog，未授权构建。等级沿用原发现，不表示已修复。行号为发现时位置，实施前重新定位。

[P2] cmd/bb-browser/main.go:1036：run adapter.js --help=true仍发eval；统一布尔帮助参数，请求帮助时零RPC。
[P3] cmd/bb-browser/main.go:956：run -- --foo.js错误拒绝存在脚本；保留--选项终止边界。

来源：FT1 第1轮起review notes。
历史证据保留于原FT1线程及本机review run evidence/FT1-<对应head>/，不可假定诊断PASS表示修复通过。

验收：针对各失败路径增加永久回归，执行配置完整测试；明确记录未检查项。与FT1第6轮共享基础状态路径时，在实施前检查最新代码，避免重复修复。
