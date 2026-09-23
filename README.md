# 🐕 CrypDog (Crypto Payment Watchdog Daemon)

> 加密货币支付监听与扫链独立微服务（Web3 Payment Gateway Watchdog）。

CrypDog 专门用于将繁琐的区块链节点轮询、区块确认深度校验、微数尾数匹配防撞、异步 Webhook 回调与重试等底层逻辑与主业务系统彻底解耦。

---

## 🏛️ 整体微服务架构

```
+------------------+         1. POST /intents/allocate     +----------------------------+
|                  | ------------------------------------> |                            |
| 主业务服务端      |                                       |  CrypDog 监听独立微服务    |
| (商城 / 会员系统)  | <------------------------------------ |  (Crypto Payment Watchdog) |
|                  |      2. GET /intents/:orderId         |                            |
+------------------+         (主动轮询查询支付状态)           +----------------------------+
         ^                                                                |
         |                   3. POST {webhookUrl}                         | (多链标准 JSON-RPC
         +----------------------------------------------------------------+  / TronGrid 定时扫链)
                             (链上确认后带 HMAC 签名异步回调)
```

---

## 🚀 已实现功能清单

### 1. 多链原生标准扫链适配
- **EVM 多链支持 (Arbitrum / BSC / ETH)**：
  - 全面采用以太坊标准 JSON-RPC 规范（`eth_blockNumber` 与 `eth_getLogs`），直接监听 ERC-20 `Transfer(address,address,uint256)` 事件。
  - 彻底抛弃易被风控废弃的第三方浏览器爬虫接口，直连官方/公共 RPC 节点，无任何第三方商业 API Key 限制。
  - 内置多链稳定币精度映射字典（Arbitrum 原生 USDC 为 6 位精度、BSC USDT 为 18 位精度等），高精度浮点转换，杜绝金额倍数失真。
- **TRON 链支持 (TRC-20)**：
  - 对接 TronGrid TRC-20 实时交易流与最新区块高度同步。

### 2. 微数自动分配与并发防撞池 (Micro-amount Pool Manager)
- **智能尾数分配**：提供 `POST /api/v1/watcher/intents/allocate` 接口，主业务仅需传入基础金额（如 `10.0`），系统在 `(Chain, Token, TargetAddress, BaseAmount)` 作用域内自动在 `0.0001 ~ 0.0999` 范围内分配最小空闲尾数（如 `10.0001`）。
- **主动碰撞拦截**：常规注册接口内置防撞校验，同一收款地址同一金额若已有活跃订单，立即返回 `409 Conflict`，从根源杜绝并发撞单错单。
- **无感自然回收**：订单支付完成（`PAID`）、超时（`EXPIRED`）或主动取消（`CANCELLED`）后，占用的微数尾数自动解冻回归资源池，下一笔订单自动复用。

### 3. 动态确认数与状态流转引擎 (Confirmation Worker)
- **各链独立确认深度**：支持动态配置各链安全确认数阈值（Arbitrum: 3，BSC: 15，TRON: 19，以太坊主网: 12 等）。
- **解除死锁的动态推进**：引入 `CONFIRMING` 中间状态，后台守护协程持续核对链上最新区块高度增长，一旦满足目标确认数阈值，原子化流转为 `PAID` 并触发回调。

### 4. 高可靠异步 Webhook 回调与签名防伪
- **防伪签名校验**：每次 Webhook 回调均在请求头携带 `X-Signature-SHA256: hmac_sha256(payload, secret)`，主业务端可安全验签，防止恶意伪造。
- **指数退避重试**：如果主业务端临时宕机或网络波动，按 `2s, 10s, 30s, 2m` 自动重试最多 5 次，完整持久化投递日志。

### 5. 订单全生命周期与运维能力
- **超时自动清理**：内置 `IntentCleaner` 守护协程，按指定超时时间自动标记未转账订单为 `EXPIRED`。
- **本地模拟转账**：提供 `/intents/simulate` 接口，无需消耗真实 Gas 即可快速测试全流程。
- **双数据库驱动**：支持本地免配置的 SQLite（`crypdog.db`）以及生产环境的高性能 PostgreSQL。

---

## 📋 API 接口规范

所有管理接口均需在请求头携带：
```http
Authorization: Bearer <SERVICE_SECRET_KEY>
Content-Type: application/json
```

### 1. 自动分配微数并注册监听 (推荐)
- **POST** `/api/v1/watcher/intents/allocate`
- **请求体 (Request Body)**：
  ```json
  {
    "orderId": "ORD-2026-001",
    "chain": "ARBITRUM",            // TRON | ARBITRUM | BSC | ETH
    "token": "USDC",                // USDT | USDC
    "baseAmount": 10.0,             // 基础金额
    "timeoutSeconds": 1800,         // 监听超时时长 (秒)
    "webhookUrl": "https://api.yourdomain.com/webhook"
  }
  ```
- **响应体 (Response Body)**：
  ```json
  {
    "code": 200,
    "message": "Payment intent allocated and watching successfully",
    "data": {
      "intentId": "intent_arbitrum_a1b2c3d4",
      "orderId": "ORD-2026-001",
      "baseAmount": 10.0,
      "tailOffset": 0.0001,
      "expectedAmount": 10.0001,
      "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
      "status": "WATCHING",
      "expiresAt": "2026-09-06T21:30:00.000Z"
    }
  }
  ```

