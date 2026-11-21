# [P1] 页面 JS 异常文本含 websocket/eof 等字样时 daemon 误判 CDP 断开并整体退出

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B01；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P1。位置：`internal/daemon/browser_reconnect.go:46`（基线行号，实施前重新定位）。

触发条件：任一客户端调用 eval / run（adapter）/ snapshot --scope，页面侧抛出的异常描述里含 "websocket"、"eof"、"broken pipe"、"connection reset"、"channel closed"、"could not dial"、"use of closed network connection" 任一子串（大小写不敏感）。例：脚本抛 `Failed to execute 'send' on 'WebSocket': Still in CONNECTING state.`；`snapshot` 的 selector_scope 为 `div[data-kind=websocket`（非法选择器，异常文本会回显选择器）。

实际行为：Session.EvalAwaitContext（internal/browser/session.go:816-818）与 SnapshotContext（internal/browser/snapshot.go:161-164）把 chromedp 返回的 *runtime.ExceptionDetails 原样上抛；handler 调 cdpHint（internal/daemon/v1.go:1261）→ isCDPSessionLost 按错误字符串子串匹配（browser_reconnect.go:43-58，internal/browser/errors.go:79-87）判为 ErrorTransport → fatalCDPLost：置 sessionFailed、取消 runCtx、ListenAndServe 返回错误，进程以 1 退出。

期望行为：页面级 JS 异常只应作为该次 RPC 的 `eval failed` / `snapshot failed` 错误返回；daemon 保持运行，CDP 会话状态不变。只有真正的传输层错误（websocket 读写失败、连接关闭）才触发 fatal。

影响：任何有 RPC 权限的调用方（或被访问页面的脚本内容，经 adapter eval 透传）一次请求即可让 daemon 退出；所有客户端在途请求失败，观测缓冲、网络拦截规则、焦点等内存状态全部丢失，依赖 supervisor 重启。对爬虫类调用方表现为随机整机重启。

根因：传输错误识别基于 err.Error() 子串，而页面可控文本（异常 description、选择器回显）会进入同一条错误链；eval/snapshot/fetch 路径没有把页面异常包装成 Kind=ErrorRequest 的 CDPError。

修复方向（建议，非指定实现）：在 Session 的 eval/snapshot/fetch 路径用 errors.As 识别 *runtime.ExceptionDetails（及 *cdproto.Error 协议级错误）并包装为 Kind=ErrorRequest 的 CDPError；isCDPSessionLost 只信任类型化的 ErrorTransport（或仅对非 CDP 应答类错误做子串兜底）。

回归验收：新增 daemon 测试：fake conn 的 Eval/Snapshot 返回 *runtime.ExceptionDetails（description 分别含 websocket、EOF、非法选择器回显），RPC 返回 JSON-RPC error，且 fatalCh 无值、ensureBrowserSession 仍为 nil；browser 包测试断言 ErrorKindOf(*runtime.ExceptionDetails) != ErrorTransport；原有真实传输错误（io.EOF、"use of closed network connection"）仍判定为丢失。

证据：repro-go.log（TestAudit_PageExceptionTextMustNotKillDaemon，3 个子用例 FAIL）；repro/daemon_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
