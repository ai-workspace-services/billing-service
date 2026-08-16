SHELL := /usr/bin/env bash

GCP_PROJECT ?= xzerolab-480008
GCP_REGION ?= asia-northeast1
CLOUD_RUN_ENV ?= preview
CLOUD_RUN_SERVICE ?= $(if $(filter prod,$(CLOUD_RUN_ENV)),billing-svc-plus,preview-billing-svc-plus)
CLOUD_RUN_SERVICE_YAML ?= deploy/gcp/cloud-run/$(CLOUD_RUN_ENV)-service.yaml
CLOUD_RUN_IMAGE ?= $(GCP_REGION)-docker.pkg.dev/$(GCP_PROJECT)/cloud-run-source-deploy/billing-service/$(CLOUD_RUN_SERVICE):latest

.PHONY: test cloudrun-build cloudrun-deploy

test:
	go test ./...

cloudrun-build:
	@GCP_PROJECT='$(GCP_PROJECT)' GCP_REGION='$(GCP_REGION)' CLOUD_RUN_IMAGE='$(CLOUD_RUN_IMAGE)' bash scripts/cloudrun-build.sh

cloudrun-deploy:
	@GCP_PROJECT='$(GCP_PROJECT)' GCP_REGION='$(GCP_REGION)' CLOUD_RUN_SERVICE='$(CLOUD_RUN_SERVICE)' CLOUD_RUN_SERVICE_YAML='$(CLOUD_RUN_SERVICE_YAML)' CLOUD_RUN_IMAGE='$(CLOUD_RUN_IMAGE)' bash scripts/cloudrun-deploy.sh
