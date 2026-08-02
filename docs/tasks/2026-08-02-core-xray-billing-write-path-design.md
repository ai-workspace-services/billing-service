# 核心数据写入设计：Xray → Exporter → Billing → PostgreSQL → Accounts → Portal

## 目标与边界

本设计只解决“Xray 采集统计如何可靠写入共享 PostgreSQL，并被 Accounts/Portal 展示”。Billing 与 Accounts 共用同一个 PostgreSQL，不拆库、不新增计费服务。

```text
计费事实：Xray → compassvpn/xray-exporter v0.6.0 → Billing → PostgreSQL → Accounts → Portal
观测旁路：Xray → compassvpn/xray-exporter → Vector/Observability → Grafana
```

Vector、Observability、Grafana 不得成为计费落库的前置依赖。

## 1. 身份与聚合契约

### 1.1 canonical identity

- Xray 原始 client 同时带 `email` 和 proxy UUID。
- Exporter 从 Accounts internal identity endpoint 获取映射：`proxy UUID/email → account UUID`。
- Billing 表的 `account_uuid` 必须使用 Accounts 的 canonical account UUID；email 只作为显示/诊断字段，不能作为账单主键。
- 无法映射到 canonical UUID 的样本不得写入 UUID 外键表，应记录 source error/丢弃计数，避免产生孤儿账单。

### 1.2 多节点、多 inbound

Exporter 可以产生一个 `(node_id, uuid, inbound_tag)` 样本；Billing 的计费粒度是账户，不是线路：

```text
同一 snapshot 内：
  (uuid, xhttp) + (uuid, tcp) → (uuid, uplink_sum, downlink_sum)
跨节点：
  node-a/(uuid) + node-b/(uuid) → 两个节点的分钟桶；Accounts 查询时按 account_uuid 汇总
```

- `aggregateSamplesByUUID` 必须在 `processSnapshot` 内、读取 checkpoint 前执行；
- checkpoint key 是 `(environment + node_id, account_uuid)`，不能加入 inbound；
- minute bucket key 保留 `node_id`，以便跨节点审计，同时 Accounts 按 `account_uuid` 汇总；
- 当前不按线路计费，聚合样本的 `line_code` 置空；未来引入线路配额时，应整体增加 line 维度并重新设计 checkpoint。

## 2. Billing 读取与增量计算

每个 exporter source 有独立的 `billing_source_sync_state`。Billing 周期性拉取：

```text
GET /v1/snapshots/window?since=<UTC>&until=<UTC>&limit=<n>&cursor=<time>
Authorization: Bearer <internal token>
```

读取窗口保留约 2 分钟 overlap，依靠分钟桶/ledger 的确定性幂等键消除重复，而不是依赖“上次请求一定成功”。

对每个 `(storage_node_id, account_uuid)`：

```text
delta_up   = current_uplink_total   - checkpoint.last_uplink_total
delta_down = current_downlink_total - checkpoint.last_downlink_total
```

- delta 为负：视为 exporter/Xray counter reset，只更新 checkpoint/reset_epoch，不计负流量；
- delta 非负：写入本分钟事实；
- 首次 checkpoint 的策略必须明确。当前实现会把首个累计值作为 delta；UAT 首次启用前应确认 exporter 累计值起点，避免把历史日志重复计费。

## 3. PostgreSQL 写入顺序与一致性

### 3.1 目标事务单元

一个 `(source, snapshot, storage_node_id, account_uuid)` 应成为一个可重试事务：

1. 读取 checkpoint、billing profile、quota state；
2. 计算 delta 与 effective pricing；
3. upsert `traffic_minute_buckets`；
4. insert/upsert `billing_ledger`；
5. 只有 ledger 首次插入时扣减 `account_quota_states`；
6. 更新 checkpoint；
7. 提交事务。

### 3.2 必须保持的唯一键

| 数据 | 关键键 | 作用 |
|---|---|---|
| checkpoint | `(node_id, account_uuid)` | 累计计数器转 delta；每节点独立 |
| minute bucket | `(bucket_start, node_id, account_uuid, region, line_code)` | overlap/replay 幂等、节点审计 |
| ledger | `id = deterministic(bucket)` | 一分钟账单事实唯一 |
| quota state | `account_uuid` | 当前周期余额/欠费/限流状态 |
| source sync | `source_id` | 每个 exporter source 独立推进窗口 |

