---
branch: factory/FT28-daemon-full-target-id
round: 2
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
