# 安全加固

## Secret 管理

- 使用 Secret Manager、容器 Secret 或受限权限文件
- 每个用途使用独立随机 Secret
- 不将 `.env.production`、生产 `config.yaml` 或数据库备份提交到 Git
- 定期轮换 Secret，并安排旧 Secret 的兼容切换窗口

## 网络隔离

- 仅暴露反向代理 HTTPS 端口
- PostgreSQL 只允许应用网络访问
- Metrics 只允许监控网络访问
- pprof 默认关闭
- 限制服务器出站访问，尤其是回调和 RPC 访问范围

## Webhook SSRF

生产环境必须保持 `WEBHOOK_ALLOW_LOCAL=false`。应用会拒绝回环、私网、链路本地、组播和未指定地址；业务方仍应在网络层限制出站目的地。

## API 鉴权

业务接口使用 `Authorization: Bearer <CRYPDOG_SECRET>`，管理接口使用独立的 `CRYPDOG_ADMIN_SECRET`。所有生产 API 应通过 HTTPS 或受信任的内部网络访问。

## 供应链和发布

- 固定基础镜像和依赖版本
- 在发布前运行 `go test ./...`、`go test -race ./...` 和 `go vet ./...`
- 记录 Git commit、镜像 digest 和配置版本
- 发布包应经过代码审查和回滚演练
