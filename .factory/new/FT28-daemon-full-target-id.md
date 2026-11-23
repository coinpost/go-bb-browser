---
branch: factory/FT28-daemon-full-target-id
round: 6
---
# [P1] bb-daemon 支持完整 CDP target ID：自动化精确寻址，保留短 ID 兼容

## factory · 2026-11-22 06:42
owner 原话：
> 用 cc 去调查一下 main 分支的 bb-daemon 能否做到这里说的前提，如果做不到，给它提一个工厂任务去修复。我的意见是让 bb-daemon 支持使用长 id

这里的前提：自动化获得并使用完整 CDP target ID；显式完整 ID 精确匹配，未知或过期直接报错，不做前缀/后缀匹配，不回退当前标签、其他标签或短别名。旧目标消失后不得把请求重定向到另一个目标。不引入所有权、租约或客户端身份协议。

### CC 调查结论
基线 main@4f86c1c588facd643e4596a40643b88a69061982，实施从最新 main 出发并重新定位。
- internal/daemon/v1.go:283-288、473-482：tab_list/tab_new 仅输出短 ID，内部已有 TargetID 但未公开。
- internal/state/tab_registry.go:227-234、284-293：4 位起的 hex 后缀别名；Lookup 仅查 shortToTarget，不接受完整 CDP ID；移除目标即释放别名。
- CC 使用 fake CDP / 真实 daemon 与 state 路径的 overlay 诊断：18 个定向方法均拒绝完整 ID；创建 A（AAAA…1234）后 A 消失、出现 B（BBBB…1234），一次枚举后持旧别名 1234 的 eval 与 tab_close 均到达 B。注册表重建也可让碰撞别名换主。此为模拟 CDP 复现，非真实 Chrome 实测。
- internal/daemon/rpc_connections.go:183 起：锁键为原始请求 ID；只补长 ID 查找会让同一 target 的两种写法使用不同锁。
- internal/daemon/v1.go:243 起：idle 续期响应阶段重查别名，应沿用本次操作固定的完整身份。
- main 目前未知显式 ID 不 fallback，保持此行为。
- 6 个诊断测试及 state/daemon/store/daemonclient/CLI 五包基线测试通过；诊断 PASS 表示风险被复现，并非安全。未运行全量 vet/race，未验证真实 Chrome 或跨 Chrome 重启的 ID 唯一性。

### 行为与 API 决定
1. 新增显式 target_id 请求参数，与 tab（原短 ID）互斥，同时提供报 invalid params。长 ID 失效后绝不能落入短别名命名空间；保留 tab 兼容人工和旧客户端。
2. tab_new、tab_list 各项、tab_focus 及相关目标响应提供 target_id；原 tab 字段不变。自动化能直接使用创建结果，不需再次枚举猜对应关系。
3. 全部定向 RPC，包括 goto/reload、eval、close/select、截图、交互、fetch/snapshot、network/console/errors 读取、路由与清理，共用身份解析语义。完整 ID 只精确匹配，不折叠大小写、不猜测、不降级、不 fallback；失效直接报明确 unknown/stale target 错误。
4. 固定解析的完整身份，操作锁按 target 身份归一化，短/长写法互斥；等待锁/加锁后目标消失报错，不得重查短别名而换主。未知 ID 不留无界锁项（保持 FT17）。最终 CDP 操作使用同一完整身份。
5. idle 续期及本次 RPC 审计身份沿用实际解析结果，不在响应阶段重新查别名；长 ID 请求不能漏续期或误续其他 target。
6. pkg/protocol、daemonclient Client/Pool、CLI 同步支持。CLI 提供 --target-id，与 --tab 互斥并展示长 ID。Pool 的长引用带后端命名空间（如 backendKey:完整ID），明确编码/解码，兼容短引用，不凭裸 target ID 猜后端。
7. 更新 README、AGENTS.md 和 daemonclient 文档：短 ID 是便捷别名，自动化使用完整身份；不承诺未经证实的跨浏览器会话永久唯一性。

### 范围及关联
- 不改短别名算法（FT21），本项正确性不得依赖短别名稳定。
- FT10 负责 managed 持久化与恢复；保持其完整身份活动记录语义，不引入第二套 managed 存储。若 FT10 未合并，先保证当前内存续期及审计身份正确，明确持久化集成依赖和验收，不假称全链路通过。
- FT26 负责 Pool 陈旧绑定回收，本项仍须保证新增长引用路由正确、未知不 fallback。
- 不修改 feedscrawler，不合并 stop/FT1，不发布、不改工厂配置。

### 永久回归与验收
- 创建/list/focus 返回真实完整身份，创建结果直接用于全部定向方法，操作抵达相同 target。
- A（…1234）消失、出现 B（…1234）并枚举：持 A 的完整 ID 调用全部定向方法均报错，B 不收到操作、不被关闭；覆盖解析后消失、等待锁时消失。
- 后缀碰撞目标改变登记顺序及重建 registry/daemon 后，存活的完整 ID 仍指向自身，过期 ID 不命中其他目标。
- 不存在完整 ID、前缀、后缀、大小写变体均报错（测试数据确保变体并非另一个合法完整 ID）；target_id 不 fallback 同名短别名；两参数并给报错。
- 同一目标短/长 ID 并发被串行化（race）；大量未知 ID 不永久积累锁；目标消失/别名重绑不误操作或误续期 B。
- 受管标签只有长 ID 请求仍正常续期，审计保留实际身份；与 FT10 集成后验证重启恢复活动时间，不重查别名。
- Pool 至少两后端：TabNew 长引用用于后续路径正确路由；未知/失效引用明确报 UnknownTabError，不选其他后端或标签。CLI/Client/Pool 均有永久回归。
- 保持现有短 ID 兼容；正确行为回归记录修复前失败、修复后通过。全量：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...
- 测试 HOME/缓存/临时目录隔离、不依赖外网。真实 Chrome 验证仅用隔离的非 headless 实例，不操作用户窗口。有限测试不能证明跨进程 ID 永不复用；假设、未验边界如实记录。

## factory · 2026-11-22 09:46
owner 最新要求（覆盖上文保留短 ID 兼容的设计与验收）：
> 把 go-bb-browser FT28 的优先级提高，然后以后都使用长 id，不再使用短 id，短 id 碰撞的问题可以不用处理了
> 对 go-bb-browser FT28 改成移除短 id 的支持，以后只用长 id

优先级：下一次 build 优先选择 FT28，排在其他 new/ 任务之前。这是 owner 的明确调度覆盖，不只是严重等级调整。本次不自动启动 build、不启用暂停的定时任务、不发布。

