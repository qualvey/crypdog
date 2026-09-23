# 商户收银台与支付监听接口规范 (Watcher API)

本模块面向**主业务系统（商户端 Merchant）**，用于获取收银台展示选项、创建收款意图、查询支付进度、取消订单以及接收可靠的 Webhook 异步充值通知。

---

## 1. 通用规范与鉴权

- **基础路径**：`/api/v1/watcher`
- **传输协议**：开发环境支持 HTTP / HTTPS；生产环境的 `webhookUrl` 必须使用 HTTPS
- **数据格式**：`application/json; charset=utf-8`
- **API 鉴权**：
  所有接口需在请求头携带 Bearer 令牌：
  ```http
  Authorization: Bearer <service_secret>
  ```
  `service_secret` 由服务端 `server.secret`（或环境变量 `CRYPDOG_SECRET`）指定。

---

## 2. 接口列表速查

| 接口方法 | 路径 | 描述 |
| :--- | :--- | :--- |
| `GET` | `/options` | 获取收银台动态支持的公链与代币选项 |
| `POST` | `/intents/allocate` | **[推荐]** 创建意图并自动分配防撞微数金额与收款地址 |
| `POST` | `/intents` | 创建意图（指定目标金额，不分配微数） |
| `GET` | `/intents/:orderId` | 查询指定订单的实时支付与确认状态 |
| `POST` | `/intents/:orderId/cancel` | 主动取消未支付或确认中的意图并释放微数 |
| `GET` | `/transfers` | 查询链上捕获的转账流水明细（支持对账与漏单排查） |
| `POST` | `/intents/simulate` | **[开发测试]** 本地模拟链上入账流水（生产环境禁用） |

---

## 3. 详细接口定义

### 3.1 获取动态收银台支付选项 (`GET /options`)

根据数据库当前启用的代币白名单（`chain_tokens`）以及系统配置与地址池，动态渲染前端收银台下拉选项。

#### 请求示例
```http
GET /api/v1/watcher/options HTTP/1.1
Host: 127.0.0.1:8080
Authorization: Bearer <CRYPDOG_SECRET>
```

#### 响应示例 (200 OK)
```json
{
  "code": 200,
  "data": {
    "defaultToken": "USDT",
    "defaultChain": "ARBITRUM",
    "tokens": [
      {
        "symbol": "USDT",
        "name": "Tether USD",
        "icon": "fa-solid fa-circle-dollar-to-slot"
      },
      {
        "symbol": "USDC",
        "name": "USD Coin",
        "icon": "fa-solid fa-circle-dollar-to-slot"
      }
    ],
    "chains": {
      "USDT": [
        {
          "chain": "ARBITRUM",
          "name": "Arbitrum One (L2)",
          "badge": "极速 / 低Gas",
          "decimals": 6
        },
        {
          "chain": "SOLANA",
          "name": "Solana",
          "badge": "极速",
          "decimals": 6
        }
      ]
    }
  }
}
```

---

### 3.2 创建并分配微数订单 (`POST /intents/allocate`)

**这是最推荐的下单方式**。系统将自动从收款钱包池挑选可用地址，并针对基础金额自动累加一个微小随机偏移行（例如 `10.000000` -> `10.000100`），保证多个用户并发付款到同一地址时**绝对不会碰撞丢单**。

#### 请求体参数
| 字段名 | 类型 | 必选 | 说明 | 示例 |
| :--- | :--- | :--- | :--- | :--- |
| `orderId` | string | 是 | 商户系统全局唯一订单号 | `"ORDER-20260919-001"` |
| `chain` | string | 是 | 支付公链（`TRON`、`ARBITRUM`、`BSC`、`SOLANA`、`ETH` 等） | `"ARBITRUM"` |
| `token` | string | 是 | 代币符号（`USDT`、`USDC` 等） | `"USDT"` |
| `baseAmount` | string | 是 | 商户订单原本应付基准金额 | `"10.00"` |
| `webhookUrl` | string | 是 | 支付成功后的异步回调通知 URL | `"https://api.example.com/pay/notify"` |
| `timeoutSeconds` | int | 否 | 订单有效等待时间（秒），默认 900 秒（15分钟） | `900` |
| `targetAddress` | string | 否 | 指定收款地址，若不填则由系统地址池按权重自动轮询分流 | `""` |

#### 请求示例
```json
{
  "orderId": "ORDER-20260919-001",
  "chain": "ARBITRUM",
  "token": "USDT",
  "baseAmount": "10.00",
  "webhookUrl": "https://api.example.com/pay/notify",
  "timeoutSeconds": 900
}
```

#### 响应示例 (200 OK)
```json
{
  "code": 200,
  "data": {
    "orderId": "ORDER-20260919-001",
    "chain": "ARBITRUM",
    "token": "USDT",
    "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
    "allocatedAmount": "10.000100",
    "baseAmount": "10.00",
    "offset": "0.000100",
    "status": "WATCHING",
    "expiresAt": 1790003456,
    "requiredConfirmations": 10
  }
}
```
> [!IMPORTANT]
> 前端收银台必须展示并提示付款人转账 **`allocatedAmount`（即带有微数尾数的精确金额）**，否则链上撮合引擎无法判定归属！

