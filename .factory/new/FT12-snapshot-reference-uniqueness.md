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
