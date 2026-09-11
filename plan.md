将加密货币支付监听与扫链解耦为一个**独立微服务（Crypto Payment Listener Service）**是完全符合企业级架构最佳实践的（如 Web3 支付网关服务）。

它将复杂的区块链 RPC/API 节点轮询、区块确认深度校验、微数匹配、重试逻辑与主业务隔离。

我为您设计了一套符合 RESTful + Webhook 异步回调 标准的最佳实践接口规范与架构设计方案。

🏛️ 整体微服务架构图
+------------------+         1. POST /intents          +----------------------------+
|                  | --------------------------------> |                            |
| 主业务服务端      |                                   |  加密货币监听独立微服务      |
| (StreamVIP API)  | <-------------------------------  |  (Crypto Listener Service) |
|                  |      2. GET /intents/:orderId     |                            |
+------------------+         (轮询主动查询状态)         +----------------------------+
         ^                                                           |
         |                   3. POST {webhookUrl}                    | (定时扫链 TRON/BSC)
         +-----------------------------------------------------------+
                             (链上确认后异步WebHook回调)
📋 最佳实践 API 接口设计规范
1. 注册支付监听意图 (Register Payment Intent)
主业务生成订单后，调用此接口向监听微服务注册一个待监控的支付任务（支持自带尾数，系统内置防撞保护）。

请求方式: POST /api/v1/watcher/intents
请求头: Authorization: Bearer <SERVICE_SECRET_KEY>
请求体 (Request Body):
```json
{
  "orderId": "ORD-1722509988123",
  "chain": "TRON",               // 链类型: TRON (TRC-20) | BSC (BEP-20) | ARBITRUM | ETH
  "token": "USDT",               // 代币符号
  "targetAddress": "TY7x9N2m8Qk4Pz1v6W3s5R7u9Y2X4B6C8V", // 收款钱包地址
  "expectedAmount": 12.3611,    // 带微数尾数的期望到账金额
  "timeoutSeconds": 1800,        // 监听超时时长 (秒)，如 30 分钟
  "webhookUrl": "https://api.yourdomain.com/api/v1/paywall/crypto/webhook" // 链上确认后的回调地址
}
```
响应体 (Response Body):
```json
{
  "code": 200,
  "message": "Payment intent registered successfully",
  "data": {
    "intentId": "intent_tron_1722509988123",
    "status": "WATCHING",         // 状态: WATCHING(监听中) | PAID(已支付) | EXPIRED(已超时)
    "expiresAt": "2026-08-01T10:06:33.000Z"
  }
}
```

1.1 自动分配微数并注册监听 (Allocate Payment Intent - 推荐)
主业务只需传入基础金额（如 10.0），由微服务自动在 `0.0001 ~ 0.0999` 区间内分配唯一可用微数，开箱即用，超时或取消自动回收。

请求方式: POST /api/v1/watcher/intents/allocate
请求头: Authorization: Bearer <SERVICE_SECRET_KEY>
请求体 (Request Body):
```json
{
  "orderId": "ORD-1722509988124",
  "chain": "ARBITRUM",
  "token": "USDC",
  "targetAddress": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
  "baseAmount": 10.0,
  "timeoutSeconds": 1800,
  "webhookUrl": "https://api.yourdomain.com/api/v1/paywall/crypto/webhook"
}
```
响应体 (Response Body):
```json
{
  "code": 200,
  "message": "Payment intent allocated and watching successfully",
  "data": {
    "intentId": "intent_arbitrum_xxx",
    "orderId": "ORD-1722509988124",
    "baseAmount": 10.0,
    "tailOffset": 0.0001,
    "expectedAmount": 10.0001,
    "status": "WATCHING",
    "expiresAt": "2026-08-01T10:06:33.000Z"
  }
}
```

2. 主动查询订单链上支付状态 (Query Intent Status)
主业务服务端或客户端可随时主动查询该订单在区块链上的最新到账状态。

请求方式: GET /api/v1/watcher/intents/:orderId
响应体 (Response Body):
json
{
  "code": 200,
  "message": "success",
  "data": {
    "orderId": "ORD-1722509988123",
    "status": "PAID",            // WATCHING | PAID | PARTIAL_PAID | EXPIRED
    "expectedAmount": 12.3611,
    "receivedAmount": 12.3611,
    "txHash": "a1075389f4b98877123456789abcdef0123456789abcdef0123456789abcdef0",
    "blockNumber": 62891024,
    "confirmations": 19,         // 链上区块确认数
    "paidAt": "2026-08-01T09:37:15.000Z"
  }
}
3. 链上确认异步 Webhook 回调 (Async Webhook Callback)
当监听服务在链上扫到符合 expectedAmount (如 12.3611 USDT) 的交易且区块确认数达标时，主动向主业务服务器发起 Webhook 通知。

回调请求: POST {webhookUrl} (主业务服务器提供)
安全请求头:
X-Signature-SHA256: hmac_sha256(request_body, SHARED_WEBHOOK_SECRET) (防止假 Webhook 伪造开通)
请求体 (Webhook Payload):
json
{
  "event": "PAYMENT_SUCCESS",
  "orderId": "ORD-1722509988123",
  "chain": "TRON",
  "token": "USDT",
  "targetAddress": "TY7x9N2m8Qk4Pz1v6W3s5R7u9Y2X4B6C8V",
  "amount": 12.3611,
  "txHash": "a1075389f4b98877123456789abcdef0123456789abcdef0123456789abcdef0",
  "blockTimestamp": 1722501234000,
  "timestamp": "2026-08-01T09:37:15.000Z"
}
4. 钱包历史流水反查 API (Reconciliation / Audit API)
用于管理员或对账系统查询某个地址在指定时间段内的所有链上进账列表。

请求方式: GET /api/v1/watcher/transfers?address=TY7x9...&token=USDT&limit=50
响应体:
json
{
  "code": 200,
  "data": {
    "transfers": [
      {
        "txHash": "a1075389...",
        "from": "TFromAddress...",
        "to": "TY7x9N2m8...",
        "amount": 12.3611,
        "token": "USDT",
        "blockTimestamp": 1722501234000,
        "matchedOrderId": "ORD-1722509988123"
      }
    ]
  }
}
🛡️ 微服务关键设计原则 (Best Practices)
防伪签名机制 (HMAC-SHA256)： Webhook 回调请求必须带有 X-Signature-SHA256 签名，主业务端校验 HMAC 签名无误后才执行开通，防止恶意黑客伪造 Webhook 请求免费刷 VIP。
幂等性保障 (Idempotency)： 同一个 txHash 对应的转账，即便监听服务多次触发 Webhook，主业务端也只处理一次，防止重复开通或重复累加时长。
超时释放与微数回收 (Automatic Cleanup)： 超过 timeoutSeconds（如 30 分钟未转账）的 Intent 自动变为 EXPIRED 状态，回收对应的微数尾数给后续订单重用。
区块确认深度 (Block Confirmations)： TRON 链建议在交易块被锁定 confirmations >= 19（Solidity Block）后触发 PAYMENT_SUCCESS，彻底防止双花（Double Spending）与分叉。