# 监控与日志

## 健康检查

| 端点 | 用途 | 鉴权 |
|---|---|---|
| `/healthz/live` | 进程存活探针 | 无 |
| `/healthz/ready` | 数据库和服务就绪探针 | 无 |
| `/health` | 兼容性健康检查 | 无 |
| `/metrics` | Prometheus 指标 | 使用 `CRYPDOG_METRICS_SECRET` 时需要 Bearer 鉴权 |

建议容器编排使用 `/healthz/live` 和 `/healthz/ready`，不要使用业务 API 判断进程健康。

## 日志

生产容器建议：

```yaml
log:
  level: info
  format: json
  output: stdout
```

日志应由容器平台或日志代理采集，不建议在容器内部长期写本地文件。重点关注：

- 扫链 RPC 连续失败和备用节点切换
- Webhook 投递失败、重试和达到最大次数
- 订单状态推进失败
- 数据库连接或迁移失败
- SSRF 校验拒绝事件

日志中不得输出 Secret、完整 Authorization Header 或数据库密码。

## 建议告警

- 服务存活或就绪探针连续失败
- RPC 错误率持续升高
- Webhook `retry` / `max_retried` 持续增加
- 未匹配转账数量持续增加
- 数据库连接失败、磁盘空间不足
- 区块扫描高度长时间不推进

## pprof

pprof 默认关闭。只有排障时临时开启，并通过网络 ACL 限制访问；使用完毕后立即关闭并重启服务。
