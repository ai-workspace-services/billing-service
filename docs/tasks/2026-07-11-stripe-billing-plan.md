# Stripe 订阅 / 账单 / 计费打通 —— 项目任务与交接文档

> **Status**: 🟢 P0 / P1 / P1.5 / F0 已上线 · ⬜ P2 / P3 / F1 未开工
> **Date**: 2026-07-11(立项)· **2026-07-22(最近一次状态核对)**
> **设计文档**: [docs/stripe-billing-integration-plan.md](../stripe-billing-integration-plan.md)(总体设计)· [docs/open-platform-finops-control-plane.md](../open-platform-finops-control-plane.md)(FinOps 上位蓝图)
> **跨仓**: accounts(`ai-workspace-services/accounts`,收入/权益侧)· 本仓 billing-service(用量/成本侧)· playbooks(`ai-workspace-infra/playbooks` → 合入 `x-evor/playbooks`,部署)
> **本文定位**: 交接用的**单一事实来源**。上半部分是当前状态与接手指南,下半部分保留立项与演进的历史记录。

---

## 一、给接手者的 5 分钟摘要

**目标**:Stripe 订阅收款 ↔ accounts 订阅记录 ↔ billing-service 计量/配额,三方连成可运营闭环,且价格/套餐/权益**数据驱动**(改表不改代码、不重部署)。

**现在能跑通什么**:用户在 console 下单 → Stripe checkout → webhook 回调 accounts → 按 `billing_plans` 目录写权益档案并重置配额 → xray 流量经 xray-exporter 计量 → billing-service 评率扣配额 → 欠费满 14 天自动 suspend → accounts 的 agent sync 把该用户从 xray 配置摘掉(真断流)→ 付清/人工清欠后恢复。**收入→权益→用量→欠费执行**这条主链路已完整闭合。

**还差什么**(按建议优先级):

1. **P2 自助与政策**(退款 / 升降级 proration / console 定价页 / 注销联动)—— 未开工,决策已拍板,可直接开工
2. **P3 对账与可观测**(reconcile-stripe 对账 job / usage/window 端点 / 指标)—— 未开工
3. **F1 成本×收入报表**(毛利、单位成本反哺定价)—— 未开工,依赖已合并的 F0
4. 若干**设计与实现的偏离/欠账**,见 §四

**当前处于 Stripe sandbox 模式**,尚未切 live。切换 SOP 见 §三.3。

---

## 二、分期执行状态(交叉核对 PR 后的实况)