新的实现契约：
- 移除短 ID 支持，而非仅建议自动化用长 ID。bb-daemon、protocol、daemonclient Client/Pool、CLI 及内部调用统一使用完整 CDP target ID；创建、列举、焦点响应与定向操作全部使用长 ID，不再发出或解析短别名，不保留短 ID 兼容路径。
- 可以让现有 tab/--tab 字段承载完整 ID，以最小 API 改动完成；若另设 target_id/--target-id，也必须统一长 ID 语义并拒绝冲突，不能留下短 ID 解析入口。更新全部调用方、帮助和 README/AGENTS.md，明确破坏性变更及旧客户端迁移方式。
- 完整 ID 原样精确匹配，不接受截断、前后缀或大小写变体，不补全、不 fallback 当前标签或其他目标。未知/过期直接报错。首次解析后固定完整身份，等待锁与最终 CDP 操作不得重解析而换主。
- 锁按完整 target 身份统一；审计、idle 续期、持久化恢复沿用实际身份。Pool 长引用必须携带后端命名空间，未知引用不能猜后端。旧日志若缺失完整身份，保守跳过，不凭短 ID 猜测认领标签。
- 不再修复短别名碰撞、登记顺序稳定性或别名升级。FT21 已按 owner 意见移入 stop/，不继续修复或合并该分支。FT10/FT26 的独立生命周期职责不因此取消。
- 取消原短 ID 兼容、长短双路径及长短共享锁验收；替换为所有旧短别名输入均拒绝、全部定向方法可直接使用创建返回的长 ID。同后缀 A/B 仍可作为测试数据：A 消失后持 A 完整 ID 的所有操作均失败，B 不收到操作、不被关闭、不被误续期。
- 保留解析后消失、等待锁时消失、同目标并发串行、未知 ID 不积累锁、审计/续期/恢复身份一致及多后端路由的永久回归；覆盖 daemon/CLI/Client/Pool 全链路。
- 不新增所有权/租约协议，不承诺 CDP ID 跨浏览器会话永久唯一。原测试隔离、全量 gofmt/vet/race 与如实记录真实 Chrome 等未验边界的要求保持不变。

## factory · plan · 2026-11-22 10:08

## Behaviour
创建、列举、焦点及全部定向 RPC 返回/接收原样完整 CDP target ID。保持 tab/--tab 字段以降低接口噪声，不增加平行参数。未知、过期、截断、大小写变化直接失败；无显式 ID 的原有焦点选择可保持，但任何显式错误输入都不能触发 fallback。零标签仍能创建，空列表正常返回。移除全部短别名生成/查找，不修 FT21。
## Scope
修改 registry、daemon 全部调用、protocol、Client/Pool、CLI、审计/idle/recovery 和文档/测试。Pool 引用为 backendKey:完整ID，不猜后端。保留生命周期枚举代数、锁引用计数、观测清理等已有正确性。旧日志缺完整身份保守跳过。不得引入所有权或租约，不合并其他工厂分支，不发布。
## Decisions
完整 ID 视为 CDP 提供的不透明字符串，精确匹配已知目标；不以大小写或截断归一化。移除误导性的 ShortID 内部命名/短别名算法，按需要做简单直接的数据结构替换，避免双身份层。检查已合并 FT10/持久化代码实际状态，以真实完整身份贯穿成功操作、续期、审计与恢复。若历史格式无法证明完整身份，宁可跳过而非认领。以最新 main 为基线。
## Checking
加入永久回归：全部定向方法接受创建返回长 ID；旧短别名/前后缀/大小写变体拒绝；A/B 同后缀，A 消失、解析后消失、等待锁时消失不影响 B；同目标串行、未知输入锁不泄露；活动和审计身份固定，恢复不误认领旧短日志。Client/Pool 多后端和 CLI 覆盖长 ID 路由和无 fallback。保存修复前失败/修复后成功的证据。全量 gofmt/vet/go test -race。测试用隔离 HOME/TMPDIR，无外网，真实 Chrome 若测必须隔离非 headless 实例，不操作用户浏览器；未测的明确列出。

## factory · 2026-11-22 10:08
按 owner 本次明确要求，立即使用 Claude Code 构建 FT28。最新 main@7223e5a 起步，未合并 FT21 或其他任务。

实现提交：74d5be7、d86db7a、42cc475（head）。未推送前统一提交元数据为 factory 身份，避免携带本机默认身份；未改任何已发布历史。
- 移除 registry 短别名算法及映射，全链路仅使用原样完整 CDP target ID；沿用 tab/--tab 字段，无双身份 API。
- daemon 18 个定向方法固定同一完整身份，锁、CDP 操作、审计与 idle 续期贯穿；显式未知、过期、截断、大小写或空白变体均拒绝，不 fallback。
- RPC 日志使用 target_id，回放与轮转快照只接受该身份；旧格式缺可靠完整身份保守跳过并告警，不认领旧短别名标签。升级后旧受管标签须人工处理。
- Pool 使用 backendKey:完整ID，精确绑定，无后端猜测或迁移；CLI 显式空 tab 拒绝。README、AGENTS、Client/Pool 与 skills 文档已同步。
- 永久回归覆盖同后缀 A/B、失效与等待锁消失、全部定向方法、未知锁清理、续期/审计/恢复、多后端与 CLI。
- 红绿证据：基线 daemon 9 项新测试失败（如 tab_new 返回 1234），Pool 精确匹配及 CLI 空白/空值输入测试失败；修复后通过。store 新字段测试无法在旧基线编译，无单独红证据。
- manager 独立复验：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过。无新增未 pin 工具或依赖安装。
- 未验：真实 Chrome（全部 fake CDP/stub）、跨 Chrome 进程 ID 唯一性、未合并 FT10/FT26 的跨分支集成。解析后目标消失以 fake CDP 模拟错误；不承诺跨浏览器会话永不复用。
已推送代码，new/ → review/，等待独立 review；未合并 main、未发布、未启用定时任务。

## factory · review · 2026-11-22 10:15
head: 42cc475ac164a6c138777e48e1793061a3bf7e50
verdict: fix
fix:
- [P1] internal/state/tab_registry.go:123: 迟到的枚举重新登记已关闭的完整 ID。tab_list 取得含 A 的快照后，tab_close(A) 成功，再放行枚举，持 A 旧完整 ID 的 network_clear、console_clear、errors_clear 均返回成功。Codex 在同后缀双 tab 基线对照中验证三者原先均拒绝。枚举代数必须覆盖删除，过滤请求发起后已删除的目标，避免重新登记或列出过期目标。
notes:
- [P2] internal/store/store.go:341: idle 自动关闭不从恢复集合移除，完整 ID 不复用后轮转快照随历史创建次数增长。16 个目标全部自动关闭后 HEAD 留 16 条合成 tab_new，基线 1 条。确认移除后按完整身份清理恢复集合，并保持审计写入顺序。
- [P2] internal/daemon/tab_idle.go:116: 枚举后、日志恢复前目标销毁，恢复凭旧快照又将其标为受管；应将当前登记状态检查/恢复与目标移除同步。
- [P2] cmd/bb-browser/main.go:1234: CLI 裁剪不透明 ID（另见 172、1131 行），fake CDP 的 X 与带首尾空格的 X 可导致默认 close 操作错目标；应仅检查空串，原样传递。此项为合成 ID 边界，不是真实 Chrome 已验证行为。
- [P3] pkg/daemonclient/README.md:3: 迁移文档残留短 tab id 和 0:abcd 示例；skills/bb-browser-adapter/SKILL.md:127 仍传 --tab ab12，应统一为创建/列举返回的完整值。

独立审查：Codex，对比 origin/main...HEAD（main@7223e5a）。全量 gofmt/vet/go test -race 通过；前四项另以临时副本对照 HEAD 与基线复现。未新增工具或依赖；使用已安装 Go 1.27.4，测试隔离 HOME/TMPDIR/缓存，不依赖真实外网。未验证 Go 1.26、真实 Chrome 或跨 Chrome 会话 ID 唯一性。审查工作树干净，无代码变更。
round 1/3，review/ → new/，下一次 build 修复阻塞项；本次仅 review，未开始修复，未合并、发布或启用定时任务。

## factory · plan · 2026-11-22 14:11
第 2 轮仅修复最新 review 的 P1：删除必须参与枚举代数；删除前发起的迟到快照不能重新登记或列出该目标，三种 clear 操作必须拒绝其旧完整 ID，且不影响同后缀 B。保留新建/重新登记保护和后续当前枚举语义。采用有界删除记忆，避免永久无界墓碑。增加确定性 registry 与 fake CDP 屏障红绿回归，复验全量 gofmt/vet/race；P2/P3 notes 不作为本轮额外任务。

