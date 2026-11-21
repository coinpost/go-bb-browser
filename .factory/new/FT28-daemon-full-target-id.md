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
