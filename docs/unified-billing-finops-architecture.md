# 统一账单·计费·支付·成本架构(双平面视图)

> **一句话**:一套共享数据底座(PostgreSQL + 统一身份/标签),两个平面 —— **消费平面**(订阅者的支付/账单/配额体验)与 **治理平面**(运营者的成本/毛利/优化闭环),四个域(支付、账单、计费、成本)各守边界、经明确接点相连。
> 连接 OpenTofu、Terraform、Infracost、OpenCost、云账单、运行指标与 PostgreSQL,覆盖 Kubernetes 与非 Kubernetes 负载,实现从部署前成本预测到运行时成本分摊、成本分析与优化治理的完整闭环。
> 上位文档:[open-platform-finops-control-plane.md](open-platform-finops-control-plane.md)(治理平面总纲)、[stripe-billing-integration-plan.md](stripe-billing-integration-plan.md)(消费平面实施)
> 定稿:2026-07-12

---

## 1. 四域定义与边界(先分清,再连接)

| 域 | 回答的问题 | 事实源 | 载体 |
|---|---|---|---|
| **支付 Payment** | 钱怎么进来 | **Stripe**(卡/订阅/退款;paygo 充值 P2+) | accounts `stripe.go`(唯一持 Stripe key) |
| **计费 Metering & Rating** | 用了多少、该记多少 | **用量事实**:xray 流量、AI token(LiteLLM) | xray-exporter → billing-service collect-and-rate → `traffic_minute_buckets`/`billing_ledger`/`account_quota_states` |
| **账单 Billing** | 该收/已收用户多少 | **订阅+权益**:`subscriptions`、`billing_plans`、Stripe invoice | accounts(entitlement sync、webhook 审计)+ Stripe 发票 |
| **成本 Cost** | 我们自己花了多少 | **成本事实**:云账单(CUR/Export)、Price Book(VPS/裸金属)、OpenCost(K8s)、AI 上游账单、Infracost(预测) | FinOps Control Plane(`cloud_vendor_costs` → FOCUS 化 Cost Warehouse) |

**铁律(成本口径,FINOPS-601)**:
- 用户侧总账只由 *账单域* 出(authoritative = Stripe invoice + ledger 应收);
- 平台侧总账只由 *成本域* 出(authoritative = 云账单/Price Book + adjustment);
- **计费域的 rating 结果既服务账单(配额扣减/超额计费)也服务成本分摊(usage-based allocation),但它本身不是总账**;
- estimated(Infracost)只作对比,allocated(分摊)不重复求和。

## 2. 双平面总图

```
                        ┌─────────────────────────────────────────────┐
                        │        共享数据底座 (PostgreSQL)              │
                        │  统一身份: user/service/product/team 标签      │
                        │  pgvector · pg_jieba · pgmq(billing_events)  │
                        └───────▲──────────────────────────▲──────────┘
                                │                          │
        ══ 消费平面(收入)══════╪══════════════════════════╪══ 治理平面(成本)══
                                │                          │
  订阅消费者                     │                          │              运营管理者
  ┌────────────┐   checkout    │                          │   PR diff   ┌────────────┐
  │ console    │──────────────▶ Stripe ◀─ webhook ─┐       │◀────────────│ Git PR /   │
  │ 定价页/面板 │               (支付域)            │       │  Infracost  │ IaC 仓     │
  └────┬───────┘                                  ▼       │             └────────────┘
       │ GET /api/billing/plans          accounts 账单域:  │
       │ 用量/账单/配额查询                webhook 审计去重  │   云账单 CUR/Export ── AWS/GCP/Azure
       ▼                                 entitlement sync  │   Price Book ───────── VPS/裸金属
  billing_plans 目录 ──────────────────▶ profiles/quota ───┤   OpenCost ─────────── K8s(有则接)
  (运营改表即调价,不发版)                     │             │   LiteLLM cost ─────── AI API
                                           ▼             ▼
                          计费域: xray-exporter → collect-and-rate     FOCUS 标准化
                          分钟桶 → ledger → 配额扣减/arrears            → Cost Warehouse
                                           │                          → 分摊引擎(team/product/
                                           │ usage 数据 ──────────────▶   service/env, 共享成本,
                                           │                              Unallocated 兜底)
                                           ▼                               │
                          PGMQ billing_events(生命周期事件)               ▼
                          activated/paid/failed/deleted        Grafana · Cost API · AI FinOps
                                                                (毛利 = 收入 − 分摊成本)
```

