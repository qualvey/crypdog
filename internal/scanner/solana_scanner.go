package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/metrics"
	"crypdog/internal/model"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var knownSolanaTokens = map[string]TokenMeta{
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": {Symbol: model.TokenUSDT, Decimals: 6},
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": {Symbol: model.TokenUSDC, Decimals: 6},
}

type SolanaScanner struct {
	// 1. 基础依赖
	db          *gorm.DB                // 进度落库防丢、查询监听地址列表
	cfg         *config.ChainNodeConfig // 统一配置 (批次大小、扫描间隔、重试次数等)
	client      *http.Client            // Solana 专用 RPC Client (或标准的 *http.Client)
	latestBlock atomic.Uint64           // 保证并发安全
	// 2. 接口实现所需状态 (实现 Scanner.GetLatestBlock())
	latestSlot atomic.Uint64 // 记录当前全网最新 Slot 高度
	rpcURL     string
	// 3. 业务进度位点 (按监控地址维护 Signature 锚点)
	sigMu          sync.RWMutex      // 保证 Map 并发读写安全
	lastSignatures map[string]string // targetAddress -> last known tx signature
}

func NewSolanaScanner(db *gorm.DB, cfg *config.ChainNodeConfig) Scanner {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	return &SolanaScanner{
		db:             db,
		cfg:            cfg,
		client:         client,
		rpcURL:         cfg.RPCURL,
		lastSignatures: make(map[string]string),
	}
}

func (s *SolanaScanner) Chain() model.Chain {
	return model.ChainSolana
}

// 满足 Scanner 接口
func (s *SolanaScanner) GetLatestBlock() uint64 {
	return s.latestBlock.Load()
}

// 最佳工程实践：对于这种 I/O 密集型轮询任务，通常更推荐显式延时（time.Sleep 或每次执行完后 Reset(timer)），
// 确保两次扫描之间始终有固定的休息间隔，或者引入并发防重入锁。
func (s *SolanaScanner) Start(ctx context.Context, transferChan chan<- model.ChainTransfer) error {
	interval := 3 * time.Second
	if s.cfg != nil && s.cfg.ScanIntervalSec > 0 {
		interval = time.Duration(s.cfg.ScanIntervalSec) * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop() // 1. 必加：防止 timer 泄露
	log.Printf("[SolanaScanner] Started SOLANA scanner daemon (Interval: %v)", interval)

	s.scanActiveAddresses(ctx, transferChan)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[SolanaScanner] Stopped: %v", ctx.Err())
			return ctx.Err() // 2. 惯用法：返回 context 错误
		case <-ticker.C:
			s.scanActiveAddresses(ctx, transferChan)
		}
	}
}

func (s *SolanaScanner) scanActiveAddresses(ctx context.Context, transferChan chan<- model.ChainTransfer) {
	addresses := s.getActiveTargetAddresses()
	if len(addresses) == 0 {
		return
	}

	s.refreshLatestSlot(ctx)

	for _, addr := range addresses {
		select {
		case <-ctx.Done():
			return
		default:
			s.scanSingleAddress(ctx, addr, transferChan)
		}
	}
}
func (s *SolanaScanner) refreshLatestSlot(ctx context.Context) error {
	res, err := s.callRPC(ctx, "getSlot", []interface{}{})
	if err != nil {
		metrics.RecordScanError("SOLANA", "solana")
		return err
	}
	var slot uint64
	if err := json.Unmarshal(res, &slot); err != nil {
		metrics.RecordScanError("SOLANA", "solana")
		return err
	}
	// 原子存储更新
	if slot > s.latestBlock.Load() {
		s.latestBlock.Store(slot)
	}
	metrics.RecordScanBlock("SOLANA", s.latestBlock.Load(), slot)
	return nil
}

// 2. 独立提取：只查询并去重目标地址
func (s *SolanaScanner) getActiveTargetAddresses() []string {
	var addrs []string
	s.db.Model(&model.PaymentIntent{}).
		Where("status = ? AND UPPER(chain) = ?", model.StatusWatching, "SOLANA").
		Distinct().
		Pluck("target_address", &addrs)
	return addrs
}

