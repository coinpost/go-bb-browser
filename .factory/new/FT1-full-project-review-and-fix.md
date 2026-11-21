---
branch: factory/FT1-full-project-review-and-fix
round: 5
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


## factory · plan · 2026-11-21 01:01
第 3 轮补充计划。
Behaviour：启动枚举锁外执行并捕获 epoch；idle 仅关闭核定的完整 target；创建在途销毁不得复活；释放和记账由同一生命周期边界负责；所有退出路径等待 RPC 与后台审计生产者；重启与轮转按完整 target ID 恢复。
Scope：最新 review 六项 P1 及直接相关 audit 容量、关闭记录身份问题，增加 barrier/回环假 CDP 的真实 ListenAndServe 回归；不扩展到 snapshot/CLI 或纯重构。
Decisions：不跨 CDP 持生命周期锁；创建销毁记录限于在途窗口；旧短 ID 记录无法证明归属，一律不收养并作废，即使只有唯一同后缀候选也不猜测；升级前旧 managed tab 将失去自动 idle 清理，需显式关闭或重新创建。关停关闭 RPC 准入、取消并等待在途 handler，再取消并等待后台任务，最后关闭 session/audit/store。
Checking：逐项并发回归、重启碰撞顺序变化、轮转完整身份、legacy 替代用户页、启动失败重试、强制关停与在途 handler；完整 gofmt/vet/race 测试和定向重复测试。

## factory · 2026-11-21 01:01
第 3/3 轮已构建并推送，new/ → review/。
branch: factory/FT1-full-project-review-and-fix
head: 62531a7b644ad1c635383271265de8fb967bb331
本轮两个提交：
- be46919：修复启动自锁、idle 短 ID 复用误关、sweeper 提前 Forget、create 回包复活已毁 target、后台生产者未等待及短 ID 恢复误收养；完整 ID 写入创建/释放/活跃日志与轮转快照。顺带限制 audit 实际容量，关闭记录绑定完整 target。
- 62531a7：manager 验证要求去掉旧格式唯一候选推断，旧短 ID 记录一律不收养；补 RPC handler 准入/取消/join，强制关停后仍等待生产者，防止 store 提前关闭。README/AGENTS 同步。

验证：Claude Code 报告最终内容完整 gofmt/vet/race 测试通过；daemon/state/store -race -count=30 通过；启动/重启/关停/legacy 等 -race -count=20 -cpu 1,2,8 通过；handler 关停定向 -race -count=30 通过；两轮共 14 项旧行为变异均被对应测试检出。manager 独立执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，全部通过；核对 origin/main..HEAD 提交范围、diff --check、禁止路径未改和干净工作树。
安装/依赖：无新增工具，复用已有 Go 1.27.4；go.mod 中 gobwas/ws v1.4.0 从间接改为直接依赖，仅假 CDP 测试直接使用，版本及 go.sum 不变。HOME/cache/临时文件均在本 run TMPDIR。

兼容性：旧短 ID 日志无法证明所有权，首次恢复时丢弃其 managed 归属并告警；旧版创建的存活 tab 不再自动 idle 清理，需显式关闭或重新创建。新格式按完整 ID 恢复；降级旧版会恢复旧风险。
留给 review：创建在途记录保留边界、背景事件与过期枚举交错、FIFO 锁序/容量、全 ID 日志轮转、强制关停 producer join。忽略 context 的底层调用会让关停持续等待，刻意不在生产者完成前关闭 store；正常 CDP 调用遵守取消。
未检查：未启动真实 Chrome，真实 CDP 事件投递/UI/截图/网络拦截、systemd、Docker、跨平台未验证；未注入磁盘写满、轮转重开失败。两项关停回归包含 200/300ms 等待假设，重复运行通过但并非纯 barrier。既有 handler 关停测试等待从 3s 放宽到 10s，coder 在旧 HEAD 复现了 net/http 空连接最长 5s 等待导致的原有失败。
仍未修复的范围外问题：tab_list tabs/focus 快照不一致、旧枚举复活已关闭 registry target、两项 snapshot ref、run --help=true、run -- --foo.js。六项 P1 的修复仍需独立 review，不宣称全库无缺陷。无关联 issue/PR。


