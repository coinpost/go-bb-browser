# [P3] adapter 包装器的括号扫描把正则字面量里的 // 当行注释，合法 adapter 被截断成永不执行的脚本

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B17；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/site/run.go:85`（基线行号，实施前重新定位）。

触发条件：adapter 采用文档推荐的匿名 `async function(args) {…}` 形式，函数体内含正则字面量且其中出现 `//`（如 `/^https?:\/\//`），并且同一行后面有未在本行闭合的 `{`（最常见的多行 if 写法）。带引号或花括号的正则、含嵌套 `${}` 的模板字符串同理。

实际行为：matchingJSBrace（run.go:84-91、104-120）不识别正则字面量：把 `\/\//` 里的 `//` 当成行注释跳到行尾，漏计该行的 `{`，于是提前匹配到函数体中间的 `}`。rewriteAnonymousAsyncAdapter 在该处插入 `}; return await __bb_run(__args);`，生成的脚本语法合法但外层函数只定义 __bb_run 不调用它：结果为 undefined，fetch 调用 0 次。

期望行为：合法的 adapter 源码被完整包装并执行，返回其结果 {"ok":true}；无法安全改写时应报错而不是生成语义不同的脚本。

影响：含常见 URL 正则的 adapter 静默返回 null/空结果（CLI 退出码 0），极难排查；也可能把函数后半段代码暴露在错误的作用域里执行。

根因：手写的 JS 词法扫描只处理了字符串/注释，没有处理正则字面量与模板插值。

修复方向（建议，非指定实现）：避免做括号匹配：把 adapter 源码整体作为表达式包装，例如 `const __bb_run = (<源码去掉 export default 与尾部分号>); return await __bb_run(__args);`（匿名 async function/箭头函数均适用），仅在源码不是函数表达式时保留旧路径。

回归验收：site 测试：对含 `/^https?:\/\//` 多行 if、含 `/[{}"]/`、含嵌套模板字符串的匿名 async adapter，RunScript 输出经 `node --check` 通过且在 stub fetch 下执行返回 adapter 的真实结果；现有 adapter 包装测试保持通过。

证据：repro-go.log（TestAudit_AdapterWithRegexLiteralMustWrapWholeFunction FAIL）；repro-node.log（node 执行包装结果：adapter result = undefined）；repro/site_audit_test.go；repro/adapter_wrapped_run.js。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
