# Cloud Run deployment

The VPS `docker run`/Compose deployment remains the default runtime contract.
The files under `deploy/gcp/cloud-run/` are a separate Cloud Run contract:
Cloud Run supplies `PORT=8080`, while the database URI and internal token come
from Secret Manager.

Build and deploy the preview service:

```bash
make cloudrun-build CLOUD_RUN_ENV=preview
make cloudrun-deploy CLOUD_RUN_ENV=preview
```

For production, use `CLOUD_RUN_ENV=prod`. Override `GCP_PROJECT`,
`GCP_REGION`, `CLOUD_RUN_SERVICE`, or `CLOUD_RUN_IMAGE` when the project or
Artifact Registry path differs.

Required Secret Manager secrets:

- `billing-database-url`: the runtime PostgreSQL/Session pooler URI
- `internal-service-token`: the service-to-service bearer token

The Cloud Run service is configured for push ingestion. Pull mode remains
available in the existing VPS deployment through its normal environment
variables.