## factory · review · 2026-11-21 01:11
head: 62531a7b644ad1c635383271265de8fb967bb331
verdict: fix
fix:
- [P1] internal/daemon/v1.go:528: 创建期间已确认 target 销毁，仍返回成功及已释放短 ID；回包前用户页复用该 ID，调用方关闭返回值会关闭用户页。保留完整 ID 创建/释放审计，但向调用方返回 target 已消失的 RPC 错误，覆盖成功响应前复用交错。
- [P1] internal/daemon/tab_idle.go:280: 显式关闭排队后才解析短 ID，两个针对原页的并发关闭会先后关闭原页和复用短 ID 的用户页。排队前绑定完整 target ID，取得操作锁后复核映射，变化则返回失效错误。
- [P1] internal/browser/session.go:282: 附着已有用户页后初始化失败直接 cancel，chromedp 会关闭该用户页；本地假 CDP 仅令 Page.getFrameTree 失败即可复现。已有 target 的失败清理只 detach，断言无 Target.closeTarget。
- [P1] internal/browser/session.go:313: target discovery 实际在页面 session 上启用，生命周期监听却在 browser 层；按实际订阅会话投递事件时，创建回包前的销毁未到跟踪器，已消失 target 仍被管理且磁盘只有 tab_new。在 browser executor 显式启用 discovery 并检查结果，假 CDP 按订阅会话投递。
- [P1] internal/daemon/browser_reconnect.go:53: 首次连接丢弃父 context 取消且握手没有期限；假 CDP 暂扣 attach 回复时取消 daemon，ListenAndServe 直到释放回复才退出。为初次握手设置可取消的独立期限，已有页仅 detach。
notes:
- [P2] internal/daemon/tab_idle.go:319: 显式关闭成功未通知在途创建跟踪器，关闭先于创建回包且销毁事件未处理时，已空浏览器仍留下 managed 状态及 tab_close,tab_new。成功关闭应在 lifeMu 内登记完整 ID 销毁并协调唯一释放记录。
- [P2] internal/daemon/browser_reconnect.go:130: 生命周期队列溢出后，重同步仍继续消费旧枚举前事件，旧 created 复活已消失 target。为事件和重同步建立次序边界。
- [P2] internal/daemon/v1.go:268: 成功操作后再按短 ID 查 target 写活跃审计，复用时把旧 target 的操作记到新 target。首次解析保存完整 ID，用于续期和审计。
- [P2] internal/daemon/v1.go:493: 强制关停时创建已执行、回包尚在途，handler 取消后留下未管理也未记成功创建的页面。为已发创建命令提供有界收尾并按可证明归属的完整 ID 记账。
- [P2] internal/store/store.go:331: 轮转先改名旧日志再发布快照，中断窗口重启丢失所有 managed 归属。先写完整临时快照再原子发布并覆盖中断恢复。
- [P2] internal/store/store.go:322: 轮转重开失败后 logFile 永久为 nil，后续释放持续失败；触发轮转的 append 却报成功。保留旧可写句柄至切换成功，检查改名错误并支持重试。
- [P2] internal/state/tab_registry.go:108: 旧枚举重新注册已关闭 target。记录删除版本，拒绝过期枚举复活。
- [P2] internal/daemon/v1.go:318: 列表与焦点来自不同时点，复现 tabs=[]、focus=1234。使用同一注册表快照。
- [P2] internal/browser/snapshot.go:109: 清理旧 ref 早于 selector_scope 解析，上次 ref 作为范围得到空结果；先保存根节点。
- [P2] internal/browser/snapshot.go:108: 脱离文档的旧标记未清除，插回后 ref 重复；跟踪并直接清理上轮标记节点。
- [P2] cmd/bb-browser/main.go:1036: run adapter.js --help=true 仍发 eval；统一布尔帮助选项，确保零 RPC。
- [P3] cmd/bb-browser/main.go:956: run -- --foo.js 拒绝存在的脚本；保留 -- 消歧边界。

