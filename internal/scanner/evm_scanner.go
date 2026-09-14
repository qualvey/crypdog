package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/logger"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/shopspring/decimal"
)

const ERC20TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

func parseAddressFromTopic(topic string) string {
	clean := strings.TrimPrefix(topic, "0x")
	if len(clean) < 40 {
		return ""
	}
	// 截取后 40 个字符（20 字节十六进制）
	return "0x" + strings.ToLower(clean[len(clean)-40:])
}

var knownTokens = map[string]map[string]model.TokenSpec{
	"ARBITRUM": {
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831": {Symbol: model.TokenUSDC, Decimals: 6}, // Native USDC
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": {Symbol: model.TokenUSDC, Decimals: 6}, // Bridged USDC.e
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": {Symbol: model.TokenUSDT, Decimals: 6}, // USDT
	},
	"BSC": {
		"0x55d398326f99059ff775485246999027b3197955": {Symbol: model.TokenUSDT, Decimals: 18},
		"0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d": {Symbol: model.TokenUSDC, Decimals: 18},
		"0xe9e7cea3dedca5984780bafc599bd69add087d56": {Symbol: "BUSD", Decimals: 18},
	},
	"ETH": {
		"0xdac17f958d2ee523a2206206994597c13d831ec7": {Symbol: model.TokenUSDT, Decimals: 6},
		"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48": {Symbol: model.TokenUSDC, Decimals: 6},
	},
	"POLYGON": {
		"0xc2132d05d31c914a87c6611c10748aeb04b58e8f": {Symbol: model.TokenUSDT, Decimals: 6},
		"0x3c499c542cef5e3811e1192ce70d8cc03d5c3359": {Symbol: model.TokenUSDC, Decimals: 6},
	},
}

// 这只是一个查有没有到账的微服务，那就坚决果断地放弃引入完整的 go-ethereum，选择纯手写 HTTP
// EvmScanner 通用 EVM 链扫描器（实例级别独立，链间彻底隔离）
type EvmScanner struct {
	chain            model.Chain // "BSC", "ARBITRUM", "POLYGON"
	rpcURL           string
	client           EvmRPCClient
	db               *gorm.DB
	lastScannedBlock uint64
	cfg              *config.ChainNodeConfig
	// 运行状态
	latestBlock atomic.Uint64 // 当前全网高度
	scannedSlot uint64        // 当前已确认落库的扫描进度 (单协程内维护无需 atomic)
	curBatch    uint64        // 动态 batch 大小 (支持遇到 10000 limit 时自适应下调)
	// 新增：本链支持的代币集合 (Key 为小写合约地址，原生币可约定为空字符串)
	tokensMu sync.RWMutex
	tokens   map[string]model.TokenSpec // 或使用自定义的 TokenMeta

	// 监控目标缓存 (需提供并发安全读写)
	watchMu sync.RWMutex
	wallets map[string]struct{} // O(1) 匹配监控地址

	// 区块链上真实时间戳缓存，避免同块多次调用 RPC
	blockTimeMu    sync.RWMutex
	blockTimeCache map[uint64]int64
}

func NewEvmScanner(Chain model.Chain, db *gorm.DB, cfg *config.ChainNodeConfig) (*EvmScanner, error) {
	if cfg == nil {
		return nil, fmt.Errorf("chain %s config is nil", Chain)
	}
	scanner := &EvmScanner{
		chain:          Chain,
		db:             db,
		cfg:            cfg,
		client:         *NewEvmRPCClient(cfg.RPCURL),
		wallets:        make(map[string]struct{}),
		tokens:         make(map[string]model.TokenSpec),
		blockTimeCache: make(map[uint64]int64),
	}
	chainkey := strings.ToUpper(string(Chain))
	if defaultTokens, ok := knownTokens[chainkey]; ok {
		for contract, meta := range defaultTokens {
			scanner.tokens[strings.ToLower(contract)] = model.TokenSpec{
				Identifier: strings.ToLower(contract),
				Symbol:     meta.Symbol,
				Decimals:   meta.Decimals,
			}
		}
	}
	// 2. 外部注入：从 cfg.Tokens 追加或覆盖自定义代币
	for _, t := range cfg.Tokens {
		if t.Identifier == "" {
			continue
		}
		scanner.tokens[strings.ToLower(t.Identifier)] = model.TokenSpec{
			Identifier: strings.ToLower(t.Identifier),
			Symbol:     t.Symbol,
			Decimals:   t.Decimals,
		}
	}
	return scanner, nil
}
func (s *EvmScanner) Chain() model.Chain {
	return s.chain
}
func (s *EvmScanner) GetLatestBlock() uint64 {
	return s.latestBlock.Load()
}
func (s *EvmScanner) SupportedTokens() []model.TokenSpec {
	s.tokensMu.RLock()
	defer s.tokensMu.RUnlock()
	list := make([]model.TokenSpec, 0, len(s.tokens))
	for _, spec := range s.tokens {
		list = append(list, spec)
	}
	return list
}