## factory · 2026-11-22 14:11
Claude Code 完成第 2 轮修复，提交 3576461（合并 origin/main、解决 11 文件冲突）及 91ee3a1（P1 修复，当前 head）。未合并其他 factory 分支。
- 登记和删除都推进代数；保留最近 1024 次删除，迟到枚举过滤请求后已删除的目标，不重新登记或返回。删除历史溢出时，旧枚举保守拒绝登记无法证明仍存活的目标；后续当前枚举/生命周期事件补充。首次登记前收到销毁也记录，重复销毁不无限占位。
- 合并 main 时保留 FT10 的 managed_tab_open/managed_tab_retire 生命周期与持久化 target 字段；完整 ID 贯穿恢复，缺完整身份的旧短 ID 日志不认领。此前本分支 target_id 字段名已改为 target，需独立 review 注意迁移兼容边界。
- 永久回归：4 种枚举入口 × tab_close/销毁事件共 8 个 daemon 子用例，三种 clear 均拒绝 A、B 缓冲和关闭状态不受影响；另有乱序枚举、重新登记、并发与删除记忆有界测试。
- 红绿：修复前 registry overlay 4 项失败，daemon 8 子用例失败（accepted the id of a closed tab）；修复后通过。2 个依赖新内部结构的测试无独立红证据。相关 race 重复 20 次通过。
- manager 独立复验：test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过，Go 1.27.4、隔离 HOME/TMPDIR/GOCACHE、GOPROXY=off、GOSUMDB=off。未更改 never 路径；提交仅含本项及 main 合并。
- 无新增未 pin 工具/依赖，go.mod/go.sum 未变。偏差：coder 早期 3 次测试在临时 HOME 未指定模块缓存，从代理下载已 pin 模块，违反本轮不访问外网要求；后续红绿、全量及 manager 复验均禁用代理，仅使用已有缓存。
- 未验：真实 Chrome（仅 fake CDP/stub）、Go 1.26、跨 Chrome 会话 ID 唯一性；超过 1024 次删除和首次登记前销毁仅 registry 层覆盖，未做 daemon 端到端。review 的 P2/P3 notes 未作为额外修复，其中 FT10 带来的退役机制未单独复验原 16 目标场景。无 UI 变更。
已推送代码，new/ → review/，round 2/3，等待独立审查；未合并到 main、未发布。


## factory · review · 2026-11-22 14:21
head: 91ee3a1bd0475f9a58e5291170169d5bf4da0736
verdict: fix
fix:
- [P1] internal/state/tab_registry.go:179: 重复删除未刷新代数，上轮 P1 仍有遗漏时序：tab_close(A) 确认后，tab_list 取得仍含 A 的快照，随后 targetDestroyed(A) 到达但已有 removedGen 记录而跳过更新；放行快照又登记并列出 A，持 A 旧完整 ID 的 network_clear、console_clear、errors_clear 全返回成功。销毁通知必须刷新最后删除代数，同时保持删除记忆有界，并新增该时序的确定性回归。基线亦能复现，不是本轮新引入，但违反本项失效身份契约。
notes:
- [P2] internal/daemon/tab_idle.go:198: 本轮新增回归：1025 次其他目标删除使历史溢出，CDP 仍返回存活受管 A，但 registry 保守过滤该行；恢复对账把“无法验证”当作“确认缺席”写入永久退役，后续新枚举和日志重放也丢失 A。删除发生在请求途中或本次枚举批量清理均可复现，HEAD 失败而 origin/main 两例通过。应区分不确定与确认消失，不确定时保留持久化成员并重试枚举。
- [P2] cmd/bb-browser/main.go:1250: 上轮 CLI 裁剪不透明 ID 问题仍在，默认焦点、open 输出及适配器新建路径应只检查空串、原样回传；合成带空白 ID 边界，维持原评级。
- [P3] pkg/daemonclient/README.md:3: 上轮短 ID 文案及 0:abcd 示例仍在，skills/bb-browser-adapter/SKILL.md:127 的 --tab ab12 也应改为创建/列举返回的完整值。

Codex 独立审查 origin/main@e63a15c...HEAD，涵盖完整长 ID 实现、第 2 轮修复及 main 合并冲突处理。全量 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 通过，Go 1.27.4。上轮原始 8 子用例在修复前 3576461 全失败、HEAD 带 race 重复 20 次全通过；额外临时诊断复现上述遗漏和新回归。FT10 集成检查：16 个受管目标全部 idle 关闭后，无残留 managed_tab_open，重放恢复数为 0，基线和 HEAD 均通过；target 字段日志恢复正常，旧 tab/首轮 target_id 日志保守跳过。上轮恢复与销毁并发 P2 未提级。
无新增工具或依赖安装；测试隔离 HOME/TMPDIR/GOCACHE，复用已有模块缓存，GOPROXY=off、GOSUMDB=off，无外网访问。未验证真实 Chrome、Go 1.26 或跨 Chrome 进程 ID 唯一性。工作树干净，无代码修改。
round 2/3，review/ → new/，下一轮修复 fix 列表；未合并 main、未发布。


## factory · plan · 2026-11-22 15:07
第 3 轮补充：仅修最新 review 的 P1。每次删除通知都刷新最后删除代数，包括已删除/未登记目标；保持 RemoveTarget 返回值及清理语义，删除记忆有界。永久回归固定 close(A) → 取得仍含 A 的枚举快照 → destroyed(A) → 放行，三种 clear 拒绝 A、同后缀 B 不受影响；覆盖重复删除、有界性、溢出与后续当前枚举。先红后绿、重复 race 及全量检查。P2/P3 notes 不扩入本轮。按 owner 明确优先 FT28 的调度覆盖选择本项。

## factory · 2026-11-22 15:07
Claude Code 完成第 3/3 轮，提交 a7bbb077d6f2987a6e7128d54bd32687d17027b2。origin/main@e63a15c 已为祖先，本轮无需合并；未合并其他 factory 分支。
- 每次 RemoveTarget 均推进删除代数；重复通知更新同一 id 的槽位并移动至队尾，不挤出其他删除记录。removedLog/removedGen 一一对应，最多 1024 个 id；溢出保守策略不变。仅修改 registry、回归、测试 fake CDP 及 AGENTS 相关说明。
- 红证据：TestRepeatedRemovalRefreshesRemovalGeneration、TestRepeatedRemovalOfNeverRegisteredTargetRefreshes、TestRepeatedRemovalStaysBounded，以及 TestEnumerationAfterCloseDoesNotReviveTabDestroyedMeanwhile 四入口（tab_list/tab_focus/tab_select_other_tab/resolveTab_fallback）在生产修复前失败，修复后通过。daemon 用例验证三种 clear 拒绝旧 A、同后缀 B 缓冲与存活状态不变。
- TestRepeatedRemovalSurvivesOverflow 和 TestEnumerationAfterCloseListsLingeringTargetUntilDestroyed 在旧代码本就通过，不算红证据。当前枚举在 close 后、destroyed 到达前仍可发现浏览器继续报告的 A，为既有行为，已明确固定；真实 Chrome 是否存在该时序未验证。
- coder 定向 race 重复 20 次通过，全量通过；manager 独立隔离 HOME/GOCACHE/TMPDIR，GOPROXY=off、GOSUMDB=off 运行 test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... 全通过（Go 1.27.4，11 个测试包、1 个无测试包）。diff --check 通过，never 路径无修改，工作树干净。红绿与全量证据保留在本机 build run evidence/FT28-round3/ 下 red.log、green.log、race-repeat.log、full.log、manager-full.log。
- 安装：无；go.mod/go.sum 未变，复用已有模块缓存，不访问真实外网。
- 未验：真实 Chrome/CDP（仅 fake CDP/stub）、Go 1.26、Docker/systemd 真实运行、跨 Chrome 会话 ID 唯一性。无 UI 变更，未启动浏览器或操作用户窗口。
- 最新 review 的 P2/P3（删除历史溢出导致不确定目标被持久退役、CLI 裁剪不透明 ID、残留文档示例）未作为本轮额外修复，仍待 owner/独立 review 注意。
状态：代码已推送，new/ → review/，round 3/3，待独立审查；本轮仅 build，未合并 main、未发布。


