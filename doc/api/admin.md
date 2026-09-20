# 管理运维后台接口规范 (Admin API)

本模块面向**运营后台或管理系统**，提供热钱包收款地址池与代币合约白名单的在线动态增删改查（CRUD）能力。修改后即时生效，无需重启扫链服务。

---

## 1. 通用鉴权

- **基础路径**：`/api/v1/admin`
- **认证方式**：HTTP Bearer Token（使用独立的 `server.admin_secret`）：
  ```http
  Authorization: Bearer <admin_secret>
  ```
  若缺少或秘钥不符，系统返回 `401 Unauthorized`。

---

## 2. 收款钱包池管理 (`/api/v1/admin/wallets`)

### 2.1 钱包地址列表 (`GET /wallets`)

查询平台当前纳管的收款钱包地址。支持按公链和启用状态进行过滤。

#### 查询参数 (Query Parameters)
| 参数名 | 类型 | 说明 | 示例 |
| :--- | :--- | :--- | :--- |
| `chain` | string | 过滤指定公链（大小写不敏感） | `ARBITRUM` |
| `enabled` | bool | 过滤启用状态 (`true` / `false`) | `true` |
| `page` | int | 页码，默认 1 | `1` |
| `page_size` | int | 分页大小，默认 20，上限 100 | `20` |

#### 响应示例 (200 OK)
```json
{
  "code": 200,
  "data": {
    "total": 2,
    "page": 1,
    "pageSize": 20,
    "list": [
      {
        "id": 1,
        "chain": "ARBITRUM",
        "address": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
        "label": "Arbitrum Hot Wallet 01",
        "weight": 10,
        "enabled": true,
        "createdAt": "2026-09-19T10:00:00Z",
        "updatedAt": "2026-09-19T10:00:00Z"
      }
    ]
  }
}
```

---

### 2.2 添加收款地址 (`POST /wallets`)

新增一个收款热钱包。系统会自动根据目标公链（EVM / TRON / Solana）进行**地址格式校验**，非法地址将被直接拒绝。

#### 请求体参数
| 字段名 | 类型 | 必选 | 说明 | 示例 |
| :--- | :--- | :--- | :--- | :--- |
| `chain` | string | 是 | 所属公链（`TRON`、`ARBITRUM`、`BSC`、`SOLANA`、`ETH` 等） | `"ARBITRUM"` |
| `address` | string | 是 | 链上有效收款地址 | `"0x7BDc49542978B16566e82c8f90DB1EB03804C675"` |
| `label` | string | 否 | 钱包标识备注 | `"冷钱包补充归集 02"` |
| `weight` | int | 否 | 轮询分流权重，默认 10 | `10` |
| `enabled` | bool | 否 | 是否即刻启用，默认 `true` | `true` |

#### 响应示例 (200 OK)
```json
{
  "code": 200,
  "data": {
    "id": 2,
    "chain": "ARBITRUM",
    "address": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
    "label": "冷钱包补充归集 02",
    "weight": 10,
    "enabled": true,
    "createdAt": "2026-09-19T10:30:00Z",
    "updatedAt": "2026-09-19T10:30:00Z"
  }
}
```

---

### 2.3 修改收款地址 (`PUT /wallets/:id`)

支持调整钱包的展示备注、轮询权重与启用/禁用状态。

#### 请求体参数
| 字段名　　| 类型　 | 说明　　　　　　　　　　　　　　　　|
| :----------| :-------| :------------------------------------|
| `label`　 | string | 新备注　　　　　　　　　　　　　　　|
| `weight`　| int　　| 调整权重　　　　　　　　　　　　　　|
| `enabled` | bool　 | 设为 `false` 可暂停该地址接入新订单 |

---

### 2.4 删除收款地址 (`DELETE /wallets/:id`)

从地址池中永久移除指定钱包。

> [!CAUTION]
> **安全阻断保护机制**：若该地址当前有正在等待付款（`WATCHING`）或确认中（`CONFIRMING`）的进行中订单，系统将返回 `400 Bad Request` 拦截删除，防止出现充值无法匹配的孤儿单。

---

## 3. 代币白名单管理 (`/api/v1/admin/tokens`)

### 3.1 代币列表 (`GET /tokens`)

查询平台支持的代币合约及前端元数据。结果默认按 `priority DESC` 降序排列。

#### 查询参数 (Query Parameters)
| 参数名 | 类型 | 说明 |
| :--- | :--- | :--- |
| `chain` | string | 按公链筛选 |
| `enabled` | bool | 按启用状态筛选 |

#### 响应示例 (200 OK)
```json
{
  "code": 200,
  "data": [
    {
      "id": 1,
      "chain": "ARBITRUM",
      "symbol": "USDT",
      "name": "Tether USD",
      "contract": "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9",
      "decimals": 6,
      "isNative": false,
      "icon": "fa-solid fa-circle-dollar-to-slot",
      "badge": "极速 / 低Gas",
      "priority": 100,
      "enabled": true
    }
  ]
}
```

---

### 3.2 添加支持代币 (`POST /tokens`)

在指定公链上添加一个新的代币合约。

#### 请求体参数
| 字段名 | 类型 | 必选 | 说明 | 示例 |
| :--- | :--- | :--- | :--- | :--- |
| `chain` | string | 是 | 所属公链 | `"ARBITRUM"` |
| `symbol` | string | 是 | 代币代号 | `"USDT"` |
| `name` | string | 是 | 代币展示全称 | `"Tether USD"` |
| `contract` | string | 否 | 智能合约地址（主网原生币如 ETH/SOL 留空） | `"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9"` |
| `decimals` | int | 是 | 链上真实精度位数（如 6, 18, 9） | `6` |
| `isNative` | bool | 否 | 是否为主网原生币，默认 `false` | `false` |
| `icon` | string | 否 | 前端 FontAwesome 图标 Class 或图片链接 | `"fa-solid fa-circle-dollar-to-slot"` |
| `badge` | string | 否 | 前端标签（如 `低手续费 / 推荐`） | `"推荐"` |
| `priority` | int | 否 | 排序权重，越大越靠前，默认 0 | `100` |
| `enabled` | bool | 否 | 是否启用，默认 `true` | `true` |

---

### 3.3 修改代币信息 (`PUT /tokens/:id`)

支持调整合约精度、名称、图标、推荐标签、排序权重以及是否启用。

---

### 3.4 删除代币 (`DELETE /tokens/:id`)

永久移除指定代币。

> [!CAUTION]
> **安全阻断保护机制**：若该代币当前有关联进行中的订单，接口将拒绝删除操作。
