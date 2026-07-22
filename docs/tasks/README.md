# docs/tasks — 任务归档(项目记忆)

每次任务对话结束后,在此按**任务**建一个 Markdown 文件:目标、结论、关联 PR(带 ID + 状态)、决策、遗留待办。作为跨会话的全局项目记忆。

## 约定

- 文件名:`YYYY-MM-DD-<kebab-任务名>.md`
- 顶部状态块(Status / Date / Related PRs);PR 用完整链接标 [MERGED]/[OPEN]/[CLOSED]
- 规划类任务无 PR 时,关联对应设计文档

## 索引

| 日期 | 任务 | 状态 | 关联 |
|---|---|---|---|
| 2026-07-12 | [Stripe 计费 P1.5(arrears 宽限期 → suspend 状态迁移)](2026-07-12-stripe-billing-p15.md) | ⏳ 待合并(本 PR) | 本仓本 PR;accounts [#30](https://github.com/ai-workspace-services/accounts/pull/30) |
| 2026-07-11 | [Stripe 订阅/账单/计费打通规划](2026-07-11-stripe-billing-plan.md) | 📋 规划定稿待排期 | [stripe-billing-integration-plan.md](../stripe-billing-integration-plan.md) |
