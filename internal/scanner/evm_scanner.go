package scanner

import (
	"bytes"
	"context"
	"crypdog/internal/config"
	"crypdog/internal/logger"
	"crypdog/internal/model"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

type TokenMeta struct {
	Symbol   string
	Decimals int
}

const ERC20TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

func parseAddressFromTopic(topic string) string {
	clean := strings.TrimPrefix(topic, "0x")
	if len(clean) < 40 {
		return ""
	}
	// 截取后 40 个字符（20 字节十六进制）
	return "0x" + strings.ToLower(clean[len(clean)-40:])
}

var knownTokens = map[string]map[string]TokenMeta{
	"ARBITRUM": {
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831": {Symbol: "USDC", Decimals: 6}, // Native USDC
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": {Symbol: "USDC", Decimals: 6}, // Bridged USDC.e
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": {Symbol: "USDT", Decimals: 6}, // USDT
		"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": {Symbol: "DAI", Decimals: 18},
	},
	"BSC": {
		"0x55d398326f99059ff775485246999027b3197955": {Symbol: "USDT", Decimals: 18},
		"0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d": {Symbol: "USDC", Decimals: 18},
		"0xe9e7cea3dedca5984780bafc599bd69add087d56": {Symbol: "BUSD", Decimals: 18},
	},
	"ETH": {
		"0xdac17f958d2ee523a2206206994597c13d831ec7": {Symbol: "USDT", Decimals: 6},
		"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48": {Symbol: "USDC", Decimals: 6},
	},
	"POLYGON": {
		"0xc2132d05d31c914a87c6611c10748aeb04b58e8f": {Symbol: "USDT", Decimals: 6},
		"0x3c499c542cef5e3811e1192ce70d8cc03d5c3359": {Symbol: "USDC", Decimals: 6},
	},
}

// 这只是一个查有没有到账的微服务，那就坚决果断地放弃引入完整的 go-ethereum，选择纯手写 HTTP
// EvmScanner 通用 EVM 链扫描器（实例级别独立，链间彻底隔离）
type EvmScanner struct {
	chain            model.Chain // "BSC", "ARBITRUM", "POLYGON"
	rpcURL           string
	db               *gorm.DB
	lastScannedBlock uint64
	cfg              *config.ChainNodeConfig
	client           *http.Client
	// 运行状态
	latestBlock atomic.Uint64 // 当前全网高度
	scannedSlot uint64        // 当前已确认落库的扫描进度 (单协程内维护无需 atomic)
	curBatch    uint64        // 动态 batch 大小 (支持遇到 10000 limit 时自适应下调)

	// 监控目标缓存 (需提供并发安全读写)
	watchMu sync.RWMutex
	wallets map[string]struct{} // O(1) 匹配监控地址
}

// 标准 JSON-RPC 请求结构体
type jsonRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

// 标准 JSON-RPC 响应结构体
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// 对应 eth_getLogs 返回的单条日志结构
type evmLogItem struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	Data            string   `json:"data"`
	BlockNumberHex  string   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	BlockNumber     uint64   `json:"-"` // 解析后填入
}

func NewEvmScanner(Chain model.Chain, db *gorm.DB, cfg *config.Config) *EvmScanner {
	return &EvmScanner{
		chain:  Chain,
		rpcURL: cfg.Chains[Chain].RPCURL,
	}
}
func (s *EvmScanner) Chain() model.Chain {
	return s.chain
}
func (s *EvmScanner) GetLatestBlock() uint64 {
	return 888888888
}

func (e *EvmScanner) Start(ctx context.Context, transferChan chan<- model.ChainTransfer) error {
	interval := time.Duration(e.cfg.ScanIntervalSec) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second // 默认兜底 3 秒
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
	// 3. 立即执行首次扫描（不用干等第一个周期触发）
	if err := e.scanNextBlocks(ctx, transferChan); err != nil {
		log.Printf("[%s Scanner] 首次扫块异常: %v", e.Chain(), err)
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
			// 周期性触发下批次区块扫描
			if err := e.scanNextBlocks(ctx, transferChan); err != nil {
				log.Printf("[%s Scanner] 扫块异常: %v", e.Chain(), err)
			}
		}
	}
}

