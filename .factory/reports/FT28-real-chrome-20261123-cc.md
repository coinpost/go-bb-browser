# FT28 @ 39f1a61：CC 独立真实 Chrome 核验

核验对象：原始 review（`review/evidence/FT28-39f1a61/REVIEW.md`）与已有真实 Chrome 验证（本目录 `REPORT.md` / `run.sh` / `export/` / `logs/final-5.log`）。
只核验：未改生产源码、未提交推送、未动 factory 看板与 FT7。所有计数均为我本次亲自运行，stub 结果一律不计入。

## 结论

1. **P1-1 成立，且比两份报告写的更容易触发。** 经完整生产启动路径（`ListenAndServe` → `browser.Connect` → `runTargetSync` + watchdog）和真实 HTTP `/v1`，daemon 向真实 Chrome 发出 `Target.closeTarget(A)`，A 消失、B 存活。第二次 `tab_list` 不是必要条件：一次在途 `tab_list` 加一次 `eval(A)` 就够。
2. **P1-2 成立，但需加限定。** 不扣留任何事件也复现。但在完整生产路径下，"立即形态"的 registry/idle/磁盘分叉会在约 1 个同步 tick 内自愈；持久分叉需要创建回复晚于那次自动重枚举才到达。
3. **P1-3 / P1-4 / P2 在未改动的生产连接上不可达（Chrome 157 + chromedp v0.15.1）。** 生产连接从不发送 browser 级 `Target.setDiscoverTargets`，35 次未注入的运行里 daemon 连接收到的 root 级生命周期事件为 0。三项只有在测试代理额外注入 browser 级 discovery 后才复现。它们是真实存在的代码缺陷，但"当前生产可达的 P1"这一判断没有证据支持。
4. 已有真实 Chrome 报告的三项结果我原样复跑各 5/5，探针调度与原 review 一致，其 P1-4 的 discovery 限定是正确且必要的；需要补充的限定见下文。

## 我亲自复跑的结果（`cc-logs/cc-final-5.log`，`-race -count=5`，65 次独立 Chrome 启停，全部退出码 0，无 race 报告）

| 项目 | 测试 | 路径 / 干预 | 次数 | 结果 |
|---|---|---|---:|---|
| P1-1 | `TestCCDaemonP1_1_ListThenEval` | 完整生产 daemon；只延迟一条 getTargets 回复 | 5/5 | closeTarget=[A]，A 消失，B 存活 |
| P1-1 | `TestCCDaemonP1_1_EvalOnly` | 同上，去掉第二次 tab_list | 5/5 | 同上 |
| P1-1 对照 | `TestCCDaemonP1_1_ControlNoAttach` | 同调度但不 eval | 5/5 | closeTarget=[]，A 存活并重新被列出 |
| P1-1 | `TestRealChromeAttachedLiveDiscovery`（原探针，未改） | tabHook=Session，无消费者 | 5/5 | closeTarget=[A]，A 消失，B 存活 |
| P1-2 | `TestCCDaemonP1_2_LateCreate` | 完整生产 daemon；延迟 getTargets 与 createTarget 两条回复，**不扣留 destroyed** | 5/5 | tab_new 成功，3 个 clear 接受死 A，registry/idle/磁盘重放均有 A |
| P1-2 自愈观察 | `TestCCDaemonP1_2_LateCreateThenHeal` | 同上，提交后 2.2 秒不发 RPC | 5/5 | registry=false，idle=false，磁盘 managed=false |
| P1-2 持久形态 | `TestCCDaemonP1_2_LateCreateAfterResync` | 同上，创建回复在同步循环重枚举之后放行 | 5/5 | 2.2 秒无 RPC 后仍 registry/idle=true，3 个 clear 成功，磁盘 managed=true |
| P1-2 | `TestRealChromeConfirmedAbsenceAfterContested`（原探针） | tabHook，无同步循环，扣留 destroyed | 5/5 | 与原报告一致 |
| 事件路由 | `TestCCRouting_Production` | 完整生产 daemon，**无任何延迟/扣留/注入** | 5/5 | root 级 setDiscoverTargets=0，root 级事件=0 |
| 事件路由 | `TestCCRouting_ProductionZeroPages` | 零页面启动 | 5 次 SKIP | 未覆盖（见下） |
| P1-4 | `TestRealChromeEventReadBeforeAbsentRequest`（原探针） | 注入 discovery；手动调 `applyTargetEvent` | 5/5 | 死 A 留在 registry，3 个 clear 成功 |
| P1-4 | `TestCCInjectedP1_4_SyncLoop` | 注入 discovery；生产 `runTargetSync` 消费 | 5/5 | 同上；补发被扣的 destroyed 后 A 被移除 |
| P1-3 | `TestCCInjectedP1_3_DirtyBeforeDestroyEvidence` | 注入 discovery；真实事件填满 256 队列；回调入口屏障；创建回复包装 | 5/5 | 死 A 提交后经补偿枚举仍在 registry/idle/磁盘，其后 2.2 秒无再次枚举 |
| P2 | `TestCCInjectedP2_ReverseConflict` | 注入 discovery；真实事件填满队列；生产 `runTargetSync` | 5/5 | 存活 A 2.15 秒内未回到 registry，期间 getTargets=2；显式 tab_list 后恢复 |

