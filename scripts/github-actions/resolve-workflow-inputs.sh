#!/usr/bin/env bash
set -euo pipefail

deploy_env="sit"
push_image="false"

if [[ "${GITHUB_EVENT_NAME:-}" == "workflow_dispatch" ]]; then
  deploy_env="${INPUT_DEPLOYMENT_ENVIRONMENT:-uat}"
  push_image="true"
elif [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" ]]; then
  deploy_env="sit"
elif [[ "${GITHUB_REF:-}" == refs/heads/main || "${GITHUB_REF:-}" == refs/heads/release/* ]]; then
  deploy_env="uat"
  push_image="true"
elif [[ "${GITHUB_REF:-}" == refs/tags/v* ]]; then
  deploy_env="prod"
  push_image="true"
elif [[ "${GITHUB_REF:-}" == refs/tags/prod-* ]]; then
  deploy_env="prod"
  push_image="true"
elif [[ "${GITHUB_REF:-}" == refs/tags/sit-* ]]; then
  deploy_env="sit"
  push_image="true"
elif [[ "${GITHUB_REF:-}" == refs/tags/uat-* || "${GITHUB_REF:-}" == *daily-build-* ]]; then
  deploy_env="uat"
  push_image="true"
elif [[ "${GITHUB_REF:-}" == refs/tags/* ]]; then
  deploy_env="sit"
  push_image="true"
fi

case "${deploy_env}" in
  sit|uat|prod) ;;
  *)
    echo "Unsupported deployment environment: ${deploy_env}. Use sit, uat, or prod." >&2
    exit 1
    ;;
esac

echo "deployment_environment=${deploy_env}"
echo "push_image=${push_image}"
