# CrypDog 架构与核心流程设计文档

CrypDog 是一个轻量级、高并发的去中心化虚拟币支付监控网关系统。系统支持多链（EVM 兼容链、TRON、Solana 等）、单地址微数尾数防撞撮合（Micro-Amount Matching）、多阶段区块确认度推进以及带 HMAC-SHA256 防伪签名的异步 Webhook 回调通知。

---

## 1. 整体系统架构与数据流向 (System Architecture)

系统由 **API 网关服务**、**微数资金池管理器**、**多链独立扫描驱动池**、**交易流水通道 Pipeline**、**撮合引擎** 及 **后台异步守护集群** 共同构建。

```mermaid
flowchart TD
    subgraph ClientLayer["外部接入层 (Clients / Merchants)"]
        Merchant["主业务系统 (Merchant Server)"]
        UserClient["终端付款用户 (Payer)"]
    end

    subgraph APILayer["API 接入网关 (HTTP / Gin)"]
        Router["RESTful API 路由 (/api/v1/watcher)"]
        AuthMid["Bearer Token 鉴权中间件"]
        IntentHandler["订单意图处理器 (Intent Handler)"]
    end

    subgraph ServiceLayer["业务逻辑与规则层"]
        IntentService["支付意图服务 (Intent Service)"]
        PoolManager["微数分配与防撞管理器 (MicroAmountManager)"]
    end

    subgraph ChainLayer["区块链外部网络 (Public Blockchains)"]
        EVMChain["EVM 节点 (ETH / BSC / Arbitrum / Polygon)"]
        TronChain["TRON 节点 (TRC-20)"]
        SolChain["Solana 节点 (SPL)"]
    end

    subgraph ScannerLayer["链扫描驱动集群 (Scanner Drivers)"]
        ScannerMgr["扫描器管理器 (Scanner Manager)"]
        EVMScanner["EVM 扫描器"]
        TronScanner["TRON 扫描器"]
        SolScanner["Solana 扫描器"]
    end

    subgraph PipelineLayer["高并发缓冲区 (Channel Pipeline)"]
        TransferChan[("transferChan (chan ChainTransfer, Buffer: 500)")]
        Consumer["流水消费调度器 (Safe Process Pipeline)"]
    end

    subgraph EngineLayer["核心撮合与状态推进 (Engine & Workers)"]
        Matcher["撮合引擎 (Matcher Engine)"]
        ConfirmWorker["确认度巡检协程 (Confirmation Worker)"]
        Cleaner["超时订单自动释放守护协程 (Intent Cleaner)"]
    end

    subgraph StorageLayer["数据持久化层 (Database)"]
        DB[("GORM / SQLite / MySQL")]
    end

    subgraph NotifyLayer["可靠异步通知 (Notification & Retry)"]
        Dispatcher["Webhook 分发器 (Webhook Dispatcher)"]
        RetryQueue["指数退避重试调度 (2s, 10s, 30s, 2m)"]
    end

    %% 业务交互连线
    Merchant -->|"1. 注册或分配订单 (/intents/allocate)"| Router
    Router --> AuthMid --> IntentHandler --> IntentService
    IntentService <--> PoolManager
    PoolManager <--> DB
    IntentService --> DB

    %% 付款与扫链
    UserClient -.->|"向聚合收款地址转账"| EVMChain & TronChain & SolChain
    EVMChain -.->|"eth_getLogs 轮询"| EVMScanner
    TronChain -.->|"getEventsByContract 轮询"| TronScanner
    SolChain -.->|"getSignaturesForAddress 轮询"| SolScanner

    ScannerMgr --> EVMScanner & TronScanner & SolScanner
    EVMScanner & TronScanner & SolScanner -->|"投递 ChainTransfer 流水"| TransferChan

    %% Pipeline消费与撮合
    TransferChan --> Consumer --> Matcher
    Matcher -->|"原子写入并排重流水"| DB
    Matcher -->|"查询待匹配 WATCHING/CONFIRMING"| DB
    Matcher -->|"满足确认数达标"| Dispatcher
    Matcher -.->|"确认数不足"| ConfirmWorker

    %% 后台状态同步
    ConfirmWorker -->|"轮询当前各链最新块高"| ScannerMgr
    ConfirmWorker -->|"达标更新为 PAID"| DB
    ConfirmWorker --> Dispatcher

    Cleaner -->|"定期释放超期 WATCHING 订单"| DB

    %% Webhook 发送
    Dispatcher -->|"HMAC-SHA256 签名通知"| Merchant
    Dispatcher -.->|"通知失败时"| RetryQueue
    RetryQueue -.-> Dispatcher
```

