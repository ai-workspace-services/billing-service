# Task: Multi-Cloud FinOps Billing Sync
**PR ID**: [#6](https://github.com/ai-workspace-services/billing-service/pull/6)
**Date**: 2026-07-11
**Status**: Completed (Foundation)

## Background
The previous billing service implementation only handled end-user proxy/VPN traffic billing via the `traffic_minute_buckets` and `billing_ledger` tables. To support global FinOps auditing and AI querying of infrastructure costs, we needed a decoupled architecture to synchronize the real bills from cloud providers (AWS, GCP, Azure) into PostgreSQL.

## Implementation Details

### 1. Schema Design
Added the `cloud_vendor_costs` table to `sql/billing-service-schema.sql` to support heterogeneous multi-cloud billing data:
- Unified fields: `provider`, `account_id`, `service_name`, `region`.
- Introduced a unique constraint `uq_cloud_vendor_cost_period` for Postgres UPSERT. This provides idempotency so that cron retries do not generate duplicate financial records.

### 2. Backend Model & Repository
- **Model**: Defined `CloudVendorCost` struct in `internal/model/finops.go`.
- **Repository**: Implemented `UpsertCloudVendorCost` in `internal/repository/postgres.go` using prepared statements to prevent SQL injection.
- **Mocking**: Updated `memoryRepo` in `service_test.go` to ensure all existing unit tests pass cleanly.

### 3. Background Syncer Service
- Created `internal/service/finops.go` containing the `FinOpsSyncer` daemon.
- Integrated the syncer into the application lifecycle in `cmd/billing-service/main.go` using a daily ticker.
- Scaffolded stub methods for `syncAWS`, `syncGCP`, and `syncAzure`. The AWS method includes a complete demonstration of injecting dummy data into the Postgres database.

## Next Steps
- Integrate `aws-sdk-go-v2/service/costexplorer` to replace the AWS stub.
- Integrate Google Cloud Billing / BigQuery APIs for GCP.
- Integrate Azure Cost Management APIs for Azure.
- Pipe Vault configuration (e.g. `finops_vault_secret`) from `.env` into `config.go` to authenticate the syncers.

## Related Artifacts
- Implementation Plan: `implementation_plan.md`
- Code walkthrough: `walkthrough.md`
