---
branch: factory/FT1-full-project-review-and-fix
round: 1
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