验证：Codex 独立审查 origin/main...HEAD 的 11 个提交、32 个文件。配置 gofmt/vet/go test -race 全通过；从本机缓存恢复锁定模块后关闭代理、清除测试结果缓存，完整配置测试再次通过。daemon 定向 race 重复20次、state/store全包重复30次通过。额外只读 overlay + barrier/回环假 CDP 诊断重复5次；诊断 PASS 表示成功复现异常。manager 核对配置离线日志、交错复现输出及关键实现，HEAD 未变，工作树干净；无代码提交、禁止路径未改。
前轮六项 P1 中，启动枚举锁自锁、idle 完整 ID 关闭、sweeper 释放记账、producer join、完整 ID 重启恢复的原交错已修复；创建销毁覆盖仍有事件会话和成功回包缺口。audit FIFO/容量竞争测试通过，未发现反向 lifeMu 锁序。
安装：复用已有 Go 1.27.4 和 Node，无新增工具或依赖清单修改，仅恢复锁定依赖。HOME/cache/overlay/临时文件位于本 run TMPDIR；复现源码与日志保留在本 review run 的 evidence/FT1-62531a7/。未找到 review-code skill，按独立代码审查执行。
未检查：未启动真实 Chrome；真实 UI/CDP、截图、网络拦截、systemd、Docker、跨平台、磁盘写满未实测。无关联 issue/PR，无需 GitHub 评论。

第3/3轮仍有5项P1，已达 max_rounds，留在 review/ 等 owner 决定；如需继续修复，请添加说明并移回 new/。本轮不自动开始第4轮。

## Guoyin · 2026-11-21 14:28
再来一轮
你不要用 cloud agent，直接把任务挪到 new 就可以了


## factory · plan · 2026-11-21 15:48
第4轮补充计划（owner 已要求再来一轮）。
Behaviour：创建回包前已销毁 target 时返回 RPC 错误、不暴露可复用短 ID，保留完整身份审计；显式关闭在排队前绑定完整 ID、加锁后复核；已有页 attach 失败只 detach；browser executor 启用 discovery 并检查失败；初始握手有独立期限且受 daemon 取消，成功后会话独立。
Scope：最新 review 五项 P1 及直接相关生命周期/连接回归，不扩展到其余 snapshot/CLI/store 问题。
Decisions：复用中断构建的修改但逐项核实；连接采用仅 browser control（零页不创建、不附着用户页）；不修改依赖上游，避免隐式 CloseTarget。旧测试可替换但必须保留有价值的兼容覆盖。锁序和取消/清理必须可解释。
Checking：本地假 CDP 按实际订阅 session 投递；barrier 覆盖创建销毁/短 ID 复用、并发关闭、attach 初始化失败无 close、握手取消/期限、连接成功后父 context 取消不破坏 session；gofmt/vet/race 全套和定向重复。真实 Chrome/UI 未验明确记录。

## factory · 2026-11-21 15:48
第4轮按 owner「再来一轮」授权修复，new/ → review/。未修改 max_rounds 配置；本轮仍需独立 review，若有 P1 则等 owner 决定。
branch: factory/FT1-full-project-review-and-fix
head: 27815e5964e03f94ef7fd73df6b23db494be7c4e
本轮由本地 Claude Code 完成，未使用 cloud agent。恢复了中断构建的未提交修改（先 stash 留存备份，再应用继续）；清理了本 run 过期临时缓存，Go 模块只读目录经赋予当前用户写权限后清理，无 sudo。

构建：一个主题提交 27815e5，修复最新 review 五项 P1：创建回包前销毁返回 RPC 错误、不分配短 ID，并以完整身份记录创建/释放；显式关闭排队前绑定完整 target，取得操作锁后复核映射；连接只建立 browser control、不附着已有页或零页时新建页，页面懒附着初始化失败仅 detach；browser executor 显式启用 discovery 并检查结果；初始握手受 daemon 取消且有独立30秒期限，成功后会话寿命独立。顺带使显式关闭通知在途创建跟踪器。README/AGENTS 同步。移除不再使用的 /json/list 探测、stale-target 回退及对应3项旧单测，新增替代连接和取消覆盖。

验证：Claude Code 报告完整 gofmt/vet/race 通过；daemon 相关回归 -race -count=20 -cpu 1,2,8、browser 全包 -race -count=20 通过；五项修复共7种旧行为变异均被对应测试检出。manager 独立在隔离 HOME/TMPDIR 下关闭代理执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，全部通过。核对 origin/main..HEAD 只含本条目提交，diff --check、禁止路径与工作树干净检查通过，然后推送。假 CDP 按实际订阅会话投递，真实 ListenAndServe + barrier 覆盖并发关闭与短 ID 接替。
安装：无新增工具，无 manifest/lockfile 变更，复用已有 Go 1.27.4，仅离线恢复锁定模块。所有测试 HOME/cache/临时物位于本 run TMPDIR。

