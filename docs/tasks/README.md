# docs/tasks — 任务归档(项目记忆)

每次任务对话结束后,在此按**任务**建一个 Markdown 文件:目标、结论、关联 PR(带 ID + 状态)、决策、遗留待办。作为跨会话的全局项目记忆。

## 约定

- 文件名:`YYYY-MM-DD-<kebab-任务名>.md`
- 顶部状态块(Status / Date / Related PRs);PR 用完整链接标 [MERGED]/[OPEN]/[CLOSED]
- 规划类任务无 PR 时,关联对应设计文档

## 索引

| 日期 | 任务 | 状态 | 关联 |
|---|---|---|---|
| 2026-07-11 | [**Stripe 订阅/账单/计费打通 —— 项目任务与交接文档**](2026-07-11-stripe-billing-plan.md) | 🟢 P0/P1/P1.5/F0 已上线;P2/P3/F1 未开工(**交接入口,状态以此为准**) | [stripe-billing-integration-plan.md](../stripe-billing-integration-plan.md) · accounts #18-#22 #30 · 本仓 #6 #8 #11 |
| 2026-07-12 | [Stripe 计费 P1.5(arrears 宽限期 → suspend 状态迁移)](2026-07-12-stripe-billing-p15.md) | ✅ 已合并上线 | 本仓 [#11](https://github.com/ai-workspace-services/billing-service/pull/11) [MERGED];accounts [#30](https://github.com/ai-workspace-services/accounts/pull/30) [MERGED] |
| 2026-07-12 | [PR-8 多云 FinOps SDK 集成](PR-8-finops-sdk-integration.md) | ✅ 代码已合并;凭据接线待核实 | 本仓 [#8](https://github.com/ai-workspace-services/billing-service/pull/8) [MERGED] |
| 2026-07-11 | [PR-6 多云 FinOps billing sync 基础](PR-6-finops-billing-sync.md) | ✅ 已合并 | 本仓 [#6](https://github.com/ai-workspace-services/billing-service/pull/6) [MERGED] |
