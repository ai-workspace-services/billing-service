# billing-service

`billing-service` is the v1 minute-delta and replay-safe writer for the Cloud
Network Billing & Control Plane.

It accepts normalized snapshots from Vector, computes deltas from cumulative
counters, and writes idempotent usage and billing facts into the existing
`accounts.svc.plus` PostgreSQL schema. Direct pulls from `xray-exporter` remain
available only as an explicit compatibility mode.

Deployment note:

- `billing-service` must point either `SUPABASE_CONNECT_URI` (Supabase Cloud)
  or `DATABASE_URL` (VPS/self-hosted fallback) at the already provisioned
  `accounts.svc.plus` database. `SUPABASE_CONNECT_URL` is accepted only as a
  compatibility alias.
- it should not be used to create or bootstrap schema on production hosts
- the accounting tables it reads and writes are expected to exist before the
  service starts

For Supabase Cloud runtime, inject the Session pooler URI through
`SUPABASE_CONNECT_URI`. Keep the migration/backup-only `DATABASE_DIRECT_URL`
outside the service runtime; `DATABASE_SESSION_POOLER_URL` is the Vault source
used to populate the runtime URI.

## Endpoints

- `GET /api/ping`
- `POST /v1/jobs/collect-and-rate`
- `POST /v1/jobs/reconcile`
- `POST /v1/ingest/snapshots` (Bearer `INTERNAL_SERVICE_TOKEN`)
- `GET /healthz`
- `GET /v1/status`

## Documentation

- `docs/design.md` - current implementation design, main collect-and-rate flow,
  idempotency rules, and module boundaries
- `docs/reference/` - code-level reference for `cmd/` and `internal/`
  packages, including types, interfaces, and functions
- `docs/README.md` - documentation index and verification notes
- `docs/architecture.md` - deployment and data-flow diagrams
- `docs/api.md` - task API surface and upstream/downstream boundaries
- `sql/billing-service-schema.sql` - bootstrap/reference DDL aligned with the
  current `accounts.svc.plus` accounting schema
- `docs/tasks/2026-08-02-vector-billing-ingest.md` - Vector fan-out contract

## CI/CD 与 Vault 鉴权 (Vault OIDC Role)

本仓库的持续集成流水线 (`.github/workflows/ci-pipeline.yml`) 使用 GitHub Actions OIDC 机制与 HashiCorp Vault (`vault.svc.plus`) 进行无密钥身份认证。

为了遵循最小权限原则（Least Privilege）和环境隔离，本仓库的 CI 拥有独立的 Vault Policy 和 Role，具体安全约束如下：

1. **凭据访问范围（路径隔离）**
   - CI 流水线仅拥有 `kv/data/CICD` 的**只读**权限。
   - 该路径仅包含基础的公共服务凭据（例如 GHCR_USERNAME 和 GHCR_TOKEN），用于构建完成后推送镜像。
   - CI 无法读取任何环境特有的底层云资源凭据、Terraform State 或主机 SSH 部署私钥。

2. **身份铸造限制（绑定收紧）**
   - 本服务在 Vault 中对应 3 个独立环境的 Role（`sit`、`uat`、`prod`）。
   - **`job_workflow_ref` 白名单钉死**：Vault 强制校验调用方的流水线文件。只有本仓库白名单内的流水线（即 `ci-pipeline.yml`）发起的请求才能成功换取 Token。
   - 仓库内任何人**新增**或**重命名**未经授权的 workflow 文件，皆无法绕过限制获取凭据。

> **⚠️ 排障指南 (403 Forbidden)**
> 如果 CI 流水线在 `Fetch Vault Secrets` 步骤报 `403` 权限拒绝，请确认：
> 1. 请求的凭据路径是否超出了 `kv/data/CICD` 层的范围。
> 2. 流水线文件名称或仓库名称是否发生了变更。
> 
> 如果确需修改流水线名称，必须由管理员在 `platform-ops-toolkit` 仓的 `docs/tasks/vault_service_repo_roles.sh` 中更新白名单，并重新对 Vault 服务端执行该脚本。
