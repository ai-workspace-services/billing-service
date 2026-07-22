# Stripe 计费 P1.5:arrears 宽限期 → suspend 状态迁移(本仓侧)

> **Status**: ⏳ 代码完成 + 全量测试通过,待合并部署
> **Date**: 2026-07-12
> **Related PRs**: 本仓分支 `feat/stripe-billing-p1.5`(commit `2b9f4ef`);accounts 分支 `feat/stripe-billing-p1`(commit `841f20c`,断流执行面)
> **设计文档**: `docs/stripe-billing-integration-plan.md` §1.5 / §4 P1.5(分支 docs/stripe-plan-and-tasks / feat/stripe-billing-p1 上)

## 背景

P1(accounts entitlement sync)落地后,`invoice.payment_failed` 只会置 `arrears=true`;规划决策 #3 要求 14 天不清欠 → suspend。时间驱动的升级归本仓(billing-service),断流执行归 accounts。

## 实现内容(本仓)

- **`arrears_since` 列**(`account_quota_states`):评率主循环余额转负时置位(同一欠费期不重复推进),恢复正数清零;schema 镜像文件 `sql/billing-service-schema.sql` 同步更新。accounts 侧持有正式迁移 `sql/20260712_arrears_since.sql` + 启动幂等 DDL
- **`SuspendSyncer`**(`internal/service/suspend.go`):
  - `ARREARS_SWEEP_INTERVAL`(默认 1h)扫描 `ListArrearsAccounts()`(arrears=true 且未 suspended)
  - `arrears_since` 距今超过 `ARREARS_SUSPEND_THRESHOLD`(默认 336h=14d)→ `suspend_state='suspended'`
  - 无 `arrears_since` 的存量行不猜测、不升级
- **恢复**:不归本仓 —— accounts 在 invoice.paid / 手动清欠时置回 `active`(字段所有权:suspended 置位归本仓,active 恢复归 accounts)
- **测试**:sweep 升级/宽限期内不动/存量行豁免/已 suspended 与已清欠跳过;`go test ./...` 全过

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ARREARS_SUSPEND_THRESHOLD` | `336h`(14d) | 欠费持续多久升级为 suspended(Go duration 格式) |
| `ARREARS_SWEEP_INTERVAL` | `1h` | 扫描周期 |

## 遗留待办

- [ ] 与 accounts `feat/stripe-billing-p1`(P1+P1.5)协同合并部署;先 accounts(迁移列)后本仓(扫描)
- [ ] 催缴通知消费者(PGMQ billing_events 的 payment_failed/arrears_cleared)未实现
- [ ] P3:reconcile-stripe 对账 job + `GET /v1/usage/window` 端点