## factory · review · 2026-11-22 15:36
head: a7bbb077d6f2987a6e7128d54bd32687d17027b2
verdict: fix
fix:
- [P1] internal/daemon/v1.go:485: 迟到的 tab_new 回复重新登记已关闭且已处理 targetDestroyed 的 A，三种 clear 全成功，idle 跟踪及磁盘重放受管记录也复活。创建请求前捕获生命周期代数，让创建登记、受管记录与删除有一致先后顺序。HEAD/main 均复现，非本轮新引入，但违反完整身份失效契约。
- [P1] internal/state/tab_registry.go:126: close(A) 后挂起含 A/B 的旧列表，较新列表已确认仅 B，放行旧列表仍重新登记并列出 A，三种 clear 全成功。较新枚举确认的缺席必须约束更早回复，包括已注销 ID，并保留并发新建保护。HEAD/main 均复现。
notes:
- [P2] internal/daemon/browser_reconnect.go:121: 生命周期队列满触发重新枚举确认只剩 B 后，排队更早的 targetInfoChanged(A) 又登记 A，三种 clear 成功。事件排序应与对账代数一致，淘汰快照之前的陈旧事件。真实 chromedp + 回环 WebSocket stub，HEAD/main 各 3/3。
- [P2] internal/daemon/tab_idle.go:198: 保留上轮评级。1025 个其他 ID 删除导致保守过滤存活 A，恢复对账将不确定当作确认缺席并持久退役，后续枚举/重放无法恢复。HEAD 失败、main 通过；区分无法验证与明确消失。
- [P2] internal/daemon/tab_idle.go:231: 显式关闭收到明确 No target with given id found 后未清理登记/受管状态，三种 clear 仍成功。明确目标消失应清理该身份，同时保留关闭错误响应。HEAD/main 均复现。
- [P2] internal/daemon/v1.go:437: tab_select 绕过同目标锁，idle 清理已持锁进入 CloseTarget 时仍成功并刷新 idle，随后目标被关闭。选择应参与同一锁、获锁后检查存活。HEAD/main 均复现。
- [P2] cmd/bb-browser/main.go:1250: 保留上轮合成不透明字符串边界：open/默认 close 裁剪带首尾空白的完整 ID，错关 X 而非焦点的带空白 ID。第 188、1147、1250–1252 行仅检查空串并原样传递。HEAD 失败、main 通过，非真实 Chrome 验证。
- [P2] pkg/daemonclient/pool.go:337: 双后端测试中池外关闭后 Eval 正确路由原后端，但明确 unknown tab id 返回 RPCError，不能 errors.As 为要求的 UnknownTabError。应保留底层错误并提供 UnknownTabError 语义；绑定回收仍属 FT26。HEAD/main 相同。
- [P3] pkg/daemonclient/README.md:3: 残留短 tab id、0:abcd 示例，skills/bb-browser-adapter/SKILL.md:127 仍用 --tab ab12，应使用创建/列举返回的完整值。

Codex 独立只读审查 origin/main@e63a15c...HEAD 全部 47 文件；未找到可用 review-code skill，按请求与计划审查。上轮 P1 原始 close → 旧快照 → destroyed → 放行调度已修复：91ee3a1 四入口均失败，HEAD 四入口各 -race 重复 20 次通过；首轮八子用例亦各通过 20 次。上述为新增遗漏调度，不是沿用已修复 finding。
配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过，11 个测试包、1 个无测试包，Go 1.27.4。定向覆盖18方法、同后缀、等待锁消失、未知锁回收、审计/旧短日志跳过及双后端；16 个受管目标 idle 关闭、轮转后重放为0，原枚举后销毁再恢复调度通过。配置通过不覆盖新增诊断缺陷。manager 核对全量日志、诊断源码说明和 HEAD 失败日志，工作树干净，未另行重复全量。
证据保留本机 review run evidence/FT28-a7bbb07/：reproduce.txt、review_*_test.go、probe-current/main.log、queue-current/main.log、retire-current/main.log、pool-current/main.log、cli.log、cli-main.log、p1-previous.log、p1-head-counts.log、full.log。诊断为工作树外导出副本测试，未提交为永久回归。
无安装、清单或依赖变更。测试隔离 HOME/TMPDIR/缓存，复用已有模块，GOPROXY=off/GOSUMDB=off，无真实外网。未验 Go1.26、真实 Chrome、跨浏览器会话 ID 唯一性；未操作用户窗口。无链接 issue/PR，不发 GitHub 评论。
第3/3轮发现P1，已达 max_rounds，留在 review/ 等 owner 决定；如需修复请添加说明并移至 new/。未改代码、未合并 main、未发布。

## factory · 2026-11-23 12:47
owner 本次明确要求：
> FT28 移到 new 然后继续用 cc 去 build

按 owner 授权从 review/ 移回 new/，继续原分支第 4 轮（超过 max_rounds 的本次人工授权，不修改配置）。使用 Claude Code 修复最新 review 两项 P1：迟到创建回包恢复已销毁身份、较旧枚举覆盖较新枚举确认的缺席；保留完整 ID 不复用及既定任务范围。P2/P3 notes 供检查关联风险，不自动扩为本轮任务；FT7 本次不启动，不合并或发布。

## factory · plan · 2026-11-23 13:01

## Behaviour
- tab_new：创建请求发出前取生命周期代数；回包登记 A 时，若 A 在此之后已被删除（tab_close 确认 / targetDestroyed / 枚举移除），不再登记、不写受管记录、不选中，返回明确错误（tab 在创建完成前已被关闭）。登记后再写受管记录并复核登记，受管（idle + rpc.jsonl）与删除保持一致先后，不会在删除之后留下 managed_tab_open。
- 枚举：每次枚举应用都记入代数。旧回复列出但当前未登记的 id，只要其请求之后已有另一枚举应用且该枚举未列出它（确认缺席），就不登记、不列出；包括已注销、从未登记的 id。已登记目标、并发新建保护（请求后登记的目标不被旧快照删除）、后续当前枚举照常登记不变。
- 三种 clear / 其它定向 RPC 对失效完整 id 继续 unknown tab id；同后缀 B 不受影响。

## Scope
- internal/state/tab_registry.go、internal/daemon（tab_new、枚举后受管对账）、对应测试、AGENTS/README 相关说明。
- 关联修复（避免新有界记忆扩大已知 P2）：受管对账只把“浏览器回复确实未列出”的候选退役；回复列出但 registry 因历史遗忘无法验证的不退役并重试枚举。
- 不做：生命周期队列旧事件(P2)、明确 target-gone 清理(P2)、tab_select 锁(P2)、CLI/Pool/文档美化(P2/P3)、FT7。

