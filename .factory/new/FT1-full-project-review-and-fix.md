---
branch: factory/FT1-full-project-review-and-fix
round: 2
---
# 全项目 review 并修复发现的问题

## Guoyin · 2026-11-20 22:57
工厂任务：对整个项目做一下 review 并修复发现的问题

## Guoyin · 2026-11-20 23:02
移到 new

## factory · plan · 2026-11-20 23:16
Behaviour：按 CLI → HTTP/RPC → Chrome CDP、状态/观测、审计恢复、公开 client/pool 检查全库；覆盖正常、空输入/零 tab、错误、取消/超时、并发及关闭/首次启动边界；保持既有协议与 INV-1～INV-7，不静默回退或泄漏凭证。

Scope：修复有代码证据或可复现测试的具体缺陷，增加回归测试，必要时同步文档；不做纯风格重构、新功能或推测性架构调整，不部署，不修改禁止路径。

Decisions：请求未限定子系统，采用全库风险优先检查，独立缺陷独立提交。测试使用本地 fake/注入/httptest，不依赖真实外网；真实 Chrome 不能在不打开用户窗口的条件下验证时，明确列为未检查。

Checking：修复配套回归测试；执行 gofmt 检查、go vet ./...、go test -race ./...，关键并发用定向重复测试；记录覆盖范围及真实 Chrome/UI 未验证项。

## factory · 2026-11-20 23:16
已构建并推送，new/ → review/，第 1 轮。
branch: factory/FT1-full-project-review-and-fix
head: 58c80bf
起点：origin/main d55fbc5；7 个主题提交：
- c19a98c：避免页面 JS 异常文本误判为 CDP 断连而退出 daemon。
- 0fdb381：受管 tab 非 RPC 关闭或恢复时确认已消失后，补记合成 tab_close，避免日志永久残留。
- 6684279：通过注册 epoch 保留枚举开始后并发创建的 tab，防止过期枚举丢失受管状态。
- 7ec9500：run 允许 adapter 自定义参数，并保留全局参数处理。
- da714ec：snapshot 编号前清理旧 ref，避免重复快照点击错误节点。
- 2eba435：click/fill ref 同时转义反斜杠与引号。
- 58c80bf：systemd 示例增加 Install 段，支持文档中的 enable。

验证：coder 报告前 3 项及后续代码修复有回归测试；并发定向 -race -count=30 通过。manager 独立运行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，全部通过。禁止路径未改，工作树干净。沿用已安装 Go 1.27.4、现有 node；无新增工具安装，无依赖清单变更，仅恢复锁定依赖。临时 HOME/cache 位于本 run 临时目录。

审查覆盖：全部主要 Go 运行时子系统、CLI、协议、client/pool，补查平台 launch、Dockerfile、deploy 与 bb-browser skill。bb-browser-adapter 文档只核对命令行示例，JS 写法未逐段审读；不是完整安全审计结论。

未检查：未启动真实 Chrome，CDP 附着、截图、观测、拦截及 snapshot 的真实浏览器/UI 行为未验证；snapshot 行为使用 node 最小 fake DOM，ref 选择器未用真实 CSS 引擎验证；本机无 systemd，unit 未实测。Docker 命名卷权限疑点未经证实，未修改。

留给 review 重点：合成关闭记录和 RPC 审计排序、epoch 同步边界、run 全局选项与 adapter 参数冲突、snapshot ref 清理。已知未解决风险：日志只存短 id，停机期间短 id 碰撞仍可能误收养；日志半行写入及轮转重开失败处理；Pool 映射清理；JPEG 默认后缀仍是 png。本轮不宣称已消除所有问题。

