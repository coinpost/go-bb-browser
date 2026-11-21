---
branch: factory/FT16-docker-state-volume-permissions
round: 1
---
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


## factory · plan · 2026-11-22 05:41

### Behaviour
- 新镜像在 /var/lib/bb-daemon 预置 uid/gid 65532 可写状态目录；首次普通 named volume 挂载继承该目录所有权，daemon 可创建 rpc.jsonl，容器重建后保留状态。
- 保持 distroless/nonroot 运行，不引入 root 入口、运行时 chown 或 shell；错误 CDP 端点不再被状态目录权限错误遮蔽。
- 已有 root-owned 卷、bind mount 或 volume nocopy 不自动修复；文档说明预先备份并校正权限，不能删除数据换取通过。空卷首次运行和旧卷边界须明确。
### Scope
- 仅 Dockerfile、必要构建上下文资产/GoReleaser上下文设置、compose/README 说明和永久部署回归；不改 Go 业务、其他审查项、受保护路径或 stop/FT1。
### Decisions
- 优先在镜像构建阶段创建 nonroot 所有目录；检查 GoReleaser 的隔离构建上下文，任何 COPY 新资产必须在那里同样可用。选不需额外联网构建阶段、且容易验证的最小方案。
- Docker 可用则用隔离镜像/全新专用卷验证所有权、写盘及重用，不启动/改动用户 Docker 服务；不可用则记录静态验证局限，不伪装实际容器验收。
### Checking
- 先添加正确行为永久回归并记录修复前失败；静态检查部署契约纳入 go test ./...，避免仅凭字符串包含宣称容器成功。
- 可用时实测 Docker 构建、nonroot uid/gid、新卷创建 rpc.jsonl 和重用；CDP仅本地 fake 或故意不可达，不依赖外网测试、不启动 headless Chrome。
- 配置全量 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...；测试 HOME/缓存/临时文件在 TMPDIR 下；明确真实 Chrome/Docker/systemd 未测项。

## factory · 2026-11-22 05:41

基线：main@d55fbc55162db73a43c01e1a6b782750437cfb5a，独立构建，未合并 stop/FT1。Claude Code 提交：3fd4f0f、7383fec；head: 7383fec。

实现：Dockerfile 用 FROM scratch + WORKDIR 创建空状态目录，在最终 distroless 镜像以 COPY --from=state-dir --chown=65532:65532 放到 /var/lib/bb-daemon；保持 USER nonroot:nonroot，无运行时 root/chown/shell，不增加构建上下文资产或额外镜像。GoReleaser 隔离上下文仍只用两个二进制。README/compose 说明新空卷、旧卷、bind mount、nocopy 的边界，并提示先备份、确认带项目名前缀的实际卷名后原地校正权限，不能删除数据换取通过。

决定：不改 daemon 默认 StateDir、不添加 VOLUME、不改 Go 业务或其他任务。manager 要求将初版415行通用解析测试精简为120行针对性静态断言，并纠正维修命令的 compose 卷名前缀，已完成后续提交，无改写历史。永久测试明确命名 TestStatic，仅锁定 Dockerfile/compose/GoReleaser 的文本契约，等价重排也可能需同步更新，不能代替引擎验收。

回归证据：先加永久正确断言、生产修复前 go test ./deploy 失败，指出最终镜像缺少 nonroot 状态目录；精简后的最终回归对基线 Dockerfile 再跑仍失败，恢复修复后通过。GoReleaser 上下文检查基线原已通过，不当作修复证明。日志保留在 build run evidence/FT16-7383fec/。

测试：coder 两次配置完整 gofmt/vet/race 通过；manager 独立再次执行 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... 退出0，9个测试包通过、2个无测试包，多数命中缓存；Go1.27.4。git diff --check 通过，保护路径无差异，工作树干净，提交仅本项。测试 HOME、缓存、临时文件在本 run TMPDIR 下，GOPROXY=off。

安装：无；已有 mise Go1.27.4 和 pinned 模块缓存，go.mod/go.sum 未变。

未检查：本机无 Docker/podman/colima/nerdctl，未启动服务；镜像实际构建、GoReleaser/buildx、目录实际uid/gid、named volume属主继承、rpc.jsonl写入/重用、compose启动、不可达CDP错误分类及文档维修命令均未运行。COPY --chown 的真实目录结果仅按预期语义推断，review 应在有 Docker 环境补验。真实 Chrome/CDP、systemd 未测，未启动浏览器或操作用户窗口，无UI改动。

状态：代码分支已推送；new/ → review/，第1/3轮。仅build，未执行独立review、未合并main、未发布。


## factory · review · 2026-11-22 06:05
head: 7383fecd8d388fbbca434a72dc8036de44850603
verdict: pass
notes:
- [P2] README.md:149（同 deploy/docker-compose.example.yml:36）：bind mount 宿主机直接 chown 65532:65532 仅适用于 UID/GID 直接映射；启用 userns-remap 时容器 65532 对应其他宿主机 ID，应注明条件并使用映射后的宿主机 UID/GID。
- [P3] README.md:145（同 deploy/docker-compose.example.yml:28）：Docker 初始化按卷是否为空而非是否已存在判断；旧镜像留下的空 root 卷默认挂载时仍可从新镜像初始化。应区分空卷/非空卷，只在实际权限仍错误时修复。
- [P3] deploy/deploy_test.go:53：只断言最终阶段曾出现 USER nonroot:nonroot，末尾再加 USER root 仍通过；建议检查最后一条 USER 指令。

Codex 独立只读审查 origin/main...HEAD 全部4个文件，无 P0/P1。分支与 origin/main 合并检查无冲突，未改文件/提交代码。构建时目录预置及 COPY --chown 逻辑、GoReleaser 两架构隔离上下文经代码审查未发现阻断；静态断言不代表引擎验收。

测试：Codex 使用 Go1.27.4、TMPDIR 下隔离 HOME/缓存执行完整 test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...，退出0；同一测试对 origin/main Dockerfile 验证预置属主断言失败，对 HEAD 3个部署静态检查通过。manager 确认 HEAD、工作树干净及 git diff --check 通过。安装：无新增工具或 manifest 变更。

未验证：没有可用 Docker 引擎，未启动服务；GoReleaser/buildx 双架构镜像构建、新空 named volume 的真实 UID/GID、rpc.jsonl 写入及重建持久化、compose 启动和维修命令未实测。真实 Chrome/CDP、systemd 未测，无 UI 改动。owner 应重点在 Docker 环境补验并看上述非阻断文档/测试建议。review/ → ready/，未合并 main、未发布。
