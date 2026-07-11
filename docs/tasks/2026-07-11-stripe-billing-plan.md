# Stripe 订阅 / 账单 / 计费打通规划

> **Status**: 📋 规划定稿,决策已拍板,待排期实施
> **Date**: 2026-07-11
> **Related PRs**: 暂无(P0 起才产生 PR)
> **设计文档**: [docs/stripe-billing-integration-plan.md](../stripe-billing-integration-plan.md)
> **跨仓**: 实施涉及 accounts.svc.plus(stripe.go / entitlement sync)+ 本仓(用量查询 / 对账)

## 目标

把 Stripe 订阅收款 ↔ accounts 订阅记录 ↔ billing-service 计量/配额 三方连成可运营闭环,且价格/套餐/权益**数据驱动**(改表不改代码、不重部署)。

## 现状(2026-07-11 实勘)

- **已有七成骨架**:accounts `api/stripe.go` 有 checkout/portal/webhook(签名校验+订阅 upsert+期末取消);本仓有分钟桶计量/ledger/`account_quota_states`/`account_billing_profiles`(共享 accounts PG)。
- **两大断点**:①prod `app.env` 无 `STRIPE_*` 密钥,线上 Stripe 全灭;②webhook 只写 `subscriptions`,无人写 `account_billing_profiles`/重置配额 —— 买套餐 ≠ 拿配额。

## 已拍板决策(2026-07-11)

1. entitlement sync **放 accounts 内联**(webhook 驱动,零滞后;billing-service 不碰 Stripe)
2. 套餐目录 **走 admin 运营后台配置**(`billing_plans` 表 + admin CRUD)
3. 欠费降级梯度:1 次 failed=`arrears`,3 次或 7 天=`throttle`,14 天=`suspend`
4. paygo 充值 **推迟 P2+**,`kind=paygo_topup` 表结构 P1 预留

## 分期(详见设计文档 §4)

- **P0**(半天):prod `STRIPE_*` 密钥接线,复用 OAuth 的 Vault→GH secrets→playbooks app.env 模式
- **P1**(核心,1~2 周):`billing_plans` 目录表 + `stripe_webhook_events` 审计去重表 + entitlement sync(invoice.paid 重置配额、payment_failed 驱动 arrears→throttle→suspend、退订降级)+ checkout 改读目录
- **P2**(1 周):7 天低用量(<5%)退款(用量查本仓 `traffic_minute_buckets`)+ 升降级 proration + console 定价页/退款入口 + 注销联动
- **P3**(0.5 周,本仓):`reconcile-stripe` 对账 job + `GET /v1/usage/window` 内部端点 + 指标

## 补充调研(2026-07-11 下午:xray-exporter ↔ 生命周期)

- **绑定**:注册即绑定(`users.ProxyUUID`→xray client UUID,agent 每 syncInterval 拉 `/api/agent-server/v1/users` 仅 Active 用户渲染 xray 配置);xray-exporter(cloud-neutral-toolkit 仓)轮询 xray 计数 + `/api/internal/network/identities` 富化 → snapshots → 本仓 collect-and-rate。
- **暂停**:唯一真机制=admin `pauseUser`(Active=false → agent sync 断流 + identities 停归属),纯手动;billing-service 的 `arrears→throttled` 无消费者、`SuspendState` 无人置 suspended;策略通道(`/api/internal/policy` + policy_snapshots)无生产者无消费者,空转。
- **结论**:欠费→执行缺最后一公里,新增 **P1.5**(suspend 状态迁移 + agent users/identities 过滤,复用现成 sync 通道断流);throttle 真限速 xray 不原生支持,降级为预警。详见设计文档 §1.5。
- **节点侧实勘**(tky-proxy.svc.plus,`ssh admin@`):systemd 跑 `xray-tcp.service` + `xray-exporter-tcp/xhttp.service` 二实例(`-l 127.0.0.1:8080/8081 -e 127.0.0.1:18080/18081 -p /var/log/xray/access.log`)+ `agent-svc-plus.service`(`/etc/agent/account-agent.yaml`);控制面(accounts/billing/Vault/PG)全在 install.svc.plus。

## 修订(2026-07-11 晚,用户指令)

- **消息队列定型**:用 PG 扩展 **pgmq v1.8.0**(postgresql.svc.plus 镜像内置)建 `billing_events` 队列;accounts 已实现生产者(feat/stripe-billing-p1,优雅降级);本仓 P1.5/P3 接消费者。不引入外部 MQ。
- **Vault 路径定型**:Stripe 密钥归 `kv/billing-service`(STRIPE_SECRET_KEY / STRIPE_WEBHOOK_SECRET),与 accounts OAuth 密钥分域。
- P1 实现进度见 accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19)(目录/审计/entitlement sync/PGMQ 生产者,测试全绿)。

## 扩版(2026-07-12):并入成本侧 FinOps

用户指令:把多云 FinOps 线([PR#6](https://github.com/ai-workspace-services/billing-service/pull/6) MERGED 表+骨架、[PR#8](https://github.com/ai-workspace-services/billing-service/pull/8) OPEN 真 SDK)并入本规划 → 文档升级为三线 FinOps 全景(收入 Stripe / 用量 xray / 成本 multi-cloud),新增 §0 全景、§2.3 成本侧、F0/F1 分期;FinOps 三云凭据(AWS keys / GCP SA JSON+BQ 三元组 / Azure 四元组)与 Stripe 密钥同归 Vault `kv/billing-service`(§2.2 密钥总表)。成本侧不走 PGMQ(T-2 定时拉取无事件性);F1 出毛利/单位成本报表反哺套餐定价。

## 再扩版(2026-07-12):Open Platform FinOps 总纲入库

用户提供完整 **Cloud-Neutral FinOps Control Plane** 工作流规范(14 Workstream,FINOPS-001~1303,Plan→Estimate→Deploy→Measure→Allocate→Analyze→Optimize 全生命周期,Phase 1-3 交付计划)→ 落库 `docs/open-platform-finops-control-plane.md` 作为总纲,本计费规划降为其早期增量。**增值:§0 现状映射表**(已有资产↔FINOPS 编号):VictoriaMetrics/Grafana ✅已部署、node/process_exporter 🟡tky-proxy 已有、PR#8 = FINOPS-401~403 的 API 版垫脚石(蓝图要求 CUR/Export 文件级,需演进)、无 K8s → OpenCost 后置 / Price Book(304)与 LiteLLM AI 成本(305)提前、Vault kv/billing-service ✅、连接器接口(404)/FOCUS(501)/Cost Warehouse(701)/Cost API(9xx)缺。组织建议:billing-service 保持商业计费域,五个 finops-* 服务另立 open-platform/finops,FinOpsSyncer 成熟后迁 finops-ingestor。

## 遗留待办

- [ ] P0 起排期,产出 PR 后回填本文件的 Related PRs
- [ ] 首批套餐结构由运营在 admin 后台定(不阻塞开发)
- [ ] 共享 PG 字段所有权在 schema 注释写死(profiles 归 accounts,quota 消耗归本仓)
