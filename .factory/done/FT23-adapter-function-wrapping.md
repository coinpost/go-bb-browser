---
branch: factory/FT23-adapter-function-wrapping
round: 1
---
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


## factory · plan · 2026-11-22 07:27
### Behaviour
- 开头匿名 async function adapter 完整绑定并 await 调用，参数为 __args；URL 正则、多行 if、引号/花括号正则、嵌套模板字符串不改变语义。
- 保留前导注释/@meta、export default、尾分号和脚本体 return；空脚本沿用现状。非法源码报错，不因截断变成合法但不同的脚本；字符串、注释及嵌套函数不得被误改写。
### Scope
- 只改 internal/site 包装逻辑、永久回归及必要说明；不新增 JS parser/运行时依赖，不涉及其他审查项和保护路径。
### Decisions
- 移除不可靠的大括号扫描，只识别开头 adapter 形式，原样保留函数体；Node 仅用于本地语法和行为测试，不访问外网。
- 不自动调用裸箭头函数，保持文档既有契约；脚本体可以自行 return 箭头 IIFE。
### Checking
- 先新增永久回归并在改生产代码前确认失败；node --check 后在 stub fetch 下执行并断言真实结果、调用次数及参数。
- 覆盖正则、嵌套模板、meta/export/default/分号、脚本体及误识别；执行完整 gofmt、vet、race 测试。真实 Chrome/CDP、Docker、systemd 未检查时明确记载。


## factory · 2026-11-22 07:27
基线：origin/main@4f86c1c588facd643e4596a40643b88a69061982；提交：65ea3e1637fef14f0f49334dcf0eb8e167b6f844。Claude Code 实现，未合并 stop/FT1。

实现：删除 matchingJSBrace 等手写扫描；仅识别开头空白/注释、可选 export default 后的匿名 async function，添加 const __bb_run 前缀及独立成行的 await 调用后缀，函数体不扫描不改写。增加函数类型校验；更新 adapter 文档及永久 Node 语法/执行回归。

决定与行为边界：使用 const 赋值而非括号包围整份源码，保留尾分号、尾注释及函数后提升的 helper 声明。不自动调用裸箭头或命名函数；前面有可执行语句的匿名函数不再被偶然接受，而是语法错误。函数后的可执行语句现在会先于 adapter 调用运行（旧行为在 return 后不会执行）；这点留给 review 检查是否符合单一函数 adapter 契约。export default 只在源码开头剥除。

回归证据：修改生产代码前永久回归复现 URL 正则多行 if 返回 undefined、fetch 0 次，嵌套模板和 meta/export default 同样 undefined，含引号/花括号正则及嵌套 async 函数生成语法错误。修复后同一组回归全部通过，Node 检查与实际 stub fetch 执行均通过，无 skip。证据保留于 build run evidence/FT23/ 的 before-fix.log、after-fix.log、manager-test.log。

测试：coder 完整配置检查通过；manager 独立以 throwaway HOME、缓存和临时目录执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全部通过，10 个有测试包通过，2 个无测试包。git diff --check 通过；所有提交仅涉及本项，保护路径无变更，工作树干净。

安装：无，依赖清单/锁文件未变；使用已有 Go 1.27.4、Node v26.4.0，未联网。

未检查：真实非 headless Chrome/CDP 页面执行、Docker、systemd 未实测；没有 UI 变更。包装结果只在本地 Node/V8 中验证。执行偏差：coder 首次运行环境脚本未生效，用过真实 HOME Go 缓存和系统默认临时目录；其报告称测试临时文件已自动清理，未手动删除 /tmp。后续失败证据及全部验证都使用隔离 HOME/TMPDIR。

状态：分支已推送，new/ → review/，第 1 轮。仅 build，未 review、未合并 main、未部署。


## factory · review · 2026-11-22 07:44
head: 65ea3e1637fef14f0f49334dcf0eb8e167b6f844
verdict: pass
notes:
- [P2] internal/site/run.go:45: 调用被追加到整份源码末尾，函数后 `; return;` 会使 adapter 不执行而成功返回 undefined，尾部 await fetch 则先于 adapter 执行；基线行为不同。建议保留函数声明边界及原调用顺序，补充尾部可执行语句回归。
- [P2] internal/site/run.go:21: const 赋值改变函数后的语法边界：换行 `.bind(null, replacement)` 从基线 SyntaxError 变为可执行并替换参数；同一行紧接 function helper 声明从合法变为 SyntaxError。建议保留声明语法边界，分别加入回归。
- [P2] internal/site/run.go:40: export default 和 async 之间有块注释或行注释时，旧实现正常，新实现 SyntaxError。应在剥除 export default 后继续跳过并保留注释。
- [P3] internal/site/run.go:56: 前导扫描只识别四种 ASCII 空白且行注释只找 LF，UTF-8 BOM 及 CR 结束的前导行注释从正常变为 SyntaxError；应按 ECMAScript 空白/行终止符识别。

审查：Codex 对 origin/main@4f86c1c588facd643e4596a40643b88a69061982...HEAD 完成代码审查；以上兼容性问题均以基线与 HEAD 的真实 RunScript 输出在本地 Node 离线对照复现。无 P0/P1，依 factory 规则判定 pass，review/ → ready/；P2/P3 不阻断，但建议 owner 在 ship 前查看这些兼容性边界。coder 建议修复后合并，不等同于本流程的 P0/P1 阻断判定。

测试：`test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 退出 0，10 个包通过、2 个无测试；`go test -count=1 -v ./internal/site -run '^TestRunScript'` 通过且无 skip，含实际 node --check、stub fetch 返回值/参数/调用次数断言。git diff --check 通过，工作区干净，保护路径无变更；与当前 main 可干净合并。review 未改代码、未提交代码、未推送代码分支。

安装：无，使用已有 Go 1.27.4、Node v26.4.0。所有本轮测试临时 HOME、缓存与复现文件位于本轮隔离临时目录。未检查：真实 Chrome/CDP、Docker、systemd；无 UI 变更。无 linked issue/PR，未发 GitHub 评论。仅完成审查，未合并 main、未部署。

## factory · 2026-11-22 09:39

owner 要求：把可以交付的交付掉。已将已审查 head 65ea3e1637fef14f0f49334dcf0eb8e167b6f844 合入 main 并推送，merge: 3495dedee7f86674ee45a4e87cc0186619f5345c。无合并冲突。同一批仅交付当时 ready 且可干净合入的项；未改代码。

测试：变更包 `go test -race ./internal/site` 通过（ok 2.168s）。累计 main 完整 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 退出 0（10 个测试包通过、2 个无测试包）。go.mod/go.sum 未改。保护路径无变更。

按 ship=[merge,push] 未打 tag、未发布、未部署；无关联 issue/PR 需关闭，未删除代码分支 factory/FT23-adapter-function-wrapping。原 review 的非阻断 P2/P3 保留。ready/ → done/。
