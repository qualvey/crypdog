
| Name　　　　　　　　　　　 | Comment　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　　 |
| ----------------------------| -----------------------------------------------------------------------------------------------|
| BlockNumber/Slot　　　　　 | 扫块器通过 last_scanned_block 持续递增前进，服务重启依赖持久化该指针防止漏扫。　　　　　　　　|
| Safe Block & Confirmations | 链可能发生微小回滚（分叉 Reorg）。最新块不可信，必须落后最新高度 $N$ 个块后才判定为安全入库。 |
| TxHash + LogIndex　　　　　| 一笔合约交易内可能拆发多笔 Transfer。必须使用 (Chain, TxHash, LogIndex) 组合排重。　　　　　　|
| Decimals (代币精度)　　　　| 链上只存整型。解析出的大数必须转为字符串，再基于 Decimals 计算为高精度业务金额。　　　　　　　|



链上事件扫描与安全高度（Scanner）防分叉延迟（Reorg Delay）：$$\text{SafeBlock} = \text{LatestOnChain} - \text{BlockDelay}$$必须校验 $\text{LatestOnChain} > \text{BlockDelay}$，防止无符号整数下溢。

分批扫描：按 [fromBlock, min(fromBlock + batchSize - 1, safeBlock)] 步进，防止 RPC 触发 Too Many Logs 超时或限流。

地址归一化规则（NormalizeAddress）：

EVM (0x...) / Bech32 (bc1...)：强制全小写。

Base58 (TRON 'T...', Solana, BTC Legacy)：严格保留原始大小写，严禁使用 strings.ToLower 或 EqualFold。


构造与数据无损转换（Factory）扫描器捕获到底层日志后，统一走 NewChainTransfer 构造，保证进入流水线的数据天然合规：

LogIndex 提取：

EVM 系列：提取 ERC-20 Transfer 日志的 LogIndex。

Solana 系列：提取 PostTokenBalances 中的 AccountIndex（防同 Tx 多笔转账漏单）。

金额转换：

$$\text{Amount} = \frac{\text{RawValue}}{10^{\text{Decimals}}}$$

统一使用 shopspring/decimal，杜绝任何 float64 浮点数参与运算。

幂等排重与落库（Deduplication）

CREATE UNIQUE INDEX idx_chain_tx_log ON chain_transfers (chain, tx_hash, log_index);
先写数据库捕获唯一键冲突，或使用原生原子写入操作。通过 DB 的原子约束阻断重复消费，而非依赖内存锁

3.5 订单候选查询与精度撮合（Matching）
```sql
SELECT * FROM payment_intents 
WHERE status IN ('watching', 'confirming') 
  AND chain = ? 
  AND token = ? 
  AND target_address = ?;
```