---

## 2. 核心业务时序图 (Core Sequence Diagrams)

### 2.1 订单注册与微数动态分配流程 (Intent Allocation & Registration)

商户生成订单后，向系统申请收款任务。若未指定精准尾数，系统自动通过地址轮换与动态步长分配独占的微数金额，防止同地址同金额碰撞。

```mermaid
sequenceDiagram
    autonumber
    actor Merchant as 商户主业务服务
    participant API as API 网关 (/api/v1/watcher)
    participant Service as 意图服务 (IntentService)
    participant Pool as 微数管理器 (MicroAmountManager)
    participant DB as 数据库 (Database)

    Merchant->>API: POST /intents/allocate (orderId, chain, token, baseAmount, webhookUrl)
    API->>API: 验证 Bearer Token 签名
    API->>Service: RegisterOrReactivate(dto)

    Service->>DB: 查询是否存在该 orderId
    alt 订单已存在且 PAID
        DB-->>Service: 返回 PAID 记录
        Service-->>API: 报错 ErrOrderAlreadyPaid
        API-->>Merchant: 400 Bad Request
    else 订单存在且 WATCHING/CONFIRMING (幂等请求)
        DB-->>Service: 返回现有订单信息
        Service-->>API: 幂等返回已有 Intent
        API-->>Merchant: 200 OK (已存在的 intentId)
    else 订单为全新创建 或 已超时/取消(重新激活)
        Service->>Pool: AllocateUniqueAmount(chain, token, baseAmount)
        Pool->>DB: 查询并以 last_used_at 升序获取可用钱包地址
        DB-->>Pool: 返回收款钱包 (targetAddress)
        Pool->>DB: 查询当前 targetAddress 活跃的 (WATCHING/CONFIRMING) 意图
        DB-->>Pool: 返回活跃金额集合
        Pool->>Pool: 遍历区间 [0.0001, 0.0999] 步长 0.0001<br/>计算首个未占用的尾数 offset
        Pool-->>Service: 分配完成 (targetAddress, expectedAmount = base + offset)
        
        Service->>Pool: CheckCollision(chain, token, targetAddress, expectedAmount)
        Pool-->>Service: 校验通过 (无碰撞)
        
        Service->>DB: INSERT / UPDATE PaymentIntent (Status = WATCHING)
        DB-->>Service: 保存成功
        Service-->>API: 返回新构建的 PaymentIntent
        API-->>Merchant: 200 OK (intentId, targetAddress, expectedAmount, expiresAt)
    end
```

---

### 2.2 扫链、流水入库与实时撮合时序 (Blockchain Scanning & Matching Pipeline)

多链扫描器并发轮询各个公链节点，发现链上代币转账时包装为统一的 `ChainTransfer` 并发往无锁通道，由 Pipeline 消费者安全调度撮合引擎。

```mermaid
sequenceDiagram
    autonumber
    actor Payer as 付款用户 (钱包/交易所)
    participant Chain as 区块链网络 (EVM/Tron/Solana)
    participant Scanner as 链扫描驱动 (Scanner Driver)
    participant Pipe as 缓冲管道 (transferChan)
    participant Matcher as 撮合引擎 (Matcher Engine)
    participant DB as 数据库 (Database)
    participant Dispatcher as Webhook 分发器

    Payer->>Chain: 发起转账交易 (目标地址, 尾数金额)
    Note over Scanner,Chain: 扫描器按照步长轮询新区块 / 事件日志

    Scanner->>Chain: 拉取最新区块及 Transfer 事件日志
    Chain-->>Scanner: 返回转账事件列表
    Scanner->>Pipe: 封装 model.ChainTransfer 并写入 channel (容量 500)

    Pipe->>Matcher: Pipeline 消费者取出 Transfer 并触发 ProcessTransfer(transfer, currentBlock)
    
    critical 流水去重与持久化
        Matcher->>DB: 基于 (chain, tx_hash, log_index) 复合唯一键插入 (OnConflict DoNothing)
        DB-->>Matcher: 返回是否为新流水 (isNew)
    end

    alt 已存在历史流水
        Matcher->>Matcher: 终止处理 (防止重复入账)
    else 全新入库流水
        Matcher->>DB: 查询待撮合候选订单<br/>(status IN [WATCHING, CONFIRMING] & chain & token & targetAddress)
        DB-->>Matcher: 返回候选 Intent 列表
        
        Matcher->>Matcher: 内存精确比对: |expectedAmount - amount| <= epsilon
        
        alt 未找到匹配订单
            Matcher->>Matcher: 视为外部未知转账，流水保留但暂无归属
        else 命中目标订单
            Matcher->>Matcher: 计算确认数 confirmations = currentBlock - txBlock + 1
            
            alt confirmations >= Required (直接达标)
                Matcher->>DB: 开启事务: 更新 Intent -> PAID, 关联流水 matched_order_id
                DB-->>Matcher: 事务提交成功
                Matcher->>Dispatcher: 异步触发通知 DispatchAsync(intent, txHash, timestamp)
            else confirmations < Required (需等待更多确认块)
                Matcher->>DB: 更新 Intent -> CONFIRMING (记录中间 confirmations 与 txHash)
                DB-->>Matcher: 更新成功，交由后台 Worker 持续轮询
            end
        end
    end
```