// VerifyTransaction 二次核验交易在主链上是否成功确认，防孤儿块与重组回滚
func (s *EvmScanner) VerifyTransaction(ctx context.Context, txHash string, blockNumber uint64) (bool, error) {
	receipt, err := s.client.GetTransactionReceipt(ctx, txHash)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		return false, nil // 交易收据不存在（可能未打包或已被回滚丢弃）
	}
	// status "0x1" 表示 EVM 交易执行成功
	cleanStatus := strings.TrimPrefix(strings.ToLower(receipt.Status), "0x")
	return cleanStatus == "1", nil
}

// getBlockTimestamp 优先从内存缓存获取区块真实链上时间戳，未命中则实时请求链上 RPC 并缓存
func (s *EvmScanner) getBlockTimestamp(ctx context.Context, blockNumber uint64) int64 {
	s.blockTimeMu.RLock()
	ts, ok := s.blockTimeCache[blockNumber]
	s.blockTimeMu.RUnlock()
	if ok && ts > 0 {
		return ts
	}

	chainTs, err := s.client.GetBlockTimestamp(ctx, blockNumber)
	if err != nil || chainTs <= 0 {
		log.Printf("[%s Scanner] 无法从 RPC 获取区块 %d 时间戳: %v", s.Chain(), blockNumber, err)
		return time.Now().Unix()
	}

	s.blockTimeMu.Lock()
	if len(s.blockTimeCache) > 2000 {
		s.blockTimeCache = make(map[uint64]int64)
	}
	s.blockTimeCache[blockNumber] = chainTs
	s.blockTimeMu.Unlock()
	return chainTs
}

func (e *EvmScanner) Start(ctx context.Context, transferChan chan<- model.ChainTransfer) error {
	interval := 3 * time.Second
	if e.cfg != nil && e.cfg.ScanIntervalSec > 0 {
		interval = time.Duration(e.cfg.ScanIntervalSec) * time.Second
	}
	log.Printf("[%s Scanner] 启动 EVM 扫描守护协程 (扫描间隔: %v)", e.Chain(), interval)
	// 1. 初始化游标：必须成功才能开启事件循环；网络抖动时做指数退避重试
	for {
		if err := e.initCursor(ctx); err != nil {
			log.Printf("[%s Scanner] 初始化游标失败，5秒后重试: %v", e.Chain(), err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			}
		}
		break // 初始化成功，跳出重试
	}
	// 3. 立即执行首次扫描并快速追平历史落后块
	for {
		if err := e.scanNextBlocks(ctx, transferChan); err != nil {
			log.Printf("[%s Scanner] 首次扫块异常: %v", e.Chain(), err)
			break
		}
		if e.lastScannedBlock+5 >= e.latestBlock.Load() || ctx.Err() != nil {
			break
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop() // 1. 退出时清理 Ticker，避免内存泄漏

	// 4. 事件循环
	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s Scanner] 收到上下文退出信号，安全停止", e.Chain())
			return ctx.Err()

		case <-ticker.C:
			// 周期性触发扫块；若滞后则快速连续追赶
			for {
				if err := e.scanNextBlocks(ctx, transferChan); err != nil {
					log.Printf("[%s Scanner] 扫块异常: %v", e.Chain(), err)
					break
				}
				if e.lastScannedBlock+5 >= e.latestBlock.Load() || ctx.Err() != nil {
					break
				}
			}
		}
	}
}

