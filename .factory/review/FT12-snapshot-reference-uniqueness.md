---
branch: factory/FT12-snapshot-reference-uniqueness
round: 1
---
# [P2] snapshot 不清除旧的 __bb_snap_ref 属性，click/fill @ref 可能命中上一次快照遗留的其它元素

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B05；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`internal/browser/snapshot.go:81`（基线行号，实施前重新定位）。

触发条件：同一页面上先后两次 snapshot，且第二次没有遍历到第一次编号过的某些元素：例如先全页 snapshot 再 `snapshot --scope`；或两次之间有元素被隐藏、被 --compact/--depth 剪掉。随后 `click @N` / `fill @N`。

实际行为：每次 snapshot 从 1 重新编号并 setAttribute(__bb_snap_ref)，但从不移除未遍历元素上的旧值；daemon 把 ref 展开为 `[__bb_snap_ref="N"]`（internal/daemon/v1.go:768-771、1190-1193），按文档序取第一个匹配。复现：第二次快照声明 @1 = “Submit order”，选择器实际解析到上一次遗留 "1" 的 “Delete account” 按钮；隐藏横幅场景解析到已隐藏的旧元素。

期望行为：最新一次 snapshot 返回的每个 @ref 必须唯一指向该次快照中的那个元素；旧快照的 ref 应失效（报错）而不是指向别的元素。

影响：agent 按最新快照操作却点击/填写了别的元素（可能是破坏性按钮或把文本填进错误输入框）；或命中不可见旧元素导致 click 超时 60s。

根因：快照脚本只增量写属性，没有在开始时清理文档内已有的 __bb_snap_ref，也没有快照代次区分。

修复方向（建议，非指定实现）：在 snapshotStub 开头 `document.querySelectorAll("[__bb_snap_ref]")` 逐个 removeAttribute；或把代次编进属性值/属性名（如 `g3-12`），refs 映射与 click 解析同步使用。

回归验收：用 repro/snapshot_stale_refs.js 同款 fake DOM（或 headless Chrome e2e）断言：全页→scope、元素隐藏、pruneEmpty、maxDepth 四种组合下，refs[N] 选择器在文档内恰好匹配 1 个元素且为快照文本中 @N 对应的元素；旧 ref 不再匹配任何元素。

证据：repro-node.log（snapshot_stale_refs.js 两个场景 FAIL）；repro/snapshot_stale_refs.js。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

去重边界：backlog/FT5 中“旧 ref 标记残留/脱离节点重插”与本任务共享清理机制，本任务统一验收，不另开重复实现；需先保存 selector_scope 对应节点，再清理旧 ref，防止清理后 scope=@ref 解析失败。


## factory · plan · 2026-11-22 04:44

### Behaviour
- 每次成功快照产生的 ref 在当前文档唯一对应快照中的节点；全页改 scope、隐藏、interactiveOnly、pruneEmpty、maxDepth 均不可留下冲突旧标记。
- 旧快照引用失效而不是误指向新元素；脱离后重插的节点不得恢复旧 ref。先解析并保留 selector_scope 节点，再作引用失效，支持 scope 使用上次 ref 选择器。
- 无 body、scope 未匹配、空快照及错误选择器的处理需明确且永久测试覆盖，避免半清理后悄悄误操作。
### Scope
- snapshot 脚本、必要的 ref 解析兼容、最小用户文档和永久离线回归；不处理其他审查项，不合并 stop/FT1，不修改保护路径。
### Decisions
- 不仅清除当前 document 标记：检查脱离再插入与旧编号重用问题。优先保留现有纯数字 @ref 协议，使用文档生命周期内不复用的编号或同等简单机制；若需改格式，必须同步 click/fill 并覆盖兼容性。
- 引用表/节点追踪不得无限保留所有历史 DOM。由 coder 在阅读实现后选择最小可靠方案并说明边界，不引入无关架构或真实外网依赖。
- 仓库文档中的 headless 建议不适用于本任务：禁止 headless Chrome，禁止操作用户窗口。优先可执行真实生产 JS 的离线 DOM 测试，不使用只匹配源码字符串的伪回归。
### Checking
- 永久回归先在未修生产代码时运行，记录正确断言确实失败，再修复。覆盖 full→scope、隐藏、pruneEmpty、maxDepth、interactiveOnly、scope 使用旧 ref、脱离重插、旧 ref 失效以及 click/fill 解析。
- 执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；测试 HOME、缓存及所有临时产物均置于本 worktree 的 TMPDIR。
- 记录真实非 headless Chrome/CDP、Docker、systemd 是否实测；未实测不得宣称通过。


## factory · 2026-11-22 04:44

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a；未合并 stop/FT1。构建提交：f66deab、9ed00cd、f76baa4（分支 head f76baa4）。

实现：snapshot 先保存 scope 节点，再清理上一快照跟踪节点和文档内遗留标记；包含脱离后重插节点。每个 document 维护不复用的数字计数器及仅最近一次快照的节点列表。click/fill 共用 SnapshotRefSelector；补充生命周期文档及离线永久测试。

决定与兼容：保持纯数字 @ref 和原选择器形式；同文档第二次快照不再从 1 编号。新文档（导航/刷新）重新从 1 开始，因此不保证跨文档旧 ref 失效。非法 scope 选择器在清理前抛错，保留旧引用；scope 不匹配、无 body、不可见或过滤后为空的成功响应会使旧引用失效。仅保留最近一次快照节点，脱离节点最多保留到下一快照/导航。

回归：goja + fake DOM 执行生产脚本并实际解析 refs 选择器；coder 报告在生产修复前 full→scope、隐藏、pruneEmpty、maxDepth、interactiveOnly、scope 使用旧 ref、脱离重插、引用不复用、空快照等正确行为断言失败，修复后通过。典型修复前失败：`@4 resolves to 2 elements, want exactly 1`。daemon 永久测试验证 click/fill 对 N 和 @N 的选择器与 snapshot refs 一致，该测试未记录修复前失败。

安装：新增直接测试依赖 github.com/dop251/goja v0.0.0-20250630131328-58d95d85e994，及间接依赖 dlclark/regexp2 v1.11.4、go-sourcemap/sourcemap v2.1.3+incompatible、google/pprof v0.0.0-20230207041349-798e818bf904；x/text v0.31.0 显式列为 indirect，原有依赖版本及 Go 1.26 指令未改变。go.mod/go.sum 更新，无额外工具安装，使用已有 Go 1.27.4。

测试：coder 完整配置测试通过；manager 在最终提交独立再次执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过（部分包命中 cache，GOPROXY=off）。8 个有测试包通过，2 个无测试包。git diff --check 通过，保护路径无变更，工作树干净。测试 HOME、缓存和临时产物均隔离于本次 TMPDIR。

未检查：真实非 headless Chrome/CDP、真实 DOM 布局和 innerText、跨 Runtime.evaluate 的 document 属性、bfcache、旧 ref 实际 click 超时行为、Docker、systemd、部署和 UI 均未实测；未启动 headless Chrome，未操作用户窗口。fake DOM 仅支持脚本所需 DOM API 与有限选择器。snapshot 之后页面自行 cloneNode 复制标记、iframe/shadow DOM 不在覆盖内。review 请重点检查这些边界及文档内/跨文档生命周期。

状态：代码分支已推送，new/ → review/，第 1 轮；本次仅 build，未 review、未合并 main。
