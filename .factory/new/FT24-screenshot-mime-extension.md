# [P3] `screenshot --format jpeg` 未指定路径时把 JPEG 数据写入 screenshot-<ts>.png

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B18；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P3。位置：`cmd/bb-browser/main.go:521`（基线行号，实施前重新定位）。

触发条件：`bb-browser screenshot --format jpeg`（或 jpg），不带位置参数和 --output。

实际行为：默认文件名固定为 `screenshot-%d.png`（main.go:520-522），与实际格式无关；复现中文件 screenshot-<ts>.png 的内容是 JPEG（FF D8 魔数）。

期望行为：默认文件名的扩展名与实际 MIME 一致（.jpg/.jpeg）；可直接使用 RPC 返回的 mime 字段决定。

影响：下游按扩展名判断类型的工具（上传、图像库、agent 的多模态输入）解析失败或误判。

根因：默认文件名硬编码 .png，未参考 --format 或响应的 mime。

修复方向（建议，非指定实现）：解析响应中的 mime（image/jpeg → .jpg，否则 .png）生成默认文件名。

回归验收：CLI 测试：fake daemon 返回 mime=image/jpeg 时生成的默认文件以 .jpg 结尾且内容为 JPEG；png 时仍为 .png；显式路径/--output 不被改写。

证据：repro-go.log（TestAudit_ScreenshotJPEGDefaultNameMustNotBePNG FAIL）；repro/cli_audit_test.go。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
