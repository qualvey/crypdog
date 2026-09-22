# 系统配置与多环境运维指南 (Configuration Reference)

CrypDog 遵循 **The Twelve-Factor App** 配置设计准则，支持以 YAML 文件为蓝本，并允许使用操作系统环境变量进行层叠覆盖（Environment Overrides）。

---

## 1. 配置文件加载机制

配置加载由 `internal/config/config.go` 中的 `LoadConfig()` 执行，优先级规则如下：
1. **默认配置文件**：读取当前工作目录下的 `config.yaml`。
2. **环境变量覆盖**：如果存在同名环境变量（如 Docker / Kubernetes 容器编排场景），将自动覆盖 YAML 中的默认值。
3. **安全准入校验**：当 `env: "production"` 时，系统将启动 `ValidateProduction()` 强制进行安全合规审查。

---

## 2. 核心配置字段字典 (YAML & ENV)

### 2.1 基础与服务段 (`server:`)

| YAML 路径 | 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `env` | `APP_ENV` | `development` | 运行环境，可选 `development` 或 `production` |
| `server.port` | `CRYPDOG_PORT` | `8080` | HTTP API 监听端口 |
| `server.secret` | `CRYPDOG_SECRET` | 开发默认值 | API 鉴权使用的 Bearer 服务秘钥；生产必须使用随机值 |
| `server.admin_secret` | `CRYPDOG_ADMIN_SECRET` | - | 管理接口独立 Bearer 秘钥，生产环境必须配置 |
| `server.allowed_origins` | `CRYPDOG_ALLOWED_ORIGINS` | 空 | 允许浏览器跨域访问的 Origin，多个值用逗号分隔 |
| `server.rate_limit_per_minute` | `CRYPDOG_RATE_LIMIT_PER_MINUTE` | `300` | 单客户端每分钟请求上限，边缘代理仍应配置独立限流 |
| `server.enable_simulation` | `CRYPDOG_ENABLE_SIMULATION` | `false` | 是否开启本地模拟充值流水接口 (`/intents/simulate`)，**生产环境必须为 false** |

---

### 2.2 回调通知段 (`webhook:`)

| YAML 路径 | 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `webhook.webhook_secret` | `CRYPDOG_WEBHOOK_SECRET` | 开发默认值 | Webhook 签名使用的 HMAC-SHA256 共享秘钥；生产必须使用随机值 |
| `webhook.max_retries` | - | `3` | 回调失败时的最大重试次数 |
| `webhook.timeout_sec` | - | `5` | 单次回调商户接口的 HTTP 超时时间（秒） |
| `webhook.allow_local` | `WEBHOOK_ALLOW_LOCAL` | `false` | 是否允许回调本地回环地址（`localhost` / `127.0.0.1`）。本地联调设为 `true`，**生产环境必须为 false 以严格防御 SSRF 攻击** |

---

### 2.3 数据库段 (`database:`)

| YAML 路径 | 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `database.driver` | `DB_DRIVER` | `sqlite` | 数据库驱动，可选 `sqlite` 或 `postgres` |
| `database.dsn` | `DB_DSN` | `crypdog.db` | 连接串。SQLite 为文件路径；PostgreSQL 为完整连接 URI |

---

### 2.4 日志与可观测性 (`log:`, `metrics:`, `pprof:`)

| YAML 路径 | 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `log.level` | `LOG_LEVEL` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `log.format` | `LOG_FORMAT` | `text` | 日志格式：开发环境用 `text`，生产容器环境推荐 `json` |
| `log.output` | `LOG_OUTPUT` | `stdout` | 输出目标：`stdout` 或 `file` |
| `log.file_path` | `LOG_FILE_PATH` | `""` | 当 output 为 file 时的磁盘写入路径 |
| `log.timestamp` | `LOG_TIMESTAMP` | `true` | 文本日志是否输出 ISO-8601 时间戳；设为 `false` 可使用无时间戳的简洁格式 |
| `metrics.enabled` | `METRICS_ENABLED` | `true` | 是否暴露 Prometheus 监控指标端点 |
| `metrics.path` | `METRICS_PATH` | `/metrics` | 指标拉取路径 |
| `metrics.secret` | `CRYPDOG_METRICS_SECRET` | 空 | 指标端点独立 Bearer 秘钥，生产环境启用 Metrics 时必须配置 |
| `pprof.enabled` | `PPROF_ENABLED` | `false` | 是否开启 Go 运行时性能探针（`/debug/pprof`） |

---

### 2.5 区块链节点配置段 (`chains:`)

针对每个链（如 `TRON`、`BSC`、`ARBITRUM`、`POLYGON`、`SOLANA`）：

```yaml
chains:
  ARBITRUM:
    enabled: true                      # 是否开启该链的扫链监听
    driver: "evm"                      # 驱动类型：evm / tron / solana
    rpc_url: "https://arb1.arbitrum.io/rpc" # 基础 RPC 地址
    backup_rpc_urls:                   # 容灾备用 RPC 清单（主节点故障时自动轮询）
      - "https://arbitrum.llamarpc.com"
    scan_interval_sec: 3               # 扫链轮询间隔（秒）
    confirmations: 10                  # 该链判为 PAID 所需的最小安全确认块数
    block_delay: 20                    # 安全滞后扫描块数（严格防御链软分叉 Reorg）
    batch_size: 500                    # 每次 eth_getLogs 批量扫块跨度
```

---

## 3. 生产环境安全校验规则 (`ValidateProduction`)

当配置中 `env: "production"` 时，系统启动时会自动执行安全审查。若存在以下任意风险项，**服务将拒绝启动并返回配置错误**：

1. **弱口令检查**：
   - `server.secret` 严禁使用代码中的开发默认值，且长度必须 $\ge 16$ 字符。
   - `webhook.webhook_secret` 严禁使用代码中的开发默认值，且长度必须 $\ge 16$ 字符。
2. **SSRF 防御阻断**：
   - `webhook.allow_local` 必须为 `false`。严禁向局域网内部私网 IP 发起回调，防止内网探测渗透。
3. **模拟入账阻断**：
    - `server.enable_simulation` 必须为 `false`。严禁在生产开启无凭证的本地入账模拟接口。
4. **权限隔离**：
    - `server.admin_secret` 必须与业务 `server.secret` 不同。
5. **指标保护**：
    - 启用 Metrics 时必须配置独立的 `metrics.secret`。

---

## 4. 生产环境部署推荐环境变量 (.env.production)

```ini
APP_ENV=production
CRYPDOG_PORT=8080
CRYPDOG_SECRET=<random-secret-at-least-16-characters>
CRYPDOG_ADMIN_SECRET=<different-random-secret>
CRYPDOG_WEBHOOK_SECRET=<different-random-secret>
CRYPDOG_ENABLE_SIMULATION=false
CRYPDOG_METRICS_SECRET=<random-metrics-secret>
CRYPDOG_ALLOWED_ORIGINS=https://admin.example.com
WEBHOOK_ALLOW_LOCAL=false

DB_DRIVER=postgres
DB_DSN=host=postgres-prod.internal user=crypdog password=<database-password> dbname=crypdog port=5432 sslmode=require

LOG_LEVEL=warn
LOG_FORMAT=json
LOG_OUTPUT=stdout

METRICS_ENABLED=true
PPROF_ENABLED=false
```

> 以上值均为占位符，不要直接复制到生产环境。完整的 Docker Compose 部署流程见[生产部署](../deployment/production.md)，上线前请逐项执行[检查清单](../deployment/checklist.md)。
