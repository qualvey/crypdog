# Web3 异构区块链支付底层技术参考 (Web3 Technical Reference)

在去中心化区块链世界中，公链的异构性（EVM、TRON、Solana）以及分布式共识的异步性给支付系统的确定性带来了巨大的技术挑战。本文档总结 CrypDog 在处理异构链事件监听、金额计算与防分叉时的核心技术规范。

---

## 1. 核心概念与异构公链对照矩阵

| 核心概念 / 公链特性 | EVM 体系 (ETH / BSC / Arbitrum / Polygon) | TRON (波场) | Solana (高性能网络) |
| :--- | :--- | :--- | :--- |
| **高度单位** | Block Number (区块高度) | Block Number | Slot / Block |
| **共识出块速度** | 0.25s (Arbitrum) ~ 12s (ETH) | 3s (DPoS) | 0.4s (PoH) |
| **推荐安全确认数** | Arbitrum: 10, BSC: 15, Polygon: 64 | 19 块 (约 1 分钟终态) | 32 Slots (Finalized) |
| **地址编码** | Hex (0x 前缀, 40 位十六进制) | Base58Check ('T' 开头, 34 字符) | Base58 (32~44 字符公钥) |
| **大小写敏感性** | **不敏感** (全小写归一化) | **严格敏感** (大小写不可篡改) | **严格敏感** (大小写不可篡改) |
| **代币协议标准** | ERC-20 | TRC-20 | SPL Token |
| **事件日志标识** | `LogIndex` (单 Tx 内自增索引) | `LogIndex` / 合约事件 | `AccountIndex` / Instruction Index |
| **主流代币精度** | USDT: 6, USDC: 6, BSC-USDT: 18 | USDT: 6, USDC: 6 | USDT: 6, USDC: 6 |

---

## 2. 扫链核心机制与防分叉算法

### 2.1 防分叉滞后扫描 (Reorg Delay)
区块链可能发生微小软分叉或孤块回滚（Chain Reorganization）。最新产生的头部块（Head Block）处于不确定状态，直接扫描可能读取到随后被孤立的假交易。

#### 计算公式
$$\text{SafeBlock} = \text{LatestOnChain} - \text{BlockDelay}$$

#### 安全边界保护
扫描驱动必须先校验：
$$\text{LatestOnChain} > \text{BlockDelay}$$
防止 `uint64` 减法发生无符号整型下溢（Underflow）导致程序崩溃。

---

### 2.2 批量分页扫块与 RPC 限流防护
公链节点普遍对 `eth_getLogs` 等过滤接口有单次拉取块高限制（如 500~2000 块）与请求频次限制（Rate Limit）。

#### 步进区间
$$\text{ScanRange} = [\text{FromBlock}, \min(\text{FromBlock} + \text{BatchSize} - 1, \text{SafeBlock})]$$

每批次扫完后持久化更新本地游标 `last_scanned_block`，保证服务意外重启时无缝续扫、不漏单、不重复。

---

## 3. 地址归一化规则 (Address Normalization)

不同链对于字符串大小写的解读截然不同，大小写处理错误是加密支付系统最容易产生资金挂起的核心 Bug：

### 3.1 强制小写族 (EVM / Bech32)
- **规则**：存储与查询一律采用全小写 (`strings.ToLower`)。
- **原因**：EVM 采用 EIP-55 校验和混合大小写，本质上 `0xAbC...` 与 `0xabc...` 为同一个私钥控制的账户。

### 3.2 严格区分大小写族 (Base58)
- **涉及网络**：TRON (`T...`)、Solana (Base58 公钥)、BTC (Legacy P2PKH/P2SH)。
- **规则**：**严禁转换大小写！** 必须保留其原始字符流。
- **原因**：Base58 编码包含大小写区分（去掉了容易混淆的 0, O, I, l），大小写改变会导致哈希校验和完全失真，变成完全不存在的钱包或他人地址。

---

## 4. 高精度金额与无损计算

链上智能合约不存储小数（浮点数），仅存储定点大整数（Raw Value）。

### 4.1 精度转换公式
$$\text{Amount} = \frac{\text{RawValue}}{10^{\text{Decimals}}}$$

### 4.2 工业级运算避坑原则
1. **杜绝原生 `float64` / `double`**：IEEE 754 浮点数存在二进制舍入误差（如 `10.0001` 在内存中可能变为 `10.000100000000001`），直接导致与订单金额撮合失败。
2. **使用高精度定点库**：全链路统一采用 `shopspring/decimal` 进行解析、比对与微数累加。
3. **数据库存储类型**：数据表中金额字段统一采用 `DECIMAL(36, 18)` 或字符串，杜绝浮点截断。

---

## 5. 流水幂等排重与链上二次核验

### 5.1 唯一键复合约束
一笔复杂的合约调用（如批转合约、去中心化路由 DEX）可能在单个交易哈希中包含多笔转账：
```sql
CREATE UNIQUE INDEX idx_chain_tx_log ON chain_transfers (chain, tx_hash, log_index);
```
通过数据库级唯一索引排重，配合 `OnConflict DoNothing`，在底层死锁防御与重复数据拦截上实现毫秒级原子幂等。

---

### 5.2 链上收据二次核验 (Receipt Verification)
为了抵御恶意攻击者通过修改日志构造的假充值欺诈（Fake Deposit），撮合引擎在将订单置为 `PAID` 前，支持向全节点发起 `eth_getTransactionReceipt` 二次深层核验：
- 确认交易状态 `status == 1`（成功）。
- 确认事件发起方合约地址确为代币白名单合约，而非同名伪造代币（Fake Token）。
