# Vector fan-out billing ingestion

## Goal

The UAT usage path is:

`Xray -> compassvpn/xray-exporter -> Vector -> billing-service -> PostgreSQL -> Accounts -> Portal`

The real-time path remains independent:

`Xray -> compassvpn/xray-exporter -> Vector prometheus_remote_write -> Observability/Grafana`

Billing must not be a prerequisite for real-time monitoring, and Billing must
not pull the exporter in the default mode.

## Contract

- Exporter pushes one normalized `Snapshot` JSON document to Vector's local
  `http_server` source.
- Vector forwards the document to `POST /v1/ingest/snapshots`.
- Both Vector and Billing use the same `INTERNAL_SERVICE_TOKEN` Bearer token.
- Billing validates `collected_at`, `node_id`, and `env`, then reuses the
  existing checkpoint and deterministic ledger keys for retry idempotency.
- `BILLING_INGEST_MODE` defaults to `push`.
- The legacy pull path is available only with explicit
  `BILLING_INGEST_MODE=pull` and `EXPORTER_BASE_URL`/`EXPORTER_SOURCES_JSON`.

## Rollout order

1. Deploy Billing with `BILLING_INGEST_MODE=push`; verify `/healthz` and the
   authenticated ingest endpoint.
2. Deploy Vector with the UAT Billing sink and local snapshot input.
3. Deploy `xray-exporter` with `VECTOR_SNAPSHOT_URL` pointing at the local
   Vector listener.
4. Generate a small UAT traffic sample and verify Billing's minute bucket,
   ledger, quota state, Accounts usage summary, and Portal account panel.
5. Verify the existing Prometheus remote-write stream and Grafana dashboard
   independently.

## Safety

This change does not alter production Xray or production exporter settings.
The Billing pull implementation remains source-compatible for rollback, but
is not started unless the explicit pull mode is selected.
