```powershell
Invoke-RestMethod -Uri "http://localhost:8080/api/v1/watcher/intents" -Method Post -Headers @{
    "Content-Type" = "application/json"
    "Authorization" = "Bearer <CRYPDOG_SECRET>"
} -Body (@{
    orderId = "MY-ORDER-333"
    chain = "ARBITRUM"          # 👈 建议写大写无空格简码 "ARBITRUM"
    token = "USDC"              # 👈 建议写大写 "USDC"
    targetAddress = "0x7BDc49542978B16566e82c8f90DB1EB03804C675"
    expectedAmount = 4.313313
    timeoutSeconds = 1800
    webhookUrl = "http://localhost:8080/api/v1/mock/webhook"
} | ConvertTo-Json)

```

## 本地无真实转账测试链路

模拟接口只在开发环境开启，**不要在生产配置中开启**：

```yaml
server:
  enable_simulation: true
webhook:
  allow_local: true
```

启动服务后，先注册一个监听意图。推荐使用自动分配接口：

```powershell
$headers = @{
    "Content-Type" = "application/json"
    "Authorization" = "Bearer crypdog-secret-key-123456"
}

$intent = Invoke-RestMethod `
  -Uri "http://localhost:8080/api/v1/watcher/intents/allocate" `
  -Method Post `
  -Headers $headers `
  -Body (@{
      orderId = "SIM-ORDER-001"
      chain = "ARBITRUM"
      token = "USDC"
      baseAmount = 10
      timeoutSeconds = 1800
      webhookUrl = "http://localhost:8080/api/v1/mock/webhook"
  } | ConvertTo-Json)

$intent.data
```

再将响应中的 `targetAddress` 和 `expectedAmount` 注入模拟转账：

```powershell
Invoke-RestMethod `
  -Uri "http://localhost:8080/api/v1/watcher/intents/simulate" `
  -Method Post `
  -Headers $headers `
  -Body (@{
      targetAddress = $intent.data.targetAddress
      amount = [double]$intent.data.expectedAmount
      chain = "ARBITRUM"
      token = "USDC"
  } | ConvertTo-Json)
```

模拟流水会进入与真实扫描器相同的 pipeline，随后可验证：

1. `GET /api/v1/watcher/intents/SIM-ORDER-001` 是否变为 `PAID`；
2. `/api/v1/mock/webhook` 是否收到 `get` 和 `confirm` 事件；
3. `GET /api/v1/watcher/transfers` 是否能查到模拟流水；
4. 重复提交同一个模拟金额是否能验证幂等、撞单和重复流水处理；
5. 将 `webhookUrl` 换成失败地址，可验证重试与 `webhook_logs`。

模拟交易哈希统一以 `mock_` 开头，仅用于跳过真实链上交易二次核验。模拟接口受服务鉴权保护，且生产环境配置校验会拒绝开启它。