## Decisions
- 枚举确认缺席用有界“已应用枚举”日志（相同集合合并，超出上限按不可验证保守处理），不用永久墓碑。
- 判定按“请求—应用”窗口而非令牌先后：两个交叠请求在浏览器侧的快照顺序不可知，窗口内任一应用未列出该 id 即不可信。
- 创建遇到删除历史已遗忘（不可验证）时用一次当前枚举裁决，而不是乐观登记。
- 合并 origin/main@2be49f8（仅 main），文档冲突沿用完整 id 契约。

## Checking
- 先红后绿：registry + daemon 屏障回归（迟到 tab_new 回包；较新列表确认缺席后放行旧列表），三种 clear、同后缀 B、idle 跟踪、磁盘重放。
- 有界性/溢出/并发新建保护回归。
- test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；定向 -race -count=20。
- 证据：本目录 red.log / green.log / race-repeat.log / full.log。

## factory · 2026-11-23 13:01
Claude Code 完成 owner 人工授权第4轮，提交 fcdb42f（合并 origin/main@2be49f8 并解决 AGENTS/Pool 冲突）、f1bd04507dfaa1c5d7236235eadd156bf8a03a36（修复及永久回归）。仅合并 main，未合并其他 factory 分支。
- 创建请求发出前取生命周期令牌；迟到创建回复若目标已删除，不登记、不选中、不写受管记录，返回 tab closed before tab_new completed。登记/受管记录后复核，防止与删除交错导致 idle 或磁盘重放复活。删除记忆溢出时用当前枚举裁决。
- 已应用枚举的缺席证据约束交叠请求的旧回复，含此前已注销或从未登记的身份；保留并发新建保护。记忆为最近64个不同目标集合，相邻相同集合合并，溢出保守拒绝未验证登记。代价：真实外部新 tab 可能晚一次枚举登记。
- 关联修复原P2：新增有界记忆不能扩大“不确定被持久退役”风险，生产对账以浏览器原始回复判断缺席；被列出但未验证者保留受管记录并重试。旧诊断直接调用底层 reconcileIdleFromLog 绕过生产包装仍失败；新增包装层回归通过，需 review 核查生产循环。
- 红绿：合并main后、修复前 registry 5项失败；daemon迟到创建5种删除调度、旧枚举2种前置×4入口均失败；修复后通过，三种clear拒绝A、同后缀B不受影响、idle/磁盘重放不恢复A。新API/有界性及对账测试无独立红证据；并发压力在旧代码亦通过，仅算race检查。既有 TestRepeatedRemovalStaysBounded 对调两次应用顺序以符合新缺席契约，断言保留，review 注意是否掩盖旧保证。
- coder定向 -race -count=20 通过、全量通过；manager独立 test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... 通过（Go1.27.4，11测试包、1无测试包）。diff --check通过，never路径未变；工作树干净。证据本机 build/evidence/FT28-round4/ 下 plan.md、red.log、green.log、race-repeat.log、full.log、manager-full.log。
- 安装：无；go.mod/go.sum未变。HOME/TMPDIR/GOCACHE隔离、复用模块缓存、GOPROXY=off/GOSUMDB=off，无真实外网。
- 未验：真实Chrome（仅fake CDP/stub）、Go1.26、跨浏览器会话ID唯一性；不确定对账后重试未经真实runTargetSync循环端到端验证；创建溢出裁决枚举失败分支无专门测试。双重溢出/裁决失败可能留下存活但未受管tab并返回错误，需review关注。
- 未处理P2/P3：队列旧事件重新登记、明确target-gone不清理、tab_select绕锁诊断仍失败；CLI裁剪、Pool错误类型、残留短ID示例未作为本轮修复或重新验证。无UI变更。
状态：代码已推送，new/ → review/，round 4（本次owner授权，不更改max_rounds）；待独立审查，未合并main、未发布、不启动FT7、不改变定时任务。


## factory · review · 2026-11-23 13:12
head: f1bd04507dfaa1c5d7236235eadd156bf8a03a36
verdict: fix
fix:
- [P1] internal/state/tab_registry.go:314: 迟到创建回包刷新已登记 A 的代数，覆盖实际创建之后的缺席证据：挂起 tab_new 回包→列表登记 A→A 关闭→挂起仅含 B 的新列表→放行创建→放行列表，三种 clear 仍接受 A，idle 与磁盘重放仍保留 A。另一调度先成功 tab_close(A)，再被仍含 A 的当前列表登记，第305行因此跳过已知删除证据，创建也错误成功。提交创建时先检查令牌之后的已知删除，不受当前是否登记影响；已登记目标保留原登记代数，仅首次登记推进，并保留历史溢出的当前验证及两种永久屏障回归。
notes:
- [P2] internal/daemon/tab_idle.go:289: 普通 tab_list 并发登记 A 可让生产重试因 Has(A) 提前结束，A 仍持久受管但没有 idle 跟踪。应按本次接受的 snaps 判断重试，或同步补齐跟踪。无并发列表时生产自动重试已验证成功。
- [P2] internal/daemon/tab_idle.go:67: 删除历史溢出后，创建裁决枚举被过滤或瞬时失败，留下存活但未受管的 tab。区分确认缺席和证据不足，在期限内重试；最终无法提交时有界回滚刚创建的完整 ID 并报告清理状态。
- [P2] pkg/daemonclient/pool.go:891: main 合并引入的 TrimSpace 让 TabList/TabFocus 返回带尾空格的原 ID，却绑定裁剪值；原值无法 Eval。删除观测绑定处裁剪。合成不透明 ID 边界，非真实 Chrome 已验证行为；合并前同用例通过。
- [P2] cmd/bb-browser/main.go:1250: 合成 X 与带首尾空格的 X 同时存在，open/默认 close 裁剪导致错关。第188、1147、1250–1252行仅检查空串并原样输出/回传。
- [P2] internal/daemon/browser_reconnect.go:123: 溢出重枚举确认 A 消失后，排队旧 targetInfoChanged 又登记 A；应淘汰成功快照覆盖的旧事件。main 亦复现。
- [P2] internal/daemon/tab_idle.go:315: 显式 close 收到明确 ErrorTargetGone 后仍保留登记/受管；清理该身份，同时保留错误响应。main 亦复现。
- [P2] internal/daemon/v1.go:437: idle 持锁关闭时 tab_select 仍成功并续期；应获取同一目标锁，获锁复核存活并持锁续期。main 亦复现。
- [P2] pkg/daemonclient/pool.go:444: 正确后端返回 unknown tab id 时缺 UnknownTabError 语义；callBound 两条返回路径包装明确错误并保留底层 RPCError。main 亦复现。
- [P3] pkg/daemonclient/README.md:3: 短 tab id、0:abcd 及 skills/bb-browser-adapter/SKILL.md:127 的 --tab ab12 仍残留，应改为创建/列举返回的完整值。