所有结果都是**真实 Chrome + 受控回复/事件时序**，不是自然并发重现，不能用来估计线上概率。唯一没有任何时序控制的是 `TestCCRouting_Production`。

## 关键证据

**P1-1 线上顺序**（`cc-run/logs/TestCCDaemonP1_1_EvalOnly-2833251759/cdp.jsonl`）：

- id17 `Target.getTargets` 发出，回复被暂存（只含 B）
- 独立连接 `Target.createTarget` → A=`99023174FCB2746908315C86CF931190`
- id18 `Target.getTargets`（`eval` 的 `resolveTab` 自己发起，列出 A）→ id19 `attachToTarget(A)` → id33 `Runtime.evaluate "1"` 成功
- 放行 id17 → id34 `Target.detachFromTarget` → **id35 `Target.closeTarget(A)`**
- 之后的枚举不再有 A；独立连接查询 A=false、B=true

源码对应：`internal/daemon/tab_idle.go:186` 对 `Contested` 移除也调 `ForgetTarget`，`internal/browser/session.go:480` 取消 chromedp 子上下文即发 closeTarget。对照组证明关闭确由已附着的子上下文引起。

**事件路由**：chromedp v0.15.1 对 `RemoteAllocator` 强制 `first=false`（`chromedp.go:133`），`WithTargetID` 走 `attachTarget`，其 `SetDiscoverTargets` 用页面 session 执行器发出（`chromedp.go:466`）；带 `sessionId` 的消息被路由给页面 Target（`browser.go:270`），到不了 `ListenBrowser`。线上统计与此一致：非注入的 35 次运行均为 `setDiscoverTargets root=0`、`root(no sessionId)=map[]`，session 级事件正常存在。

**同一测试的无时序控制观察**（5/5）：daemon 自己 `tab_new` 的标签被用户关闭 2.3 秒后，registry=true、idle=true、3 个 clear 仍成功；一次显式 `tab_list` 后才清除。即生产中 destroyed 通知永远到不了 registry，标签消失只靠枚举发现。

## 原报告需要修正或补充的限定

**原始 REVIEW.md**

- P1-1：探针里"事件已读入队"的断言在真实 Chrome 上不成立也不需要；触发条件应写成"在途枚举 + 对新标签的任一附着型操作"，第二次 `tab_list` 可省。
- P1-2："销毁通知尚未交付"不是窄窗口，生产中是常态。但应补充：冲突时 `MarkTargetDirty` 触发的重枚举会在 ≤1 tick 内移除迟到提交的 A，除非创建回复晚于该重枚举；持久形态靠下一次枚举（`tab_list`、任何未解析 tab 的 RPC）或 idle 关闭收敛。原探针用 fake、无同步循环，看不到自愈。
- P1-3 / P1-4 / P2：前提是 root 级生命周期事件进入 `ListenBrowser` 队列。原 stub 发的是不带 sessionId 的事件，与真实 Chrome 对生产连接的行为不符。严重级别应加"当前生产连接不可达"的限定；是否保留 P1 由 reviewer/owner 决定。我没有验证其他 Chrome 版本或其他 CDP 宿主（源码提到 Obscura），所以不能说它们在任何部署下都不可达。
- 连带影响：第 5 轮两项 P1 的修复（读出时登记 destroyed）及其回归同样只在 stub 的 root 级事件上验证过，在真实 Chrome 生产连接上这条通路不工作。这不在原 review 的问题列表里，建议单独评估。

**已有真实 Chrome REPORT.md**

