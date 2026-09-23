# 数据库运维

## 生产数据库选择

生产环境使用 PostgreSQL。SQLite 适合本地开发、单实例测试，不建议用于生产多副本或高并发部署。

Compose 使用 `pgdata` volume 持久化 PostgreSQL 数据。删除 volume 会导致数据丢失，执行清理命令前必须确认备份可恢复。

## 备份

```bash
docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env exec -T postgres \
  pg_dump -U crypdog -d crypdog --format=custom > crypdog-$(date +%Y%m%d-%H%M%S).dump
```

备份应存放在独立于应用主机的位置，并定期验证恢复。

## 恢复

先停止应用写入，再恢复数据库：

```bash
docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env stop crypdog
cat crypdog-backup.dump | docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env exec -T postgres \
  pg_restore -U crypdog -d crypdog --clean --if-exists
docker compose -f docker-compose.prod.yml --env-file .env --env-file .release.env start crypdog
```

恢复后检查 `/healthz/ready`、订单状态和 Webhook 投递记录。

## 数据库变更

应用启动时会执行 GORM AutoMigrate。升级前仍应备份数据库，并在预发布环境验证迁移。不要把自动迁移当作备份或回滚机制。

## 常见风险

- SQLite 文件和 WAL 文件必须整体备份
- PostgreSQL 密码不得写入命令历史或日志
- 多副本部署必须共用 PostgreSQL，不能让每个副本使用本地 SQLite
- 数据库磁盘、连接数和慢查询应纳入监控