---

### 3.3 查询订单实时状态 (`GET /intents/:orderId`)

商户轮询或对账查询订单支付状态。

#### 请求示例
```http
GET /api/v1/watcher/intents/ORDER-20260919-001 HTTP/1.1
Authorization: Bearer <CRYPDOG_SECRET>
```

#### 响应示例 (200 OK)
```json
{
  "code": 200,
  "data": {
    "orderId": "ORDER-20260919-001",
    "chain": "ARBITRUM",
    "token": "USDT",
    "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
    "expectedAmount": "10.000100",
    "status": "PAID",
    "confirmations": 12,
    "requiredConfirmations": 10,
    "txHash": "0x89abcdef1234567890abcdef1234567890abcdef1234567890abcdef12345678",
    "paidAt": 1790002800,
    "expiresAt": 1790003456
  }
}
```

#### 状态机枚举 (`status`)
- `WATCHING`：正在监听入账（等待付款人向区块链广播交易）。
- `CONFIRMING`：链上已匹配到首笔转账，正在累加区块确认数（防分叉/双花安全阶段）。
- `PAID`：确认数达标，支付彻底完成。
- `EXPIRED`：超时未支付，微数额度已自动释放归还。
- `CANCELLED`：已被商户主动取消。

---

### 3.4 取消订单 (`POST /intents/:orderId/cancel`)

主动终止一个未完成的支付意图，立即释放微数防撞槽位。已终态（`PAID`）的订单无法被取消。

#### 请求示例
```http
POST /api/v1/watcher/intents/ORDER-20260919-001/cancel HTTP/1.1
Authorization: Bearer <CRYPDOG_SECRET>
```

---

## 4. Webhook 接入与签名验证指南

### 4.1 双段 Webhook 通知机制
为了兼顾**前端极致响应速度**与**资产绝对安全性**，CrypDog 实现了双段回调机制：

1. **第一段：初次捕获 (`event: "get"`)**：
   - 触发时机：扫链器首次在链上抓取到有效交易（`status: "CONFIRMING"`）。
   - 商户动作：可提前更新 UI 为“已检测到入账，正在确认中”，给用户带来即时反馈，**但不建议在此刻提前发货**。
2. **第二段：区块终验 (`event: "confirm"`)**：
   - 触发时机：区块深度达到公链安全确认数（`status: "PAID"`）。
   - 商户动作：**核销订单、正式给用户上账或发货**。

---

### 4.2 Webhook 数据载荷 (Payload)

```json
{
  "orderId": "ORDER-20260919-001",
  "event": "confirm",
  "status": "PAID",
  "chain": "ARBITRUM",
  "token": "USDT",
  "amount": "10.000100",
  "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
  "txHash": "0x89abcdef1234567890abcdef1234567890abcdef1234567890abcdef12345678",
  "confirmations": 10,
  "requiredConfirmations": 10,
  "timestamp": 1790002800
}
```

---

### 4.3 签名防伪算法 (HMAC-SHA256)

每次 Webhook 请求附带防伪 HTTP 标头：
```http
X-Signature-SHA256: <hex_encoded_hmac_sha256>
```
- **签名算法**：`HMAC_SHA256(raw_request_body_bytes, webhook_secret)`
- **输出格式**：64 位全小写十六进制字符串（Hex Encoded）。
- **密钥**：配置中的 `webhook.webhook_secret`。

#### 验签代码实现示例

##### Node.js (TypeScript / Express)
```typescript
import crypto from 'crypto';
import { Request, Response } from 'express';

export function verifyWebhook(req: Request, res: Response) {
  const signature = req.headers['x-signature-sha256'] as string;
  const webhookSecret = process.env.WEBHOOK_SECRET;
  if (!webhookSecret) throw new Error('WEBHOOK_SECRET is required');

  // 必须使用原始未经解析的 Raw Body 字节流进行校验
  const rawBody = (req as any).rawBody || JSON.stringify(req.body);
  const expectedSignature = crypto
    .createHmac('sha256', webhookSecret)
    .update(rawBody)
    .digest('hex');

  if (signature !== expectedSignature) {
    return res.status(401).send('Invalid signature');
  }

  const { orderId, event, status } = req.body;
  if (event === 'confirm' && status === 'PAID') {
    // 幂等发货处理逻辑...
  }

  return res.status(200).send('OK');
}
```

##### Python (FastAPI / Flask)
```python
import hmac
import hashlib

def verify_crypdog_signature(raw_body: bytes, header_signature: str, secret: str) -> bool:
    expected = hmac.new(secret.encode('utf-8'), raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header_signature)
```

##### Go
```go
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func VerifySignature(rawBody []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}
```

---

### 4.4 商户幂等与防重发货原则

1. **响应状态码**：商户成功处理后必须返回 HTTP `200 OK`。
2. **重试机制**：若商户返回非 2xx 或超时，CrypDog 分发器将按照指数退避机制自动重试（2秒、10秒、30秒、2分钟、5分钟）。
3. **幂等控制**：商户收到 `event: "confirm"` 时，必须使用本地数据库事务与唯一键（如 `order_id`）确保**同一笔订单即使多次收到重试回调，也只发货一次**。