- 调度与原 review 一致，确实经过生产 `browser.Session`、生产 RPC 处理和真实 Chrome；代理只转发真实字节。但它用 `s.tabHook = sess` 注入，没有走 `ListenAndServe`，没有 `runTargetSync` 和 watchdog。"通过生产 daemon"应写成"生产 Session + 生产 RPC handler，启动接线为测试注入"。
- P1-2 的"idle=true、磁盘 managed=true"是在无同步循环下立即测得；完整生产路径下需区分立即形态（约 1 秒自愈）与持久形态。
- P1-2 的 `holdDestroyed` 没有实际作用：不扣留也 5/5。
- P1-4 的 discovery 限定正确。应再加一句：不注入时该前提 0/35 出现，且源码分析表明生产连接任何启动分支都不会开启 root 级 discovery。
- 其"未运行 P1-3/P2"已由本次补上，但同样只在注入 discovery 下成立。

## 我的探针与原调度的差异

- P1-3：真实 Chrome 会为 A 的创建发事件，队列已满时它们先被丢弃，所以 dirty 在 destroyed 之前就已置位；原 stub 调度里只有 destroyed 被丢。回调入口屏障和"补偿窗口开启 → 创建提交 → 销毁证据入账"的顺序与原探针相同。创建回复用测试包装保持（与原探针相同手法），不是代理延迟。
- P1-3 / P2 的队列用真实 `targetInfoChanged(B)` 填满（独立控制 session 上 200 次 `location.hash` + 56 次 `data:` 导航）。
- P2 / P1-3 / P1-4 用 `tabHook` 模式手动启动生产 `runTargetSync`，不是 `ListenAndServe`。

## 未覆盖

- 自然并发/压力重现、线上发生频率：未做。
- 零页面启动：headless Chrome 157 关掉最后一页后，生产 `Connect` 报 `Failed to open new tab - no browser is open (-32000)`，无法启动 daemon，5 次均 SKIP。"零页面分支也不开启 root discovery"只有源码依据，没有实测。
- 非 headless Chrome、其他 Chrome 版本、其他 CDP 宿主：未测。
- 前版 `dab7d41` / base `2be49f8` 在真实 Chrome 上的对照：未跑，"第 6 轮回归"的归因仍只有原 review 的 stub 证据。
- 同后缀 ID 隔离、`tab_focus` 路径、idle 关闭、Pool/CLI：未测。

## 隔离与清理

- 每个测试自建临时 profile（`cc-run/tmp/cc-chrome-profile-*`），`--headless=new`，`127.0.0.1` 随机端口，每进程 `listeners.txt` 断言唯一监听。只访问 `about:blank` 与 `data:`。
- 运行前后 `pgrep -fl "Google Chrome"` 均为空，无残留 profile（`cc-logs/chrome-processes-before.txt`、`cc-logs/cleanup-after.txt`）。
- 工作树 HEAD=39f1a61，`git status` 为空；`cc-export` 与 `git archive HEAD` 逐文件一致，只多两个 `_test.go`（`cc-integrity.txt`）。
- 原有证据未覆盖：`export/`、`run.sh`、`REPORT.md`、`logs/` 的哈希与 mtime 不变。唯一共用的是构建缓存 `gocache/`（磁盘仅剩约 400MiB，另建缓存时写满导致编译失败；那次失败日志已被同名的 `cc-logs/smoke-1.log` 重跑覆盖，没有保留）。

## 命令与文件

```sh
cd <local-evidence-dir>/FT28-real-chrome-20261123
COUNT=5 bash cc-run.sh > cc-logs/cc-final-5.log 2>&1
# 单项：COUNT=3 RUN='^TestCCDaemonP1_1' bash cc-run.sh > cc-logs/<新名字>.log 2>&1
```

- `cc-run.sh`：环境与准确 go test 命令
- `cc-export/internal/daemon/cc_real_chrome_test.go`：我的探针；同目录 `real_chrome_review_test.go` 为原探针未改副本
- `cc-logs/cc-final-5.log`：正式日志；`cc-final-runs.txt`：65 个证据目录索引
- `cc-run/logs/<Test>-<n>/`：`cdp.jsonl`、`daemon.log`（daemon 模式）、`chrome.log`、`chrome-command.txt`、`listeners.txt`、`version.json`、`state/`
- `cc-logs/smoke-*.log`：调试阶段记录，不计入结果。`smoke-2-all-cc.log` 与 `smoke-3.log` 中的失败是我探针自身的问题（零页面未处理、队列填充方式），已保留未隐藏
- `cc-integrity.txt`：工作树、导出一致性、哈希