// 3. 独立处理单个地址的扫描与游标更新
func (s *SolanaScanner) scanSingleAddress(ctx context.Context, addr string, ch chan<- model.ChainTransfer) {
	// 1. 从内存锁中读取上次记录的游标 (string)
	s.sigMu.RLock()
	lastSig := s.lastSignatures[addr]
	s.sigMu.RUnlock()

	// 2. 传入 string 类型的游标，只增量拉取新签名
	sigs, err := s.getSignaturesForAddress(ctx, addr, lastSig)
	if err != nil || len(sigs) == 0 {
		return
	}

	// 3. 更新最新水位线（sigs[0] 是最新的一笔交易）
	s.sigMu.Lock()
	s.lastSignatures[addr] = sigs[0].Signature
	s.sigMu.Unlock()

	// 4. 处理交易
	for _, sigInfo := range sigs {
		if sigInfo.Err != nil {
			continue
		}
		s.processTransaction(ctx, sigInfo.Signature, addr, ch)
	}
}

// 4. 独立提取：纯交易解析逻辑，方便编写单元测试
func (s *SolanaScanner) processTransaction(ctx context.Context, sig, targetAddr string, ch chan<- model.ChainTransfer) {
	tx, err := s.getTransaction(ctx, sig)
	if err != nil || tx == nil {
		return
	}

	// 解析出所有的入账事件
	transfers := s.extractIncomingTransfers(tx, targetAddr)

	for _, tr := range transfers {
		select {
		case <-ctx.Done():
			return
		case ch <- tr:
		}
	}
}

func (s *SolanaScanner) extractIncomingTransfers(tx *solTx, targetAddr string) []model.ChainTransfer {
	if tx == nil {
		return nil
	}

	targetAddr = strings.TrimSpace(targetAddr)

	// 1. 将 PreTokenBalances 按 (accountIndex) 建立快速索引
	preMap := make(map[int]decimal.Decimal, len(tx.Meta.PreTokenBalances))
	for _, pre := range tx.Meta.PreTokenBalances {
		// 优先使用 UiAmountString 保证绝对精度
		if val, err := decimal.NewFromString(pre.UiTokenAmount.UiAmountString); err == nil {
			preMap[pre.AccountIndex] = val
		}
	}

	var transfers []model.ChainTransfer

	// 获取交易哈希
	txHash := ""
	if len(tx.Transaction.Signatures) > 0 {
		txHash = tx.Transaction.Signatures[0]
	}

	// 2. 遍历 PostTokenBalances，只关注属于目标地址 targetAddr 的账户
	for _, post := range tx.Meta.PostTokenBalances {
		// 校验当前 Token 账户的所有者是否是目标地址
		if !strings.EqualFold(strings.TrimSpace(post.Owner), targetAddr) {
			continue
		}

		postAmount, err := decimal.NewFromString(post.UiTokenAmount.UiAmountString)
		if err != nil {
			continue
		}

		// 如果 pre 中没有记录，说明这是第一笔入账（从无到有），pre 默认为 0
		preAmount := preMap[post.AccountIndex] // 默认 decimal.Zero

		// 计算增量：Post - Pre
		diff := postAmount.Sub(preAmount)

		// 只有余额增加，才构成充值转账
		if diff.GreaterThan(decimal.Zero) {
			// 映射代币 Symbol（如根据 Mint 合约映射为 USDT / USDC）
			tokenSymbol := model.Token(post.Mint)
			if meta, ok := knownSolanaTokens[post.Mint]; ok {
				tokenSymbol = meta.Symbol
			}

			// 直接构造完整的 ChainTransfer
			transfer := model.ChainTransfer{
				TxHash:         txHash,
				Chain:          model.ChainSolana,
				LogIndex:       int64(post.AccountIndex), // 以 accountIndex 作为 tx 内唯一索引
				Contract:       post.Mint,
				FromAddress:    "Unknown",
				TargetAddress:  targetAddr,
				Amount:         diff,
				RawValue:       post.UiTokenAmount.Amount, // 原始大整数
				Token:          tokenSymbol,
				BlockNumber:    tx.Slot,
				BlockTimestamp: tx.BlockTime * 1000,
			}
			transfers = append(transfers, transfer)
		}
	}

	return transfers
}