---

### 2.3 区块确认度巡检推进时序 (Confirmation Worker Workflow)

针对已命中交易但区块确认数暂未达到安全阈值（如 TRON 需 19 个块防分叉/双花）的订单，后台巡检协程持续追踪并推进最终状态。

```mermaid
sequenceDiagram
    autonumber
    participant Worker as 确认度协程 (ConfirmationWorker)
    participant DB as 数据库 (Database)
    participant ScannerMgr as 扫描器管理器 (ScannerManager)
    participant Dispatcher as Webhook 分发器

    loop 每 2 秒巡检一次 (Ticker 2s)
        Worker->>DB: 查询 status = 'CONFIRMING' 且 tx_hash != '' 的订单列表
        DB-->>Worker: 返回待确认意图列表

        loop 遍历每一个待确认意图
            Worker->>ScannerMgr: GetLatestBlock(intent.Chain)
            ScannerMgr-->>Worker: 返回当前链最新高度 (currentBlock)
            
            Worker->>Worker: 计算实时确认数 = currentBlock - intent.BlockNumber + 1
            
            alt 确认数 >= 所需深度 (RequiredConfirmations)
                Worker->>DB: 更新 Intent 状态 -> PAID, 记录 paidAt
                Worker->>DB: 关联 ChainTransfer 记录 matched_order_id
                Worker->>DB: 查询流水原始 block_timestamp
                DB-->>Worker: 返回区块时间戳
                Worker->>Dispatcher: 触发异步回调 DispatchAsync(intent, txHash, blockTs)
            else 确认数更新但仍未达标
                Worker->>DB: 更新 Intent.Confirmations = 最新确认数
            end
        end
    end
```

---

### 2.4 双段 Webhook 可靠回调与指数退避重试 (Dual-Phase Webhook Delivery & Exponential Backoff)

CrypDog 采用双段 Webhook 架构：
1. **初次捕获 (Phase 1: `get`)**：当链上首次撮合到充值流水时立即发送，携带最新哈希与当前确认数；
2. **确认达成 (Phase 2: `confirm`)**：当区块确认数达成后再次发送，驱动商户完成核销发货。

每段通知均带有独立签名的 `HMAC-SHA256`。若商户响应异常，启动最多 5 次指数退避持久化重试。

```mermaid
sequenceDiagram
    autonumber
    participant Dispatcher as Webhook 分发器
    participant Merchant as 商户接收端 (Merchant Webhook URL)
    participant DB as 数据库 (WebhookLog)

    Note over Dispatcher: Phase 1: 首次匹配流水
    Dispatcher->>Dispatcher: 构造 WebhookPayload (event: "get")
    Dispatcher->>Merchant: POST {webhookUrl} [get]<br/>Header: X-Signature-SHA256
    Merchant-->>Dispatcher: 200 OK (已捕获充值，等待确认)

    Note over Dispatcher: Phase 2: 区块确认数达标
    Dispatcher->>Dispatcher: 构造 WebhookPayload (event: "confirm")
    Dispatcher->>Merchant: POST {webhookUrl} [confirm]<br/>Header: X-Signature-SHA256
    
    alt 商户返回 2xx (成功)
        Merchant-->>Dispatcher: 200 OK
        Dispatcher->>DB: 记录 WebhookLog (Success = true, StatusCode = 200)
    else 请求超时 / 错误 (5xx / 4xx)
        Merchant-->>Dispatcher: 连接失败 / 500 Internal Server Error
        Dispatcher->>DB: 记录 WebhookLog (Success = false, Attempt = 1, StatusCode = 500)
        
        Note over Dispatcher,Timer: 启动第 1 次重试延时: 2 秒
        Dispatcher->>Timer: time.AfterFunc(2s)
        Timer-->>Dispatcher: 触发 Attempt 2
        Dispatcher->>Merchant: POST {webhookUrl} [Attempt 2]
        
        alt 仍然失败
            Note over Dispatcher,Timer: 依次类推：第 3 次 (10s) -> 第 4 次 (30s) -> 第 5 次 (2m)
            Dispatcher->>Timer: 重试直至第 5 次
            Timer-->>Dispatcher: 第 5 次尝试依然失败
            Dispatcher->>DB: 记录最终失败日志 (不再自动重试，等待人工对账)
        end
    end
```

