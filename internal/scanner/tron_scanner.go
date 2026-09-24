package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/logger"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"encoding/json"
	"fmt"

	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 1. 结构体扁平定义，各链自给自足
type TronScanner struct {
	db             *gorm.DB
	Cfg            config.ChainNodeConfig
	latestBlock    atomic.Uint64
	lastScanOK     atomic.Int64
	client         *http.Client
	tokensMu       sync.RWMutex
	tokens         map[string]model.TokenSpec
	tsMu           sync.RWMutex
	lastTimestamps map[string]int64 // address -> last seen timestamp in milliseconds
}

// 预置已知的 TRON 知名代币（防空投垃圾币、投毒币）
var defaultTronTokens = []model.TokenSpec{
	{
		Symbol:     "USDT",
		Identifier: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		Decimals:   6,
		IsNative:   false,
	},
	{
		Symbol:     "USDC",
		Identifier: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8",
		Decimals:   6,
		IsNative:   false,
	},
}

// SupportedTokens implements [Scanner].
func (t *TronScanner) SupportedTokens() []model.TokenSpec {
	t.tokensMu.RLock()
	defer t.tokensMu.RUnlock()

	// 返回 map 的值切片（即 []model.TokenSpec）
	result := make([]model.TokenSpec, 0, len(t.tokens))
	for _, spec := range t.tokens {
		result = append(result, spec)
	}
	return result
}

type TronTRC20Tx struct {
	TransactionID string `json:"transaction_id"`
	TokenInfo     struct {
		Symbol   model.Token `json:"symbol"`
		Address  string      `json:"address"`
		Decimals int32       `json:"decimals"`
		Name     string      `json:"name"`
	} `json:"token_info"`
	BlockTimestamp int64  `json:"block_timestamp"`
	BlockNumber    uint64 `json:"block_number"`
	From           string `json:"from"`
	To             string `json:"to"`
	Type           string `json:"type"`
	Value          string `json:"value"`
}

type TronTRC20Resp struct {
	Data    []TronTRC20Tx `json:"data"`
	Success bool          `json:"success"`
}

func NewTronScanner(db *gorm.DB, nodeCfg *config.ChainNodeConfig) (Scanner, error) {

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	scanner := &TronScanner{
		db:             db,
		Cfg:            *nodeCfg,
		client:         httpClient,
		tokens:         make(map[string]model.TokenSpec),
		lastTimestamps: make(map[string]int64),
	}
	// 1. 装载 TRX 原生代币
	scanner.tokens["TRX"] = model.TokenSpec{
		Symbol:     "TRX",
		Identifier: "",
		Decimals:   6,
		IsNative:   true,
	}
	// 2. 装配系统预置的知名 TRC20 代币
	for _, spec := range defaultTronTokens {
		scanner.tokens[spec.Identifier] = spec
	}

	// 3. 外部注入：从 Cfg.Tokens 读取用户在 YAML 中自定义或覆盖的代币
	for _, t := range nodeCfg.Tokens {
		trimmedAddr := strings.TrimSpace(t.Identifier)
		if trimmedAddr == "" {
			continue
		}
		scanner.tokens[trimmedAddr] = model.TokenSpec{
			Symbol:     t.Symbol,
			Identifier: trimmedAddr, // TRON Base58 严格区分大小写，严禁 ToLower
			Decimals:   t.Decimals,
			IsNative:   false,
		}
	}
	return scanner, nil
}

func (t *TronScanner) Chain() model.Chain {
	return model.ChainTron
}

func (t *TronScanner) GetLatestBlock() uint64 {
	return t.latestBlock.Load()
}

func (t *TronScanner) IsHealthy() bool {
	last := t.lastScanOK.Load()
	if last == 0 {
		return false
	}
	interval := time.Duration(t.Cfg.ScanIntervalSec) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return time.Since(time.Unix(last, 0)) <= maxScannerHealthAge(interval)
}

func (t *TronScanner) setLatestBlock(blk uint64) {
	for {
		current := t.latestBlock.Load()
		if blk <= current {
			return
		}
		if t.latestBlock.CompareAndSwap(current, blk) {
			return
		}
	}
}

