# billing-service

`billing-service` is the v1 minute-delta and replay-safe writer for the Cloud
Network Billing & Control Plane.

It pulls windowed normalized snapshots from one or more `xray-exporter`
sources, computes deltas from cumulative counters, and writes idempotent usage
and billing facts into the existing `accounts.svc.plus` PostgreSQL schema.

Deployment note:

- `billing-service` must point `DATABASE_URL` at the already provisioned
  `accounts.svc.plus` database
- it should not be used to create or bootstrap schema on production hosts
- the accounting tables it reads and writes are expected to exist before the
  service starts

## Endpoints

- `GET /api/ping`
- `POST /v1/jobs/collect-and-rate`
- `POST /v1/jobs/reconcile`
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