Codex 独立只读审查 origin/main@2be49f8...HEAD 全52文件，含 main 合并；无可用 review-code skill。上轮两个P1原始调度 HEAD 各20/20通过，修复前及main失败。本次P1是新遗漏调度，不是重复原finding：A仅创建一次，不复用完整ID，同后缀B不受影响。HEAD、修复前fcdb42f及main两种新调度各5/5失败，属于既有缺陷遗漏；manager核对源码与日志，并独立以导出副本 -race -count=3 重现两种调度全部失败。未声称是第4轮新引入。
配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过，11测试包、1无测试包，Go1.27.4；相关永久回归和原P1定向race各20轮通过，16目标idle关闭轮转重放为0。manager核对配置日志，未另行重复全量；诊断失败与配置检查分开记录，不能以配置通过宣告安全。
证据保留本机 review/evidence/FT28-f1bd045/：REVIEW.md、reproduce.md、probes/、configured.log、original-head-20.log、round4-count20.log、refresh-head/pre/main.log、linger-head/pre/main.log、r4-head/pre.log、pool-premerge.log、manager-diagnostics.log。临时诊断尚非永久回归；工作树干净，无代码变更、提交或安装，go.mod/go.sum及保护路径未变。HOME/TMPDIR/缓存隔离，复用固定模块，GOPROXY=off/GOSUMDB=off，仅fake CDP/本地回环stub。
未验真实非headless Chrome、Go1.26、部署或跨浏览器重启；未启动浏览器或操作用户窗口。无链接issue/PR，不发GitHub评论。
round=4，已超过max_rounds=3；owner只授权该轮build，不自动追加修复。发现P1，留在review/等owner决定；需继续请添加说明并移至new/。本次仅review，未改代码、未合并main、未发布。


## factory · 2026-11-23 13:30
owner 最新明确要求：
> 把 ft28 rebase 一下 main 然后推倒从头开始，不需要兼容短 id

本次授权：review/ → new/，继续使用 Claude Code，按重做而非第5轮局部补丁执行。保留历史审查与有效回归，不重置 round，不改 max_rounds。按工厂禁止已发布历史重写的约束，以合并最新 origin/main 同步基线，不执行 rebase/force-push；代码设计以当前 main 为起点重新审视，不能沿用叠加补丁作为默认方案。FT7 不动，不合并 main、不发布、不修改定时任务。

## factory · plan · 2026-11-23 13:30
Behaviour：所有层仅接收/输出完整 CDP target ID，原样精确匹配；旧短 ID、截断、大小写/空白变体无匹配就报错，不 fallback。创建、枚举、生命周期事件、关闭和受管持久化采用清楚一致的先后关系，迟到回复/事件不能复活确认失效身份；并发新建不得被旧枚举误删。创建未能提交时明确错误并尽可能有界回滚，不留无声受管/idle 分裂。
Scope：从 main 重新审视 registry 与 daemon 身份生命周期实现及 protocol/Client/Pool/CLI/持久化调用链。删除短 ID 兼容路径与无必要的历史补丁结构；保留 main 已合并功能和有效回归，相关生命周期 P2 与文档残留本次纳入整体重做，不再只修最新 P1。不要合并 FT7/FT21 或其他 factory 分支，不开展 writer 退役重设计。
Decisions：先书面明确系统边界、登记/失效权威、异步结果顺序和有界内存回收规则，再实施简单直接的模型。不要用任意大小历史环与多层 fallback 叠加掩盖时序；若需限制/重试，说明可证明的不变量、失败与回收路径。完整 ID 不承诺跨浏览器会话唯一，不新增所有权/租约协议。保留 tab/--tab 承载完整 ID，避免平行 API。
Checking：将第4轮审查两种迟到创建调度纳入永久屏障回归，并保留历轮有效语义（测试可随内部模型重写，不得仅删断言）。覆盖乱序枚举、排队旧事件、关闭/销毁、明确 target-gone、同目标 select/idle 互斥、创建失败回滚、持久化/idle 一致、内存有界、多后端错误与原样身份。同步 README/AGENTS/Client/skills 文档清除短 ID 示例。隔离 HOME/TMPDIR/缓存，GOPROXY=off/GOSUMDB=off，不访问真实外网；全量 gofmt/vet/race 及关键调度 race 重复20次，保留红绿证据；未测真实 Chrome 如实记录。


## factory · 2026-11-23 13:54
owner 工作期间再次确认：
> 对，从 main 直接重做，使用完整 target id，不兼容短 id

Claude Code 完成本次整体重做（round 5，人工授权，不修改配置）。代码提交 0655a08、dab7d41（head）；origin/main@2be49f8 已是分支祖先，无须新增合并。保留已发布历史，未 rebase/force-push；不是清空分支历史后从 main 新建提交，而是重新实现核心并复用仍有效的完整 ID 调用链与回归。
- registry 改为在途请求窗口携带失效/交叠枚举证据，请求结束释放；删除原1024删除历史环、64快照历史环和不可验证裁决路径。已有登记不被迟到创建/事件刷新代数，创建提交先检查窗口内失效，与当前是否登记无关。
- lifeMu 串行登记/失效及缓冲、idle、同步受管记录的应用，不跨 CDP 往返；事件在连接读回调盖章，旧队列证据不得覆盖较新枚举/关闭，改触发重枚举。恢复采用本进程尚未恢复的持久化差集。
- 完整 target ID 贯穿 daemon/Client/Pool/CLI，移除短ID兼容；Pool/CLI去掉身份裁剪，未知后端目标提供 UnknownTabError并保留RPCError。明确target-gone清理，select参与同目标锁，创建提交失败有一次5秒有界回滚，错误报告清理结果。README/AGENTS/Client/skills文档清理旧短ID示例。
- 红绿：新永久屏障/调用链回归放到旧f1bd045导出副本共10项失败，重做后通过，含最新P1两调度、回滚、旧队列事件、select/idle、target-gone、Pool原样ID/错误类型和CLI。新registry/Session/recovery API测试无独立红证据；窗口释放/16目标轮转旧版已通过，不算修复证据。
- CC关键调度 -race -count=20通过、全量gofmt/vet/race通过。manager 独立隔离HOME/GOCACHE/TMPDIR，GOPROXY=off/GOSUMDB=off，执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... 全通过（Go1.27.4，11测试包、1无测试包）；diff --check通过、工作树干净、never路径及go.mod/go.sum未变。无安装。证据在build/evidence/FT28-rebuild/（design.md、red/green/race-repeat/full/manager-full.log）。
- 需独立review重点核查：旧“重复登记刷新代数”断言反转为不刷新；旧token复用改一次性窗口；有界历史测试改窗口生灭，须审查语义而非只看通过。原临时探针verification_filtered依赖已删除裁决流程而超时，concurrent_list_true依赖已删除溢出过滤前提未通过原写法，coder声称由新模型回归覆盖，仍需审查，不视为全部旧诊断原样通过。
- 设计边界：窗口证据内存随在途请求数及期间失效数增长，靠操作期限释放，非固定容量硬上限；事件顺序依赖chromedp连接监听回调顺序，尚未真实Chrome验证。关闭后才发起的新枚举若浏览器仍列旧target，仍可暂时再登记直至destroyed（保留旧行为，review核对契约）。创建请求失败/超时拿不到ID无法回滚，可能留下未受管tab；回滚失败仅报告不重试。交叠证据保守拒绝时存活外部tab可能晚一次枚举登记。
- 未验真实Chrome、Go1.26、部署、跨浏览器会话ID唯一性；只有fake CDP/本地回环stub。无UI变更，未启动浏览器或操作用户窗口。
已推送代码，new/ → review/，等待独立审查，不能据测试通过视为验收完成。未合并main、未发布、不启动FT7、不修改定时任务。


