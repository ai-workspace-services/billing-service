# billing-service docs

This directory holds service-owned documentation for `billing-service`.

## 总览

- [design.md](design.md) - 当前实现下的系统设计、主执行流程、幂等约束和模块边界
- [unified-billing-finops-architecture.md](unified-billing-finops-architecture.md) - 统一架构双平面视图:四域边界(支付/账单/计费/成本)+ 消费者/运营者两个能力面 + 三个闭环
- [open-platform-finops-control-plane.md](open-platform-finops-control-plane.md) - Open Platform FinOps 总纲:14 Workstream(FINOPS-001~1303)、三阶段交付、现状映射(§0)
- [stripe-billing-integration-plan.md](stripe-billing-integration-plan.md) - Stripe 订阅/账单/计费打通规划:现状缺口、目标架构、billing_plans 套餐目录、生命周期实勘、P0–P3 分期
- [tasks/](tasks/README.md) - 任务归档(按 PR 的项目记忆)

## 代码参考

- [reference/cmd.md](reference/cmd.md) - 进程入口、依赖装配与生命周期
- [reference/config.md](reference/config.md) - 配置结构、环境变量和配置解析函数
- [reference/model.md](reference/model.md) - 全部共享数据模型与字段语义
- [reference/exporter.md](reference/exporter.md) - exporter 客户端与窗口拉取契约
- [reference/repository.md](reference/repository.md) - 仓储接口、PostgreSQL 实现与表映射
- [reference/service.md](reference/service.md) - 业务服务、主流程与内部辅助函数
- [reference/httpapi.md](reference/httpapi.md) - HTTP 路由、handler 与状态码映射

## 系统边界与外部契约

- [architecture.md](architecture.md) - deployment topology, billing data flow,
  and current-vs-target architecture notes
- [api.md](api.md) - task endpoints, upstream snapshot contract, and downstream
  read-model boundaries
- [multi-node-https-plan.md](multi-node-https-plan.md) - target-state plan for
  evolving from a single exporter URL to secure multi-node HTTPS ingestion

## SQL 契约

- [../sql/billing-service-schema.sql](../sql/billing-service-schema.sql) -
  reference DDL for the accounting tables `billing-service` depends on

## Scope

These docs describe the `billing-service` role inside the Cloud Network Billing
& Control Plane.

- `billing-service` is the task-oriented write model
- `accounts.svc.plus` is the PostgreSQL-backed read model
- `console.svc.plus` is the presentation layer and does not query
  `billing-service` directly

System-wide contracts still live in
`github-org-cloud-neutral-toolkit/docs/architecture/network-billing-contracts.md`.

## Deployment Verification

Local or operator dry-run validation:

```bash
cd /Users/shenlan/workspaces/cloud-neutral-toolkit/playbooks
export DATABASE_URL=postgres://...
# Supabase Cloud runtime (takes precedence over DATABASE_URL):
# export SUPABASE_CONNECT_URI=postgres://...pooler.supabase.com:5432/postgres
ANSIBLE_CONFIG=./ansible.cfg \
ansible-playbook -i ./inventory.ini -D -C ./deploy_billing_service.yml -l jp_xhttp_contabo_host
```

Notes:

- `SUPABASE_CONNECT_URI` or the VPS fallback `DATABASE_URL` must be exported
  before running `deploy_billing_service.yml`
- on `jp-xhttp-contabo.svc.plus`, `DATABASE_URL` should reference the same
  `account` database used by `accounts.svc.plus`
- `BILLING_DB_USER` and `BILLING_DB_PASSWORD` are the preferred deployment
  inputs when synthesizing `DATABASE_URL`
- the runtime account should be a non-superuser service role such as
  `svcplus_vps` or a dedicated `billing` role
- avoid `postgres` for runtime traffic; reserve it for maintenance and
  bootstrap only
- check mode may report predicted changes; the goal is to pass the preflight
  assertion and render a valid deployment plan
- GitHub Actions uses the `BILLING_SERVICE_DATABASE_URL` secret to satisfy the
  same precondition in the `deploy-billing-service` job