## factory · review · 2026-11-20 23:35
head: 58c80bffe320d1b5e88933947c4c3c1ed5d9caab
verdict: fix
fix:
- [P1] internal/daemon/tab_idle.go:52: 合成 tab_close 与 tab_new 仍会乱序：target 销毁可先于创建响应审计，队列满时直接落盘也会越过已入队记录。已复现 tab_close → tab_new，重启后错误收养同短 ID 的用户 tab。须在注册/释放同一串行流程确定审计顺序，并让满队列维持 FIFO，补充两条回归测试。
- [P1] internal/daemon/browser_reconnect.go:128: 运行期重同步在合成关闭记录尚未落盘时重放旧日志，可将同短 ID 的用户 tab 错误纳入管理；已复现落盘后磁盘集合为空而内存仍管理用户 tab，随后 idle 清理关闭它。将日志恢复限于启动后的首次成功枚举，运行期按完整 target ID 更新状态，并添加异步落盘交错回归测试。
notes:
- [P2] internal/daemon/tab_idle.go:79: epoch 只保护注册表；注册表同步与 SyncPresent 之间新建的 tab 会失去 idle 记录。只清理枚举确认移除的完整 target ID，覆盖该交错。
- [P2] internal/state/tab_registry.go:113: epoch 保留的 tab 未进入返回快照，focus 却读取当前注册表；已复现 tab_list 返回 tabs=[]、focus=1234。tabs/tab/focus 应来自同一快照。
- [P2] internal/daemon/tab_idle.go:187: targetDestroyed 在显式 CloseTarget 返回前到达，一次成功 RPC 写两条关闭记录。CDP 调用前标记正在关闭的 target，统一决定唯一记录并覆盖失败分支。
- [P2] internal/browser/snapshot.go:109: 清理旧 ref 早于解析 selector_scope，上次快照的 CSS selector 作为范围时返回空 refs。先解析并保存根节点，再清理和编号。
- [P2] internal/browser/snapshot.go:108: 脱离文档的旧标记节点不会被清理，插回后与新 ref 重复。跟踪上一轮标记节点并直接清理，覆盖脱离/插回场景。
- [P2] cmd/bb-browser/main.go:1036: run adapter.js --help=true 从 base 的仅打印帮助退化为实际 eval 执行脚本。统一处理帮助布尔选项并断言请求帮助时零 RPC。
- [P3] cmd/bb-browser/main.go:956: run -- --foo.js 在 base 可用，在 HEAD 被误判选项；保留 -- 消歧边界。

验证：Codex 独立运行配置中的 gofmt 检查、go vet ./...、go test -race ./...，全部通过；四组相关回归测试 -race -count=30 通过。额外只读 overlay 并发测试复现上述两项 P1 及状态交错问题；manager 核对了并发复现日志与相关实现。工作树干净，HEAD 未变，禁止路径未改。
安装：复用已有 Go 1.27.4，仅恢复锁定 Go 依赖，无新增工具安装。
局限/执行偏差：coder 使用隔离的 headless Chrome、本地页面和回环服务验证了 DOM/ref、daemon 错误与 CLI 行为；这不符合配置要求的非 headless Chrome，因此不将其视为符合要求的可视浏览器验收。未验证非 headless UI、systemd 实际运行、Docker、截图、网络拦截和跨平台启动。并发 P1 复现不依赖 Chrome。复现源码和日志保留在本 review run 的 evidence/FT1-58c80bf/ 下，临时目录按流程清理。

第 1/3 轮存在 P1，review/ → new/；下一轮构建修复 fix 列表。无关联 issue/PR，未发 GitHub 评论。

## factory · plan · 2026-11-20 23:56
第 2 轮补充计划。
Behaviour：创建/销毁/响应审计并发、满 audit 队列、初次枚举失败重试和运行期 resync 时，保持日志顺序，禁止误收养用户 tab；空 tabs、错误路径维持协议。
Scope：最新 review 两项 P1 及直接相关 lifecycle/state 交错和回归测试，不扩展到无关重构。
Decisions：统一 managed lifecycle 串行边界和 FIFO；运行期不重放旧日志；锁不跨 CDP 调用，满队列施加保序背压。
Checking：fake/barrier 测试覆盖审计乱序、异步落盘 resync、首次恢复、关闭成功失败及快照一致性，定向 -race 重复测试并执行配置完整测试。

