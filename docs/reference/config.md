# config 包参考

本文档覆盖 `internal/config/config.go`，说明运行配置结构、环境变量来源和全部生产函数。

## 包职责

`internal/config` 负责把环境变量解析成运行时可直接使用的 `Config`，并处理：

- 镜像标识拆解
- exporter 来源列表构造
- 默认值和必填项校验
- 数值型环境变量解析

## 类型

### `ExporterSource`

签名：`type ExporterSource struct`

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `SourceID` | `string` | 来源唯一标识，写入 `billing_source_sync_state` 的主键 |
| `BaseURL` | `string` | exporter 基础地址，`FetchWindow` 会在其后拼接 `/v1/snapshots/window` |
| `ExpectedNodeID` | `string` | 期望上游返回的 `snapshot.node_id`，用于来源校验 |
| `ExpectedEnv` | `string` | 期望上游返回的 `snapshot.env`，用于来源校验 |
| `Enabled` | `bool` | 是否参与当前轮采集 |
| `TimeoutSeconds` | `int` | 单个来源 HTTP 请求超时秒数，<= 0 时使用默认值 15 |

### `Config`

签名：`type Config struct`

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `ImageRef` | `string` | 原始 `IMAGE` 环境变量 |
| `ImageTag` | `string` | 从 `ImageRef` 中解析出的 tag |
| `ImageCommit` | `string` | 从 tag 中解析出的完整 40 位 commit |
| `ImageVersion` | `string` | 当前实现与 `ImageCommit` 相同，用于 `/api/ping` |
| `ExporterBaseURL` | `string` | 旧单来源兼容入口，对应 `EXPORTER_BASE_URL` |
| `ExporterSources` | `[]ExporterSource` | 当前实际启用的来源清单，推荐由 `EXPORTER_SOURCES_JSON` 生成 |
| `InternalServiceToken` | `string` | 调用 exporter 的 Bearer token |
| `DatabaseURL` | `string` | Resolved PostgreSQL DSN; `SUPABASE_CONNECT_URI`, then `SUPABASE_CONNECT_URL`, then `DATABASE_URL` |
| `ListenAddr` | `string` | HTTP 监听地址，默认 `:8081` |
| `CollectInterval` | `time.Duration` | 后台定时采集间隔，默认 1 分钟 |
| `DefaultRegion` | `string` | 写入分钟桶时的默认地域 |
| `SourceRevision` | `string` | 当前写路径版本标识，默认 `billing-service-v1` |
| `PricePerByte` | `float64` | 默认每字节价格 |
| `InitialIncludedQuotaBytes` | `int64` | 默认初始包含流量 |
| `InitialBalance` | `float64` | 默认初始余额 |

### `rawExporterSource`

签名：`type rawExporterSource struct`

这是 `EXPORTER_SOURCES_JSON` 的中间反序列化结构，不直接暴露给其他包。

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `SourceID` | `string` | JSON `source_id` |
| `BaseURL` | `string` | JSON `base_url` |
| `ExpectedNodeID` | `string` | JSON `expected_node_id` |
| `ExpectedEnv` | `string` | JSON `expected_env` |
| `Enabled` | `*bool` | JSON `enabled`，指针用于区分“未传”和“显式 false” |
| `TimeoutSeconds` | `int` | JSON `timeout_seconds` |

## 环境变量映射

| 环境变量 | 是否必填 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `IMAGE` | 否 | 空字符串 | 供 `/api/ping` 返回镜像身份 |
| `EXPORTER_SOURCES_JSON` | 条件必填 | 无 | 推荐的多来源配置入口 |
| `EXPORTER_BASE_URL` | 条件必填 | 无 | 当前兼容路径，仅在未提供 `EXPORTER_SOURCES_JSON` 时使用 |
| `INTERNAL_SERVICE_TOKEN` | 是 | 无 | exporter Bearer token |
| `SUPABASE_CONNECT_URI` | 否 | 空字符串 | Supabase PostgreSQL 连接 URI；设置后优先于其他数据库连接变量 |
| `SUPABASE_CONNECT_URL` | 否 | 空字符串 | `SUPABASE_CONNECT_URI` 的兼容别名 |
| `DATABASE_URL` | 条件必填 | 无 | VPS/self-hosted PostgreSQL 连接串；仅在两个 Supabase 变量均为空时使用 |
| `LISTEN_ADDR` | 否 | `:8081` | HTTP 监听地址 |
| `COLLECT_INTERVAL` | 否 | `1m` | 后台定时采集间隔 |
| `DEFAULT_REGION` | 否 | 空字符串 | 分钟桶地域字段 |
| `SOURCE_REVISION` | 否 | `billing-service-v1` | 写路径版本号 |
| `PRICE_PER_BYTE` | 否 | `0` | 默认单价 |
| `INITIAL_INCLUDED_QUOTA_BYTES` | 否 | `0` | 默认初始包含流量 |
| `INITIAL_BALANCE` | 否 | `0` | 默认初始余额 |