### 3.3 当前实现的 P0 风险

当前 `processSample` 已按固定顺序调用多个 repository upsert，但 repository 尚未暴露统一事务边界。若 ledger 已提交、quota 更新失败，重试时看到 `ledgerExisted=true` 可能跳过 quota 更新，形成 ledger 与 quota 不一致。

P0 修复方向：

- 为 repository 增加 `WithTx/ProcessRatedSample` 原子操作；
- 在同一 PostgreSQL transaction 内完成 bucket、ledger、quota、checkpoint；
- 使用 `INSERT ... ON CONFLICT DO NOTHING RETURNING` 判断 ledger 是否首次插入；
- 重放已存在 ledger 时，必须能校验/修复 quota，而不是单纯跳过；
- 保留 memory repository 测试，并增加 PostgreSQL transaction failure/retry acceptance test。

如果暂时不能引入 transaction，最低临时方案是让 quota 更新可依据 deterministic ledger 重建；这只能降低风险，不等价于事务。

## 4. Accounts 读模型

Accounts 读取同一共享 PostgreSQL，`GET /api/account/usage/summary` 返回：

- `totalBytes/uplinkBytes/downlinkBytes`：账户所有已落库分钟桶的权威累计统计；
- `includedQuotaBytes/remainingIncludedQuota/usedBytes/usagePercent`：当前配额周期 O(1) 汇总，`usedBytes = max(included - remaining, 0)`；
- `periodStart/periodEnd`：由订阅 entitlement reset 写入，Stripe 周期优先，无订阅时自然月兜底；
- `lastBucketAt/syncDelaySeconds`：采集新鲜度；
- `billingProfile/quota state`：套餐、余额、限流和欠费状态。

`periodStart/periodEnd` 属于 Accounts 的 quota-grant 字段；Billing 只消费 quota/profile，不负责 Stripe 周期。

Portal 的“AUTHORITATIVE USAGE”展示 `totalBytes`；“MONTHLY QUOTA”展示 `usedBytes/includedQuotaBytes`，不能用历史全量 `totalBytes` 计算百分比。未初始化时显示 loading/暂无采集/尚未初始化，不伪造为已采集 0。

## 5. Portal 展示契约

Portal 不直连 PostgreSQL、不读取 Grafana：

```text
Browser /panel/account → Portal /api/account/* proxy
  → Accounts /api/account/usage/summary → shared PostgreSQL
```

新增统计卡可以调整布局，但必须保留现有订阅、账户、连接、登录和安全功能。最小展示：权威总流量、当前周期用量/配额进度、剩余配额与周期结束时间、数据源/同步延迟/Billing 状态。

## 6. 端到端验收顺序

### P0：写入正确性

1. 同一 UUID 两个 inbound 只生成一份账户 delta；
2. 两个 exporter node 同一账户分别落桶，Accounts 汇总为总量；
3. 重复拉取同一窗口，bucket/ledger/quota 不重复扣减；
4. counter reset 不产生负账单；
5. 模拟 ledger 成功、quota 失败后重试，最终状态一致（事务测试）。

### P1：读模型与周期

1. Accounts 可读取共享 schema 的依赖表；
2. invoice.paid/订阅激活写入 period bounds 并重置 quota；
3. summary 的 `usedBytes` 与 quota remaining 一致；
4. Portal `/panel/account` 展示非零值和 reset 时间。

### 旁路：实时观测

1. Vector 采集 exporter `/scrape`；
2. node-exporter/process-exporter 本地 endpoint 健康；
3. Grafana 只核对实时指标与 Billing 落库结果的数量级，不作为 billing truth。

## 7. 当前状态

- 已完成：Exporter 基于 `compassvpn/xray-exporter v0.6.0`、canonical identity、snapshot window、多 inbound UUID 聚合、Billing 多 source 配置。
- 已完成：Accounts quota period fields、summary contract、Portal quota cards 的最小实现。
- 需要优先补齐：Billing 多表写入的事务/失败恢复语义。
- UAT Vector `9100/9256` 的 503/timeout 属于旁路观测问题，单独处理，不改变计费写入依赖边界。
