---
branch: factory/FT21-stable-tab-alias-collisions
round: 1
---
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

## factory · plan · 2026-11-22 07:02

### Behaviour
- 修复同后缀 target 的别名与恢复误归属；先增加永久回归，证明原实现失败，再修复。
- 明确定义可实现的契约：无碰撞时继续返回 4 位；相同存活 target 集合的碰撞结果与登记顺序无关。未来新碰撞允许使原来的短别名失效或升级，不能静默把旧别名交给另一 target；不能声称所有旧 4 位 id 永远稳定。需要对有歧义别名 fail closed。
- 重启恢复以完整 CDP target 身份为准，不仅凭短别名；缺少身份的旧记录保守跳过，不允许认领其他 tab。轮转快照必须保留身份。空列表、未知/已关 target、恢复失败均不产生误归属。
### Scope
- state 注册表、daemon 的别名使用/审计和 idle 恢复、store 完整身份记录及轮转、相应测试和必要文档；不合并 FT1 或其他任务分支，不顺带修复 B03/FT10 等其他缺陷。
### Decisions
- 三项要求不能无条件兼得，优先身份安全与确定性；允许碰撞出现时旧短别名变为硬错误，文档必须说明。具体实现选择最简单能覆盖既有调用方的方案；如需要重映射，检查 selected、观测响应、handler、idle 清理等持有别名的路径，不能只改注册表。
- 新日志包含完整 target 身份；旧的无身份日志不用于恢复受管资格。不能在 tab 已移除后靠 Lookup 补身份来错误归属；在能确认身份的生命周期点捕获。
- 不使用真实外网作为测试依赖，不启动/操作用户 Chrome。
### Checking
- 两种 Register 顺序、Sync 重建、原 4 位碰撞、后续碰撞、删除/重新登记、非 hex / 极短 target 不覆盖其他映射；原短别名只能返回原 target 或拒绝。
- 回放必须验证完整 target；测试错配/缺失身份、关闭后重用短 id、轮转后重启、无碰撞正常恢复。
- 完整 gofmt/vet/race；真实 Chrome、Docker、systemd 若未运行如实说明。

## factory · 2026-11-22 07:02

已基于 main@4f86c1c 独立完成并推送，提交 e7d2a8b、f0bcb35；未合并 FT1 或其他任务分支。

实现：注册表按当前存活 target 集合统一计算别名；无碰撞保留 4 位，歧义别名硬错误，返回最短唯一别名。selected 与跨请求状态按完整 target 身份维护；多个别名共享 target 操作锁。审计在解析时捕获完整 target，回放、轮转快照与 idle 清理均按完整身份处理，不按短别名认领 tab。

契约与决定：相同 target 集合下结果与登记顺序无关；后来发生碰撞允许旧短 id 变为歧义硬错误，客户端需重新 tab_list；关闭释放别名后可重用，不承诺任意历史短 id 永久有效。缺失 target 的旧日志保守跳过，升级前创建的 tab 将不恢复受管资格、不会自动 idle 关闭，需手动清理。文档同步更新。

检查：新增 state、daemon、store 永久回归，覆盖顺序、Sync 重建、歧义、退化 id、别名锁、错配/缺失身份、轮转后恢复、idle 身份隔离。在起点副本运行新回归，state 5 项及 daemon 8 项修复前失败；其中身份恢复串台三项直接复现原缺陷，其余为新契约验证，不混称已有测试失败。修复后 coder 与经理分别执行完整 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 均通过；coder 另以 race 重复 15/20 次跑相关测试。经理核对提交范围、never 路径与 diff --check 均正常。

安装：无，使用已有 mise Go 1.27.4，manifest/lockfile 未变。

未验证：未运行真实 Chrome、Docker、systemd，未做真实 CDP 碰撞时序/UI 人工检查。需 review 重点检查碰撞后客户端/Pool 重新取 id 的兼容性、别名/target 两级锁和身份捕获时序。Pool 未自动刷新歧义别名；极端锁竞争时 resolveTab 可能返回 unknown tab 而不是超时错误。B03 受管记录外部关闭后的积累问题不在本任务修复范围。

Claude Code 额度接近用尽：5 小时窗口已用 99%，返回时约 1 小时 28 分后重置；本次未重试。

new/ → review/，round: 1。
