# API 概览

CrypDog API 基础地址取决于部署方式。Docker Compose 默认通过宿主机的 `8081` 端口访问容器内的 `8080` 端口。

## 鉴权

业务接口使用：

```http
Authorization: Bearer <CRYPDOG_SECRET>
Content-Type: application/json
```

管理接口使用独立的：

```http
Authorization: Bearer <CRYPDOG_ADMIN_SECRET>
```

## 端点分类

- 健康检查：`/health`、`/healthz/live`、`/healthz/ready`
- Metrics：配置的 `metrics.path`，默认 `/metrics`
- Watcher API：见 [Watcher API](watcher.md)
- Admin API：见 [Admin API](admin.md)

## 调试原则

生产环境 API 必须通过 HTTPS 或受信任的内部网络访问。不要在日志、截图或工单中暴露 Authorization Header、Webhook Secret 或数据库连接串。