## factory · 2026-11-20 23:56
第 2/3 轮已构建并推送，new/ → review/。
branch: factory/FT1-full-project-review-and-fix
head: 30ff503
本轮两个主题提交：
- 7d767c8：lifeMu 串行管理创建/释放与审计入队；满队列按 FIFO 排空；恢复只在首次成功枚举读取启动快照一次，运行期按完整 target ID 清理；覆盖初次失败重试和恢复前短 ID 释放。直接相关 P2：仅清理枚举确认移除的 target，显式关闭提前标记 closing，并覆盖成功/失败唯一关闭记录。
- 30ff503：epoch 保留 tab 纳入枚举返回快照，修复 tabs/tab/focus 不一致。
README、AGENTS 同步恢复和 FIFO 说明。禁止路径未改，manager 核对提交范围和干净工作树后推送。

验证：Claude Code 报告完整 gofmt/vet/race 测试通过；daemon 生命周期/审计/idle/关停定向 -race -count=30、state/store 全包 -race -count=30 通过；6 种旧行为变异验证对应测试失败。manager 独立执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，全部通过。
安装：无新增工具，无 manifest/lockfile 变更；复用 Go 1.27.4，仅恢复锁定依赖。测试 HOME/cache/临时文件均位于本 run TMPDIR。

留给 review：满队列入队方可在 lifeMu 下同步落盘，writer 不取 lifeMu；应继续核对锁序、背压和关停。显式 tab_close 的 RPC 审计仍在响应后入队，同短 ID 的新 tab_new 若抢先入队，重启可能少恢复该 managed tab；coder 明确保留该风险。仅短 ID 持久化导致的停机期间碰撞风险未变。最新 review 中两项 snapshot P2、run --help=true P2、run -- --foo.js P3 未在本轮修复。
未检查：未启动真实 Chrome，真实 CDP 事件/响应顺序与 runTargetSync 实际事件流未验；connectBrowserLocked/runTargetSync 循环无直接单测，仅覆盖所调用同步入口。日志轮转与新 audit 队列组合未新增专测；未验 UI、systemd、Docker 或跨平台。无关联 issue/PR。

