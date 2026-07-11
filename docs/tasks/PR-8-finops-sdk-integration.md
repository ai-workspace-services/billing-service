# Task: Multi-Cloud FinOps API Integration
**PR ID**: [#8](https://github.com/ai-workspace-services/billing-service/pull/8)
**Date**: 2026-07-12
**Status**: Planning / In Progress

## Background
Following the foundation laid out in PR #6, we have the `cloud_vendor_costs` table and the `FinOpsSyncer` daemon scaffolding ready in the `billing-service` repository. The goal of this task is to replace the stub implementations with authentic cloud provider SDK integration, allowing the system to automatically synchronize infrastructure billing data from AWS, GCP, and Azure into PostgreSQL.

## Implementation Plan

### 1. Dependency Management
Introduce official cloud provider Go SDKs:
- **AWS**: `github.com/aws/aws-sdk-go-v2` suite (specifically the `costexplorer` service).
- **GCP**: `cloud.google.com/go/bigquery` (since BigQuery export is the recommended approach for accurate billing logs).
- **Azure**: `github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/consumption/armconsumption`.

### 2. Configuration & Secrets Injection
Extend `internal/config/config.go` to capture FinOps credentials exported from Vault:
- `AWSAccessKeyID`, `AWSSecretAccessKey`
- `GCPCredentialsJSON`, `GCPBillingProject`, `GCPBillingDataset`, `GCPBillingTable`
- `AzureTenantID`, `AzureClientID`, `AzureClientSecret`, `AzureSubscriptionID`

### 3. API Integration Strategies
To account for standard billing calculation delays across all cloud providers, the synchronization strategy will use a **T-2 (Two days ago)** lookup window.
- **AWS**: `costexplorer.GetCostAndUsage` grouped by `SERVICE` with unblended amortized costs.
- **GCP**: Execute BigQuery SQL statements via the BigQuery SDK against the billing export dataset to retrieve grouped costs.
- **Azure**: `armconsumption.UsageDetailsClient` fetching daily usage and costs by resource/service.
All results will be converted to `model.CloudVendorCost` and upserted into the database.

## Open Questions & Review
Before moving to full execution, we need confirmation on:
1. **GCP Billing Setup**: ~~The user initially requested to directly use the GCP Cloud Billing API instead of BigQuery...~~ **Resolved**: User has accepted the BigQuery export plan, noting that it should be configured to minimize extra costs (which is negligible for standard billing logs).
2. **T-2 Time Window**: Defaulting to T-2 lookup to prevent incomplete billing data due to cloud reconciliation delays.

## Progress
- [x] Branch `feature/finops-api-integration` created and Draft PR #8 opened.
- [x] Initial implementation plan documented.
- [x] Resolving technical blocker regarding GCP API limitations (BigQuery approved).
- [x] Execute SDK integration and configurations (AWS, GCP, Azure SDKs embedded).
- [x] Compilation and mocking verification completed.