## 3. 面向订阅消费者(消费平面)

**设计原则**:消费者只看得见四样东西 —— *价格、配额、账单、控制权*。所有 FinOps 复杂度(分摊/毛利/口径)对其不可见。

### 3.1 旅程与能力面

| 旅程阶段 | 能力 | 实现载体 | 状态 |
|---|---|---|---|
| **见价** | 定价页,套餐/配额/功能对比 | `GET /api/billing/plans`(公开,active only)← `billing_plans` 目录 | ✅ P1(console 接入 P2) |
| **购买** | Stripe Checkout(卡);paygo 充值 | `POST /api/auth/stripe/checkout`(目录校验价格) | ✅(paygo P2+) |
| **开通** | 支付成功 → 秒级拿到配额与权益 | webhook → entitlement sync → profiles+quota;trial 自动开通 | ✅ P1 |
| **用量透明** | 实时看剩余配额/本期用量/历史曲线 | `GET /api/account/usage/summary|buckets`(计费域数据) | ✅ 已有 |
| **账单** | 发票、支付历史、下期扣款额 | Stripe Portal(`POST /api/auth/stripe/portal`)+ invoice 历史(P3 端点) | ✅/🟡 |
| **变更** | 升/降级(proration:升级即时、降级期末) | `POST /api/auth/subscriptions/change` | 🔴 P2 |
| **退订/退款** | 期末取消;首订 7 天用量 <5% 全额退 | cancel ✅;refund(用量判定查计费域分钟桶) | ✅/🔴 P2 |
| **异常体验** | 欠费:先标记→提醒→限流→14 天断流;恢复付款即自动复通 | arrears 链(P1)+ P1.5 执行面 + 催缴邮件 | 🟡 |
| **知情权** | 配额将尽/已停/欠费的主动通知 | PGMQ billing_events → 通知消费者(邮件/console 提示) | 🔴 P2 |

### 3.2 消费者视角的对账恒等式

```
本期应付 = 套餐固定费(Stripe invoice)
         + max(0, 用量 − included_quota) × 单价(超额,计费域 rating)
         − 退款/调整
```
消费者在 console 看到的每个数字必须能落到这条式子上 —— **用量数字与扣费数字同源**(同一套分钟桶),不允许"页面一个数、账单另一个数"。

### 3.3 消费者不可见但为其兜底的机制

- webhook 事件审计/去重:重复回调不会重复扣配额;
- 目录数据驱动:调价不发版、存量订阅按旧价续费(Stripe 行为);
- suspend 恢复自动化:付清即复通,无需人工工单。

## 4. 面向运营管理者(治理平面)

**设计原则**:运营者要的是 *一张能对上的总账* 和 *可执行的下一步*。生命周期 `Plan → Estimate → Deploy → Measure → Allocate → Analyze → Optimize` 每一环都有数据面与动作面。

### 4.1 生命周期能力面