兼容与 review 重点：零页连接不再创建 about:blank；移除了非 Chrome CDP 的旧回退，项目范围仍只支持 Chrome。已销毁创建在日志记 ok:true 表示实际创建过，但 RPC 返回错误，需复核恢复语义及清理锁序；手动调用 allocator 建立 browser-only 连接需复核 chromedp 上下文取消与 Browser 发布同步。部分握手测试有100–450ms等待，不全是纯 barrier。
未检查：未启动真实 Chrome，真实 browser discovery/CDP/UI/零页连接、非Chrome CDP兼容、截图/网络拦截、systemd/Docker/跨平台均未实测。tab_new 成功返回后再销毁并复用短 ID 仍受协议本身限制。其余 review P2/P3（含store轮转、过期枚举、快照/CLI等）未修复；本轮不宣称全库无缺陷。无关联 issue/PR。


## factory · review · 2026-11-21 16:14
head: 27815e5964e03f94ef7fd73df6b23db494be7c4e
verdict: fix
fix:
- [P1] internal/daemon/tab_idle.go:280: 显式关闭入口调用 resolveTab，查无映射时会重新枚举并绑定请求到达后才打开的用户页；已复现原页关闭后重复关闭1234，入口浏览器/注册表均为空，枚举期间新用户页复用1234，RPC成功关闭用户页（10/10）。入口只从已有注册映射绑定完整ID，无绑定立即返回unknown tab id；取得操作锁后复核，不得把失效请求重新指向未来页面。
- [P1] internal/daemon/v1.go:818: eval先排短ID操作锁、后解析完整ID；排队期间原页关闭、用户页接替，业务表达式实际在用户页执行（真实HTTP+fake CDP，5/5）。eval及同顺序的goto/click/fill/fetch/观测入口应排队前绑定完整ID，锁内复核，执行与审计始终共用该身份。
notes:
- [P2] internal/daemon/server.go:253: 优雅关停时target同步先于在途HTTP请求退出，期间销毁事件漏记，磁盘仍保留已关闭页；RPC排空后收齐生命周期事件再停同步。下次完整ID枚举可纠正。
- [P2] internal/daemon/v1.go:268: 操作成功后再按短ID查target写活跃审计，会给接替页记账并续期；保存实际执行完整ID。
- [P2] internal/daemon/v1.go:492: 强制关停创建已执行但回包在途时，留下未管理页面且审计ok=false；给已发命令有界收尾并按确认完整ID记账。
- [P2] internal/daemon/browser_reconnect.go:133: 队列溢出重同步后旧created事件可重新注册已消失页；建立事件/枚举次序边界。
- [P2] internal/state/tab_registry.go:108: 删除未计入epoch，旧枚举重新注册已关闭target；记录删除版本并过滤过期快照。
- [P2] internal/daemon/v1.go:318: tab_list仍可返回tabs=[]、focus=1234；从同一注册表快照生成列表和焦点。
- [P2] internal/store/store.go:331: 轮转改名后、快照发布前中断导致恢复集合为空；先写完整临时快照再原子发布并保留上一代。
- [P2] internal/store/store.go:322: 轮转重开失败后追加报成功，后续即使恢复权限仍持续log file not open；保留旧句柄至切换成功、传播错误并支持重试。
- [P2] internal/browser/snapshot.go:109: 清理旧ref早于scope解析，使用上次ref作为范围返回空；先保存根节点。
- [P2] internal/browser/snapshot.go:108: 脱离文档旧标记未清除，插回后ref重复；直接清理上一轮节点或区分快照轮次。
- [P2] cmd/bb-browser/main.go:1036: run adapter.js --help=true仍执行eval；统一布尔帮助选项并确保零RPC。
- [P3] cmd/bb-browser/main.go:956: run -- --foo.js仍拒绝存在的脚本；保留--消歧信息。
- [P3] internal/daemon/v1.go:560: 显式关闭先于创建回包时一次关闭记tab_close,tab_new,tab_close；恢复集合正确，但应协调迟到创建记账避免重复成功释放。