// initCursor 负责启动时确定安全的扫描起始高度
func (e *EvmScanner) initCursor(ctx context.Context) error {
	// A. 先获取当前链上最新高度，更新内存原子状态
	latestOnChain, err := e.client.GetLatestBlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("获取链上最新高度失败: %w", err)
	}
	e.setLatestBlock(latestOnChain)

	// B. 如果外部未显式设置历史断点 (e.lastScannedBlock == 0)
	if e.lastScannedBlock == 0 {
		// 1. 优先尝试从 DB 进度表中加载上次落库断点
		var progress model.ScanProgress
		err := e.db.WithContext(ctx).
			Where("chain = ?", e.Chain()).
			First(&progress).Error

		if err == nil && progress.LastScannedBlock > 0 {
			e.lastScannedBlock = progress.LastScannedBlock
			logger.Info("[%s Scanner] 从数据库恢复断点成功，起始高度: %d", e.Chain(), e.lastScannedBlock)
			return nil
		}

		// 2. DB 查不到，若配置中配置了 StartBlock，则优先使用
		if e.cfg.StartBlock > 0 {
			e.lastScannedBlock = e.cfg.StartBlock - 1
			log.Printf("[%s Scanner] 未发现历史进度，采用配置的起始高度: %d", e.Chain(), e.cfg.StartBlock)
			return nil
		}

		// 3. 都没有，以最新高度后退安全冗余 (防 uint64 下溢)
		safetyMargin := uint64(5)
		if e.cfg.BlockDelay > 0 {
			safetyMargin = e.cfg.BlockDelay
		}

		if latestOnChain > safetyMargin {
			e.lastScannedBlock = latestOnChain - safetyMargin
		} else {
			e.lastScannedBlock = 0
		}
		log.Printf("[%s Scanner] 未配置历史进度，默认从链上安全高度开始: %d (最新: %d, 冗余: %d)",
			e.Chain(), e.lastScannedBlock, latestOnChain, safetyMargin)
	}

	return nil
}
func (e *EvmScanner) setLatestBlock(blk uint64) {
	for {
		current := e.latestBlock.Load()
		if blk <= current {
			return
		}
		if e.latestBlock.CompareAndSwap(current, blk) {
			return
		}
	}
}

// scanNextBlocks 核心步进扫块逻辑
func (e *EvmScanner) scanNextBlocks(ctx context.Context, transferChan chan<- model.ChainTransfer) error {
	latestOnChain, err := e.client.GetLatestBlockNumber(ctx)
	if err != nil {
		metrics.RecordScanError(string(e.Chain()), "evm")
		return fmt.Errorf("获取最新区块高度失败: %w", err)
	}
	e.setLatestBlock(latestOnChain)
	metrics.RecordScanBlock(string(e.Chain()), e.lastScannedBlock, latestOnChain)
	// 1. 防分叉安全高度计算 (Safe Block)
	blockDelay := uint64(5) // 默认 5 个确认数
	if e.cfg.BlockDelay > 0 {
		blockDelay = e.cfg.BlockDelay
	}
	safeBlock := latestOnChain - blockDelay
	// 还没有新出的安全块，等待下一轮
	if safeBlock <= e.lastScannedBlock {
		return nil
	}
	fromBlock := e.lastScannedBlock + 1

	// 2. 查出当前正在等待收款的钱包地址（用于内存过滤与服务端 Topic 过滤）
	var activeWallets []model.WalletAddress
	_ = e.db.Where("UPPER(chain) = ? AND enabled = ?", e.Chain(), true).Find(&activeWallets).Error
	if len(activeWallets) == 0 {
		// 没有待监听地址，直接大步推进游标落库
		toBlock := fromBlock + 500 - 1
		if toBlock > safeBlock {
			toBlock = safeBlock
		}
		return e.commitProgress(ctx, toBlock)
	}
	walletMap := make(map[string]bool, len(activeWallets))
	targetAddrs := make([]string, 0, len(activeWallets))
	for _, w := range activeWallets {
		walletMap[strings.ToLower(w.Address)] = true
		targetAddrs = append(targetAddrs, w.Address)
	}

	batchSize := uint64(500)
	if e.cfg.BatchSize > 0 {
		batchSize = e.cfg.BatchSize
	}
	toBlock := fromBlock + batchSize - 1
	if toBlock > safeBlock {
		toBlock = safeBlock
	}

	// 3. 查询此区间内的 ERC20 Transfer 日志（使用服务端 topics 过滤，毫秒级响应）
	logs, err := e.client.GetERC20Logs(ctx, fromBlock, toBlock, targetAddrs...)
	if err != nil {
		return fmt.Errorf("拉取区块 [%d - %d] 日志失败: %w", fromBlock, toBlock, err)
	}
	// 3. 过滤并投递交易
	for _, l := range logs {
		// Topic0 必须是 Transfer 事件，且至少需要包含 3 个 topic (event, from, to)
		if len(l.Topics) < 3 || strings.ToLower(l.Topics[0]) != ERC20TransferTopic {
			continue
		}

		// 解析接收者地址 (EVM topic 存的是 32 字节 pad0 格式，需截取后 20 字节)
		targetAddress := parseAddressFromTopic(l.Topics[2])
		if !walletMap[strings.ToLower(targetAddress)] {
			continue // 不是系统当前监控的地址，直接跳过
		}
		// 解析链上原始金额大数 (避免浮点数精度丢失)
		amountBig := new(big.Int)
		cleanData := strings.TrimPrefix(l.Data, "0x")
		if cleanData == "" {
			cleanData = "0"
		}
		if _, ok := amountBig.SetString(cleanData, 16); !ok {
			log.Printf("[%s Scanner] 解析金额失败，跳过: tx=%s", e.Chain(), l.TransactionHash)
			continue
		}

		// 校验代币合约并提取代币元信息（支持 knownTokens 及 config 自定义注入代币）
		e.tokensMu.RLock()
		tokenMeta, isKnown := e.tokens[strings.ToLower(l.Address)]
		e.tokensMu.RUnlock()
		if !isKnown {
			// 未知或非监控代币（如投毒假币等），直接过滤
			continue
		}

		fromAddress := parseAddressFromTopic(l.Topics[1])
		valDec, _ := decimal.NewFromString(amountBig.String())
		readableAmount := valDec.Div(decimal.New(1, int32(tokenMeta.Decimals)))

		transfer := model.NewChainTransfer(
			e.chain,
			l.TransactionHash,
			l.LogIndex,
			l.Address,
			targetAddress,
			amountBig.String(),
			l.BlockNumber,
		)
		transfer.Token = tokenMeta.Symbol
		transfer.Amount = readableAmount
		transfer.Decimals = uint8(tokenMeta.Decimals)
		transfer.FromAddress = fromAddress
		transfer.BlockTimestamp = e.getBlockTimestamp(ctx, l.BlockNumber)

		// 关键数据安全保障：流水先落库持久化，再推入内存 Channel，防止崩溃位点推进导致丢单
		if e.db != nil {
			_ = e.db.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "chain"},
					{Name: "tx_hash"},
					{Name: "log_index"},
				},
				DoNothing: true,
			}).Create(&transfer).Error
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case transferChan <- transfer:
			metrics.RecordTransferCaptured(string(e.Chain()), string(transfer.Token))
			log.Printf("[%s Scanner] 捕获充值: Tx=%s, To=%s, Token=%s, Amount=%s, RawAmount=%s, Block=%d",
				e.Chain(), transfer.TxHash, transfer.TargetAddress, transfer.Token, transfer.Amount.String(), transfer.RawValue, transfer.BlockNumber)
		}
	}

	// 5. 游标必须事务落库持久化
	return e.commitProgress(ctx, toBlock)
}