| 环节 | 能力 | 载体 | 状态 |
|---|---|---|---|
| **Plan/Estimate** | PR 里看到本次变更的月成本 diff;策略卡点(超 500 USD/未标 cost_center 拒绝) | Infracost CLI + GitHub Actions + policy(FINOPS-101~104) | 🔴 Phase 1 |
| **Deploy** | IaC 统一标签(service/product/team/cost_center)贯穿资源 | OpenTofu/Terraform 模块规范(FINOPS-003) | 🔴 |
| **Measure** | 成本事实进仓:云账单(CUR/Export)、**Price Book(svc.plus 主来源:Contabo/Vultr 月租)**、OpenCost(有 K8s 时)、LiteLLM AI 成本、运行指标(node/process_exporter→VictoriaMetrics ✅已部署) | FinOpsSyncer(PR#8 API 版先行)→ FOCUS 化 Cost Warehouse(FINOPS-501/701) | 🟡 |
| **Allocate** | 主机→服务分摊(CPU/Mem/Disk/Net 权重)、共享成本(观测/CI/DB 平台)六种规则、Unallocated 兜底可监控 | 分摊引擎(FINOPS-6xx) | 🔴 Phase 2 |
| **Analyze** | 三张对比:Estimate vs Actual(工程)、**收入 vs 成本(毛利)**、预算 vs 实际(财务);Grafana 五面板 + Cost API | FINOPS-7xx/8xx/9xx | 🔴 |
| **Optimize** | 异常检测(DoD/WoW/预测偏离)、浪费清单(闲置 VM/盘/IP、异常 token)、优化建议(right-size/schedule/AI 模型路由) | FINOPS-10xx/11xx + AI FinOps Assistant | 🔴 Phase 3 |

### 4.2 运营者独有的三个闭环(两平面在此相连)

**① 毛利闭环(收入×成本)**
```
毛利(user/plan/period) = Stripe 净收入 − Σ 分摊成本(节点分摊 by 该用户流量占比 + AI 成本 by token + 共享成本)
```
回答:哪个套餐在亏钱?哪个重度用户是负毛利?→ 反哺 `billing_plans` 调价/调配额(改表即生效)。

**② 定价校准闭环(成本→目录)**
```
单位成本 = 分摊后月成本 / rated_bytes(或 token)
套餐健康度 = included_quota × 单位成本 vs 套餐定价
```
Price Book + 云账单给出真实单位成本,运营在 admin 后台改 `billing_plans` —— **这是"数据驱动调价"的完整回路**,消费平面(3.1 见价)即时反映。

**③ 催缴闭环(账单→执行)**
```
payment_failed → arrears(P1 ✅)→ 3次/7天 throttle 预警 → 14天 suspend
→ agent sync 断流(P1.5)→ invoice.paid → 自动复通
PGMQ billing_events 驱动通知与升级,全程留审计
```

### 4.3 运营者的口径纪律(防止总账打架)

- 报表必标成本角色(estimated/authoritative/allocated/adjustment)与币种口径(FINOPS-502:原币不可覆盖);
- OpenCost 与云账单不重复计费(FINOPS-204/MVP-8);
- 收入侧 `billing_ledger`(应收)与成本侧 warehouse 分域存放,毛利报表是**视图级 join**,不混写;
- 全部成本归属到 Team/Product 或显式进 Unallocated,Unallocated 比例超阈值告警(FINOPS-603)。

### 4.4 运营动作面(不只是看)

| 动作 | 入口 |
|---|---|
| 调价/上新/调配额 | admin `PUT /api/auth/admin/billing/plans/:planId`(✅ P1) |
| 暂停/恢复用户 | admin pause/resume(✅)+ 欠费自动链(P1.5) |
| 分摊规则/Price Book 维护 | FINOPS-304/602(版本化、可模拟、可重算历史) |
| 预算与告警 | FINOPS-902/1002 |
| 问 AI | FINOPS-1101 Tool API:"为什么 accounts 本周成本涨 32%?"→ 带口径/证据/置信度的解释 |

## 5. 两平面的连接点(汇总)

| 接点 | 方向 | 机制 |
|---|---|---|
| `billing_plans` 目录 | 治理→消费 | 运营调价,消费者见价/购买,单一事实源 |
| 用量数据(分钟桶) | 计费→双向 | 消费者的配额扣减 与 运营者的 usage-based 分摊,同源 |
| PGMQ `billing_events` | 消费→治理 | 生命周期事件驱动催缴/对账/通知,accounts 不感知消费者 |
| Revenue-based 分摊(FINOPS-602) | 消费→治理 | 收入数据参与共享成本分摊 |
| 毛利/单位成本报表 | 治理→消费 | 经调价回路影响定价页 |
| 统一身份/标签(FINOPS-003/503) | 底座 | user/service/product/team 贯穿四域,是一切 join 的前提 |

## 6. 分阶段汇总(两平面合并视图)

| 阶段 | 消费平面 | 治理平面 |
|---|---|---|
| **已达成** | 注册/OAuth→trial→Stripe checkout→权益→用量展示→期末取消(P0/P1) | 计量评率上线;云成本 API 版同步(PR#8);观测栈就绪 |
| **近期**(P1.5/P2 ∥ F0/Phase1) | 欠费断流与复通;退款;升降级;console 定价页;通知 | PR#8 合并+凭据;**Price Book(提前)**;AI 成本接入(提前);Cost Warehouse 十表;Infracost PR diff |
| **中期**(P3 ∥ Phase2) | invoice 历史;paygo | FOCUS 标准化;分摊引擎;毛利/单位成本报表;Grafana 面板 |
| **远期**(Phase3) | 个性化成本洞察(可选) | 预算/预测/异常/浪费检测/AI FinOps Assistant/策略治理 |
