# main review → 工厂任务映射

基线：`d55fbc55162db73a43c01e1a6b782750437cfb5a`。owner 授权：FT1 → stop；CC 全项目只读审查；每个 bug 一个 new 任务。

| 报告 | 任务 | CC 等级 |
|---|---|---|
| B01 | [FT8](../../new/FT8-typed-cdp-error-classification.md) | P1 |
| B02 | [FT9](../../new/FT9-run-named-arguments.md) | P1 |
| B03 | [FT10](../../new/FT10-persist-managed-target-retirement.md) | P2 |
| B04 | [FT11](../../new/FT11-enumeration-preserves-new-targets.md) | P2 |
| B05 | [FT12](../../new/FT12-snapshot-reference-uniqueness.md) | P2 |
| B06 | [FT13](../../new/FT13-pool-tab-new-distribution.md) | P2 |
| B07 | [FT14](../../new/FT14-run-adapter-error-exit.md) | P2 |
| B08 | [FT15](../../new/FT15-systemd-enable-install.md) | P2 |
| B09 | [FT16](../../new/FT16-docker-state-volume-permissions.md) | P2 |
| B10 | [FT17](../../new/FT17-unknown-tab-lock-retirement.md) | P2 |
| B11 | [FT18](../../new/FT18-jsonl-tail-append-recovery.md) | P3 |
| B12 | [FT19](../../new/FT19-memory-store-bounded-retention.md) | P3 |
| B13 | [FT20](../../new/FT20-private-rpc-log-permissions.md) | P3 |
| B14 | [FT21](../../new/FT21-stable-tab-alias-collisions.md) | P3 |
| B15 | [FT7](../../new/FT7-observation-writer-retirement.md) | P3 |
| B16 | [FT22](../../new/FT22-network-route-failure-rollback.md) | P3 |
| B17 | [FT23](../../new/FT23-adapter-function-wrapping.md) | P3 |
| B18 | [FT24](../../new/FT24-screenshot-mime-extension.md) | P3 |
| B19 | [FT25](../../new/FT25-pool-hung-backend-isolation.md) | P3 |
| B20 | [FT26](../../new/FT26-pool-stale-binding-retirement.md) | P3 |
| B21 | [FT27](../../new/FT27-rpc-timeout-contract.md) | P3 |

共21项：20个新任务 FT8–FT27，加复用并移入 new 的 FT7（B15）。FT2–FT6 保持 backlog，FT4/FT5 追加去重说明；FT1 保持 stop，分支未合并。CC 原始分级保留；B15 原FT7已有P2记录，不据此降级。报告是一次有边界的审查，不代表全库无其他缺陷。17项待确认风险不创建修复任务。

manager 已核对 main HEAD/干净工作树、配置测试日志、18条Go诊断FAIL（B06占两条；另B05为Node）、关键源码及部署文件。未独立复跑全部诊断；不能把静态部署推断称为Docker/systemd实测。B03/B05涉及误关/误操作，实施时不得因CC原始P2评级降低身份安全验收。报告建议是待实施方案，不保证无条件成立（特别是B14别名契约）。