验证：本地Codex独立审查origin/main...HEAD共12个提交、37个文件。第4轮五项P1原始复现均通过；新增两项P1为具体交错证实的错误操作用户页，不是将既有P2简单升级。基线源码已存在未命中重新枚举和操作锁后解析ID的行为，仍属本条目全项目review范围。
配置gofmt/vet/go test -race ./...离线全部通过；daemon定向race重复20次、browser/state/store全包各重复20次通过。只读overlay+barrier、真实daemon HTTP与回环fake CDP复现两项P1及notes；诊断测试PASS表示异常已复现。allocator发布前后取消、初始化失败detach后重试、browser discovery失败、握手取消、FIFO/容量/handler join专项通过。manager核对配置日志、两项P1复现输出和相关实现。HEAD不变、工作树干净，未改代码或提交。
安装：复用已有Go1.27.4，无新增工具或依赖清单修改，仅恢复锁定模块。临时HOME/cache/overlay均在本run TMPDIR；证据源码与日志保留于本review run的evidence/FT1-27815e5/。
未检查：未启动真实Chrome；真实UI/CDP投递时序、截图、网络拦截、systemd、Docker、跨平台、磁盘写满未实测。无关联issue/PR，无需GitHub评论。

第4轮为owner额外授权，round=4已达/超过max_rounds=3；仍有2项P1，留在review/等owner决定，不自动开始第5轮。继续修复需owner添加说明并移回new/。

## Guoyin · 2026-11-21 22:29
继续下一轮


## factory · plan · 2026-11-21 22:51
第5轮补充计划（owner「继续下一轮」额外授权；不修改 max_rounds）。
Behaviour：指定短 tab id 的入口排队前只从已有注册表绑定完整 target ID；无映射直接 unknown tab id，不枚举。取得操作锁后复核同一绑定；CDP、buffer/lease、成功活跃审计始终使用该完整身份，不操作接替短 ID 的用户页。空/非法参数、取消/锁超时保持协议。
Scope：最新两项 P1 及直接相关审计重新解析短 ID 的 P2；覆盖页面操作、关闭、选择、观测读/清除、路由，不扩展到 store/snapshot JS/CLI 或纯重构。
Decisions：未知/过期别名 fail closed，调用方可先 tab_list 再重试；用小 helper 明确绑定、锁所有权和失败路径，不跨 CDP 持 lifecycle/registry 锁。tab_select 枚举前绑定并按完整 ID 选中；tab_focus 按其枚举的完整 ID 记账。
Checking：barrier/本地假 CDP/真实 HTTP 覆盖入口无映射后用户页出现、排队时复用、成功后映射切换、正常路径和取消；各入口不向新页发送业务请求、不改 buffer/续期。完整 gofmt/vet/race 与定向重复，旧行为及变异必须被新测试检出。

## factory · 2026-11-21 22:51
第5轮由本地 Claude Code 完成，new/ → review/；owner 额外授权，不改 max_rounds=3，本轮仍需独立 review。
branch: factory/FT1-full-project-review-and-fix
head: 82ece1bbe61e71d18609cf6b2c78f18c28cc283a
新增一个主题提交 82ece1b：bindTab 排队前绑定、锁内复核、删除 resolveTab 未命中枚举；17 个带锁入口（goto/reload/close/screenshot/eval/click/fill/fetch/snapshot/route/unroute、三类观测读与三类 clear）均使用完整 ID。tab_select 枚举前绑定、SelectTarget 选中；tab_focus 的选中/审计使用枚举目标。成功后审计和 idle 续期使用执行时绑定身份，不再重查短 ID。README/AGENTS 同步；三个依赖隐式枚举的旧测试改为显式注册。

验证：Claude Code 完整 gofmt/vet/go test -race -count=1 ./... 通过；daemon 全包 -race -count=20、state 全包 -race -count=30、关键回归 -race -count=20 -cpu 1,2,8 通过。新增表驱动测试覆盖17入口未知ID/排队复用/取消/正常路径，另有成功后换映射及 tab_select 回归；两条真实HTTP+回环假CDP覆盖最新review P1。旧源码运行新测试出现6个顶层失败，5种变异均被检出。manager 独立在隔离 HOME/TMPDIR 和本地模块缓存环境运行完整 gofmt/vet/go test -race -count=1 ./...，全部通过；核对提交范围、diff --check、禁止路径未改及干净工作树后推送。相关测试日志已保留于本 build run 的 evidence/FT1-82ece1b/。
安装：无新增工具，无 manifest/lockfile 变更；复用 Go 1.27.4，只恢复锁定模块。所有测试 HOME/cache/临时文件位于本 run TMPDIR。全平台 go mod download 缺本机未缓存的 mousetrap，但本机配置完整测试离线通过，不宣称跨平台已验。
兼容：未注册短 ID 不再隐式枚举；新开的用户页事件未到达时会暂时 unknown tab id，需 tab_list。未知 ID 的判定先于锁超时。操作进行中 target 仍可能销毁，此时 CDP 按完整 ID 失败而不重定向接替页。
未检查：未启动真实 Chrome，真实 CDP/UI、截图、网络拦截、systemd/Docker/跨平台未实测。clear 成功后换映射仅由共用记账路径覆盖，无单独可插桩调用。其余 review P2/P3（列表/焦点快照不一致、过期枚举复活、生命周期队列溢出、关停/创建收尾、store轮转、snapshot/CLI、重复释放记录）未修复。本轮不宣称全库无缺陷；无关联 issue/PR。