// initCursor 负责启动时确定安全的扫描起始高度
func (e *EvmScanner) initCursor(ctx context.Context) error {
	// A. 先获取当前链上最新高度，更新内存原子状态
	latestOnChain, err := e.fetchLatestBlockNumber(ctx)
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
	latestOnChain, err := e.fetchLatestBlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("获取最新区块高度失败: %w", err)
	}
	e.setLatestBlock(latestOnChain)
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
	batchSize := uint64(20)
	if e.cfg.BatchSize > 0 {
		batchSize = e.cfg.BatchSize
	}
	// 每次最大扫描跨度限制为 20 个块，防止公共 RPC 报 "query returned more than 10000 results" 或超时
	toBlock := fromBlock + batchSize - 1
	if toBlock > safeBlock {
		toBlock = safeBlock
	}

	// 2. 查出当前正在等待收款的钱包地址（用于内存极速命中过滤）
	var activeWallets []model.WalletAddress
	_ = e.db.Where("UPPER(chain) = ? AND enabled = ?", e.Chain(), true).Find(&activeWallets).Error
	if len(activeWallets) == 0 {
		// 没有待监听地址，直接推进游标落库，避免白白拉取全网日志
		return e.commitProgress(ctx, toBlock)
	}
	walletMap := make(map[string]bool)
	for _, w := range activeWallets {
		walletMap[strings.ToLower(w.Address)] = true
	}
	// 3. 查询此区间内的 ERC20 Transfer 日志
	logs, err := e.getLogs(ctx, fromBlock, toBlock)
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

		transfer := model.ChainTransfer{
			Chain:         e.chain,
			TxHash:        l.TransactionHash,
			Contract:      strings.ToLower(l.Address),
			TargetAddress: targetAddress,
			RawValue:      amountBig.String(),
			BlockNumber:   l.BlockNumber,
			CreatedAt:     time.Now(),
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case transferChan <- transfer:
			log.Printf("[%s Scanner] 捕获充值: Tx=%s, To=%s, RawAmount=%s, Block=%d",
				e.Chain(), transfer.TxHash, transfer.TargetAddress, transfer.RawValue, transfer.BlockNumber)
		}
	}

	// 5. 游标必须事务落库持久化
	return e.commitProgress(ctx, toBlock)
}

// commitProgress 原子持久化游标
func (e *EvmScanner) commitProgress(ctx context.Context, toBlock uint64) error {
	err := e.db.WithContext(ctx).
		Model(&model.ScanProgress{}).
		Where("chain = ?", e.Chain()).
		Update("last_scanned_block", toBlock).Error

	if err != nil {
		return fmt.Errorf("持久化扫描游标失败: %w", err)
	}

	e.lastScannedBlock = toBlock
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
func (e *EvmScanner) fetchLatestBlockNumber(ctx context.Context) (uint64, error) {
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		ID:      1,
	}

	var resp jsonRPCResponse
	if err := e.callRPC(ctx, reqBody, &resp); err != nil {
		return 0, err
	}

	if resp.Error != nil {
		return 0, fmt.Errorf("rpc error [%d]: %s", resp.Error.Code, resp.Error.Message)
	}

	var hexBlock string
	if err := json.Unmarshal(resp.Result, &hexBlock); err != nil {
		return 0, fmt.Errorf("decode blockNumber hex failed: %w", err)
	}

	// 将 0x 开头的十六进制解析为 uint64
	cleanHex := strings.TrimPrefix(hexBlock, "0x")
	blockNum, err := strconv.ParseUint(cleanHex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse hex blockNumber (%s) failed: %w", hexBlock, err)
	}
	logger.Info("latest block num %d", blockNum)

	return blockNum, nil
}

// 2. 实现 getLogs: 调用 eth_getLogs
func (e *EvmScanner) getLogs(ctx context.Context, fromBlock, toBlock uint64) ([]evmLogItem, error) {
	filterParam := map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   fmt.Sprintf("0x%x", toBlock),
		"topics": []interface{}{
			ERC20TransferTopic, // 仅监听 Transfer(address,address,uint256)
		},
	}

	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "eth_getLogs",
		Params:  []interface{}{filterParam},
		ID:      2,
	}

	var resp jsonRPCResponse
	if err := e.callRPC(ctx, reqBody, &resp); err != nil {
		return nil, err
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error [%d]: %s", resp.Error.Code, resp.Error.Message)
	}

	var rawLogs []evmLogItem
	if err := json.Unmarshal(resp.Result, &rawLogs); err != nil {
		return nil, fmt.Errorf("decode getLogs result failed: %w", err)
	}

	for i := range rawLogs {
		cleanHex := strings.TrimPrefix(rawLogs[i].BlockNumberHex, "0x")
		rawLogs[i].BlockNumber, _ = strconv.ParseUint(cleanHex, 16, 64)
	}

	return rawLogs, nil
}

// 3. 通用 HTTP POST RPC 发送封装
func (e *EvmScanner) callRPC(ctx context.Context, reqData interface{}, out interface{}) error {
	payload, err := json.Marshal(reqData)
	if err != nil {
		return fmt.Errorf("marshal json-rpc request failed: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.rpcURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create http request failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// 复用嵌入在 BaseScanner 中的 client
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("execute http rpc request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rpc node returned http %d: %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}
