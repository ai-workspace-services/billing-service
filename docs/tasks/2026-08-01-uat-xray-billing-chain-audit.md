# UAT Xray → Exporter → Billing → PostgreSQL → Accounts → Portal 链路审计

> Status: 🟡 代码侧 P0 已合入；Exporter window API [PR #3](https://github.com/ai-workspace-xstream/xray-exporter/pull/3) [OPEN]；部署/Source routing 仍待独立 PR 与只读验收
> Date: 2026-08-01
> Related PRs: Billing [#24](https://github.com/ai-workspace-services/billing-service/pull/24) [MERGED]；Exporter [#3](https://github.com/ai-workspace-xstream/xray-exporter/pull/3) [OPEN]；Accounts [#45](https://github.com/ai-workspace-services/accounts/pull/45) / [#46](https://github.com/ai-workspace-services/accounts/pull/46) [MERGED]；Portal [#130](https://github.com/ai-workspace-services/portal/pull/130) [MERGED]；GitOps [#129](https://github.com/x-evor/gitops/pull/129) / [#130](https://github.com/x-evor/gitops/pull/130) [MERGED]

本记录基于 2026-08-01 各仓 `origin/main` 的只读检查。没有修改 UAT 数据库、GitOps main、生产 `svc.plus`，也没有重复实现 Billing 的 UUID 聚合修复。

## 基线与已确认完成项

- Billing `origin/main` = `5fbf776dd44d518315355052022aff765e84ce9d`。`processSnapshot` 已先调用 `aggregateSamplesByUUID`，同一 snapshot 内同一 canonical UUID 的多个 inbound 累计上下行先合并；邮箱只保留为观测字段，聚合样本不再携带 `InboundTag`。多 inbound 回归测试已合入，`go test ./...` 通过。
- Accounts `origin/main` = `41d25d20a147c40aa4f5988cb37403677b28caa3`，已持久化 `period_start` / `period_end`，并包含共享 accounting schema 的幂等 bootstrap。
- Portal `origin/main` = `409cfb7dd8dd3aecfa79899ded152d955d2f1c7e`；UAT 已钉住包含月度配额卡的 `daily-build-2026.08.01-r2`。
- 共享 PostgreSQL 仍是 Accounts/Billing 的同一 accounting 数据库；本审计不建议拆库。
- GitOps UAT 版本轴已使用不可变 daily-build tag：Billing `daily-build-2026.08.01-r1` 对应 Billing #24，Accounts `daily-build-2026.08.01-r2`，Console `daily-build-2026.08.01-r2`。这部分没有发现需要直接改 UAT 的安全代码缺口。

## 仍存在的部署/Exporter 契约缺口

### 1. Billing 容器的 exporter 地址不可达

GitOps `compose/web-saas/docker-compose.yml` 的 `billing` 服务声明：

```yaml
EXPORTER_BASE_URL: "http://127.0.0.1:8080"
EXPORTER_SOURCES_JSON: '[{"source_id":"xhttp-local","base_url":"http://127.0.0.1:8080"}]'
```

同一服务加入外部 bridge 网络 `docker_shared_network`，没有 `network_mode: host` 或 `extra_hosts`。因此 Billing 容器内的 `127.0.0.1` 指向 Billing 自己，不是 agent-proxy 主机上由 systemd 监听的 exporter。该配置即使 JSON 只有一个 source，也不能证明 Billing 能访问 exporter。

建议在 GitOps feature PR 中选择一种明确拓扑：

1. 把 exporter 以可发现的网络服务暴露给 web-saas（推荐使用受控内网 DNS/反向代理）；或
2. 明确使用 Linux `host-gateway`/host 网络，并为所有 source 增加认证与网络边界；不能把 `127.0.0.1` 当作跨容器地址。

### 2. 多 source 配置没有覆盖当前两个 exporter 实例

infra `playbooks/roles/vhosts/xray-exporter/defaults/main.yml` 当前声明 XHTTP `127.0.0.1:8080` 与 TCP `127.0.0.1:8081` 两个实例，但 GitOps Billing source 只声明 `xhttp-local`，也没有 `expected_node_id` / `expected_env`。这不满足 UAT 验收契约：每个 source 应有稳定 `source_id`、可达 `base_url`、唯一的 exporter `node_id`、以及 `expected_env=uat`。

如果两个 exporter 是同一物理节点的不同采集面，仍需为 Billing source 选择不会共享 checkpoint 的稳定 node identity；若它们确实代表同一计费 node，则必须先定义跨 source 的累计值语义，不能只复制 source 条目。当前最小安全路径是：每个独立累计计数面使用独立 node id，Accounts 侧再按 canonical UUID 汇总多 node 的分钟桶。

### 3. Billing 依赖的 window API 尚未形成可部署的 exporter main 版本

Billing 的 exporter client 调用 `GET /v1/snapshots/window`。本地 `cloud-neutral-toolkit/xray-exporter` 的 `origin/main` 只声明 `/v1/snapshots/latest`；提供认证 window history 的代码在 `codex/multi-node-billing-ingestion` 分支（`af2719e`），尚未合入 exporter main。

infra role 当前仍从外部 `compassvpn/xray-exporter/releases/.../v0.6.0` 下载二进制，systemd 模板只传旧式命令行参数，也没有渲染新 exporter 所需的 `EXPORTER_NODE_ID`、`EXPORTER_ENV`、`ACCOUNTS_BASE_URL`、`INTERNAL_SERVICE_TOKEN`、`SNAPSHOT_STORE_PATH` 等环境变量。playbook 顶层虽然计算了部分同名 Ansible 变量，但 role template 没有消费它们。

因此下一步必须先完成 exporter 发布契约：合入 window API、发布可追溯的二进制/image，并让 infra role 明确构造每个实例的环境文件/服务参数。仅修改 Billing 的 source JSON 不足以闭环。

## 最小测试门禁（建议作为后续 PR 的验收项）

不触碰 UAT 数据库即可执行：

1. exporter：启动候选二进制，带 Bearer token 请求 `/healthz` 与 `/v1/snapshots/window`；断言返回 `node_id`、`env=uat`，且同一 UUID 的多个 inbound 样本存在并保留邮箱仅作观测。
2. exporter 多实例：每个实例使用独立 `EXPORTER_NODE_ID` 与 `SNAPSHOT_STORE_PATH`，防止 SQLite 历史文件和 Billing checkpoint 串写。
3. Billing：在 Billing 容器的网络命名空间内请求每个 source，而不是在宿主机请求；断言错误 source 不推进该 source watermark，`expected_node_id/env` 不匹配时整页拒绝。
4. 链路验收：同一 UUID 在两个 inbound/两个 node 产生增量后，Accounts `usage/summary` 的总量等于 PostgreSQL 分钟桶的跨 node 聚合；重复窗口重放不增加 ledger 或重复扣配额。
5. UAT 只读探针：`accounts-uat.onwalk.net/api/ping` 与 `/healthz` 当前返回 HTTP 200；但 `/api/ping` 的 image/tag/commit/version 为空，不能作为镜像可追溯与 exporter 链路已通的证据，需在候选发布后补 runtime metadata/日志证据。

## 后续 PR 顺序与边界

1. exporter repo：合入并发布 authenticated window API；补 HTTP contract test 与多实例配置 test。
2. infra playbooks：将 role 改为消费上述 source/env 参数，固定候选 exporter 构建来源，并为每个实例配置唯一 node id、UAT env、独立 history store。
3. GitOps：仅在前两项通过静态/容器网络测试后，提交 feature 分支的 UAT source routing；保留不可变业务镜像 tag。
4. UAT：只读验证 exporter window、Billing source status/watermark、Accounts summary 与 Portal 卡片；不在真实 UAT PG 上执行 bootstrap、清表或修数。

## 明确不做

- 不修改 `gitops` 的 UAT 文件或 main。
- 不修改 UAT/生产 PostgreSQL，不执行真实数据库 DDL/数据修正。
- 不修改生产 `svc.plus` 的 exporter、Billing、Accounts 或 Portal。
- 不回滚或重做 Billing #24 的 `aggregateSamplesByUUID`。
