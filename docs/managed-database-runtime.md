# Billing 数据库交接运行角色

Billing 与 Accounts 使用相同的显式环境变量合同，入口和后台任务必须一起切换。

| 变量 | 合同 |
| --- | --- |
| `DATABASE_RUNTIME_ROLE` | `primary` 或 `standby`；未配置时保留现有运行方式 |
| `DATABASE_BACKGROUND_WRITERS` | 必须显式 `true` / `false`；待机只允许 `false` |
| `DATABASE_URL` | 唯一连接；必须清空 `SUPABASE_CONNECT_URI` 与 `SUPABASE_CONNECT_URL` |
| `DATABASE_IDENTITY_SHA256` | migratectl 同格式的 Host/Port/Database/Role JSON SHA256；不包含密码 |
| `IMAGE` | 预构建镜像身份；探针复用已有 IMAGE 解析结果，不推测发布 SHA |

## 主库角色

主库仅接受 Selfhost `account` 数据库。启动只读校验 PostgreSQL 17、唯一且干净的 `2026100701` 检查点、共享的 53 张业务表范围，以及 Billing 自己的 `cloud_vendor_costs` 列、主键、幂等唯一键与完整读取权限。Accounts 负责完整编译目录校验，调用方必须绑定同一目标连接和全业务一致性凭证。Billing 不执行建表、修复、迁移或默认样本写入。

`DATABASE_BACKGROUND_WRITERS=false` 暂停 exporter 拉取、FinOps 云账单同步及欠费停服扫描。主库业务 API 仍然可写，只有完成来源冻结、最终追平、全业务一致性与单写者验证后才允许开放入口；后续单独启用后台任务。关闭后台任务本身不是来源冻结或单写者证明。

## 待机角色

待机只提供 `/api/ping`、`/healthz`，不连接数据库、不构造业务服务、不启动后台任务；`/readyz` 及全部业务请求返回 503。可用于退出 Serverless 的业务承载，但其他旧 Cloud Run revision、Supabase API/直接 SQL 写者以及采集器仍需单独冻结和验证。

探针包含 `database_role`、`bootstrap_writes=false`、`background_writers`、`business_requests_enabled`、`configured_database_sha256` 与 `schema_version`。待机版本为 0，表示未连接和校验业务库。连接指纹只说明配置路由，不能代替物理数据库身份或全业务数据一致性。

## 验证与发布边界

CI 使用新建的临时 PostgreSQL 17 数据库、固定 Accounts 原生 SQL SHA256 与 Billing SQL SHA256。仅 SELECT 的应用角色启动后，所有业务行、schema 检查点、列、约束、索引与函数摘要必须完全相同；待机使用不可达数据库仍须正常提供探针并拒绝业务请求。镜像发布依赖此验证。生产验收仍需实际固定镜像部署、来源冻结与全业务凭证，代码或 CI 通过不能视为生产切换完成。

未配置角色的原有路径标记为 LEGACY；满足实际部署和回退验证门槛后再移除。任何回退都不得自动恢复已经过时的 PROD Supabase 数据读取。

## Standby configuration credentials

The `standby` role can load without `INTERNAL_SERVICE_TOKEN`: it creates no business handler/exporter and rejects every business request. `primary` and the existing unmanaged business role still require that token. A standby qualification uses a password-free, unreachable loopback database URL and no production service credential. This change does not enable business requests, database access, background writers or cutover.