---

## 3. 支付意图状态机 (Payment Intent State Machine)

`PaymentIntent` 在其生命周期内严格遵循状态机流转，防止发生状态跳变或脏读并发。

```mermaid
stateDiagram-v2
    [*] --> WATCHING : 1. 创建订单 / 分配微数成功
    
    WATCHING --> CONFIRMING : 2. 扫链命中匹配交易流水<br/>(确认数 < 安全阈值)
    WATCHING --> PAID : 3. 扫链命中交易流水<br/>(确认数直接达标，如确认深度要求为 1)
    
    CONFIRMING --> PAID : 4. 后台巡检最新高度<br/>(确认数 >= 安全阈值)
    
    WATCHING --> EXPIRED : 5. 超过有效时间 (timeoutSeconds)<br/>后台 Cleaner 自动释放
    
    WATCHING --> CANCELLED : 6. 商户主动调用取消接口
    CONFIRMING --> CANCELLED : 6. 商户主动取消正在确认中的订单
    
    EXPIRED --> WATCHING : 7. 相同 orderId 重新发起激活<br/>(重新分配微数与刷新超时时间)
    CANCELLED --> WATCHING : 7. 相同 orderId 重新发起激活
    
    PAID --> [*] : 8. 终态 (不可变，拦截所有重复请求)
    EXPIRED --> [*]
    CANCELLED --> [*]
```

---

## 4. 微数资金池分配与防撞算法 (Micro-Amount Allocation Flow)

当多个订单同时要求支付相同基础金额（如 10.0 USDT）到同一个归集地址时，系统通过微数管理器给每个订单分配独立的尾数偏移（如 10.0001, 10.0002），实现**单地址多笔并发收款的唯一标识性**。

```mermaid
flowchart TD
    Start([开始: 申请分配微数]) --> FetchWallet["查询启用的收款地址列表<br/>(按 last_used_at 升序，轮换最久未使用地址)"]
    FetchWallet --> CheckWallet{是否存在可用地址?}
    CheckWallet -- 否 --> ReturnErr1["返回错误: 无可用收款地址"]
    CheckWallet -- 是 --> GetActiveIntents["查询当前地址上正在活跃中的订单<br/>(Status IN [WATCHING, CONFIRMING])"]
    
    GetActiveIntents --> BuildOccupiedMap["提取活跃订单 ExpectedAmount - BaseAmount 的差值<br/>建立占用步长位图 occupied[stepIdx] = true"]
    BuildOccupiedMap --> LoopSteps["从 step = 1 (0.0001) 迭代至 maxSteps (0.0999)"]
    
    LoopSteps --> IsOccupied{该 step 是否被占用?}
    IsOccupied -- 是 --> NextStep["step++ (尝试下一个可用偏移)"]
    NextStep --> CheckLimit{是否超出 0.0999?}
    CheckLimit -- 否 --> IsOccupied
    CheckLimit -- 是 --> ReturnErr2["返回错误: 当前地址微数槽位已满 (999个已耗尽)"]
    
    IsOccupied -- 否 --> CalcAmount["计算分配金额:<br/>expectedAmount = baseAmount + step * 0.0001<br/>保留 6 位小数四舍五入"]
    CalcAmount --> CheckCollision["调用 CheckCollision 针对 (chain, token, address, amount) 进行二次校验"]
    CheckCollision --> Collided{是否存在冲突?}
    Collided -- 是 --> NextStep
    Collided -- 否 --> ReserveSuccess(["分配成功，锁定该微数并生成 PaymentIntent"])
```

---

## 5. 核心数据库实体关系图 (Entity Relationship - ER)

系统核心包含 5 个数据模型：支付意图任务表、链上流水存证表、出网 Webhook 日志表、钱包地址表与系统支付配置表。

