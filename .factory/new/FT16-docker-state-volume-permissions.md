# [P2] 官方 compose 示例的 named volume 对镜像内 nonroot 用户不可写，daemon 启动即退出

## factory · 2026-11-22 01:11
owner 已要求将 main 全库 CC review 报告中的 bug 逐项放入 new/。本任务对应 B09；固定审查基线 main@d55fbc55162db73a43c01e1a6b782750437cfb5a，从最新 main 独立构建，不合并 stop/FT1 的累计分支。

报告：[main CC review](../reports/main-2026-11-21-d55fbc5/cc-main-review.md)；原始等级 P2。位置：`deploy/docker-compose.example.yml:25`（基线行号，实施前重新定位）。

触发条件：用仓库 Dockerfile 构建镜像，按 deploy/docker-compose.example.yml（或 README.md:124-140 的同款示例）启动：BB_BROWSER_STATE_DIR=/var/lib/bb-daemon + `bb-state:/var/lib/bb-daemon`。

实际行为：镜像基于 distroless/static:nonroot、USER nonroot（Dockerfile:1,4），镜像内不存在 /var/lib/bb-daemon；Docker 在该路径新建的 named volume 归 root:root 0755，uid 65532 无法写入。store.Open 的 MkdirAll/dirWritable 失败（internal/store/store.go:105-110），NewServer 返回错误，进程打印 `server: rpc log store: state dir … is not writable` 并以 1 退出。README.md:143 还明确写“或使用 named volume”即可。

期望行为：示例 compose 开箱可启动，状态目录可写并持久化。

影响：按官方示例部署的容器反复重启/CrashLoop，无法提供服务。

根因：镜像未预建归 nonroot 所有的状态目录，示例把卷挂在镜像中不存在的路径上。

修复方向（建议，非指定实现）：在 Dockerfile 中预建并 chown 65532 的状态目录（多阶段 `COPY --chown=65532:65532` 一个空目录到 /var/lib/bb-daemon），或把示例改到 /home/nonroot 下；同步修正 README.md:143。

回归验收：构建镜像后 `docker compose -f deploy/docker-compose.example.yml up`（配一个可达的 Chrome 或仅看启动日志）不出现 `rpc log store` 错误；`docker run --rm -v bb-state:/var/lib/bb-daemon --entrypoint /usr/local/bin/bb-daemon <image> --debugger-url 127.0.0.1:1` 的失败原因是 cdp attach 而不是 state dir；卷内出现 rpc.jsonl 且属主为 65532。

证据：static-deploy-docs.log（Dockerfile/compose/README 行号 + 本地二进制在不可写 state dir 上的退出输出）。均位于报告同目录。

执行配置完整 gofmt/vet/race 测试；把对应正确行为断言转换为永久回归，修复前失败、修复后通过。不可把预期诊断 FAIL 当作现有测试失败，也不可把诊断 PASS 当作安全通过。真实 Chrome / Docker / systemd 未验部分必须如实记录。不得使用 headless Chrome；需要真实 Chrome 时仅使用隔离的非 headless 测试实例，不操作用户窗口。