### 2. 手动指定微数注册监听 (内置防撞)
- **POST** `/api/v1/watcher/intents`
- **请求体 (Request Body)**：
  ```json
  {
    "orderId": "ORD-2026-002",
    "chain": "TRON",
    "token": "USDT",
    "targetAddress": "TY7x9N2m8Qk4Pz1v6W3s5R7u9Y2X4B6C8V",
    "expectedAmount": 12.3611,
    "timeoutSeconds": 1800,
    "webhookUrl": "https://api.yourdomain.com/webhook"
  }
  ```

### 3. 主动查询订单支付状态
- **GET** `/api/v1/watcher/intents/:orderId`
- **响应体**：
  ```json
  {
    "code": 200,
    "data": {
      "orderId": "ORD-2026-001",
      "status": "PAID",             // WATCHING | CONFIRMING | PAID | EXPIRED | CANCELLED
      "expectedAmount": 10.0001,
      "receivedAmount": 10.0001,
      "txHash": "0xabc...",
      "blockNumber": 39821010,
      "confirmations": 4,
      "paidAt": "2026-09-06T21:05:12.000Z"
    }
  }
  ```

### 4. 取消监听订单
- **POST** `/api/v1/watcher/intents/:orderId/cancel` 或 **DELETE** `/api/v1/watcher/intents/:orderId`

### 5. 链上转账流水反查
- **GET** `/api/v1/watcher/transfers?address=0x7BDc...&token=USDC&limit=50`

### 6. 本地模拟链上到账 (测试专用)
- **POST** `/api/v1/watcher/intents/simulate`
  ```json
  {
    "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
    "amount": 10.0001,
    "chain": "ARBITRUM",
    "token": "USDC"
  }
  ```

---

## 🔔 双段 Webhook 回调规范 (Dual-Phase Webhook)

CrypDog 采用**双段 Webhook 机制**向主业务服务器的 `webhookUrl` 发送 `POST` 回调：
1. **充值捕获 (`get`)**：当首次在链上匹配到该订单的转账流水时立即发送，便于业务系统将订单置为“已收到转账，等待确认”；
2. **确认达成 (`confirm`)**：当交易达到所属公链设定的区块确认深度后发送，驱动业务系统完成最终订单核销与自动履约交付。

### 安全请求头：
- `X-Signature-SHA256`: `hex(HMAC_SHA256(request_body, SHARED_WEBHOOK_SECRET))`
- `User-Agent`: `CrypDog-Webhook-Guardian/1.0`

### 1. 充值初次捕获报文 (`event: get`)
```json
{
  "event": "get",
  "orderId": "ORD-2026-001",
  "chain": "ARBITRUM",
  "token": "USDC",
  "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
  "amount": 10.0001,
  "txHash": "0xd6cac508baf973a...",
  "blockTimestamp": 1788728210,
  "confirmations": 1,
  "requiredConfirmations": 12,
  "timestamp": "2026-09-06T21:05:12Z"
}
```

### 2. 区块确认达标报文 (`event: confirm`)
```json
{
  "event": "confirm",
  "orderId": "ORD-2026-001",
  "chain": "ARBITRUM",
  "token": "USDC",
  "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
  "amount": 10.0001,
  "txHash": "0xd6cac508baf973a...",
  "blockTimestamp": 1788728210,
  "confirmations": 12,
  "requiredConfirmations": 12,
  "timestamp": "2026-09-06T21:07:36Z"
}
```

---

## ⚙️ 环境变量配置 (`.env`)

```env
APP_ENV=development
CRYPDOG_PORT=8080
CRYPDOG_SECRET=replace-with-a-random-secret-at-least-16-characters
CRYPDOG_ADMIN_SECRET=replace-with-a-different-admin-secret-at-least-16-characters
CRYPDOG_WEBHOOK_SECRET=replace-with-a-different-random-secret-at-least-16-characters
CRYPDOG_ENABLE_SIMULATION=false
CRYPDOG_METRICS_SECRET=replace-with-a-metrics-secret-at-least-16-characters
CRYPDOG_ALLOWED_ORIGINS=https://admin.example.com

# 数据库配置: sqlite 或 postgres
DB_DRIVER=sqlite
DB_DSN=crypdog.db

# 区块链节点、钱包和确认数请在 config.yaml 的 chains / initial_wallets 中配置
```

---

## 🛠️ 快速开始

### 1. 运行单元与集成测试
```bash
go test -v ./...
```

### 2. 本地启动微服务
```bash
go run ./cmd/server
```
控制台将输出启动日志与扫描器装配状态：
```
🐕 Starting CrypDog (Crypto Payment Watchdog Daemon)
[EvmScanner] Started standard EVM (Arbitrum / BSC) JSON-RPC scanner daemon
[TronScanner] Started TRON blockchain scanner daemon
[ConfirmationWorker] Started confirmation tracker daemon
[Cleaner] Started expired intent cleaner daemon
🚀 CrypDog Payment Guardian running on HTTP http://localhost:8080
```

### 3. Docker 部署
```bash
docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env up -d --no-build
```

完整的生产部署、配置、备份、监控和安全要求请参阅 [生产部署文档](doc/deployment/production.md) 或使用 MkDocs 本地预览：

```bash
pip install -r requirements-docs.txt
mkdocs serve
```
