# 生产部署

本文以 Docker Compose 为例。生产环境必须使用强随机 Secret、PostgreSQL 和经过核对的链配置。

## 1. 准备目录

```bash
git clone <repository-url> crypdog
cd crypdog
cp config.example.yaml config.yaml
```

不要直接使用仓库中的开发配置。生产配置文件和 `.env.production` 应通过 Secret 管理系统或受限文件分发，不要提交到 Git。

## 2. 生成生产 Secret

```bash
openssl rand -hex 32
```

至少生成以下互不相同的值：

- `CRYPDOG_SECRET`：业务 API 鉴权
- `CRYPDOG_ADMIN_SECRET`：管理 API 鉴权
- `CRYPDOG_WEBHOOK_SECRET`：Webhook HMAC 签名
- `CRYPDOG_METRICS_SECRET`：Prometheus Metrics 鉴权
- `POSTGRES_PASSWORD`：数据库密码

## 3. 创建环境文件

创建 `.env.production`：

```dotenv
CRYPDOG_SECRET=<随机值>
CRYPDOG_ADMIN_SECRET=<不同的随机值>
CRYPDOG_WEBHOOK_SECRET=<不同的随机值>
CRYPDOG_METRICS_SECRET=<不同的随机值>
POSTGRES_PASSWORD=<数据库随机密码>
POSTGRES_USER=crypdog
POSTGRES_DB=crypdog
CRYPDOG_ALLOWED_ORIGINS=https://admin.example.com
```

`docker-compose.yml` 会强制使用：

- `APP_ENV=production`
- PostgreSQL
- `CRYPDOG_ENABLE_SIMULATION=false`
- `WEBHOOK_ALLOW_LOCAL=false`

容器端口为 `8080`，宿主机默认映射为 `8081`。

## 4. 配置 `config.yaml`

生产配置至少应确认：

```yaml
env: production

server:
  enable_simulation: false

webhook:
  allow_local: false

metrics:
  enabled: true

pprof:
  enabled: false
```

在 `chains` 中只启用实际使用的链，并核对以下内容：

- `driver` 与链类型一致
- 主 RPC 和备用 RPC 可访问
- `confirmations`、`block_delay`、`batch_size` 符合业务风险要求
- Token 合约地址和 `decimals` 正确
- `initial_wallets` 地址属于对应链

链配置目前主要通过 `config.yaml` 提供；不要使用文档中未由代码支持的环境变量名覆盖链字段。

## 5. 启动和验证

```bash
docker compose --env-file .env.production build
docker compose --env-file .env.production up -d
docker compose ps
docker compose logs --tail=100 crypdog
```

健康检查：

```bash
curl http://127.0.0.1:8081/healthz/live
curl http://127.0.0.1:8081/healthz/ready
```

查看 Metrics（启用鉴权时）：

```bash
curl -H "Authorization: Bearer <CRYPDOG_METRICS_SECRET>" \
  http://127.0.0.1:8081/metrics
```

## 6. 反向代理和网络

建议只将反向代理的 HTTPS 端口暴露到公网，CrypDog 和 PostgreSQL 仅在内部网络通信。不要将 PostgreSQL 端口映射到公网。

生产环境还应配置：

- TLS 证书和自动续期
- 代理层请求体大小、超时和限流
- Metrics 仅允许监控网络访问
- pprof 保持关闭，临时开启时限制来源 IP

## 7. 升级和回滚

升级前先备份数据库，然后拉取目标版本并重新构建：

```bash
docker compose --env-file .env.production exec -T postgres \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" > backup.sql
docker compose --env-file .env.production up -d --build
```

若新版本异常，切换回已验证的镜像或 Git 版本，恢复数据库备份，并检查 Webhook 重试日志是否需要补偿。