## 函数

### `Load`

- 签名：`func Load() (Config, error)`
- 参数：无
- 返回：
  - `Config`：已填充默认值和解析结果的配置对象
  - `error`：必填项缺失、时长解析失败、来源 JSON 解析失败时返回
- 职责：
  - 读取全部环境变量
  - 调用 `parseImageRef` 拆解镜像信息
  - 调用 `loadExporterSources` 生成 `ExporterSources`
  - 处理默认值和必填项校验
- 调用位置：
  - `cmd/billing-service/main.go`
- 主要副作用：
  - 读取进程环境变量
- 错误/边界条件：
  - `SUPABASE_CONNECT_URI`、`SUPABASE_CONNECT_URL`、`DATABASE_URL` 全部为空时报错
  - `INTERNAL_SERVICE_TOKEN` 为空时报错
  - `COLLECT_INTERVAL` 无法解析时报错
  - `EXPORTER_SOURCES_JSON` 非法或为空列表时报错
  - 若 `EXPORTER_SOURCES_JSON` 未设置，则要求 `EXPORTER_BASE_URL` 存在

### `parseImageRef`

- 签名：`func parseImageRef(imageRef string) (tag, commit, version string)`
- 参数：
  - `imageRef`：完整镜像引用，例如 `registry.example.com/billing-service:sha-<40位SHA>`
- 返回：
  - `tag`：冒号后的 tag
  - `commit`：当 tag 为完整 40 位 SHA 或 `sha-<40位SHA>` 时提取出的 commit
  - `version`：当前实现与 `commit` 相同
- 职责：
  - 为 `/api/ping` 提供可校验的运行镜像身份
- 调用位置：
  - `Load`
- 主要副作用：无
- 错误/边界条件：
  - 空字符串、无冒号、无 tag、tag 不是完整 40 位 SHA 时，返回空结果而非报错

### `loadExporterSources`

- 签名：`func loadExporterSources(legacyBaseURL, rawJSON string) ([]ExporterSource, error)`
- 参数：
  - `legacyBaseURL`：旧单来源配置
  - `rawJSON`：`EXPORTER_SOURCES_JSON` 原始内容
- 返回：
  - `[]ExporterSource`：可直接参与采集的来源配置
  - `error`：来源缺失、JSON 非法、必填字段缺失时返回
- 职责：
  - 优先使用 `EXPORTER_SOURCES_JSON`
  - 在未设置 JSON 时退回到单来源兼容模式
  - 为来源填充默认超时和 `enabled=true`
- 调用位置：
  - `Load`
- 主要副作用：无
- 错误/边界条件：
  - 两种来源配置都缺失时报错
  - JSON 为空数组时报错
  - 某个来源缺少 `source_id` 或 `base_url` 时按来源维度报错
  - `timeout_seconds <= 0` 时回填为 15

### `parseFloatEnv`

- 签名：`func parseFloatEnv(key string, fallback float64) float64`
- 参数：
  - `key`：环境变量名
  - `fallback`：解析失败或缺失时返回的默认值
- 返回：
  - 解析成功后的 `float64`，否则返回 `fallback`
- 职责：
  - 解析浮点型环境变量，不把格式错误上抛为配置加载失败
- 调用位置：
  - `Load`
- 主要副作用：
  - 读取指定环境变量
- 错误/边界条件：
  - 缺失或格式错误时静默回退到默认值

### `parseIntEnv`

- 签名：`func parseIntEnv(key string, fallback int64) int64`
- 参数：
  - `key`：环境变量名
  - `fallback`：解析失败或缺失时返回的默认值
- 返回：
  - 解析成功后的 `int64`，否则返回 `fallback`
- 职责：
  - 解析整型环境变量，不把格式错误上抛为配置加载失败
- 调用位置：
  - `Load`
- 主要副作用：
  - 读取指定环境变量
- 错误/边界条件：
  - 缺失或格式错误时静默回退到默认值

## 当前主路径与兼容路径

当前实现仍保留两条来源配置路径：

1. 主路径：`EXPORTER_SOURCES_JSON`
2. 兼容路径：`EXPORTER_BASE_URL`

维护建议：

- 新部署应只使用 `EXPORTER_SOURCES_JSON`
- 阅读代码时不要把 `ExporterBaseURL` 误解为主设计，它只是当前尚未移除的兼容入口