| 阶段 | 内容 | 状态 | 落地 PR |
|---|---|---|---|
| **P0** | prod Stripe 密钥接线(Vault→CI→app.env) | ✅ **已上线**(sandbox) | accounts [#18](https://github.com/ai-workspace-services/accounts/pull/18) [#20](https://github.com/ai-workspace-services/accounts/pull/20) [#22](https://github.com/ai-workspace-services/accounts/pull/22);playbooks [#121](https://github.com/ai-workspace-infra/playbooks/pull/121) |
| **P1** | `billing_plans` 目录 + `stripe_webhook_events` 审计去重 + entitlement sync | ✅ **已上线** | accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19) |
| **P1.5** | 欠费 14 天 → suspended + agent sync 断流 + 人工清欠恢复 | ✅ **已上线** | accounts [#30](https://github.com/ai-workspace-services/accounts/pull/30);本仓 [#11](https://github.com/ai-workspace-services/billing-service/pull/11) |
| **F0** | 多云成本 SDK 集成(AWS CostExplorer / GCP BigQuery / Azure Consumption) | ✅ **代码已合并**(凭据接线待核实,见 §四.3) | 本仓 [#6](https://github.com/ai-workspace-services/billing-service/pull/6) [#8](https://github.com/ai-workspace-services/billing-service/pull/8) |
| **P2** | 7天低用量退款 + 升降级 proration + console 定价页 + 注销联动 | ⬜ **未开工** | — |
| **P3** | reconcile-stripe 对账 job + `GET /v1/usage/window` + 指标 | ⬜ **未开工** | — |
| **F1** | 成本×收入对账报表(毛利 / 单位成本校准定价) | ⬜ **未开工** | — |

> 各阶段的 checkbox 粒度任务清单见设计文档 [§4 分期计划](../stripe-billing-integration-plan.md#4-分期计划)。

---

## 三、运行时事实(接手必读)

### 3.1 如何验证线上 Stripe 是活的

```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  https://accounts.svc.plus/api/billing/stripe/webhook \
  -H "Content-Type: application/json" -d '{}'
```

返回 **`401 invalid_signature`** = 密钥已装配、验签生效(2026-07-22 复核通过)。
若返回 `stripe_not_configured` = 密钥没进 app.env,查 §3.2 链路。

### 3.2 密钥链路(as-built)

```
Stripe Dashboard(sandbox)  sk_test_* / whsec_*
        ▼  人工,只经操作者终端
Vault kv/billing-service    字段:SANDBOX_STRIPE_SECRET_KEY / SANDBOX_STRIPE_WEBHOOK_SECRET
                                 (PROD_STRIPE_* 两字段预留,切 live 时填)
        ▼  CI OIDC 直读(role github-actions-accounts,policy 已授 kv/data/billing-service 读)
accounts pipeline.yml「Load Vault secrets」→「Resolve Deploy Secret Source」
        │  韧性:Vault 读失败(continue-on-error)→ 回退 GH secret 镜像 → 再空(端点关闭)
        │  模式:仓库变量 STRIPE_MODE(当前 = sandbox)按整对选 SANDBOX_*/PROD_*(sk 与 whsec 永不混)
        ▼
playbooks accounts_service role(app.env.j2 + target.yml lineinfile)
        ▼
install.svc.plus:/opt/cloud-neutral/accounts/managed/prod/env/app.env → 容器 os.Getenv
```

要点:

- **密钥从 Vault OIDC 直读,不需要手工 `vault kv get | gh secret set` 同步**;GH secrets 仅作 Vault 故障期 fallback。
- 详细 runbook(密钥类型辨析 sk/whsec/pk、webhook 建立步骤、故障速查)见 accounts 仓 `docs/STRIPE_BILLING_SETUP.md`。

### 3.3 切 Stripe live 的 SOP

1. Dashboard 切 Live mode → 拿 `sk_live_*`;建正式 webhook(URL 不变,勾同样 6 个事件)→ `whsec_*`
2. `vault kv patch kv/billing-service PROD_STRIPE_SECRET_KEY='…' PROD_STRIPE_WEBHOOK_SECRET='…'`
3. `gh variable set STRIPE_MODE -R ai-workspace-services/accounts -b prod`
4. 重跑 deploy → CI 自动读 PROD_* 对
5. 回滚 = `STRIPE_MODE` 改回 `sandbox` + 重跑 deploy

### 3.4 Webhook 事件契约(Dashboard 勾选,与代码 switch 一一对应)

`checkout.session.completed` · `customer.subscription.created` · `customer.subscription.updated` · `customer.subscription.deleted` · **`invoice.paid`** · `invoice.payment_failed`

⚠️ **必须是 `invoice.paid` 而不是 `invoice.payment_succeeded`**:代码只认前者,订阅后者会落 `default` 分支被静默丢弃 → 续费重置配额/清欠永不触发。且 `invoice.paid` 语义上是超集(额外覆盖 $0 发票/试用转付费首期/余额抵扣/带外支付)。

### 3.5 关键代码位置

| 关注点 | 位置 |
|---|---|
| Stripe webhook 入口 + 事件分发 | accounts `api/stripe.go` |
| entitlement sync(权益/配额/欠费标记) | accounts `api/entitlements.go` |
| 套餐目录 CRUD + 公开定价接口 | accounts `api/billing_plans.go` |
| 人工清欠恢复(admin) | accounts `api/billing_arrears.go` |
| 欠费宽限扫描 → suspended | 本仓 `internal/service/suspend.go`(`SuspendSyncer`) |
| 多云成本同步 | 本仓 `internal/service/finops.go`(`FinOpsSyncer`) |
| 用量评率 | 本仓 `internal/service/service.go`(collect-and-rate) |

---

## 四、已知偏离、欠账与坑(交接重点)

### 4.1 PGMQ `billing_events` 队列:生产者已实现,**消费者从未实现**

设计(见历史记录"修订 2026-07-11 晚")约定 accounts 生产事件、本仓 P1.5/P3 接消费者。实际:accounts 侧生产者已实现(优雅降级),但**本仓从未实现消费者** —— P1.5 的 `SuspendSyncer` 是**直接轮询 `account_quota_states.arrears_since` 做时间判定**,与队列无关(本仓 `.go` 文件里 `pgmq`/`billing_events` 零命中)。

**影响**:功能正确(轮询方案本身没问题),但队列目前只有生产者、无消费者,处于空转。P3 若要做对账/通知,需先决定是接入队列还是继续轮询。

### 4.2 共享 PG 字段所有权未在 schema 注释中写死

原待办要求"profiles 归 accounts,quota 消耗归本仓"写进 schema 注释。核对 `sql/billing-service-schema.sql`:**没有这类所有权注释**(只有一句泛化的 "does not redefine schema ownership")。双写方的约定目前只存在于文档、不存在于 schema —— 后续改动有踩踏风险。

### 4.3 F0 代码已合并,但三云凭据尚未确认接线

PR#8 合并了 AWS/GCP/Azure SDK 集成代码,但设计文档 §4 F0 的后续步骤(三云凭据入 Vault `kv/billing-service`、billing-service 自身部署链接线、GCP BigQuery 账单导出开启、AWS/Azure 最小权限只读主体)**未确认完成**。即代码在,但同步任务大概率还拉不到真实数据。接手时需先核实 billing-service 的部署 env 是否已注入三云凭据。

### 4.4 首批付费套餐尚未录入

`billing_plans` 目录目前只有种子数据 `TRIAL-7D` 和 `FREE`。**付费套餐(plan_id ↔ stripe_price_id)需要运营在 admin 后台录入**,Stripe Dashboard 侧也需建对应 Products/Prices。这是 P2 定价页的前置。

### 4.5 throttle 是预警,不是真限速

设计原定 arrears → throttle → suspend 三级。实际 **xray 不支持 per-user 限速**,throttle 降级为"仅状态标记 + 预警",真正的执行手段只有 suspend(断流)。而预警本身(console 状态提示 + 催缴邮件)**尚未实现**,属 P1.5 清单里未完成的一项。

### 4.6 accounts 侧 CI 现状会影响部署

accounts 的 CI 已改为 `main→uat`、`release/**` 或 `v*` tag→prod(accounts #27)。**目前 `UAT_TARGET_HOST` 仓库变量未设置**,main 分支的部署会在护栏处 `exit 1`(设计如此,防止误发 prod)。接手若需部署 accounts,先确认该变量与 uat 主机资源是否就绪。

---

## 五、下一步建议顺序

1. **P2 自助与政策落地**(决策已全部拍板,无外部依赖):退款判定的用量查询来自本仓 `traffic_minute_buckets`,建议与 P3 的 `GET /v1/usage/window` 一起做;注销联动的决策 = 软删 + 30 天冷静期,退款只记录待退、由运营/billing 执行(accounts 不直接调 Stripe refund)。
2. **补 §四 的欠账**:schema 所有权注释(低成本高收益)、F0 凭据接线核实、首批套餐录入。
3. **P3 对账与可观测**,顺带决定 PGMQ 队列的去留。
4. **F1 报表**,依赖 F0 真实数据落库。

---
---

# 历史记录(立项与演进,保留原文)

> 以下为 2026-07-11 立项至 2026-07-12 数次扩版的原始记录,保留以备回溯"当初为什么这么决定"。**当前状态以上半部分为准。**

## 立项目标(2026-07-11)

把 Stripe 订阅收款 ↔ accounts 订阅记录 ↔ billing-service 计量/配额 三方连成可运营闭环,且价格/套餐/权益**数据驱动**(改表不改代码、不重部署)。

## 立项时的现状(2026-07-11 实勘)

- **已有七成骨架**:accounts `api/stripe.go` 有 checkout/portal/webhook(签名校验+订阅 upsert+期末取消);本仓有分钟桶计量/ledger/`account_quota_states`/`account_billing_profiles`(共享 accounts PG)。
- **两大断点**:①prod `app.env` 无 `STRIPE_*` 密钥,线上 Stripe 全灭;②webhook 只写 `subscriptions`,无人写 `account_billing_profiles`/重置配额 —— 买套餐 ≠ 拿配额。

> 这两个断点分别由 P0 和 P1 解决,均已上线。

## 已拍板决策(2026-07-11,仍然有效)

1. entitlement sync **放 accounts 内联**(webhook 驱动,零滞后;billing-service 不碰 Stripe)
2. 套餐目录 **走 admin 运营后台配置**(`billing_plans` 表 + admin CRUD)
3. 欠费降级梯度:1 次 failed=`arrears`,3 次或 7 天=`throttle`,14 天=`suspend`
   *(实际:throttle 无执行手段,降级为预警;见 §四.5)*
4. paygo 充值 **推迟 P2+**,`kind=paygo_topup` 表结构 P1 预留

## 补充调研(2026-07-11 下午:xray-exporter ↔ 生命周期)

- **绑定**:注册即绑定(`users.ProxyUUID`→xray client UUID,agent 每 syncInterval 拉 `/api/agent-server/v1/users` 仅 Active 用户渲染 xray 配置);xray-exporter(cloud-neutral-toolkit 仓)轮询 xray 计数 + `/api/internal/network/identities` 富化 → snapshots → 本仓 collect-and-rate。
- **暂停**:唯一真机制=admin `pauseUser`(Active=false → agent sync 断流 + identities 停归属),纯手动;billing-service 的 `arrears→throttled` 无消费者、`SuspendState` 无人置 suspended;策略通道(`/api/internal/policy` + policy_snapshots)无生产者无消费者,空转。
- **结论**:欠费→执行缺最后一公里,新增 **P1.5**(suspend 状态迁移 + agent users/identities 过滤,复用现成 sync 通道断流);throttle 真限速 xray 不原生支持,降级为预警。详见设计文档 §1.5。
- **节点侧实勘**(tky-proxy.svc.plus,`ssh admin@`):systemd 跑 `xray-tcp.service` + `xray-exporter-tcp/xhttp.service` 二实例(`-l 127.0.0.1:8080/8081 -e 127.0.0.1:18080/18081 -p /var/log/xray/access.log`)+ `agent-svc-plus.service`(`/etc/agent/account-agent.yaml`);控制面(accounts/billing/Vault/PG)全在 install.svc.plus。

> P1.5 已按此结论实施并上线(accounts #30 + 本仓 #11)。

## 修订(2026-07-11 晚,用户指令)

- **消息队列定型**:用 PG 扩展 **pgmq v1.8.0**(postgresql.svc.plus 镜像内置)建 `billing_events` 队列;accounts 已实现生产者(feat/stripe-billing-p1,优雅降级);本仓 P1.5/P3 接消费者。不引入外部 MQ。
  > ⚠️ **实际未按此实现**:本仓消费者从未落地,P1.5 走了直接轮询。见 §四.1。
- **Vault 路径定型**:Stripe 密钥归 `kv/billing-service`(STRIPE_SECRET_KEY / STRIPE_WEBHOOK_SECRET),与 accounts OAuth 密钥分域。
  > 实际实现时进一步细化为 `SANDBOX_STRIPE_*` / `PROD_STRIPE_*` 两对,由 `STRIPE_MODE` 整对切换。见 §三.2。
- P1 实现进度见 accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19)(目录/审计/entitlement sync/PGMQ 生产者,测试全绿)。

## 扩版(2026-07-12):并入成本侧 FinOps

用户指令:把多云 FinOps 线(PR#6 表+骨架、PR#8 真 SDK)并入本规划 → 文档升级为三线 FinOps 全景(收入 Stripe / 用量 xray / 成本 multi-cloud),新增 §0 全景、§2.3 成本侧、F0/F1 分期;FinOps 三云凭据(AWS keys / GCP SA JSON+BQ 三元组 / Azure 四元组)与 Stripe 密钥同归 Vault `kv/billing-service`(§2.2 密钥总表)。成本侧不走 PGMQ(T-2 定时拉取无事件性);F1 出毛利/单位成本报表反哺套餐定价。

> PR#6 与 PR#8 均已合并;凭据接线待核实,见 §四.3。

## 再扩版(2026-07-12):Open Platform FinOps 总纲入库

用户提供完整 **Cloud-Neutral FinOps Control Plane** 工作流规范(14 Workstream,FINOPS-001~1303,Plan→Estimate→Deploy→Measure→Allocate→Analyze→Optimize 全生命周期,Phase 1-3 交付计划)→ 落库 `docs/open-platform-finops-control-plane.md` 作为总纲,本计费规划降为其早期增量。**增值:§0 现状映射表**(已有资产↔FINOPS 编号):VictoriaMetrics/Grafana ✅已部署、node/process_exporter 🟡tky-proxy 已有、PR#8 = FINOPS-401~403 的 API 版垫脚石(蓝图要求 CUR/Export 文件级,需演进)、无 K8s → OpenCost 后置 / Price Book(304)与 LiteLLM AI 成本(305)提前、Vault kv/billing-service ✅、连接器接口(404)/FOCUS(501)/Cost Warehouse(701)/Cost API(9xx)缺。组织建议:billing-service 保持商业计费域,五个 finops-* 服务另立 open-platform/finops,FinOpsSyncer 成熟后迁 finops-ingestor。

## 架构定稿(2026-07-12):双平面统一视图

新增 `docs/unified-billing-finops-architecture.md`:四域边界(支付=Stripe 事实源/计费=用量评率/账单=订阅权益/成本=FinOps warehouse,含 FINOPS-601 口径铁律)+ **消费平面**(订阅者旅程九阶段能力表、对账恒等式"应付=固定费+超额−退款"、用量与扣费同源原则)+ **治理平面**(Plan→Optimize 七环能力表、三闭环:毛利=收入−分摊成本/定价校准=单位成本反哺 billing_plans/催缴=arrears→throttle→suspend→复通)+ 两平面六接点 + 分阶段合并视图。
