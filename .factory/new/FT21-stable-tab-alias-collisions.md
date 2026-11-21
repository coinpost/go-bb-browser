# [P3] 短 tab id 在后缀碰撞时取决于登记顺序，重启后同一短 id 会指向另一个 tab

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B14；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`internal/state/tab_registry.go:172`（基线行号，实施前重新定位）。

触发条件：同时存在两个 CDP target id 末 4 位 hex 相同的 page target（50 个 tab 时约 1.9%，200 个 tab 时约 26%），daemon 重启（或注册表经全量同步重建）。

实际行为：assignShortLocked 取“最短的未被占用后缀”（tab_registry.go:172-179），结果依赖登记先后：运行期按创建顺序 A=abcd、B=1abcd；重启后 SyncPageTargetsDetailed 按 target id 排序登记，得到 A=0abcd、B=abcd。旧短 id abcd 现在解析到 B。

期望行为：AGENTS.md 声明短 id 由 CDP target id 确定性派生、跨重启稳定：同一 target 的短 id 不应随登记顺序变化，旧短 id 不应解析到别的 target。

影响：客户端/Pool 持有的短 id 在重启后静默指向另一个标签页（操作串台）；rpc.jsonl 重放把受管状态与最后活动时间记到错误的 tab 上，可能误关未受管的 tab、漏关受管 tab。

根因：短 id 不是 target id 的纯函数，碰撞消解依赖当前已占用集合。

修复方向（建议，非指定实现）：让短 id 成为 target id 的纯函数（例如固定取更长的后缀/完整 id 的哈希前缀，碰撞时对所有碰撞方统一加长），或在持久化记录里同时保存完整 target id 并在恢复时校验。

回归验收：state 测试：对两个末 4 位相同的 target，以两种登记顺序（逐个 Register 与排序后的 Sync）得到的短 id 完全一致；重建注册表后 Lookup(旧短 id) 仍返回原 target；无碰撞时短 id 仍为 4 位。

证据：repro-go.log（TestAudit_ShortIDsMustBeStableAcrossRestartOnSuffixCollision FAIL）；repro/state_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。

设计约束：报告的“无碰撞保持4位、所有登记顺序一致、任意未来碰撞不改旧别名”不能无条件同时保证。实施前定义可实现的别名稳定契约；持久化恢复必须核验完整 target 身份，不能仅加长后缀就宣称防止误归属。跨任务协调 B03，但保持独立提交与回归。