// commitProgress 原子持久化游标
func (e *EvmScanner) commitProgress(ctx context.Context, toBlock uint64) error {
	progress := model.ScanProgress{
		Chain:            e.Chain(),
		LastScannedBlock: toBlock,
		UpdatedAt:        time.Now(),
	}
	err := e.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chain"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_scanned_block", "updated_at"}),
	}).Create(&progress).Error

	if err != nil {
		return fmt.Errorf("持久化扫描游标失败: %w", err)
	}

	e.lastScannedBlock = toBlock
	metrics.RecordScanBlock(string(e.Chain()), toBlock, e.latestBlock.Load())
	return nil
}
func parseEvmAmount(rawValue *big.Int, decimals int) float64 {
	if rawValue == nil {
		return 0
	}
	f := new(big.Float).SetInt(rawValue)
	divisor := new(big.Float).SetFloat64(math.Pow10(decimals))
	f.Quo(f, divisor)
	val, _ := f.Float64()
	return val
}

// // SimulateTransfer allows local testing/simulation of EVM transfers
// func (e *EvmScanner) SimulateTransfer(toAddress string, amount float64, token model.Token, transferChan chan<- model.ChainTransfer) string {
// 	txHash := fmt.Sprintf("0x%x%d", rand.Uint64(), time.Now().UnixNano())
// 	if token == "" {
// 		token = "USDC"
// 	}
// 	chain := "ARBITRUM"

// 	latest := e.GetLatestBlock()
// 	if latest == 0 {
// 		latest = 39821000
// 		e.SetLatestBlock(latest + 10)
// 	}

// 	transfer := model.ChainTransfer{
// 		TxHash:         txHash,
// 		Chain:          chain,
// 		FromAddress:    "0xfromsimulatedevmaddress1234567890",
// 		TargetAddress:  strings.ToLower(strings.TrimSpace(toAddress)),
// 		Amount:         amount,
// 		Token:          (token),
// 		BlockNumber:    latest,
// 		BlockTimestamp: time.Now().UnixMilli(),
// 	}

//		transferChan <- transfer
//		return txHash
//	}