// Start begins periodic TRON chain scanning
func (t *TronScanner) Start(ctx context.Context, transferChan chan<- model.ChainTransfer) error {
	interval := time.Duration(t.Cfg.ScanIntervalSec) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second // 兜底安全默认值
	}
	logger.Info("TRON scanner started", "chain", model.ChainTron, "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		t.scanOnce(ctx, transferChan)
		select {
		case <-ctx.Done():
			logger.Info("TRON scanner stopped", "chain", model.ChainTron)
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (t *TronScanner) scanOnce(ctx context.Context, transferChan chan<- model.ChainTransfer) {
	if err := t.updateLatestBlock(ctx); err != nil {
		metrics.RecordScanError(string(model.ChainTron), "tron")
		return
	}
	t.scanActiveTronAddresses(ctx, transferChan)
	t.lastScanOK.Store(time.Now().Unix())
}

func (t *TronScanner) updateLatestBlock(ctx context.Context) error {
	url := fmt.Sprintf("%s/v1/blocks/latest", t.Cfg.RPCURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if t.Cfg.APIKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", t.Cfg.APIKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("tron latest block API error: status %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	var tronResp struct {
		Data []struct {
			BlockHeader struct {
				RawData struct {
					Number    int64 `json:"number"`
					Timestamp int64 `json:"timestamp"`
				} `json:"raw_data"`
			} `json:"block_header"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tronResp); err != nil {
		return fmt.Errorf("解析区块 JSON 失败: %w", err)
	}
	if len(tronResp.Data) == 0 {
		return fmt.Errorf("RPC 返回的区块列表为空")
	}
	rawNum := tronResp.Data[0].BlockHeader.RawData.Number
	if rawNum <= 0 {
		return fmt.Errorf("非法区块高度: %d", rawNum)
	}
	t.setLatestBlock(uint64(rawNum))
	return nil
}

func (t *TronScanner) scanActiveTronAddresses(ctx context.Context, transferChan chan<- model.ChainTransfer) {
	if t.db == nil {
		return
	}

	// 常态化监控平台所有已启用的 TRON 收款钱包地址池，不依赖临时 WATCHING 意向
	var activeWallets []model.WalletAddress
	err := t.db.WithContext(ctx).
		Where("(chain = ? OR UPPER(chain) = ?) AND enabled = ?", model.ChainTron, "TRON", true).
		Find(&activeWallets).Error
	if err != nil || len(activeWallets) == 0 {
		return
	}

	// Address deduplication
	addressMap := make(map[string]bool)
	for _, w := range activeWallets {
		addr := strings.TrimSpace(w.Address)
		if addr != "" {
			addressMap[addr] = true
		}
	}

	baseURL := strings.TrimRight(t.Cfg.RPCURL, "/")

	// 3. 轮询各地址最近 TRC20 交易
	for addr := range addressMap {
		select {
		case <-ctx.Done():
			return
		default:
		}

		t.scanSingleAddress(ctx, baseURL, addr, transferChan)
	}
}
func (t *TronScanner) scanSingleAddress(ctx context.Context, baseURL, addr string, transferChan chan<- model.ChainTransfer) {
	t.tsMu.RLock()
	lastTs := t.lastTimestamps[addr]
	t.tsMu.RUnlock()

	// 若尚未缓存水位线，尝试从 DB 恢复
	if lastTs == 0 && t.db != nil {
		var lastTransfer model.ChainTransfer
		if err := t.db.WithContext(ctx).
			Where("chain = ? AND target_address = ?", model.ChainTron, addr).
			Order("block_timestamp DESC").First(&lastTransfer).Error; err == nil && lastTransfer.BlockTimestamp > 0 {
			lastTs = lastTransfer.BlockTimestamp * 1000 // 转为毫秒
			t.tsMu.Lock()
			t.lastTimestamps[addr] = lastTs
			t.tsMu.Unlock()
		} else {
			// 冷启动保护：若无任何历史记录，从当前时间前 30 分钟起开始增量扫描，防止拉取远古陈旧流水
			lastTs = time.Now().Add(-30 * time.Minute).UnixMilli()
			t.tsMu.Lock()
			t.lastTimestamps[addr] = lastTs
			t.tsMu.Unlock()
		}
	}

	url := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?limit=50&only_to=true", baseURL, addr)
	if lastTs > 0 {
		url = fmt.Sprintf("%s&min_timestamp=%d", url, lastTs)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	if t.Cfg.APIKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", t.Cfg.APIKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		logger.Warn("TronGrid API rate limited; backing off", "chain", model.ChainTron, "status", http.StatusTooManyRequests)
		metrics.RecordScanError(string(model.ChainTron), "rate_limit")
		return
	}

	if resp.StatusCode != http.StatusOK {
		return
	}

	var trcResp TronTRC20Resp
	if err := json.NewDecoder(resp.Body).Decode(&trcResp); err != nil || !trcResp.Success {
		return
	}

	var maxTs int64 = lastTs
	txCountMap := make(map[string]int64)

	for _, tx := range trcResp.Data {
		if !strings.EqualFold(tx.To, addr) {
			continue
		}

		if tx.BlockTimestamp > maxTs {
			maxTs = tx.BlockTimestamp
		}

		// 严格核验 TRC-20 合约地址（防假币 / 投毒币攻击）
		contractAddr := strings.TrimSpace(tx.TokenInfo.Address)
		t.tokensMu.RLock()
		spec, isKnown := t.tokens[contractAddr]
		t.tokensMu.RUnlock()
		if !isKnown {
			logger.Error("non-whitelisted or counterfeit token blocked", "chain", model.ChainTron, "contract", contractAddr, "token", tx.TokenInfo.Symbol, "tx_hash", tx.TransactionID, "target_address", tx.To)
			continue
		}

		decimals := int32(spec.Decimals)
		if decimals <= 0 {
			decimals = tx.TokenInfo.Decimals
			if decimals <= 0 {
				decimals = 6
			}
		}

		// 使用 decimal 无损高精度计算，防止大额资金丢精度
		valDec, err := decimal.NewFromString(tx.Value)
		if err != nil {
			continue
		}
		amount := valDec.Div(decimal.New(1, decimals))

		logIdx := txCountMap[tx.TransactionID]
		txCountMap[tx.TransactionID]++

		transfer := model.ChainTransfer{
			TxHash:         tx.TransactionID,
			Chain:          model.ChainTron,
			LogIndex:       logIdx,
			Contract:       contractAddr,
			FromAddress:    tx.From,
			TargetAddress:  tx.To,
			Amount:         amount,
			RawValue:       tx.Value,
			Token:          spec.Symbol, // 使用白名单中权威核验的 Symbol
			Decimals:       uint8(decimals),
			BlockNumber:    tx.BlockNumber,
			BlockTimestamp: tx.BlockTimestamp / 1000,
		}
		select {
		case <-ctx.Done():
			return
		case transferChan <- transfer:
			metrics.RecordTransferCaptured(string(model.ChainTron), string(transfer.Token))
		}
	}

	if maxTs > lastTs {
		t.tsMu.Lock()
		t.lastTimestamps[addr] = maxTs
		t.tsMu.Unlock()
	}
}

// VerifyTransaction 二次核验 TRON 交易是否在主链成功确认且执行成功
func (t *TronScanner) VerifyTransaction(ctx context.Context, txHash string, blockNumber uint64) (bool, error) {
	url := fmt.Sprintf("%s/wallet/gettransactionbyid", strings.TrimRight(t.Cfg.RPCURL, "/"))
	reqBody := fmt.Sprintf(`{"value":"%s"}`, txHash)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(reqBody))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Cfg.APIKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", t.Cfg.APIKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("tron api error: status %d", resp.StatusCode)
	}

	var txInfo struct {
		TxID string `json:"txID"`
		Ret  []struct {
			ContractRet string `json:"contractRet"`
		} `json:"ret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&txInfo); err != nil {
		return false, err
	}
	if txInfo.TxID == "" {
		return false, nil // 交易不存在或已遭孤儿块回滚
	}
	if len(txInfo.Ret) > 0 && txInfo.Ret[0].ContractRet != "SUCCESS" {
		return false, nil // 交易未执行成功
	}
	return true, nil
}

// SimulateTransfer 模拟触发一笔 TRC-20 充值事件（仅供本地开发联调、单测或 Webhook 验证）
func (t *TronScanner) SimulateTransfer(ctx context.Context, toAddress string, amount decimal.Decimal, token model.Token, transferChan chan<- model.ChainTransfer) (string, error) {
	if token == "" {
		token = model.TokenUSDT
	}

	target := strings.TrimSpace(toAddress)
	if target == "" {
		return "", fmt.Errorf("toAddress 不能为空")
	}

	// 1. 生成可读性好且唯一的模拟 TxHash
	txHash := fmt.Sprintf("mock_tron_%d_%04d", time.Now().UnixNano(), rand.Intn(10000))

	// 2. 模拟真实高度：让交易块高落后当前最新块几个高度，模拟已确认或待确认状态
	latest := t.GetLatestBlock()
	if latest == 0 {
		latest = 62891024
		t.setLatestBlock(latest + 12) // 人工模拟给 12 个确认差值
	}

	transfer := model.ChainTransfer{
		TxHash:         txHash,
		Chain:          model.ChainTron,
		FromAddress:    "TFromSimulatedWalletAddress1234567890",
		TargetAddress:  target,
		Amount:         amount,
		Token:          token,
		BlockNumber:    latest,
		BlockTimestamp: time.Now().Unix(), // 统一采用秒级时间戳
	}

	// 3. 安全推送，带超时/Context 防死锁
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case transferChan <- transfer:
		logger.Info("mock chain transfer generated", "chain", model.ChainTron, "tx_hash", txHash, "target_address", target, "amount", amount.StringFixed(4), "token", token)
		return txHash, nil
	case <-time.After(3 * time.Second):
		return "", fmt.Errorf("推送模拟事件超时，transferChan 已满")
	}
}
