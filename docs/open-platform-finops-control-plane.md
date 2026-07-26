# Open Platform FinOps · Cloud-Neutral FinOps Control Plane

> **定位**:Open Platform 解决方案的统一成本治理能力总纲(产品层 = Open Platform FinOps,能力层 = Cloud-Neutral FinOps Control Plane)。
> **与本仓关系**:billing-service 当前的三线规划([stripe-billing-integration-plan.md](stripe-billing-integration-plan.md))是本蓝图的**早期增量**——收入侧(Stripe)+ 用量侧(xray)属于商业计费域,成本侧(FinOpsSyncer)是本蓝图 Workstream E 的 API 版先行实现。映射见 §0。
> **生命周期**:`Plan → Estimate → Deploy → Measure → Allocate → Analyze → Optimize`
> 定稿:2026-07-12(用户授权全文入库)

---

## 0. 现状映射:已有资产 ↔ FINOPS 任务(2026-07-12 实勘增补)

本节为落地导航,把 svc.plus 当前资产对到蓝图任务编号,标注"已有/进行中/缺失":

| FINOPS 任务 | 现状 | 资产/差距 |
|---|---|---|
| FINOPS-401/402/403(云账单) | 🟡 **API 版先行**:billing-service PR#6(已合,`cloud_vendor_costs` 表+FinOpsSyncer)+ **PR#8 已合并**(AWS CostExplorer API / GCP BigQuery 查询 / Azure Consumption API,T-2 窗口);⚠️ 三云凭据接线待核实,代码在但可能尚未拉到真实数据 | 蓝图要求 **CUR/Export 文件级**接入(S3/BQ export/Storage export)——PR#8 的 API 路线是 MVP 垫脚石,Phase 1/2 需演进为 export 路线(资源级明细 + Credit/Refund + 摊销口径),两条路线数据可共存对账 |
| FINOPS-404(连接器接口) | 🔴 缺 | PR#8 是硬编码三云;`list_accounts/sync_cost/.../validate_credentials` 统一接口待抽象;凭证 Vault 已定 `kv/billing-service`(✅ 符合 FINOPS-1201) |
| FINOPS-202(VictoriaMetrics) | ✅ **已部署**:install.svc.plus 跑着 xstream_victoriametrics/victorialogs/victoriatraces + Grafana | vmagent scrape OpenCost 待接(依赖 K8s) |
| FINOPS-201/203/204(OpenCost/K8s) | 🔴 无 K8s 集群 | 当前 estate 全是 VPS/裸金属 docker compose —— **对 svc.plus 而言 Workstream D(非 K8s)优先于 C**,蓝图 Phase 1 的 OpenCost 项在本环境后置 |
| FINOPS-301/302(非 K8s 资产/主机指标) | 🟡 部分:tky-proxy 已跑 node_exporter + process_exporter(→VictoriaMetrics);资产清单无 | 资产表(FINOPS-301)与 VM→服务分摊(303)缺;主机指标采集面已有底子 |
| FINOPS-304(Price Book) | 🔴 缺 | svc.plus 的 Contabo/Vultr VPS 无云账单 API,**Price Book 是本环境成本事实的主来源**(月租/带宽/IPv4),优先级应提前 |
| FINOPS-305(AI API 成本) | 🟡 有底子:litellm 仓库在运营(模型路由),LiteLLM 自带 cost tracking | 采集→cost warehouse 管道缺;与 xworkmate per-agent 成本诉求直接相关 |
| FINOPS-101~104(Infracost) | 🔴 缺 | ai-workspace-infra 有 OpenTofu/Terraform(IaC 仓);CI 已是 GitHub Actions —— 接入面现成 |
| FINOPS-501(FOCUS 模型) | 🔴 缺 | `cloud_vendor_costs` 是简化前身,Phase 1 建 FOCUS 化的 `cost_actual` 后迁移 |
| FINOPS-601(成本角色) | 🟡 概念已现 | 现规划已区分 estimated/authoritative(§蓝图);billing-service 的 `billing_ledger`(收入侧应收)与成本事实分域,不混同一张总账 |
| FINOPS-701(Cost Warehouse) | 🟡 PG 就绪 | 共享 PG(pgvector/pg_jieba/pgmq 镜像)即 warehouse 载体;10 张核心表待建 |
| FINOPS-901~903(Cost API) | 🔴 缺 | 供 AI Workspace 调用是 MVP 硬条件(见 §19-10) |
| FINOPS-1101~1103(AI Assistant) | 🟡 生态就绪 | hermes-agent/openclaw 插件体系 + qmd PG memory 是天然宿主;Tool API 依赖 Cost API 先行 |
| FINOPS-1201(Vault) | ✅ 路径已定 | `kv/billing-service`(Stripe+三云凭据已规划);Infracost API Key 加同路径 |
| **收入侧(蓝图外)** | ✅ P1 完成 | Stripe→entitlement(accounts#19)不在本蓝图内,但 FINOPS-602 的 **Revenue-based 分摊**与 F1 毛利报表即两域接点 |

**组织建议(待拍板)**:billing-service 保持**商业计费域**(收入+用量+权益);蓝图的 `finops-api/ingestor/normalizer/allocator/analyzer` 五服务(FINOPS-001)另立 `open-platform/finops`;`FinOpsSyncer` 成熟后从 billing-service 迁入 finops-ingestor。共享同一 PG。

**svc.plus 环境的 Phase 1 修正**(相对蓝图 §18):OpenCost 项后置(无 K8s),提前 FINOPS-304(Price Book,VPS 成本主来源)与 FINOPS-305(AI API 成本,LiteLLM 现成),其余照蓝图。

---

## 1. 项目定位

Cloud-Neutral FinOps Control Plane 是 Open Platform 解决方案的一部分,用于统一管理多云、Kubernetes、虚拟机、裸金属、托管数据库、SaaS 与 AI 服务成本。

系统覆盖完整 FinOps 生命周期:

```
Plan → Estimate → Deploy → Measure → Allocate → Analyze → Optimize
```

核心目标:

- 在代码提交阶段预测基础设施成本
- 在资源运行阶段采集实际成本
- 对 Kubernetes 与非 Kubernetes 负载进行统一分摊
- 对比预计成本与实际成本
- 建立团队、项目、产品和环境维度的成本视图
- 发现成本异常、资源浪费与预算风险
- 为 AI Cost Analysis 提供标准化数据接口

## 2. Open Platform 中的位置

```
Open Platform
│
├── Infrastructure as Code
│     ├── OpenTofu
│     ├── Terraform
│     └── GitOps
│
├── Kubernetes Platform
│     ├── Cluster Management
│     ├── Runtime
│     └── OpenCost
│
├── Observability Platform
│     ├── Vector
│     ├── vmagent
│     ├── VictoriaMetrics
│     └── Grafana
│
├── Data Platform
│     ├── PostgreSQL
│     ├── Object Storage
│     └── Cost Warehouse
│
└── FinOps Control Plane
      ├── Infracost
      ├── OpenCost
      ├── Cloud Billing
      ├── Cost Allocation
      ├── Cost Analytics
      └── AI FinOps Assistant
```

## 3. 总体架构

```
Git Pull Request
       │
       ▼
OpenTofu / Terraform
       │
       ▼
Infracost CLI
       │
       ├── PR Cost Diff
       ├── Policy Check
       └── JSON Estimate
                │
                ▼
            PostgreSQL
                │
────────────────┼──────────────────────────────
                │
             Deploy
                │
       ┌────────┴────────┐
       │                 │
       ▼                 ▼
 Kubernetes         Non-Kubernetes
       │                 │
   OpenCost         Runtime Metrics
       │          node/process exporters
       │                 │
       └────────┬────────┘
                │
                ▼
          Cost Allocator
                │
                ▼
Cloud Billing / CUR / Billing Export
                │
                ▼
        Cost Normalization
                │
                ▼
     PostgreSQL Cost Warehouse
                │
       ┌────────┼────────┐
       ▼        ▼        ▼
    Grafana   Cost API   AI Analysis
```

## 4. Workstream A:项目基础与架构

### FINOPS-001|建立 FinOps 项目结构

创建组件:

```
open-platform/
└── finops/
    ├── services/
    │   ├── finops-api/
    │   ├── finops-ingestor/
    │   ├── finops-normalizer/
    │   ├── finops-allocator/
    │   └── finops-analyzer/
    ├── database/
    │   ├── migrations/
    │   └── seeds/
    ├── dashboards/
    ├── policies/
    ├── deploy/
    └── docs/
```

验收标准:
- 完成模块职责定义
- 完成服务间数据流说明
- 完成开发、测试、生产环境配置
- 所有组件可通过容器运行

### FINOPS-002|定义统一成本领域模型

定义核心概念:Cost Estimate、Actual Cost、Allocated Cost、Adjustment、Resource、Service、Product、Team、Cost Center、Billing Account、Environment、Allocation Rule

验收标准:
- 完成 ER 图
- 完成字段字典
- 明确金额、币种、时间窗口和精度规范
- 明确成本事实与分摊结果之间的关系

### FINOPS-003|定义统一标签规范

基础标签:

```
managed_by
repository
service
product
team
environment
cost_center
owner
```

验收标准:
- Terraform 与 OpenTofu 模块支持统一标签
- Kubernetes Label 与 Annotation 使用统一命名
- 云资源和运行负载可映射到相同业务维度
- 缺失关键标签时可产生告警

## 5. Workstream B:Infracost 成本预测

### FINOPS-101|集成 Infracost CLI

实现:Terraform 成本分析、OpenTofu 成本分析、Infracost JSON 输出、多项目配置支持

验收标准:`infracost breakdown` / `infracost diff` / `infracost output --format json` 均可在 CI 环境执行。

### FINOPS-102|实现 Pull Request Cost Diff

在 PR 中展示:变更前月成本、变更后月成本、月成本差异、百分比变化、主要增减资源、缺少价格信息的资源

验收标准:
- PR 自动生成成本评论
- 新提交后自动更新原评论
- 支持 GitHub Actions
- 失败不阻断时有明确提示

### FINOPS-103|实现成本策略检查

策略示例:

```
单个 PR 月成本增加不得超过 500 USD
生产环境月成本增长不得超过 20%
GPU 资源必须经过审批
未设置 cost_center 的资源不得部署
```

验收标准:
- 支持 warn 和 deny 两种策略
- 支持按环境设置不同阈值
- 策略配置可版本管理
- 结果写入 PR Check

### FINOPS-104|保存成本预测数据

保存字段:Repository、Pull Request、Commit SHA、Branch、Environment、Resource Address、Resource Type、Region、Estimated Cost Before/After/Diff、Raw JSON

验收标准:
- 同一 PR 支持多个版本
- 支持查询某个 Commit 的预测结果
- 支持按项目和环境聚合
- 原始 JSON 可追溯

## 6. Workstream C:Kubernetes 成本

### FINOPS-201|部署 OpenCost

部署组件:OpenCost、kube-state-metrics、Prometheus-compatible metrics endpoint、vmagent scrape configuration

验收标准:
- OpenCost 可读取集群指标
- 可计算 CPU、内存、存储和节点成本
- 可识别空闲资源
- 可按 Namespace 和 Workload 查询

### FINOPS-202|对接 VictoriaMetrics

实现:`OpenCost Metrics → vmagent → VictoriaMetrics`

验收标准:
- 不依赖独立 Prometheus Server
- OpenCost 查询所需指标完整
- 指标保留周期可配置
- 多集群指标可区分

### FINOPS-203|采集 OpenCost Allocation API

采集维度:Cluster、Node、Namespace、Deployment、StatefulSet、Pod、Container、Label、Service

验收标准:
- 支持小时级与日级采集
- 支持增量同步
- 同一时间窗口不可重复写入
- 原始响应可归档

### FINOPS-204|采集 Kubernetes Asset Cost

采集:Node、Disk、Load Balancer、Persistent Volume、Cluster Management Cost

验收标准:
- Asset 成本与 Allocation 成本分开存储
- 可追踪云资源 ID
- 可映射云账单资源
- 避免重复计费

## 7. Workstream D:非 Kubernetes 成本

### FINOPS-301|建立非 K8s 资产清单

覆盖:Virtual Machine、Bare Metal、VPS、Managed Database、Object Storage、CDN、Load Balancer、NAT Gateway、SaaS、AI API、GPU Instance

验收标准:
- 每项资产具有唯一标识
- 包含 Provider、Region、Account 和 Resource ID
- 可关联服务、团队和产品
- 支持资产生命周期状态

### FINOPS-302|采集主机运行指标

使用:node_exporter、process_exporter、vmagent、VictoriaMetrics

采集维度:CPU、Memory、Disk、Network、Process、Service

验收标准:
- 可以识别主机上的主要进程
- 可按服务聚合资源使用量
- 数据可用于成本分摊
- 指标缺失时有降级策略

### FINOPS-303|实现 VM 到服务的成本分摊

默认模型:

```
Service Cost =
Host Cost × (
CPU Share × CPU Weight +
Memory Share × Memory Weight +
Disk Share × Disk Weight +
Network Share × Network Weight
)
```

验收标准:
- 权重可以配置
- 分摊比例总和为 100%
- 支持固定分摊与动态分摊
- 保存使用的分摊规则版本

### FINOPS-304|建立自定义 Price Book

价格类型:VPS 月租、裸金属折旧、CPU Core Hour、Memory GB Hour、Storage GB Month、Network GB、IPv4、Backup、Support、Electricity、Rack、Management Fee

验收标准:
- 支持生效时间
- 支持不同区域和币种
- 支持版本历史
- 可应用到没有云账单的资产

### FINOPS-305|纳管 AI API 成本

支持维度:Provider、Model、Input/Output/Cached Token、Request Count、Image Generation、Embedding、Audio、GPU Runtime

验收标准:
- 支持 OpenAI-compatible 使用数据
- 支持 LiteLLM 成本数据
- 支持按用户、项目和模型分摊
- 可计算单任务和单 Agent 成本

## 8. Workstream E:云账单集成

### FINOPS-401|接入 AWS Cost and Usage Report

实现:AWS CUR 或 Data Exports、S3 原始数据读取、账单增量同步、Resource ID 与 Tag 提取

验收标准:
- 支持按天同步
- 支持 Unblended、Amortized 和 Net Cost
- 支持 Credit 与 Refund
- 支持 Account 和 Region 维度

### FINOPS-402|接入 GCP Billing Export

实现:BigQuery Billing Export、Project/Service/SKU 映射、Credit 与 Discount 处理

验收标准:
- 支持 Detailed Billing Export
- 支持项目与资源级成本
- 支持 Label 映射
- 支持增量同步

### FINOPS-403|接入 Azure Cost Export

实现:Azure Cost Management Export、Subscription 与 Resource Group 映射、Azure Storage 数据读取

验收标准:
- 支持实际成本和摊销成本
- 支持 Reservation 和 Savings Plan
- 支持 Tag 与 Resource ID
- 支持多 Subscription

### FINOPS-404|设计云账单连接器接口

统一接口:

```
list_accounts
sync_cost
sync_resources
sync_prices
get_sync_status
validate_credentials
```

验收标准:
- 云厂商连接器实现统一接口
- 新 Provider 可以插件化扩展
- 凭证存储在 Vault
- 每次同步有完整审计记录

## 9. Workstream F:成本标准化

### FINOPS-501|实现 FOCUS 数据模型映射

将不同数据源映射为统一字段:Provider、Billing Account、Resource ID、Service Category、Service Name、SKU、Charge Period、Usage Quantity、Usage Unit、List Cost、Billed Cost、Effective Cost、Currency、Tags

验收标准:
- AWS、GCP、Azure 可进入同一查询模型
- OpenCost 与自定义资产可映射
- 保留原始字段
- 映射规则可追溯

### FINOPS-502|实现币种转换

支持:原币种、统一展示币种、汇率日期、汇率来源、月度固定汇率

验收标准:
- 原始币种金额不可覆盖
- 转换金额可重新计算
- 支持 USD、CNY、JPY、EUR
- 汇率异常时停止错误换算

### FINOPS-503|建立资源身份映射

关联:

```
Terraform Resource
Cloud Resource
Kubernetes Node
Kubernetes Workload
VM Process
Service
Product
Team
```

验收标准:
- 支持一对多关系
- 支持资源名称变化
- 支持资源生命周期
- 无法映射的成本进入 Unallocated

## 10. Workstream G:成本分摊引擎

### FINOPS-601|实现成本角色分类

成本角色:

```
estimated
authoritative
allocated
adjustment
```

验收标准:
- 总成本只使用 authoritative 与 adjustment
- allocated 不参与总账重复求和
- estimated 只用于预测比较
- 所有报表明确成本口径

### FINOPS-602|实现共享成本分摊

共享成本包括:Observability、CI/CD、Kubernetes Control Plane、Database Platform、Network Gateway、Security Platform、Support、Operations

分摊方式:Equal、Proportional、Fixed、Usage-based、Revenue-based、Custom

验收标准:
- 支持规则优先级
- 支持按月生效
- 支持规则模拟
- 支持重新计算历史周期

### FINOPS-603|处理未分摊成本

建立:

```
Unallocated
Untagged
Unknown Resource
Shared Platform
```

验收标准:
- 所有成本必须进入一个归属桶
- Unallocated 比例可以监控
- 超过阈值时告警
- 支持逐步补充映射规则

## 11. Workstream H:Cost Warehouse

### FINOPS-701|创建 PostgreSQL 成本库

核心表:finops_resource、cost_estimate、cost_actual、cost_allocation、cost_adjustment、allocation_rule、price_book、billing_account、cost_sync_job、cost_anomaly

验收标准:
- 完成数据库 Migration
- 关键字段有索引
- 金额使用高精度 Decimal
- 支持小时、日和月聚合

### FINOPS-702|建立原始数据存储

原始数据:

```
Object Storage
├── infracost/
├── opencost/
├── aws-cur/
├── gcp-billing/
├── azure-billing/
└── custom-assets/
```

验收标准:
- 原始数据不可修改
- 按 Provider 和日期分区
- 可追溯到同步任务
- 支持失败后重新处理

### FINOPS-703|创建成本聚合视图

视图:cost_hourly、cost_daily、cost_monthly、cost_by_team、cost_by_product、cost_by_service、cost_by_environment、estimate_actual_variance、unallocated_cost

验收标准:
- Grafana 可直接查询
- 常用查询响应稳定
- 支持按时间和标签筛选
- 聚合结果与明细一致

## 12. Workstream I:Grafana Dashboard

- **FINOPS-801|FinOps Overview**:Total Cost、Month-to-Date、Forecast、Budget Usage、Estimated vs Actual、Cost Change、Unallocated Cost、Top Cost Services
- **FINOPS-802|Kubernetes Cost Dashboard**:Cluster/Namespace/Workload/Node/Idle Cost,CPU、Memory、GPU 和 PV Cost
- **FINOPS-803|Non-Kubernetes Cost Dashboard**:VM/Managed Database/Network/Storage/VPS/SaaS/AI API Cost
- **FINOPS-804|Engineering Cost Dashboard**:Repository、Pull Request、Commit、Terraform Project、Estimated Cost Change、Actual Cost Drift
- **FINOPS-805|Cost Allocation Dashboard**:Team、Product、Service、Environment、Cost Center、Shared Cost、Unallocated Cost

## 13. Workstream J:Cost API

### FINOPS-901|实现成本查询 API

```
GET /cost/summary
GET /cost/resources
GET /cost/services
GET /cost/teams
GET /cost/products
GET /cost/kubernetes
GET /cost/non-kubernetes
GET /cost/unallocated
```

### FINOPS-902|实现 Estimate vs Actual API

```
GET /cost/variance
GET /cost/variance/pr/{number}
GET /cost/variance/repository/{name}
GET /cost/variance/service/{name}
```

### FINOPS-903|实现预算与预测 API

```
GET /budgets
POST /budgets
GET /forecast
GET /budget-status
```

验收标准:
- API 支持分页
- 支持时间范围查询
- 支持维度过滤
- 权限按组织与项目隔离

## 14. Workstream K:异常检测与告警

### FINOPS-1001|实现成本异常检测

检测类型:Day-over-Day、Week-over-Week、Month-over-Month、Static Threshold、Percentage Threshold、Forecast Deviation、Estimate Drift

验收标准:
- 可以配置不同服务阈值
- 支持抑制短时噪声
- 异常记录保存证据
- 支持人工确认和关闭

### FINOPS-1002|实现预算告警

```
预算达到 50%
预算达到 80%
预算达到 100%
预计月底超预算
```

### FINOPS-1003|实现资源浪费检测

检测:Idle Kubernetes Node、Idle Pod、Underutilized VM、Unattached Disk、Idle Load Balancer、Unused Public IP、Oversized Database、High NAT Cost、Abnormal AI Token Usage

## 15. Workstream L:AI FinOps Assistant

### FINOPS-1101|提供 AI Tool API

```
get_cost_summary
compare_estimate_actual
find_cost_anomalies
explain_cost_change
get_idle_resources
get_untagged_cost
get_unallocated_cost
get_unit_cost
```

### FINOPS-1102|实现成本变化解释

示例问题:

```
为什么 accounts 服务本周成本上涨 32%?
哪个 PR 导致生产环境成本增加?
哪些资源可以在本月节省成本?
为什么 AWS 账单和 OpenCost 不一致?
```

输出必须包含:时间范围、成本口径、数据来源、主要变化项、推测原因、优化建议、置信度

### FINOPS-1103|实现优化建议

建议类型:Right-sizing、Shut Down、Schedule、Reserved Capacity、Savings Plan、Storage Tier、Network Optimization、Kubernetes Requests/Limits、AI Model Routing、Cache Optimization

验收标准:
- 建议必须基于真实数据
- 明确预计节省金额
- 区分风险等级
- 不自动执行破坏性操作

## 16. Workstream M:安全与权限

### FINOPS-1201|账单凭证接入 Vault

要求:AWS Role、GCP Service Account、Azure Service Principal、Infracost API Key、Billing Export Credentials
(已定路径:`kv/billing-service`,后续 finops 独立后可迁 `kv/finops`)

### FINOPS-1202|实现 RBAC

角色:Platform Admin、FinOps Admin、Billing Viewer、Team Owner、Project Viewer、Auditor

### FINOPS-1203|实现审计日志

审计范围:数据同步、Price Book 修改、Allocation Rule 修改、Budget 修改、权限修改、AI Tool 调用

## 17. Workstream N:文档与交付

- **FINOPS-1301|架构文档**:System Context、Component Architecture、Data Flow、Deployment Architecture、Cost Data Model、Security Model
- **FINOPS-1302|接入手册**:Infracost、OpenCost、AWS/GCP/Azure Billing、VPS 与裸金属、AI API 成本接入
- **FINOPS-1303|运营手册**:数据同步失败处理、成本差异排查、账单重算、分摊规则调整、月末结算、异常成本调查

## 18. 分阶段交付计划

### Phase 1|MVP

范围:Infracost PR Cost Diff、Infracost JSON 入库、OpenCost 接入*、AWS CUR 接入、PostgreSQL Cost Warehouse、Grafana 基础 Dashboard、Estimate vs Actual

> *svc.plus 环境修正(§0):OpenCost 后置(无 K8s),提前 FINOPS-304 Price Book + FINOPS-305 AI API 成本。

交付目标:

```
Git PR → Cost Estimate → Deploy → Actual Cost → Grafana
```

### Phase 2|统一成本分摊

范围:非 K8s VM 和进程成本、GCP 与 Azure Billing、自定义 Price Book、Team/Product/Service 分摊、Shared Cost、Unallocated Cost

交付目标:

```
Kubernetes + VM + Database + VPS + Cloud Billing
```

### Phase 3|FinOps Automation

范围:Budget、Forecast、Cost Anomaly、Waste Detection、Policy Enforcement、AI FinOps Assistant

交付目标:

```
Detect → Explain → Recommend → Govern
```

## 19. MVP 完成定义

1. Pull Request 可看到基础设施成本变化
2. Infracost 预测结果可以进入 PostgreSQL
3. OpenCost 可以提供 Kubernetes 成本分摊(svc.plus 环境:以 FINOPS-303 VM 分摊替代)
4. AWS CUR 可以提供实际云账单
5. 非 Kubernetes 资源可以按服务维度展示成本
6. Grafana 可以展示预计成本与实际成本
7. 系统能够区分财务事实、成本预测和成本分摊
8. 不发生 OpenCost 与云账单重复计费
9. 所有成本可以归属到 Team、Product 或 Unallocated
10. Cost API 可以供 AI Workspace 调用

## 20. 解决方案命名

- 产品层名称:**Open Platform FinOps**
- 能力层名称:**Cloud-Neutral FinOps Control Plane**

核心描述:Cloud-Neutral FinOps Control Plane 是 Open Platform 的统一成本治理能力,连接 OpenTofu、Terraform、Infracost、OpenCost、云账单、运行指标与 PostgreSQL,覆盖 Kubernetes 与非 Kubernetes 负载,实现从部署前成本预测到运行时成本分摊、成本分析与优化治理的完整闭环。
