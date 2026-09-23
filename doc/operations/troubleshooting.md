# 故障排查

## 容器无法启动

```bash
docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env ps
docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env logs --tail=200 crypdog
```

优先检查：

- `.env.production` 是否包含 Compose 要求的变量
- `config.yaml` 是否为合法 YAML
- 生产 Secret 是否满足长度和不重复要求
- PostgreSQL 是否通过健康检查
- 启用的链是否配置了 RPC URL

## `/healthz/ready` 返回失败

检查数据库连接、PostgreSQL 健康状态和容器网络。`/healthz/live` 正常而 ready 失败通常表示进程仍在运行，但依赖服务未就绪。

## 扫链高度不推进

检查日志中的 RPC 错误、限流和备用节点切换；确认 RPC 支持应用所需的 JSON-RPC 方法，并确认服务器时间正常。不要随意降低 `block_delay` 或 `confirmations`。

## Webhook 没有收到

检查：

1. 订单是否已匹配并达到对应确认数
2. `webhookUrl` 是否为 HTTPS 且可从服务端访问
3. 日志中是否出现投递失败或重试
4. 接收方是否返回 2xx
5. 接收方是否使用原始请求体和相同 Secret 验证 `X-Signature-SHA256`

不要为了让回调成功而在生产开启 `WEBHOOK_ALLOW_LOCAL`。

## 出现大量未匹配转账

核对链、Token、合约地址、`decimals`、收款地址和金额精度；同时检查是否存在错误配置的收款钱包或扫描游标。

## 回滚

保留上一个可用镜像和配置版本。回滚前确认数据库 schema 兼容性，并在回滚后检查订单状态、Webhook 重试记录和扫描进度。
