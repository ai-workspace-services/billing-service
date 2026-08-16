#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}/.."

: "${GCP_PROJECT:?set GCP_PROJECT, for example xzerolab-480008}"
: "${CLOUD_RUN_IMAGE:?set CLOUD_RUN_IMAGE}"

command -v gcloud >/dev/null 2>&1 || {
  echo "gcloud is required for Cloud Run builds" >&2
  exit 1
}

gcloud builds submit \
  --project "${GCP_PROJECT}" \
  --tag "${CLOUD_RUN_IMAGE}" \
  .