```mermaid
erDiagram
    PaymentIntent ||--o| ChainTransfer : "matched by tx_hash & log_index"
    PaymentIntent ||--o{ WebhookLog : "produces delivery records"
    WalletAddress ||--o{ PaymentIntent : "receives funds at target_address"

    PaymentIntent {
        string ID PK "业务前缀流水号 (如 pi_tron_xxx)"
        string OrderID UK "商户业务订单唯一编号"
        string TxHash "命中交易哈希"
        int64 LogIndex "事件日志索引"
        string Chain "区块链名称 (TRON/ETH/BSC/SOLANA)"
        string Token "代币符号 (USDT/USDC/ETH)"
        string TargetAddress "收款钱包地址"
        decimal ExpectedAmount "期望到账金额 (含微数尾数)"
        decimal ReceivedAmount "实际链上入账金额"
        string WebhookURL "商户异步回调地址"
        string Status "订单状态 (WATCHING/CONFIRMING/PAID/EXPIRED/CANCELLED)"
        uint64 BlockNumber "交易所在区块高度"
        uint64 Confirmations "当前已达到的确认深度"
        datetime ExpiresAt "过期失效时间"
        datetime PaidAt "支付完成时间"
        datetime CreatedAt "创建时间"
        datetime UpdatedAt "更新时间"
    }

    ChainTransfer {
        uint ID PK "自增物理主键"
        string TxHash UK "链上交易哈希"
        int64 LogIndex UK "事件在区块内的索引 (联合唯一)"
        string Chain UK "区块链名称"
        string Token "代币符号"
        string Contract "代币合约地址"
        string FromAddress "付款人链上地址"
        string TargetAddress "收款人链上地址"
        decimal Amount "代币可读格式金额"
        string RawValue "链上原始大数字符串 (防丢失精度)"
        uint64 BlockNumber "区块高度"
        int64 BlockTimestamp "出块时间戳"
        string MatchedOrderID "撮合命中的商户订单号"
        datetime CreatedAt "落库时间"
    }

    WebhookLog {
        uint ID PK "自增主键"
        string OrderID "关联商户订单号"
        string WebhookURL "请求目标 URL"
        string Payload "请求 JSON 报文"
        string Signature "HMAC-SHA256 签名值"
        int StatusCode "HTTP 响应状态码"
        string ResponseBody "商户服务端返回内容"
        boolean Success "是否投递成功 (2xx)"
        int Attempt "当前重试次数 (1~5)"
        datetime NextRetryAt "下次重试时间点"
        datetime CreatedAt "请求发起时间"
    }

    WalletAddress {
        uint ID PK "主键"
        string Chain "链类型"
        string Address "钱包公钥地址"
        boolean Enabled "是否启用"
        datetime LastUsedAt "最近一次分配使用时间"
    }

    PaymentConfig {
        uint ID PK "配置主键"
        json Alipay "支付宝配置"
        json Aggregate "聚合支付网关配置"
        json Crypto "虚拟货币支持列表及汇率"
    }
```

---

## 6. 平滑退出与优雅停机流程 (Graceful Shutdown Flow)

系统采用 Goroutine + Context + WaitGroup + Channel 排空机制，确保在容器重启、发布更新或接收到 `SIGINT / SIGTERM` 信号时不丢任何一笔链上流水。

```mermaid
flowchart TD
    Signal(["监听到系统中断信号: SIGINT / SIGTERM"]) --> StopHTTP["1. 首先停止 HTTP Server (Set 8s Timeout)<br/>拒绝外部新的 API 请求流入"]
    StopHTTP --> CancelContext["2. 触发 context.CancelFunc()<br/>级联通知 Scanners、Workers、Cleaner 退出"]
    CancelContext --> DrainPipeline["3. Pipeline 交易消费调度器进入排空模式"]
    
    subgraph DrainChannel["通道安全排空 (Safe Drain)"]
        DrainLoop{"transferChan 缓冲区中<br/>是否还有未消费的流水?"}
        DrainLoop -- 还有积压流水 --> ProcessRemaining["逐条取出并调用 Matcher.ProcessTransfer 完成入库撮合"]
        ProcessRemaining --> DrainLoop
        DrainLoop -- 缓冲区已清空 --> CloseConsumer["标记 Consumer 退出，wg.Done()"]
    end

    DrainPipeline --> DrainChannel
    CloseConsumer --> WaitGroup["4. a.wg.Wait() 等待所有已纳管 Goroutine 收尾"]
    WaitGroup --> SafeExit(["5. CrypDog 安全停止 (Process Terminated cleanly)"])
```