## factory · review · 2026-11-21 23:12
head: 82ece1bbe61e71d18609cf6b2c78f18c28cc283a
verdict: fix
fix:
- [P1] internal/daemon/v1.go:476: tab_select 枚举回包后、应用前原页销毁且用户页接替短 ID，旧枚举复活原页为另一短 ID；SelectTarget 实际选中91234，响应仍返回1234，后续关闭返回值会关闭用户页（真实HTTP+回环fake CDP，10/10）。在同一注册表临界区复核请求短 ID 仍绑定原完整 ID 后再选择，变化则报unknown；拒绝旧枚举复活已删除target。
- [P1] internal/daemon/v1.go:361: tab_focus 探测期间原页销毁、用户页已接替1234，探测不完整且无缓存焦点时仍从旧快照返回1234，调用方按返回值关闭会关闭用户页（10/10，接替发生在响应前）。返回前按完整 ID 校验候选与当前短 ID 仍匹配，响应/元数据/审计来自同一有效候选，无有效候选则错误或重新枚举。
notes:
- [P2] internal/daemon/tab_idle.go:183: target释放清空观测环后，已取走事件的writer仍可Push重建旧完整ID的环（5/5），导致旧数据/内存滞留；停止并等待writer或原子校验注册代次。未观察到接替页读取旧事件。
- [P2] internal/store/store.go:218: 合法JSON尾行缺换行时首次恢复成功，后续成功追加与尾行拼接，下次启动截断并丢失归属（HEAD10/10，基线同样复现）；接受完整尾行后补分隔符并检查错误。
- [P2] internal/store/store.go:171: 启动覆盖并删除状态目录既有.write-test普通文件（HEAD10/10，基线同样复现）；用CreateTemp独占创建探测文件，或直接检查打开日志结果。

前轮两项P1原始路径已修复：重复关闭未知短ID不再枚举，排队eval等17个带锁入口排队前绑定、锁内复核完整ID。三个clear另加成功后换映射调度点各10次，接替页buffer/idle不变且审计只记原页。以上两项P1是响应身份错误导致实际误关用户页的独立证据，不是将既有P2无证据升级。其他既有notes（队列溢出、过期枚举、列表/焦点、关停与创建、轮转、snapshot/CLI、重复释放）沿用原记录及等级。

验证：本地Codex独立审查origin/main...HEAD共13个提交、41个文件（本轮13文件）；配置gofmt检查、go vet ./...、go test -race ./...在HEAD与导出的origin/main均离线通过。17入口相关定向race重复20次，daemon生命周期/FIFO/恢复/连接/关停定向及browser/state/store全包重复10次通过。真实HTTP、回环fake CDP、只读overlay/barrier复现上述问题；异常诊断PASS表示成功复现。manager核对配置日志、两项P1复现输出及相关实现，HEAD未变、工作树干净、禁止路径未改，无代码提交。
安装：复用已有Go1.27.4和Node，仅恢复锁定模块，无新增工具或依赖清单修改。HOME/cache/overlay/临时文件位于本run TMPDIR；复现源码、报告与日志保留于本review run的evidence/FT1-82ece1b/。
未检查：未启动真实Chrome；真实UI/CDP时序、截图、网络拦截、systemd、Docker、跨平台、磁盘写满未实测。无关联issue/PR，无需GitHub评论。

第5轮为owner额外授权，round=5已超过max_rounds=3；仍有2项P1，留在review/等owner决定，不自动开始第6轮。继续修复需owner添加说明并移回new/。


