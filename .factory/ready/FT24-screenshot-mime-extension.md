---
branch: factory/FT24-screenshot-mime-extension
round: 1
---
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


## factory · plan · 2026-11-22 08:03

**Behaviour**：保持 tab 选择、RPC/base64 错误处理和 `--json` 原样输出。仅当位置路径及 `--output` 都未提供非空路径时，以响应 `mime` 生成默认文件名：`image/jpeg` → `.jpg`，其余（含缺失/未知 MIME）→ `.png`。显式路径不改写，优先级仍为位置参数 > `--output` > 自动命名；错误时不写文件。

**Scope**：仅修改 CLI screenshot 默认命名并添加 fake daemon 永久回归；不改 daemon、协议、格式校验、覆盖策略、输出文案或其他任务。

**Decisions**：以响应 MIME 而不是请求 format 为准；JPEG 统一 `.jpg`；保留 PNG 兼容回退、秒级时间戳和路径优先级。不引入额外抽象。

**Checking**：先用未修复实现运行回归并确认 JPEG 默认名断言失败，再修复；覆盖 jpeg/jpg、png、请求/响应 MIME 不一致、缺失/未知 MIME、显式路径/--output、字节保真、JSON 与错误不落盘。运行完整 gofmt/vet/race；仅使用本地 fake daemon，不需要真实 Chrome。

## factory · 2026-11-22 08:03

已构建并推送 `32aaf22`：自动文件名读取响应 MIME，大小写不敏感且忽略两端空白，`image/jpeg` 使用 `.jpg`，其余保留 `.png`；显式路径与现有 JSON/错误语义保持不变。新增 3 个测试、19 个子用例，验证输出字节未被改写。

修复前 `go test ./cmd/bb-browser -run TestScreenshot` 有 4 个 JPEG 默认名子用例按预期失败：`auto name "screenshot-<ts>.png", want match ^screenshot-\d+\.jpg$`；修复后全部通过。配置命令 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 经 coder 和 manager 分别运行均通过；未修改受保护路径，提交仅含本任务代码与回归。

安装：无；未更改 go.mod/go.sum。使用已有 Go 1.27.4（模块目标 1.26）及既有缓存。测试 HOME/临时文件位于本次专属临时目录。

未验证：真实 Chrome/daemon、Docker、systemd；未改 UI，未操作用户窗口。已从 `new/` 移至 `review/`，供下一轮审查。


## factory · review · 2026-11-22 08:16
head: 32aaf227d0b5beef3df28e70651297d014350555
verdict: pass
notes:
- [P3] cmd/bb-browser/main.go:511: MIME 以 string 解码，非标准响应中的非字符串 mime（例如 42）会让自动命名及显式路径均在写文件前失败；基线忽略该字段。协议内 daemon 始终返回字符串，故非阻塞。可用 json.RawMessage 接收，仅自动命名时尝试解码字符串，无法识别则回退 .png。
- [P3] cmd/bb-browser/screenshot_test.go:187: JSON 用例以 json.Marshal 生成紧凑有序响应再 TrimSpace 比较，不能防止解析后重序列化的未来回归；可改为带内部空白及非排序字段的固定响应，并逐字节比较 stdout 与响应加换行。

Codex 只读审查 origin/main（4f86c1c）...HEAD，未发现 P0/P1。配置完整测试 `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...` 退出 0；新增 19 子用例单独重跑均通过，临时基线副本的 4 个 JPEG 默认名断言按预期失败。另用本地 fake daemon 验证了非字符串 MIME 的差异及带格式 JSON 的当前字节保真行为。工作树干净，无代码提交。

安装：无未锁定工具或依赖清单变更。未验证：真实 Chrome/CDP 端到端、Docker、systemd，未在 Go 1.26 上复测（本轮使用 Go 1.27.4）。无关联 GitHub issue/PR，故无需评论。按无 P0/P1 规则从 review/ 移至 ready/；上述两项 P3 供 owner 参考。