## factory · review · 2026-11-21 00:10
head: 30ff5033ceff3110b7275e5c99963eb69c075864
verdict: fix
fix:
- [P1] internal/daemon/tab_idle.go:115: 启动持 tabMu 写锁执行初始枚举；旧枚举漏掉同步循环刚注册的 target，清理时 tabConn 再取读锁，HTTP 监听前自锁。缩小写锁范围，在锁外枚举/清理，并让初始枚举带开始时 epoch；用真实 ListenAndServe + 回环假 CDP 覆盖。
- [P1] internal/daemon/tab_idle.go:317: idle 过期判断使用完整 target ID，实际关闭却重新查短 ID；期间销毁和短 ID 复用会直接关闭用户页。把已核定的完整 tid 传给 CloseTarget，状态清理同样按该 tid，补充 barrier 回归。
- [P1] internal/daemon/tab_idle.go:293: 销毁流程持 lifeMu 移除映射后，sweeper 在锁外 Forget，抢先移除 managed 状态，使销毁流程跳过合成 tab_close；重启错误收养同短 ID 用户页。取消管理和记账必须由 lifeMu 内释放路径独占；sweeper 无映射时不得提前 Forget。
- [P1] internal/daemon/v1.go:486: targetCreated/targetDestroyed 都先于创建 RPC 返回时，回包处理重新注册已销毁 target，仅留下 tab_new，重启收养并 idle 关闭同短 ID 用户页。跟踪创建请求在途期间观察到的完整 target 销毁信息，注册前核对并完整记账，补交错测试。
- [P1] internal/daemon/server.go:231: 关停没有等待所有审计生产者；idle CDP 关闭成功但尚未记账时服务已返回并关闭 store，随后合成记录写入失败，重启错误收养用户页。所有退出路径先取消并等待 sweeper/target 同步等生产者，再排空 writer 和关闭 store，覆盖在途关闭。
- [P1] internal/daemon/tab_idle.go:166: 仅短 ID 持久化恢复会误收养用户页；已复现重启前后同两页均未变化，仅枚举顺序改变就把用户页从 01234 分配为 1234，随后 idle 关闭用户页。创建审计与轮转快照持久化完整 target ID 并据此恢复；旧格式有歧义时不得自动收养，补重启排序和兼容回归。
notes:
- [P2] internal/daemon/v1.go:204: 本轮先 append 后检查容量，阻塞 writer 时容量 2 实际 pending 32；FIFO 成立但容量无界。取得容量后再追加或先保序排空再重试。
- [P2] internal/daemon/v1.go:653: 显式关闭仍在响应后审计，短 ID 复用时 tab_new、tab_new、tab_close 导致重启漏恢复新 managed tab。短 ID 可复用前确定并排入旧 target 唯一关闭记录。
- [P2] internal/daemon/v1.go:295: 快照生成后、旧资源清理期间新建并选中 target，tab_list 仍可返回 tabs=[]、focus=1234。列表和选中项应来自同一快照。
- [P2] internal/state/tab_registry.go:108: epoch 只保护新增，已关闭 target 可被旧枚举重新注册；console_clear 对失效短 ID 返回成功。记录删除版本，拒绝过期枚举复活 target。
- [P2] internal/browser/snapshot.go:109: 上次 ref selector 用作 scope 时，提前清理属性导致空 refs；先保存根节点。
- [P2] internal/browser/snapshot.go:108: 脱离文档的旧标记未清理，插回后 ref 重复；跟踪并清除上一轮标记节点。
- [P2] cmd/bb-browser/main.go:1036: run adapter.js --help=true 实际发出 eval；统一布尔帮助选项并确保零 RPC。
- [P3] cmd/bb-browser/main.go:956: run -- --foo.js 错误拒绝存在的脚本；保留 -- 的选项终止边界。

前轮两个 P1 的原始复现路径已修复，以上为新增证实的其他交错或既有未解决路径；本轮新增队列容量问题为 P2，不将已知 P2 简单升级。短 ID 恢复风险本次已在浏览器 target 集合完全不变的重启场景证实，不只是停机期间变化的推测。

验证：Codex 独立审查 origin/main...HEAD 全部 9 个提交、25 个文件；配置 gofmt 检查、go vet ./...、go test -race ./... 全通过。daemon 生命周期/审计/恢复/关停定向及 state/store 全包 -race -count=30 通过。只读 overlay + barrier 关键交错重复 5 次，诊断测试 PASS 表示异常已复现；真实 ListenAndServe 配回环假 CDP 捕获启动阻塞栈及关停后写入失败；小阈值日志轮转/FIFO 正常路径 5 次通过。manager 核对配置测试日志、交错输出、启动栈、关停日志和相关代码。HEAD 未变，工作树干净，无代码提交。
安装：复用 Go 1.27.4 和已有 Node，无新增工具或依赖清单变更。测试使用本 run TMPDIR 内临时 HOME/cache，仅使用本地假 CDP/回环服务。证据源码和日志保留于本 review run 的 evidence/FT1-30ff503/，临时目录按流程清理。
未检查：未启动真实 Chrome；真实 UI/CDP 事件投递、截图、网络拦截、systemd、Docker、跨平台均未实测；未注入磁盘写满和轮转重开失败。无关联 issue/PR，无需 GitHub 评论。

第 2/3 轮存在 P1，review/ → new/；下一轮按 fix 列表修复。