// simulate JSON-RPC requests
type rpcReq struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcRes struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcErr         `json:"error,omitempty"`
}
type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *SolanaScanner) callRPC(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	reqBody := rpcReq{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", s.rpcURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res rpcRes
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("RPC error: %s", res.Error.Message)
	}
	return res.Result, nil
}

func (s *SolanaScanner) getSlot(ctx context.Context) (uint64, error) {
	res, err := s.callRPC(ctx, "getSlot", []interface{}{})
	if err != nil {
		return 0, err
	}
	var slot uint64
	if err := json.Unmarshal(res, &slot); err != nil {
		return 0, err
	}
	return slot, nil
}

type solSig struct {
	Signature string      `json:"signature"`
	Slot      int64       `json:"slot"`
	Err       interface{} `json:"err"` // null if successful
}

func (s *SolanaScanner) getSignaturesForAddress(ctx context.Context, address, until string) ([]solSig, error) {
	params := []interface{}{address}
	opts := map[string]interface{}{"limit": 50}
	if until != "" {
		opts["until"] = until
	}
	params = append(params, opts)
	res, err := s.callRPC(ctx, "getSignaturesForAddress", params)
	if err != nil {
		return nil, err
	}
	var sigs []solSig
	if err := json.Unmarshal(res, &sigs); err != nil {
		return nil, err
	}
	return sigs, nil
}

type solTx struct {
	Transaction struct {
		Signatures []string `json:"signatures"`
	} `json:"transaction"`
	Slot      uint64 `json:"slot"`
	BlockTime int64  `json:"blockTime"`
	Meta      struct {
		PreTokenBalances  []tokenBalance `json:"preTokenBalances"`
		PostTokenBalances []tokenBalance `json:"postTokenBalances"`
	} `json:"meta"`
}

type tokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"` // 当 encoding 为 jsonParsed 时，Solana 会返回所属 owner 地址
	UiTokenAmount struct {
		Amount         string `json:"amount"`         // 最小单位的大整数字符串
		Decimals       int    `json:"decimals"`       // 精度
		UiAmountString string `json:"uiAmountString"` // 格式化后的小数字符串（推荐用这个转 Decimal）
	} `json:"uiTokenAmount"`
}

func (s *SolanaScanner) getTransaction(ctx context.Context, signature string) (*solTx, error) {
	params := []interface{}{signature, map[string]interface{}{
		"encoding":                       "jsonParsed",
		"maxSupportedTransactionVersion": 0,
	}}
	res, err := s.callRPC(ctx, "getTransaction", params)
	if err != nil {
		return nil, err
	}
	if string(res) == "null" {
		return nil, nil
	}
	var tx solTx
	if err := json.Unmarshal(res, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// func (s *SolanaScanner) SimulateTransfer(toAddress string, amount float64, token string, transferChan chan<- model.ChainTransfer) string {
// 	txHash := fmt.Sprintf("sim_sol_%x%d", rand.Uint64(), time.Now().UnixNano())
// 	if token == "" {
// 		token = "USDC"
// 	}
// 	latest := s.GetLatestBlock()
// 	if latest == 0 {
// 		latest = 280000000
// 		s.latestBlock = latest + 10
// 		s.mu.Unlock()
// 	}

// 	transfer := model.ChainTransfer{
// 		TxHash:         txHash,
// 		Chain:          "SOLANA",
// 		FromAddress:    "UnknownSimulated",
// 		TargetAddress:  strings.TrimSpace(toAddress),
// 		Amount:         amount,
// 		Token:          strings.ToUpper(token),
// 		BlockNumber:    latest,
// 		BlockTimestamp: time.Now().UnixMilli(),
// 	}

// 	transferChan <- transfer
// 	return txHash
// }
