# CrypDog 开发者与架构文档中心 (Documentation Hub)

欢迎查阅 **CrypDog (Crypto Payment Watchdog Daemon)** 技术文档。本系统遵循国际通行的 **Docs as Code (文档即代码)** 与 **Diátaxis 架构模型** 编写与组织。

---

## 📚 文档全景导航 (Navigation Map)

```
doc/
├── README.md               # 📖 文档中心首页与知识索引（当前文档）
├── flow.md                 # 🏗️ [Explanation] 系统核心架构、状态机与核心时序设计
├── web3.md                 # ⛓️ [Reference] Web3 异构区块链支付与扫链底层知识库
├── api/                    # 🔌 [Reference] RESTful 接口规范字典
│   ├── watcher.md          # 🛒 商户收银台与支付监听 API (含双段 Webhook 规范与验签代码)
│   └── admin.md            # ⚙️ 运维管理后台 API (收款地址池、代币白名单 CRUD)
└── configuration/          # 🛠️ [How-To & Reference] 配置与部署指南
    └── configuration.md    # ⚙️ 配置文件字典、环境变量映射与生产安全准入清单
```

---

## 🧭 Diátaxis 文档分类索引

本工程文档按照现代软件工程推崇的 **Diátaxis 四象限模型** 归类组织：

| 象限类别 | 导向目标 | 文档链接 | 适用场景与受众 |
| :--- | :--- | :--- | :--- |
| **Explanation (理解导向)** | 阐述原理、设计决策与架构演进 | [系统架构与核心流程](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/flow.md) | 架构师、开发者了解系统运作流向、微数撮合原理与状态机设计 |
| **Reference (信息导向)** | 严格精准的技术参数、接口定义字典 | [商户收银台 API](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/watcher.md)<br/>[管理后台 API](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/admin.md)<br/>[Web3 底层技术参考](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/web3.md) | 商户对接人员查询接口参数、字段格式、错误码；工程师查询区块链底层精度与排重规则 |
| **How-To Guides (任务导向)** | 指引完成特定目标的端到端步骤 | [配置与部署参考](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/configuration/configuration.md)<br/>[商户 Webhook 验签指引](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/watcher.md#4-webhook-接入与签名验证指南) | 运维工程师部署系统、配置多链 RPC；商户工程师编写 Webhook 验签与发货处理逻辑 |
| **Tutorials (学习导向)** | 面向新手快速走通完整闭环 | [快速开始指南 (README.md)](file:///c:/Users/Ryu/Documents/workspace/crypdog/README.md) | 新加入的开发/体验人员在本地 5 分钟启动并模拟一笔链上充值 |

---

## ⚡ 常用快速跳转

- **商户收银台开发对接**：请先阅读 [收银台选项接口](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/watcher.md#1-获取动态收银台支付选项) 与 [订单分配微数接口](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/watcher.md#2-创建并分配微数订单-allocate)。
- **Webhook 回调与防重发货**：请先阅读 [双段 Webhook 通知规范](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/watcher.md#4-webhook-接入与签名验证指南)。
- **动态收款地址与代币管理**：请阅读 [后台管理接口规范](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/api/admin.md)。
- **生产环境部署与安全阻断规则**：请阅读 [生产就绪准入清单](file:///c:/Users/Ryu/Documents/workspace/crypdog/doc/configuration/configuration.md#4-生产环境安全校验规则-validateproduction)。