## factory · 2026-11-21 23:23
owner 在当前对话授权：「采纳你的判断，把这些信息补充进去，然后开启下一轮」。据此额外授权第6轮，review/ → new/，保持原分支和 round=5；成功构建后再记 round=6。不修改全局 max_rounds=3；第6轮若仍有 P0/P1，继续留在 review/ 等 owner 决定，不自动授权第7轮。沿用现有本地 Claude Code build / Codex review 流程，不使用 cloud agent。

收敛判断：前5轮有真实修复，并非同一个缺陷原地打转；但原任务“全项目 review 并修复”与每轮主要处理上一轮 P1 的方式不对称，造成范围不断外溢。当前累计13个提交、41个文件。短ID可复用，之前分别修关闭前绑定、其他入口排队前绑定，却未把注册表、枚举、选择、响应、元数据和审计统一到同一完整target身份。第2轮起记录的旧枚举复活已关闭target仍未解决，并在第5轮与选择逻辑组合成实际误操作路径。race测试和现有测试重复通过不代表业务身份一致性成立。

证据边界：最新两项P1有真实HTTP + 本地回环fake CDP复现，各10/10；tab_select实际选中91234却返回已指向接替用户页的1234，tab_focus返回旧快照失效别名，随后按返回值tab_close确实误关用户页。复现源码和日志在review run的evidence/FT1-82ece1b/；这是可重复的本地协议级证据，尚未验证真实Chrome中的发生情况。不能把诊断测试PASS当成安全回归PASS。

## factory · plan · 2026-11-21 23:23
第6轮补充计划：围绕同一身份约束一次收口，不再仅局部补tab_select/tab_focus。

Behaviour：短ID是可复用别名，不是页面身份。请求绑定、注册表变更、枚举应用、选择提交、响应/元数据及审计必须可解释地关联同一完整target ID与有效注册代次。旧枚举不得复活枚举开始后已删除的target；枚举开始后新增的target仍需保留。选择时在同一注册表临界区复核请求短ID仍指向绑定target后再提交，变化则fail closed。focus探测后必须重新验证候选，不能从失效快照返回已属于另一页的别名。列表和焦点来自同一快照。不存在有效候选时明确返回错误或有界重新枚举，不猜测归属、不隐式切换到接替页。

Scope：最新review两项P1，以及直接相关的删除epoch/旧枚举复活、选择时原子身份校验、focus候选验证、列表/焦点快照、响应/元数据/审计一致性与永久回归。检查所有直接共用这些身份/快照路径的调用点，而不是只改两个函数。无关P2/P3拆至FT2～FT7 backlog，保留历史等级与证据，本轮不做全库无缺陷承诺；新发现有证据的P0/P1仍必须报告，不能靠缩范围掩盖。

Decisions：从注册表及生命周期边界定义约束，采用最小清晰实现；不以全局长锁包围CDP调用，不做无关抽象或协议重设计。短ID在成功响应之后再次被复用属于现有协议限制，本轮必须解决的是响应提交前已经失效/接替仍返回成功的可复现路径，并在文档写明保证边界。删除版本/枚举代次的保留与清理要有明确所有权、内存边界，避免用无限增长墓碑换正确性。

Checking：把第5轮两条真实HTTP+fake CDP/barrier复现转换成仓库永久回归，断言正确错误码/有效身份且接替用户页不被选择、操作或关闭，不保留“异常发生即PASS”的诊断语义。覆盖旧枚举回包前后删除/重建、请求别名更换、focus探测不完整且无缓存、候选已删除、正常选择/列表/焦点、取消/错误路径，以及响应/元数据/审计完整ID一致。保留前轮17入口、恢复/审计等已有回归。至少用旧HEAD或定向变异证明新增测试会检出两项P1和删除版本缺口，再运行配置gofmt/vet/go test -race全套及定向重复测试。真实Chrome未验则如实记录；不得操作用户已有窗口。

Review边界：审查仍核对origin/main...HEAD的累计变更及其回归风险，但FT1验收目标以本补充范围和既有已修P1不回归为准；独立P2/P3不再反复回填FT1作为本轮修复任务。重点验证共同约束，而不只重跑上一轮两个复现。

独立未完成项已拆为backlog FT2～FT7，尚未授权构建，原记录保留不改。
