# 生产上线检查清单

## 配置和密钥

- [ ] `APP_ENV=production`
- [ ] `CRYPDOG_SECRET` 已随机生成，长度至少 16 字符
- [ ] `CRYPDOG_ADMIN_SECRET` 已配置，且不同于业务 Secret
- [ ] `CRYPDOG_WEBHOOK_SECRET` 已配置并安全保存
- [ ] `CRYPDOG_METRICS_SECRET` 已配置并安全保存
- [ ] 所有 Secret 未提交到 Git、日志或镜像
- [ ] `CRYPDOG_ENABLE_SIMULATION=false`
- [ ] `WEBHOOK_ALLOW_LOCAL=false`
- [ ] `PPROF_ENABLED=false`

## 数据库

- [ ] `DB_DRIVER=postgres`
- [ ] `POSTGRES_PASSWORD` 已设置
- [ ] PostgreSQL 数据卷已持久化
- [ ] 已完成一次备份和恢复演练
- [ ] 应用数据库用户权限符合最小权限原则

## 区块链

- [ ] 每条启用的链都有正确的 `driver`
- [ ] 主 RPC 可访问
- [ ] 备用 RPC 可访问并经过故障切换测试
- [ ] Token 合约地址和精度已核对
- [ ] 收款钱包地址已核对
- [ ] 确认数和扫描延迟符合业务风险要求

## 网络和回调

- [ ] Webhook 地址使用 HTTPS
- [ ] 业务方已完成 HMAC 验签
- [ ] Webhook 目标可从生产网络访问
- [ ] 反向代理已配置 TLS、限流和超时
- [ ] PostgreSQL 未暴露到公网

## 运行验证

- [ ] `docker compose ps` 中服务正常
- [ ] `/healthz/live` 返回 200
- [ ] `/healthz/ready` 返回 200
- [ ] Metrics 鉴权和采集正常
- [ ] 日志输出为 JSON 或符合集中采集格式
- [ ] 已创建测试订单并验证完整 Webhook 流程
- [ ] 已验证取消、过期、确认和失败重试流程
- [ ] 已记录当前版本、镜像和配置版本
