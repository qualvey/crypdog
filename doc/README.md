# CrypDog 文档

CrypDog 是用于监听多条公链加密货币支付、推进区块确认并可靠投递 Webhook 的独立服务。

## 文档导航

- [配置参考](configuration/configuration.md)
- [生产部署](deployment/production.md)
- [上线检查清单](deployment/checklist.md)
- [数据库运维](operations/database.md)
- [监控与日志](operations/monitoring.md)
- [故障排查](operations/troubleshooting.md)
- [安全加固](security/hardening.md)
- [API 文档](api/README.md)

## 本地预览

```bash
python -m venv .venv-docs
# Windows:
.venv-docs\Scripts\activate
# Linux/macOS:
source .venv-docs/bin/activate

pip install -r requirements-docs.txt
mkdocs serve
```

打开 <http://127.0.0.1:8000> 查看渲染后的文档。

构建静态站点：

```bash
mkdocs build --strict
```

生成的站点位于 `site/`，该目录不应提交到 Git。
