# bb-daemon --debugger-url 默认值 127.0.0.1:9222

## Guoyin · 2026-11-26 08:51
工厂 new：bb-daemon flag --debugger-url 设置默认值 127.0.0.1:9222

## factory · 2026-11-26 08:53
将 `bb-daemon` 的 `--debugger-url` flag 默认值设为 `127.0.0.1:9222`。

范围仅限该默认值：未显式传入 `--debugger-url` 时使用 `127.0.0.1:9222`；调用方显式传入时仍使用其给定值。不扩大 flag 设计，不改其他参数、连接逻辑或启动流程。
