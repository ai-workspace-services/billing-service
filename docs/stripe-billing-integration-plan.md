# Stripe 订阅 · 账单 · 计费打通规划

> 目标:把 Stripe 订阅收款、accounts 的订阅记录、billing-service 的计量/配额三个世界连成一条可运营的闭环,且**价格/套餐/权益调整全部数据驱动**(改表,不改代码、不重部署)。
> 最后更新:2026-07-11 · 状态:规划定稿待排期

## 0. 全景:三条线的 FinOps 闭环(2026-07-12 扩版)

> **上位蓝图**:本规划是 [Open Platform FinOps · Cloud-Neutral FinOps Control Plane](open-platform-finops-control-plane.md) 的早期增量 —— 收入/用量侧属商业计费域(蓝图外的互补面,经 Revenue-based 分摊与毛利报表相接),成本侧 FinOpsSyncer 是蓝图 Workstream E 的 API 版先行(现状映射见蓝图 §0)。

本规划从"Stripe 订阅打通"扩展为完整 FinOps 视图,三条数据线汇入同一个共享 PG:

| 线 | 内容 | 状态 | 载体 |
|---|---|---|---|
| **收入侧** | Stripe 订阅/账单 → 订阅记录 → 权益(entitlement) | P0 已合,P1 = accounts#19 | accounts(stripe.go / billing_plans / PGMQ 生产者) |
| **用量侧** | xray 流量 → 计量 → 评率/配额扣减 | 已上线运行 | xray-exporter → billing-service collect-and-rate |
| **成本侧** | AWS/GCP/Azure 基础设施成本同步 | PR#6 已合(表+骨架),**PR#8 实施中**(真 SDK) | billing-service FinOpsSyncer → `cloud_vendor_costs` |

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
- **消费者**(本仓,P1.5/P3 接入):`pgmq.read('billing_events', vt, qty)` + 处理后 `pgmq.delete/archive`;用于 arrears 升级触发、对账增量、催缴通知,替代轮询。消费失败消息按 pgmq 可见性超时自动重投。

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

- [ ] Stripe Dashboard(先 test mode):建 Products/Prices;Webhook endpoint = `https://accounts.svc.plus/api/billing/stripe/webhook`,拿 `whsec_*`
- [ ] Vault `kv/accounts.svc.plus` 增:`STRIPE_SECRET_KEY`、`STRIPE_WEBHOOK_SECRET` → 同步 GH secrets
- [ ] playbooks role:`app.env.j2` + `defaults/main.yml` + `target.yml` lineinfile 增 `STRIPE_*` 三变量(完全照抄 OAuth 那次的模式,含存量主机 lineinfile)
- [ ] accounts `pipeline.yml` deploy env 透传
- [ ] 验证:`stripe listen --forward-to` 本地回归 + prod test-mode 全流程(checkout → webhook → subscriptions 落库)

### P1.5 · 欠费执行面(0.5 周,接在 P1 后)

- [ ] billing-service:arrears 持续超阈值(14 天,配置化)→ `suspend_state='suspended'` 状态迁移
- [ ] accounts:`listAgentUsers` + `internalNetworkIdentities` 排除 `suspend_state='suspended'` 账号(join quota_states)→ agent 下次 sync 即断流
- [ ] 恢复路径:invoice.paid / 手动清欠 → `suspend_state='active'` → 下次 sync 恢复接入
- [ ] console:arrears/throttled 状态提示 + 催缴邮件(SMTP 已有)

### P1 · 打通订阅→权益闭环(核心,1~2 周)

- [ ] `billing_plans` 表 + store 层 + 种子数据(TRIAL-7D、首批付费套餐)+ admin CRUD
- [ ] `stripe_webhook_events` 表(event_id 去重、payload 审计、处理状态),webhook 先落事件再处理
- [ ] **entitlement sync**(accounts 内,webhook 驱动):
  - `customer.subscription.created/updated` → 按 plan 写 `account_billing_profiles`
  - `invoice.paid` → 重置 `account_quota_states.remaining_included_quota`、清 `arrears`
  - `invoice.payment_failed` → `arrears=true`,N 次后 `throttle_state/suspend_state` 升级(阈值进配置)
  - `customer.subscription.deleted` / trial 过期 → 降回 free 档案
- [ ] checkout 校验改读 `billing_plans`,下线 `STRIPE_ALLOWED_PRICE_IDS`
- [ ] trial→付费转换:新订阅生效时把 trial 记录标记 `superseded`
- [ ] 测试:webhook 全事件表驱动测试 + 幂等重放测试

### P2 · 自助与政策落地(1 周)

- [ ] **7 天低用量退款**:`POST /api/auth/subscriptions/refund` —— 判定(订阅 ≤7 天 && 窗口内用量 < 5% 配额,用量查 `traffic_minute_buckets` 聚合)→ Stripe refund API → 订阅终止 + 档案降级 + ledger 冲正记录
- [ ] 升级/降级:`POST /api/auth/subscriptions/change`,Stripe subscription item 替换,proration 策略 = `create_prorations`(升级即时,降级期末)
- [ ] console:定价页读 `/api/billing/plans`;`panel/subscription` 补退款/变更入口
- [ ] 注销联动(产品决策):`pending_cancellation` → 订阅期末终止 → 30 天删除冷静期;paygo 余额原路退

### F0 · 成本侧落地(并行线,不阻塞 P 线)

- [ ] 评审合并 **PR#8**(多云 SDK 集成;注意 go.mod 膨胀 +67 依赖属预期,SDK 家族使然)
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