## factory · review · 2026-11-23 14:13
head: dab7d41a67aaddb4192d7e4abe02208a4e56ff4d
verdict: fix
fix:
- [P1] internal/state/tab_registry.go:319: 生命周期队列满时 A 的创建/销毁通知被丢弃，补偿枚举已确认 A 缺席，但迟到 tab_new 仍成功，三种 clear 接受死 A，idle 与磁盘重放也保留 A。让读到的销毁证据约束在途创建，或对溢出影响的创建在提交前有界核验；保留正常并发创建不受早期空快照删除的保证。
- [P1] internal/state/tab_registry.go:189: 普通旧列表先拍到 A，A 已销毁且通知因队列满丢弃，补偿枚举只含 B；先应用旧列表再应用补偿枚举，gen > w.gen 将旧列表发现误当并发新增保护，三种 clear 仍接受死 A。区分真实创建和交叠列表发现，对矛盾证据暂停解析并用冲突后新枚举确认，保留并发新建保护。
notes:
- [P3] AGENTS.md:33: 文档称关闭后只能新枚举重新登记，实际新读到的 page info 事件也可登记仍存活的 A；统一失效时点与两类新证据说明。该滞留边界无错目标或受管身份恢复证据。
- [P3] internal/daemon/v1.go:255: 合成纯空白完整 ID 可由 tab_new 返回，却被 clear/close 和 CLI TrimSpace 判空拒绝；仅拒绝空串并逐字节查找。非真实 Chrome 已观察值，前一 head 已有此限制。

Codex 独立只读审查 origin/main@2be49f8...HEAD 全56文件，无可用 review-code skill。两个P1均以真实chromedp连接、生产同步循环和本地CDP stub确定性复现：HEAD各20/20失败，f1bd045与main各5/5失败，属于既有缺陷遗漏，不称本轮新增回归。A完整ID未复用、浏览器已实际移除A、同后缀B存活，不依赖关闭后浏览器仍列A假设。manager核对探针源码、配置日志，并在独立导出副本-race -count=3复现两项全部失败。诊断尚非永久回归。
上轮两项P1原始调度HEAD各20/20通过，更早销毁后创建及较新缺席先应用亦各20/20通过。独立检查移除历史环后的外部语义：1025次无关删除、并发列表、瞬时枚举失败后存活受管A仍恢复idle/重放；旧排队事件、target-gone、select/idle互斥、有界回滚、16目标idle关闭后轮转、18定向方法、Pool与CLI对应永久回归通过。
配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过，11测试包、1无测试包；关键永久回归与补充race各20轮通过。manager核对全量日志，未另行重复全量；配置通过不覆盖新增诊断缺陷。
证据保留本机review/evidence/FT28-dab7d41/：REVIEW.md、reproduce.md、probes/、configured.log、overflow-head/pre/main.log、list-head/pre/main.log、original-head.log、permanent-20.log、supplement-20.log、boundaries.log、manager-diagnostics.log。工作树干净，无代码修改、提交、安装、依赖清单变化。Go1.27.4，隔离HOME/TMPDIR/缓存，复用已有固定模块，GOPROXY=off/GOSUMDB=off，无真实外网；未验真实非headless Chrome、Go1.26、线上频率、部署或跨浏览器会话ID唯一性，未启动浏览器或操作用户窗口。不扩张FT7 writer重设计，无链接issue/PR不发GitHub评论。
round=5，超过max_rounds=3；owner授权本轮整体重做，不自动追加修复。发现P1，留在review/等待owner决定；如需继续请追加说明并移至new/。本次仅review，未改代码、未合并main、未发布、未修改定时任务。


## factory · 2026-11-23 14:46
owner 本次明确要求：
> 移到 new 然后让 cc 继续 build

本次人工授权 review/ → new/，使用 Claude Code 继续原分支第6轮，超过 max_rounds 但不修改配置。修复最新 review 两项 P1（队列溢出丢失销毁通知后的迟到创建、旧枚举发现误当并发创建保护），保留完整 target ID、不兼容短 ID 的契约及历轮有效回归。FT7 不动，不合并 main、不发布、不修改定时任务。

## factory · plan · 2026-11-23 14:46
在途创建须受已读到的销毁证据或溢出后有界核验约束；区分真实创建与交叠枚举发现，矛盾快照不能把已销毁目标保持可解析，需冲突后新证据确认。保持正常并发创建不被早期空快照误删、内存随请求结束释放、不回归 idle/持久化一致性。将最新 review 的生产同步循环与本地 CDP stub 两项诊断提升为永久确定性回归，保存修复前失败/修复后通过证据，关键 race 重复20次及全量 gofmt/vet/race。关联 P3 文档/纯空白 ID 可一并澄清，不扩张其他任务。


## factory · 2026-11-23 15:02
Claude Code 完成 owner 人工授权第6轮，提交 39f1a6151636bfc64ba91dd4061e11f5e2f24af7。origin/main@2be49f8 已为祖先，无额外合并，未重写历史。
- 两项P1：连接读线程收到destroyed即向所有在途窗口记录失效，事件是否成功入队均如此；丢弃时也约束更早排队的exists事件。登记区分真实创建/事件与枚举发现，交叠枚举冲突的发现不再享受并发新建保护，暂停解析并请求冲突后新枚举；Contested不当作已确认销毁，保留在途真实创建提交。
- 两条P3同时处理：daemon/CLI仅空串算缺失，纯空白完整ID逐字节传递；文档澄清关闭后发起的新枚举或之后读到的page事件都可重新登记浏览器仍报告的目标。无短ID兼容路径。
- 永久回归：真实chromedp读取器+生产同步循环+本地CDP stub两项溢出调度，生产读线程接线、存活目标冲突后恢复、纯空白ID，以及registry/Session单元与并发检查。旧“较新枚举发现享受保护”的断言改为冲突暂停解析，CLI纯空白本地拒绝改为原样转发，独立review须核对语义。
- 红绿：dab7d41导出副本中两个新永久P1测试、两个原review探针及纯空白探针各5/5失败，CLI失败；修复后7个review探针文件原样运行，12个TestReview各20/20通过。新API单元测试无独立红证据；红测有不提交shim以编译新文件。CC执行state/browser/daemon整包race重复20次通过，全量gofmt/vet/race通过。
- manager独立执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... 全通过（Go1.27.4，11测试包、1无测试包）；diff --check通过，never路径及go.mod/go.sum未变，工作树干净。证据本机build/evidence/FT28-round6/：design.md、red/green/race-repeat/full/manager-full.log、原review探针及复现说明。
- 安装：无。HOME/TMPDIR/缓存隔离，复用已有模块，GOPROXY=off/GOSUMDB=off，无真实外网，仅回环stub。
- 未验：真实Chrome、Go1.26、部署、跨浏览器会话ID唯一性。仍依赖destroyed通知与chromedp连接顺序；非page info事件未在读时记录删除。Contested存活外部tab可能短暂不可解析并丢失新观测缓冲，约1秒重枚举恢复；已登记目标在销毁通知丢弃到补偿枚举前仍短暂可解析。创建超时无ID不能回滚、回滚失败不重试沿用既有边界。无UI变更、未操作用户窗口。
状态：代码已推送，new/ → review/，round=6（本次owner授权，不改max_rounds），等待独立review，不以测试通过视为验收。未合并main、未发布、不启动FT7、不改定时任务。


