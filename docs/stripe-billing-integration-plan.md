# Stripe 订阅 · 账单 · 计费打通规划

> 目标:把 Stripe 订阅收款、accounts 的订阅记录、billing-service 的计量/配额三个世界连成一条可运营的闭环,且**价格/套餐/权益调整全部数据驱动**(改表,不改代码、不重部署)。
> 最后更新:**2026-07-22** · 状态:**M1(P0/P1/P1.5/F0)已上线闭环 · M2(P2/P3/F1)待开工**
> 🎯 **下一里程碑 M2 从 [§4.5](#45-下一里程碑m2--p2--p3--f1--开工前必须先解决的问题) 读起** —— 那里列了 4 个开工前必须先拍板的阻塞项(退款执行权归属矛盾、P2/P3 依赖倒置、与 accounts 注销规划重复、reconcile 命名冲突)与建议执行顺序。
> 📌 执行状态、运行时事实与已知欠账以 [docs/tasks/2026-07-11-stripe-billing-plan.md](tasks/2026-07-11-stripe-billing-plan.md) 为准(交接文档)。本文是设计与分期的详细依据。

## 0. 全景:三条线的 FinOps 闭环(2026-07-12 扩版)

> **上位蓝图**:本规划是 [Open Platform FinOps · Cloud-Neutral FinOps Control Plane](open-platform-finops-control-plane.md) 的早期增量 —— 收入/用量侧属商业计费域(蓝图外的互补面,经 Revenue-based 分摊与毛利报表相接),成本侧 FinOpsSyncer 是蓝图 Workstream E 的 API 版先行(现状映射见蓝图 §0)。

本规划从"Stripe 订阅打通"扩展为完整 FinOps 视图,三条数据线汇入同一个共享 PG:

| 线 | 内容 | 状态 | 载体 |
|---|---|---|---|
| **收入侧** | Stripe 订阅/账单 → 订阅记录 → 权益(entitlement) | ✅ 已上线(P0 + P1 = accounts#19,P1.5 = accounts#30 + 本仓#11) | accounts(stripe.go / billing_plans / PGMQ 生产者) |
| **用量侧** | xray 流量 → 计量 → 评率/配额扣减 | ✅ 已上线运行 | xray-exporter → billing-service collect-and-rate |
| **成本侧** | AWS/GCP/Azure 基础设施成本同步 | ✅ 代码已合并(PR#6 表+骨架,PR#8 真 SDK);⚠️ 三云凭据接线待核实 | billing-service FinOpsSyncer → `cloud_vendor_costs` |

三线相交产生运营视角:**毛利 = Stripe 收入 −(多云成本 按用量/节点分摊)**;成本异常审计(AI auditing,cloud_vendor_costs 表注释即此意);定价校准(套餐 included_quota 的单位成本依据)。

## 1. 现状盘点(2026-07-11 实勘)

### 已有(七成骨架)

| 组件 | 现状 | 位置 |
|---|---|---|
| Checkout / Portal / Webhook | 已实现:签名校验、`checkout.session.completed`、`customer.subscription.*`、`invoice.paid/payment_failed` → upsert `subscriptions` 表 | accounts `api/stripe.go` |
| 订阅取消 | `cancel_at_period_end` 已联动 Stripe(符合产品决策:期末取消不按比例退款) | accounts `api/api.go` `cancelSubscription` |
| 价格白名单 | `STRIPE_ALLOWED_PRICE_IDS` 环境变量 | accounts `cmd/accountsvc/main.go` |
| 计量/评率 | 分钟桶采集(`traffic_minute_buckets`)、账本(`billing_ledger`)、配额状态(`account_quota_states`)、计费档案(`account_billing_profiles`,含 included_quota/倍率/pricing_rule_version)、幂等重放 | billing-service(共享 accounts PG) |
| 用户面 API | `/api/account/usage|billing/summary`、`/api/auth/subscriptions`、stripe checkout/portal 代理 | accounts + portal(`panel/subscription` 页面已存在) |
| 试用 | OAuth 首登自动发 7 天 trial(`PlanID: TRIAL-7D`) | accounts oauthCallback |

### 缺口(按疼痛排序)

1. **生产 Stripe 未配置**:线上 `app.env` 无 `STRIPE_SECRET_KEY` / `STRIPE_WEBHOOK_SECRET` / `STRIPE_ALLOWED_PRICE_IDS`,当前所有 Stripe 端点在 prod 返回 `stripe_not_configured`。
2. **订阅 ↔ 计费档案零联动**(核心断点):webhook 只写 `subscriptions`,**没有任何代码写 `account_billing_profiles` / 重置 `account_quota_states`**。买了套餐 ≠ 拿到配额;trial 到期 ≠ 降级。两个世界靠人肉。
3. **套餐目录不存在**:PlanID 是 Stripe metadata 里的自由字符串,价格→权益映射无处定义;`STRIPE_ALLOWED_PRICE_IDS` 改动要改 env + 重部署,与"灵活调整"目标直接冲突。
4. **欠费/催缴不闭环**:`invoice.payment_failed` 只重新 upsert 订阅;`account_quota_states.arrears/suspend_state/throttle_state` 字段存在但无人因支付失败去驱动它。
5. **7 天低用量退款未实现**(产品决策:首订 7 天内用量 <5% 可全额退款):判定所需的用量数据在 billing-service 的分钟桶里,退款动作在 Stripe —— 正好是"打通"的典型用例。
6. **无 webhook 事件审计/去重表**:Stripe 重发事件靠 UpsertSubscription 的天然幂等硬扛,无 event_id 去重、无审计轨迹、无失败重放。
7. 升级/降级(proration)未处理;trial→付费的转换只是新 upsert 一条,老 trial 记录不终结。

## 1.5 账号生命周期链路实勘(2026-07-11 补充:激活/绑定/暂停)

### 绑定与用量归属(已跑通)

```
注册/OAuth 首登 → users.Active=true, ProxyUUID(缺省回退 user.ID)
      │
      ├─ agent 节点(accountsvc agent 模式,xray.sync.enabled)
      │    每 syncInterval 拉 /api/agent-server/v1/users(仅 Active 用户)
      │    → 渲染 xray config clients(UUID+email)→ 重启 xray   【绑定=注册即绑定】
      │
      └─ xray-exporter(cloud-neutral-toolkit 仓库)
           轮询 Xray 流量计数(per-email)
           + /api/internal/network/identities(仅 Active,email→uuid)做身份富化
           → /v1/snapshots/window(Bearer INTERNAL_SERVICE_TOKEN)
           → billing-service collect-and-rate → 分钟桶/ledger/quota_states
```

### 暂停:一个真机制 + 两个半成品

| 机制 | 现状 | 生效面 |
|---|---|---|
| **admin pauseUser**(`Active=false`) | ✅ 唯一真正生效的暂停 | ①下次 agent sync 从 xray 配置移除该 client(流量断,分钟级);②identities 排除(停止计量归属);③`RequireActiveUser` 拦 API。**纯手动** |
| **billing-service 自动降级** | 🟡 半成品 | `balance<0 → Arrears=true → ThrottleState="throttled"`(仅 DB 状态);**全工作区无任何消费者**读 throttle_state 去限速;`SuspendState` 只初始化 `"active"`,无代码置 suspended |
| **策略下发通道** | 🔴 空转 | accounts 有 `/api/internal/policy/:accountUUID` + `account_policy_snapshots` 表 + node heartbeat 端点,但**无人写快照、无人调该端点** —— 管道建好没通电 |

### 结论:欠费→执行 缺最后一公里

即便 P1 做完 entitlement sync,欠费用户也只会被标 `throttled`,**没有组件把它落到 xray 层**。补齐方案(纳入分期):

- **P1.5(推荐,轻量)**:`listAgentUsers` / `internalNetworkIdentities` 过滤时 join `account_quota_states`,排除 `suspend_state='suspended'` 的账号 —— 复用现成 agent sync 通道,分钟级断流,无新组件;同时 billing-service 补状态迁移:arrears 持续 N 天 → `suspend_state='suspended'`(阈值按 §5 决策 14 天)。
- **限速(throttle)**:xray 无原生 per-user 限速,真限速要么换低速 inbound 方案要么上层网关,复杂度高 → **P3+ 或降级为"仅 suspend 不 throttle"**,throttled 状态仅作预警(console 提示 + 邮件催缴)。
- **策略通道**:保留为 P3+ 的演进方向(节点级细粒度策略),当前不投入。

另两个小发现:①暂停用户的残余流量因 identities 排除而无法归属(计量口径小漏,可接受);②`ProxyUUID` 缺省回退 `user.ID`,admin `renewProxyUUID` 可轮换 —— 订阅无关,"激活"今天=注册即激活,试用到期不会断网(P1 entitlement sync 补:trial 过期 → 降 free 档案;free 是否保留代理接入=待产品定)。

## 2. 目标架构与职责切分

```
                         ┌────────────────────────────────────────────┐
   Stripe(钱的事实源)  │  Products/Prices · Checkout · Invoices     │
                         └───────┬───────────────────▲────────────────┘
                        webhooks │                   │ API(checkout/cancel/refund)
                         ┌───────▼───────────────────┴────────────────┐
   accounts.svc.plus     │ stripe.go(唯一 Stripe API 持有者)         │
   (订阅事实源)         │  · webhook → stripe_webhook_events(去重)  │
                         │  · upsert subscriptions                    │
                         │  · **entitlement sync**: 按 billing_plans  │
                         │    目录写 account_billing_profiles、       │
                         │    invoice.paid 重置配额、payment_failed   │
                         │    驱动 arrears/suspend                    │
                         └───────┬────────────────────────────────────┘
                     共享 PG     │ billing_plans / subscriptions /
                                 │ account_billing_profiles / quota_states
                         ┌───────▼────────────────────────────────────┐
   billing-service       │ 只管"用了多少、该扣多少":collect-and-rate │
   (用量事实源)         │ 按 profiles 评率 → ledger/quota;reconcile  │
                         │ 新增:usage-within-window 查询(退款判定)  │
                         └────────────────────────────────────────────┘
```

**职责边界(定死,避免以后扯皮)**
- **Stripe** = 钱的事实源(价格、发票、退款都以它为准)。
- **accounts** = 订阅与权益的事实源,唯一持有 Stripe API key,唯一写 `subscriptions`/`account_billing_profiles` 的服务。
- **billing-service** = 用量的事实源,只读 profiles、只写用量/账本/配额消耗;**不碰 Stripe**。
- 联动通道 = **共享 PG 表 + PGMQ 消息队列**(2026-07-11 修订):状态类数据(profiles/quota)走共享表;生命周期通知走 PGMQ `billing_events` 队列(扩展 **pgmq v1.8.0**,postgresql.svc.plus 运行时镜像内置,与 pgvector/pg_jieba 同装)。不引入外部 MQ 组件 —— 队列就在同一个 PG 里。

### 2.1 PGMQ `billing_events` 队列(已实现,accounts feat/stripe-billing-p1)

- **生产者**:accounts 在 entitlement sync 各点位发布紧凑事件:`subscription_activated` / `subscription_updated` / `invoice_paid` / `payment_failed` / `subscription_deleted` / `trial_provisioned`,payload 含 userId/planId/priceId/externalId/occurredAt。
- **降级策略**:启动时 `EnsureBillingEventQueue`(检测/尝试 `CREATE EXTENSION pgmq` + `pgmq.create('billing_events')`);扩展不可用则发布静默 no-op,webhook 主流程永不因队列失败;去重重放不重复发布。线上 accounts 库 pgmq **available 未安装**,operator 需一次 `CREATE EXTENSION pgmq`(superuser)。
- **消费者**(本仓,原计划 P1.5/P3 接入):`pgmq.read('billing_events', vt, qty)` + 处理后 `pgmq.delete/archive`;用于 arrears 升级触发、对账增量、催缴通知,替代轮询。消费失败消息按 pgmq 可见性超时自动重投。
  > ⚠️ **实际未实现(2026-07-22 核对)**:P1.5 落地时走了**直接轮询 `account_quota_states.arrears_since`** 的方案(`internal/service/suspend.go`),本仓代码中 `pgmq`/`billing_events` 零引用。队列目前只有生产者、无消费者。P3 开工时需先决定:接入队列,还是维持轮询并撤下队列。

### 2.2 密钥存储(2026-07-11 修订)

Stripe 密钥的 Vault source-of-truth 归**本服务路径 `kv/billing-service`**(与 accounts 的 OAuth 密钥 kv/accounts.svc.plus 分域):

| Vault `kv/billing-service` 字段 | 用途 | 同步到 |
|---|---|---|
| `STRIPE_SECRET_KEY` | Stripe API key(accounts 容器 env 消费) | GH secret `STRIPE_SECRET_KEY`(accounts 仓) |
| `STRIPE_WEBHOOK_SECRET` | webhook 签名校验 | GH secret `STRIPE_WEBHOOK_SECRET`(accounts 仓) |

`STRIPE_ALLOWED_PRICE_IDS` 非密钥(repo var,且 P1 后目录为准仅作 bootstrap 兜底),不入 Vault。

**FinOps 多云凭据同归 `kv/billing-service`**(2026-07-12,配套 PR#8):

| Vault `kv/billing-service` 字段 | 云 | 用途 |
|---|---|---|
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | AWS | Cost Explorer `GetCostAndUsage`(建议最小权限:`ce:GetCostAndUsage` 只读 IAM) |
| `GCP_CREDENTIALS_JSON` | GCP | BigQuery 账单导出查询(SA 只读 `bigquery.jobs.create`+dataset viewer) |
| `GCP_BILLING_PROJECT` / `GCP_BILLING_DATASET` / `GCP_BILLING_TABLE` | GCP | 非密钥,但与凭据同处便于整包同步 |
| `AZURE_TENANT_ID` / `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` / `AZURE_SUBSCRIPTION_ID` | Azure | Consumption UsageDetails(Reader on subscription) |

同步链:Vault → billing-service 部署 env(billing-service 的部署链待接线,见 §4 F0)。

### 2.3 成本侧:多云 FinOps 同步(并入 2026-07-12)

- **已合**(PR#6):`cloud_vendor_costs` 表(provider/account_id/service_name/region/时间窗/cost_amount/currency/usage_quantity,时间+provider 索引)+ `FinOpsSyncer` 守护协程骨架(随 billing-service 主进程启动)。
- **实施中**(PR#8,分支 `feature/finops-api-integration`):真 SDK 集成 ——
  - AWS:`costexplorer.GetCostAndUsage` 按 SERVICE 分组,unblended amortized
  - GCP:BigQuery SDK 查账单导出 dataset(已决策:走 BigQuery export 而非 Cloud Billing API,导出成本可忽略)
  - Azure:`armconsumption.UsageDetailsClient` 按日用量
  - **T-2 窗口**(取两天前数据,规避云商账单结算延迟)已决策
- **与收入/用量侧的接点**(后续 F1):`cloud_vendor_costs` × `billing_ledger`/`traffic_minute_buckets` 出毛利与单位成本报表;不走 PGMQ(定时 T-2 拉取模型,无事件性)。

## 3. 灵活性设计(“方便灵活调整”的落点)

### 3.1 `billing_plans` 套餐目录表(新)

一切可调参数进 DB,改表立即生效:

```sql
CREATE TABLE billing_plans (
  plan_id            TEXT PRIMARY KEY,        -- 'TRIAL-7D' / 'PRO-M' / 'PRO-Y' ...
  stripe_price_id    TEXT UNIQUE,             -- trial 类可为 NULL
  display_name       TEXT NOT NULL,
  kind               TEXT NOT NULL,            -- trial|subscription|paygo_topup
  included_quota_bytes BIGINT NOT NULL DEFAULT 0,
  package_name       TEXT NOT NULL DEFAULT 'default',
  price_multipliers  JSONB NOT NULL DEFAULT '{}',  -- region/line/peak/offpeak
  features           JSONB NOT NULL DEFAULT '{}',  -- 功能开关位,console 读
  trial_days         INT NOT NULL DEFAULT 0,
  active             BOOLEAN NOT NULL DEFAULT true,
  sort_order         INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

由此替换三个硬编码点:
- `STRIPE_ALLOWED_PRICE_IDS` env → `WHERE active AND stripe_price_id IS NOT NULL`(checkout 校验)
- webhook 里 metadata 自由字符串 → price_id 反查 plan_id
- console 定价页 → `GET /api/billing/plans`(public,读目录),改价上新不发版

管理面:`/api/auth/admin/billing/plans` CRUD(复用现有 admin 权限体系)。

### 3.2 价格调整流程(运营 SOP)

1. Stripe Dashboard 建新 Price(旧 Price archive,Stripe 价格不可变)
2. `billing_plans` 更新该 plan 的 `stripe_price_id`(或上新 plan 行)
3. 完 —— 存量订阅按旧价续费(Stripe 行为),新购走新价;不发版、不重部署

### 3.3 权益调整

改 `included_quota_bytes` / `price_multipliers` → entitlement sync 在下一次订阅事件(或手动触发 resync)时刷进 `account_billing_profiles`;billing-service 下一个评率周期自然生效。

## 4. 分期计划

### P0 · 让 Stripe 在 prod 活过来(半天,复用 OAuth 部署链路)

> ✅ **已上线(sandbox 模式)** — accounts #18/#20/#22 + playbooks #121。密钥改为 CI 经 Vault OIDC 直读 `kv/billing-service`,由仓库变量 `STRIPE_MODE` 整对切换 SANDBOX_*/PROD_*(比原计划的手工同步 GH secrets 更进一步)。
- [x] Stripe Dashboard(先 test mode):建 Products/Prices;Webhook endpoint = `https://accounts.svc.plus/api/billing/stripe/webhook`,拿 `whsec_*`
- [x] Vault `kv/accounts.svc.plus` 增:`STRIPE_SECRET_KEY`、`STRIPE_WEBHOOK_SECRET` → 同步 GH secrets
- [x] playbooks role:`app.env.j2` + `defaults/main.yml` + `target.yml` lineinfile 增 `STRIPE_*` 三变量(完全照抄 OAuth 那次的模式,含存量主机 lineinfile)
- [x] accounts `pipeline.yml` deploy env 透传
- [x] 验证:`stripe listen --forward-to` 本地回归 + prod test-mode 全流程(checkout → webhook → subscriptions 落库)

### P1.5 · 欠费执行面(0.5 周,接在 P1 后)

> ✅ **已上线** — accounts #30(agent sync 断流 + 人工清欠 admin 端点)+ 本仓 #11(`SuspendSyncer` 宽限扫描)。
> ⚠️ 最后一项 console 提示与催缴邮件**未实现**;throttle 因 xray 不支持 per-user 限速,降级为纯状态标记。
- [x] billing-service:arrears 持续超阈值(14 天,配置化)→ `suspend_state='suspended'` 状态迁移
- [x] accounts:`listAgentUsers` + `internalNetworkIdentities` 排除 `suspend_state='suspended'` 账号(join quota_states)→ agent 下次 sync 即断流
- [x] 恢复路径:invoice.paid / 手动清欠 → `suspend_state='active'` → 下次 sync 恢复接入
- [ ] console:arrears/throttled 状态提示 + 催缴邮件(SMTP 已有)

### P1 · 打通订阅→权益闭环(核心,1~2 周)

> ✅ **已上线** — accounts #19(目录/审计去重/entitlement sync/PGMQ 生产者,测试全绿)。
- [x] `billing_plans` 表 + store 层 + 种子数据(TRIAL-7D、首批付费套餐)+ admin CRUD
- [x] `stripe_webhook_events` 表(event_id 去重、payload 审计、处理状态),webhook 先落事件再处理
- [x] **entitlement sync**(accounts 内,webhook 驱动):
  - `customer.subscription.created/updated` → 按 plan 写 `account_billing_profiles`
  - `invoice.paid` → 重置 `account_quota_states.remaining_included_quota`、清 `arrears`
  - `invoice.payment_failed` → `arrears=true`,N 次后 `throttle_state/suspend_state` 升级(阈值进配置)
  - `customer.subscription.deleted` / trial 过期 → 降回 free 档案
- [x] checkout 校验改读 `billing_plans`,下线 `STRIPE_ALLOWED_PRICE_IDS`
- [x] trial→付费转换:新订阅生效时把 trial 记录标记 `superseded`
- [x] 测试:webhook 全事件表驱动测试 + 幂等重放测试

### P2 · 自助与政策落地(1 周)

- [ ] **7 天低用量退款**:`POST /api/auth/subscriptions/refund` —— 判定(订阅 ≤7 天 && 窗口内用量 < 5% 配额,用量查 `traffic_minute_buckets` 聚合)→ Stripe refund API → 订阅终止 + 档案降级 + ledger 冲正记录
- [ ] 升级/降级:`POST /api/auth/subscriptions/change`,Stripe subscription item 替换,proration 策略 = `create_prorations`(升级即时,降级期末)
- [ ] console:定价页读 `/api/billing/plans`;`panel/subscription` 补退款/变更入口
- [ ] 注销联动(产品决策):`pending_cancellation` → 订阅期末终止 → 30 天删除冷静期;paygo 余额原路退

### F0 · 成本侧落地(并行线,不阻塞 P 线)

> 🟡 **代码已合并,凭据接线待核实** — PR#8 已 MERGED;下面后三项(Vault 凭据 / GCP BQ 导出 / 拉取验证)**尚未确认完成**,接手需先核实 billing-service 部署 env 是否已注入三云凭据。
- [x] 评审合并 **PR#8**(多云 SDK 集成;注意 go.mod 膨胀 +67 依赖属预期,SDK 家族使然)
- [ ] 三云凭据入 Vault `kv/billing-service`(字段见 §2.2)→ billing-service 部署 env 接线(billing-service 自身的部署链/pipeline 需先盘点,当前无 playbooks role)
- [ ] GCP 侧前置:开 BigQuery 账单导出(已决策);AWS/Azure 建最小权限只读主体
- [ ] 验证:FinOpsSyncer T-2 拉取三云数据落 `cloud_vendor_costs`

### F1 · 成本×收入对账报表(0.5~1 周,依赖 F0 + P1 上线)

- [ ] 毛利视图:Stripe 收入(subscriptions/ledger)− 多云成本(cloud_vendor_costs),按月/按 provider
- [ ] 单位成本:cost / rated_bytes,给套餐定价(billing_plans included_quota)校准依据
- [ ] admin 端点或直接 SQL 报表(console 面板可选后置)

### P3 · 对账与可观测(0.5 周,billing-service 侧)

- [ ] billing-service 新增 `POST /v1/jobs/reconcile-stripe`:拉 Stripe subscriptions/invoices 与本地 `subscriptions`/`billing_ledger` 比对,出 drift 报告
- [ ] `GET /v1/usage/window?account&from&to` 内部端点(供退款判定与客服查询)
- [ ] 指标:webhook 失败率、entitlement sync 滞后、arrears 账户数;接现有 VictoriaMetrics

## 4.5 下一里程碑(M2 = P2 + P3 + F1)—— 开工前必须先解决的问题

> 2026-07-22 增补。P0/P1/P1.5/F0 构成的 M1(收入→权益→用量→欠费执行)已闭环上线。
> M2 的目标是**自助化与可对账**。但 §4 里 P2/P3/F1 的条目是立项期写的,直接照着开工会撞上下面 4 个问题 —— **这些不是实现细节,是需要先拍板的阻塞项**。

### ⛔ 阻塞 1:退款的执行权归属,三处说法互相矛盾(必须先定)

| 出处 | 说法 |
|---|---|
| 本文 §2 职责边界 | accounts = **唯一持有 Stripe API key**;billing-service **不碰 Stripe** |
| 本文 §4 P2 第 1 条 | accounts `POST /api/auth/subscriptions/refund` → **直接调 Stripe refund API** |
| accounts `docs/tasks/2026-07-14-self-service-recovery-and-deletion.md` 决策 #3 | 注销退款「**由 billing-service / 运营执行**;accounts 不直接调 Stripe refund」 |

三者无法同时成立:**billing-service 不碰 Stripe,就不可能由它执行 Stripe refund**。可选方案:

- **方案 A(推荐,与既有架构一致)**:退款动作统一由 accounts 执行(它已是唯一 Stripe API 持有者);billing-service 只提供退款判定所需的用量数据。accounts 那份注销文档的决策 #3 需相应修订为「**由运营在 admin 后台触发,accounts 执行**」。
- **方案 B**:一切退款只记录待退,**由运营在 Stripe Dashboard 人工退**。自动化程度低,但资金动作完全离开自动流程,审计上最保守。
- **方案 C**:给 billing-service 开 Stripe 只写 refund 的能力 —— **需推翻 §2 职责边界**,不建议(两个服务持有 Stripe 凭据会让"钱的事实源"责任模糊)。

拍板前 P2 的退款项、accounts 的自助注销退款项都无法开工。

### ⛔ 阻塞 2:P2 依赖 P3,当前排序是反的

P2 的 7 天低用量退款判定需要「窗口内用量」,而提供该数据的 `GET /v1/usage/window` 排在 **P3**。按 §4 的字面顺序开工会立刻卡住。

**建议**:把 P3 的 `GET /v1/usage/window` 提到 M2 的第一项(它是纯读端点,工作量小、无依赖),再做 P2 退款。

### ⚠️ 阻塞 3:P2「注销联动」与 accounts 侧已锁的自助注销是同一件事

accounts 已就自助注销**拍板决策并写好实现计划**(软删 `pending_deletion` + 30 天冷静期 + MFA 校验 + `cancel_at_period_end` + 发 `billing_events` 记录待退),见 accounts `docs/tasks/2026-07-14-self-service-recovery-and-deletion.md` §待实现 B。

**P2 不应重新规划这一项**,应改为引用该文档,本文只保留 billing-service 侧的接口义务(即:提供待退金额的用量/余额查询,以及退款执行的落账)。两边同时规划会产生第二份互相漂移的设计。

### ⚠️ 阻塞 4:`reconcile` 命名已被占用,语义不同

本仓**已有** `POST /v1/jobs/reconcile`,但它只是 `RunCollectAndRate(ctx, "reconcile")` 的别名 —— **做的是用量补算,不是 Stripe 对账**。P3 计划新增的 `POST /v1/jobs/reconcile-stripe` 与之仅一词之差、语义完全不同,运维极易误调。

**建议**:P3 的新端点改名为 `POST /v1/jobs/stripe-drift-report`(或把现有的改名为 `/v1/jobs/recollect`),避免两个 reconcile 并存。

### M2 建议执行顺序

```
① 拍板阻塞 1(退款执行权)          ← 纯决策,不写代码
② P3-a: GET /v1/usage/window       ← 纯读端点,解开 P2 依赖
③ P2-a: 7 天低用量退款             ← 依赖 ①②
④ P2-b: 升降级 proration           ← 独立,可与 ③ 并行
⑤ P2-c: console 定价页             ← 依赖 billing_plans 已录入付费套餐(见 §6 前置)
⑥ 注销联动                          ← 归 accounts 主导,本仓配合退款落账
⑦ P3-b: drift 报告 + 指标           ← 独立
⑧ F1: 毛利/单位成本报表             ← 依赖 F0 真实数据(凭据接线,见 §4 F0 未完成项)
```

### M2 的前置条件(不属于开发任务,但不做就卡住)

- **付费套餐尚未录入**:`billing_plans` 目前只有 `TRIAL-7D` / `FREE` 种子。定价页(⑤)和升降级(④)都需要运营先在 admin 后台录入付费套餐 + Stripe Dashboard 建对应 Products/Prices。
- **F0 三云凭据接线未确认**:F1(⑧)依赖 `cloud_vendor_costs` 有真实数据。需先核实 billing-service 部署 env 是否已注入三云凭据。
- **Stripe 仍在 sandbox**:退款/proration 这类资金动作建议在 sandbox 全量回归后再切 live(切换 SOP 见交接文档 §三.3)。

## 5. 决策(2026-07-11 已拍板)

| # | 问题 | 结论 |
|---|---|---|
| 1 | entitlement sync 放哪 | ✅ **accounts 内联**(webhook 驱动,事件零滞后;billing-service 不碰 Stripe) |
| 2 | 套餐结构 | ✅ **走 admin 运营后台配置**(`billing_plans` 表 + admin CRUD 面),运营自助改,不阻塞开发 |
| 3 | 欠费降级梯度 | ✅ 1 次 failed=`arrears`,3 次或 7 天=`throttle`,14 天=`suspend`(进配置) |
| 4 | paygo 充值 | ✅ **推迟到 P2+**;`kind=paygo_topup` 表结构 P1 预留 |

P1/P2 范围按 §4 原样执行。

## 6. 风险与依赖

- **Webhook 是唯一真相推送通道**:P1 的事件审计表 + P3 的对账 job 是兜底双保险,缺一不可。
- **共享 PG 双写方**:accounts 写 profiles、billing-service 写 quota 消耗 —— 字段所有权要在 schema 注释里写死(profiles 归 accounts,quota_states 的 remaining/arrears 归 accounts 重置、消耗归 billing-service)。
- Stripe test/live 切换:上线前用 test mode 全量回归,`whsec` 与 `sk` 成对切换,Vault 里分 `_TEST` 后缀存。
- 既有 `xworkmate-app` 等其它 Stripe 使用方(如有)不受影响:本规划只动 accounts + billing-service。
