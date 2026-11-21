---
branch: factory/FT8-typed-cdp-error-classification
round: 1
---
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

## factory · plan · 2026-11-22 03:41

### Behaviour
- 页面 JS 异常和 CDP 协议错误先按可信类型分类，不得因消息中出现 websocket/eof 等关键词升级为传输失败；仅当前 RPC 失败，daemon 仍可处理后续请求。
- 真正的 EOF、网络断开与现有 typed ErrorTransport 仍触发会话失败；取消和超时保持非 fatal。nil 和包装后的错误保持一致。
### Scope
- 仅 browser 错误分类、daemon 丢失会话判断以及对应永久回归；检查 eval/snapshot/fetch 调用链。保持 RPC 接口、supervisor 策略和其他审查任务不变；不合并 stop/FT1。
### Decisions
- 以 errors.As 检测 runtime.ExceptionDetails / cdproto.Error，优先于不可信消息字符串；typed CDPError 是权威分类，daemon 不得绕过它再次对消息匹配。保留非 CDP 应答错误的兼容性传输兜底。
- 协议级 target 不存在的错误仍应维持 target-local 清理语义；由 coder 读既有测试确定最小兼容分类，所有情况均不得 fatal。
### Checking
- 修改生产代码前运行正确行为永久回归并记录失败；覆盖全部传输关键词、原始/包装异常、typed Request/Timeout/TargetGone，以及真实传输正例。
- daemon fake conn 覆盖 eval/snapshot/fetch 相关失败，断言 JSON-RPC error、fatalCh 空、会话状态不变且下一次请求可成功；检查 ensureBrowserSession 测试不能仅靠 tabHook 旁路掩盖 sessionFailed。
- 完整 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；全部临时文件/隔离 HOME/缓存位于本 worktree 的 TMPDIR。无真实外网依赖，不启动 headless Chrome，不操作用户窗口。真实 Chrome/Docker/systemd 未检查要明确说明。

## factory · 2026-11-22 03:41

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a，未合并 stop/FT1。Claude Code 构建提交：34d596a，已推送分支。

实现：ErrorKindOf 优先采用 typed CDPError、取消/超时、runtime.ExceptionDetails 和 cdproto.Error 分类；JS 异常为 Request，协议 target 消失仍为 TargetGone，真实 EOF/网络 errno 为 Transport，无类型错误保留兼容兜底。daemon 删除第二套字符串判断，遵从统一分类。eval/snapshot/fetch/route 的 CDP 失败统一包装。

决定：JSON-RPC 结构和错误码不变，data.hint 增加操作及 target 前缀，与已有 goto/click 风格对齐。network_route 只统一错误包装，不实施 FT22 的规则回滚。

回归：coder 在生产修改前运行新增永久测试并记录失败，页面异常及协议文本、typed Request/Timeout/TargetGone 曾错误触发 fatal；修复后覆盖全部传输关键词、原始/包装错误、真正传输失败正例。daemon fake conn 挂 tabLive 而非 tabHook，真实检查会话状态，错误后下一 RPC 成功；传输失败后下一 RPC 被拒绝。

测试：coder 全量通过；manager 独立使用全新隔离 HOME/缓存重新执行配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 全量通过，Go 1.27.4，8 个测试包通过，2 个无测试包。diff --check 通过，保护路径无变化，工作树干净；提交仅本项。

安装：无，使用已有 mise Go1.27.4 和 pin 模块缓存；go.mod/go.sum 未变。

未验证：真实非 headless Chrome/CDP 端到端、实际 websocket 断开、Docker、systemd 均未实测；测试使用类型化假连接，无真实外网依赖，未启动 Chrome 或操作用户窗口。供 review 重点检查可信错误分类及 hint 兼容性。

运行偏差：coder 导出的环境表明其测试 HOME/GOCACHE 曾落到系统临时目录，而非指定 TMPDIR；manager 独立验证已严格把 HOME/缓存放在本 run TMPDIR，测试通过。未删除目录外文件；残留应由 owner 确认后清理，不在 build 越界删除。复用此前遗留的干净 FT8 工作树，无遗留改动需恢复。

状态：new/ → review/，第 1 轮；本次仅 build，未 review、未合并 main、未发布。


## factory · review · 2026-11-22 03:44
head: 34d596a4a1b89994b1c0535a0e464cbf0bbfeef6
verdict: pass
notes:
- [P2] internal/daemon/browser_reconnect.go:46: isCDPSessionLost 在 ErrorKindOf 前检查 context.Canceled/DeadlineExceeded，因此显式 ErrorTransport 包装这两类错误时仍非 fatal，与统一分类优先级不一致。临时 overlay fake conn 复现：ErrorKindOf=Transport、fatalCh 为空、后续 eval 成功。建议直接采用 ErrorKindOf(err)==ErrorTransport 并补组合回归；当前真实 EOF/网络断开正例通过，此项非阻断。
- [P3] internal/daemon/cdp_error_classification_test.go:207: 新增 RPC 回归只断言通用错误码，未锁定 error.message 和 error.data.hint；建议按方法检查 eval failed/snapshot failed 等消息以及 hint 包含原始异常文本，允许操作/target 前缀。当前实现保留异常描述，此项非阻断。

Codex 只读审查 origin/main...HEAD，基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a；无 P0/P1，页面异常/协议错误中的全部传输关键词不触发 fatal，真正 io.EOF、关闭网络连接、broken pipe 仍 fatal。与 main 合并无冲突，无需修改或推送代码分支。

测试：Codex 使用 Go 1.27.4 完整执行 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 通过（8 个测试包、2 个无测试包）；额外临时 overlay 观测未改工作树；manager 确认 HEAD、工作树干净、diff --check 通过及远端分支 head 一致。未发现 review-code skill，按需求/计划直接审查。HOME/可写缓存/临时文件均在 review run 指定 TMPDIR 内；额外安装：无。

未验证：真实非 headless Chrome/CDP 端到端、实际 WebSocket 断链、Docker、systemd；未启动浏览器或操作用户窗口。无 linked issue/PR，未发 GitHub 评论。

状态：review/ → ready/，第 1 轮；请 owner 查看两项非阻断建议及未实测范围。本次仅 review，未合并 main、未发布。


## factory · 2026-11-22 06:08
owner 授权：“先把 ready 的 ship 掉”。本次固定合并 FT8、FT9、FT11、FT12、FT14、FT15；review head 与所合并远端分支一致。

已合并并推送 main；本项 merge commit：2927998，本批 main 最终提交：8836949。配置 ship=["merge", "push"]，未打 tag、未触发版本发布、未部署。无 linked issue/PR 需关闭，分支按配置保留。

逐项合并后执行完整 gofmt/vet/race 测试，最终累计 main 配置测试再次通过（部分缓存）；git diff --check 通过，保护路径未改，工作树干净。FT12 的 go.mod 冲突及 FT14 的 README/run_test.go 冲突由 CC 仅解决整合冲突，保留双方依赖、文档与全部回归；FT9 与 FT14 测试拆文件并共存通过。无新增未 pin 工具或依赖。

review 中已有 P2/P3 未在本次 ship 修复，继续保留原记录。真实 Chrome/CDP、Docker、systemd 运行时未验证。ready/ → done/。