## factory · review · 2026-11-23 15:27
head: 39f1a6151636bfc64ba91dd4061e11f5e2f24af7
verdict: fix
fix:
- [P1] internal/daemon/tab_idle.go:186: 两次只读列表冲突可关闭存活用户标签。旧列表只含B，新列表发现A且eval已附着，旧回复将A标Contested后调用ForgetTarget，chromedp取消子上下文实际发出closeTarget(A)。争议仅暂停解析并重枚举，确认消失前不得执行会关闭页面的释放；提前解绑必须使用不关页的detach，增加已eval存活页的线上零close断言。第6轮新增，前版/main通过。
- [P1] internal/state/tab_registry.go:233: 争议暂停解析丢失A已被发现的证据；冲突后重新请求的独立列表明确确认A缺席，迟到tab_new仍在375–376行登记死A，三种clear成功且idle/磁盘重放保留A。在途创建窗口保留发现及因果先后，让发现后新请求确认缺席能拒绝迟到提交并有界回滚，交叠歧义仍保护真实创建。相对前版回归，main同样失败。
- [P1] internal/browser/session.go:382: 队列满先发布dirty再记录destroyed；同步循环可先消费dirty并开启补偿窗口，创建在失效证据记账前提交。唯一补偿回复将死A当并发新建保留，三种clear/idle/磁盘重放仍接受。先记录失效并约束旧事件，再发布dirty，保证每次丢弃留下后续核验；增加发布边界屏障。底层契约缺口main及前版已有，不称新增。
- [P1] internal/state/tab_registry.go:449: 请求前已读入队的targetCreated(A)晚处理获得较新登记代数，压过因果更晚的缺席列表，完成后clear仍接受死A。事件读取与枚举开始须有可比较顺序，早已读到的存在证据必须服从随后请求的确认缺席，同时保护真正晚于请求的新建；当前只读时钟可能不变，不能仅调整赋值。HEAD/前版/main均复现。
notes:
- [P2] internal/state/tab_registry.go:247: 两份矛盾快照反向应用时，vouchesFor拒绝存活A却不报告Contested/重枚举需求；生产循环两个tick后仍无A且未发新枚举。对因冲突跳过的行同样返回重试信号，两个应用方向都须自动恢复；前版同样失败、main通过。

Codex独立只读审查origin/main@2be49f8...HEAD全59文件，重点核查第6轮15文件；无可用review-code skill。第5轮两个原始P1调度前版各20/20失败、HEAD各20/20通过，接入生产读取器的变体亦通过；本次为相邻新调度，不重复已修finding。五项诊断HEAD各-race-count=20失败；前版及main对照如上，完整ID从未复用、同后缀B保持存活。首项回环stub确实收到closeTarget(A)，不是仅根据registry推断关页。manager核对源码/全量日志，并独立导出HEAD、-race-count=3重跑五项全部失败。
配置 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 全通过，11测试包、1无测试包；关键身份/锁/18方法/Pool/CLI/恢复/16目标idle轮转及窗口释放等永久回归各20轮通过。配置通过不覆盖诊断失败，manager未另行重复全量。工作树干净，无代码修改、提交、安装或依赖清单变化，保护路径及go.mod/go.sum相对base未变。
证据保留本机review/evidence/FT28-39f1a61/：REVIEW.md、reproduce.md、sources/、counts.txt、configured.log、diagnostics-head/pre/base-20.log、original-pre-20.log、key-head-20.log、manager-diagnostics.log。诊断在工作树外导出副本执行，尚非永久回归。Go1.27.4，隔离HOME/TMPDIR/缓存，复用固定模块，GOPROXY=off/GOSUMDB=off，仅fake CDP/本地回环stub；未验真实Chrome、Go1.26、部署、线上频率或跨浏览器ID唯一性，未操作用户窗口，不扩张FT7。无链接issue/PR，不发GitHub评论。
round=6，超过max_rounds=3，owner仅授权本次build；发现P1，留在review/等待owner决定，不自动追加修复。若继续请添加说明并移至new/。未改代码、未合并main、未发布、未改定时任务。


## factory · 2026-11-23 17:32
owner 本次明确要求：
> 补充到 ft28 然后再让 cc build 一轮

追加真实 Chrome 核验结论（39f1a61；完整核验报告见 .factory/reports/FT28-real-chrome-20261123-cc.md）：
- P1-1：完整生产 ListenAndServe + browser.Connect + runTargetSync + watchdog + HTTP /v1，真实 Chrome 各5/5。只延迟一条旧 getTargets 回复，并对新 A 做 eval 附着，即会在争议清理时发出 Target.closeTarget(A)，A 消失、B 存活；无需第二次 tab_list。未附着对照不关页。
- P1-2：完整生产路径5/5。立即的死 A 登记/idle/磁盘复活约一个同步tick可自愈；若迟到创建回复晚于冲突触发的自动重枚举，则2.2秒无RPC后仍保留死 A，三种clear成功。生产连接不收destroyed，不扣事件亦复现。须覆盖持久时序，不把短暂自愈视为修复。
- 原review P1-3/P1-4/P2 的代码问题在额外开启browser级discovery后各5/5，但原样生产连接未送root级setDiscoverTargets，35次无root生命周期通知。不能把测试额外注入的discovery说成既有生产可达。其他Chrome版本/CDP宿主未验证，结论不泛化。
- 新发现生产事件接线缺口：无任何时序注入时，手动关闭daemon创建的标签2.3秒后registry/idle仍有A、clear成功，直到显式tab_list才清理。页面session的生命周期事件未进入ListenBrowser；先前依赖读线程destroyed的修复在真实Chrome生产连接上不生效。本轮纳入修复与永久验收。
- 上述竞争是“真实Chrome + 受控时序”，不是自然概率。历史CC报告使用headless，未满足factory非headless规则；本轮真实验收必须隔离非headless Chrome、CDP仅127.0.0.1、临时profile，不碰用户会话。零页面启动先前SKIP，不能宣称已覆盖。

按owner授权review/ → new/，继续原分支第7轮；超过max_rounds为本次人工授权，不改配置。修复最新review四项P1及关联P2，并补齐生产事件接线。保持完整target ID、不兼容短ID及有效回归。不得合并其他factory分支、不得合并main/发布、不得启动FT7或改定时任务。

## factory · plan · 2026-11-23 17:32
Behaviour：只读冲突仅暂停解析并触发新核验，不得关闭存活页面；缺席证据约束迟到创建，registry/idle/磁盘一致；生产browser级生命周期事件确实进入消费者，用户关闭后无需额外RPC即失效。事件读取与枚举开始使用可比较的因果顺序，先登记销毁约束再发布dirty；矛盾快照两种应用方向均请求自动恢复。
Scope：registry、Session的CDP连接/事件路由与非关页资源清理、daemon同步与相应测试/文档；修复新发现事件接线后，原来需额外discovery的时序也必须按真实生产路径重新验收。保持清楚的控制流、资源所有权及失败语义，不叠加任意历史缓存或猜测fallback。
Decisions：以生产实际发送/接收的CDP消息证明接线，不以stub成功替代；必要的discovery由生产连接明确启用，测试代理不得偷偷注入。沿用原分支，不重写已发布历史；若main前移仅合并origin/main。
Checking：最新五项原诊断提升永久回归；保留历轮有效测试。前两项与无时序干预关闭清理需完整生产daemon + 非headless真实Chrome，5轮，记录线上closeTarget零断言/死A拒绝/B存活/idle及磁盘；接线改变后复验dirty/迟到事件/反向冲突，无额外discovery注入。关键确定性race重复20次，全量gofmt/vet/race；保留红绿与原始CDP证据，未覆盖或自然概率如实记录。测试HOME/TMPDIR/缓存隔离在run目录，不依赖真实外网。


## factory · 2026-11-23 17:51
第7轮已启动但因Claude服务端连续529 Overloaded、重试500 Internal server error中断。CC已写设计并修改5个生产文件，尚未提交；未完成永久回归、红绿、全量或真实Chrome验收，不算build完成，不推送代码、不移动review/，round仍为6。未完成改动保留在build专用原分支worktree供后续继续；设计保留build/evidence/FT28-round7/design.md。已停止本轮，不自动增加修复轮或切换coder；无新增安装，没有本轮Chrome残留。真实Chrome核验补充与本轮计划已推送；main、FT7、定时任务均未改变。
