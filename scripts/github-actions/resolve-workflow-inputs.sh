#!/usr/bin/env bash
set -euo pipefail

deploy_env="sit"
push_image="false"

if [[ "${GITHUB_EVENT_NAME}" == "pull_request" ]]; then
  deploy_env="sit"
elif [[ "${GITHUB_REF:-}" == refs/heads/main || "${GITHUB_REF:-}" == refs/heads/release/* ]]; then
  deploy_env="uat"
  push_image="true"
elif [[ "${GITHUB_REF:-}" == refs/tags/v* ]]; then
  deploy_env="prod"
  push_image="true"
fi

echo "deployment_environment=${deploy_env}"
echo "push_image=${push_image}"
