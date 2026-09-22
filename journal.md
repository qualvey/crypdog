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

失败，没有监听成
功
