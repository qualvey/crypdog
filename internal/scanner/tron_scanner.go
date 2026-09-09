package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/model"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 1. 结构体扁平定义，各链自给自足
type TronScanner struct {
	db          *gorm.DB
	Cfg         config.ChainNodeConfig
	latestBlock atomic.Uint64
	client      *http.Client
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

func NewTronScanner(db *gorm.DB, nodeCfg *config.ChainNodeConfig) Scanner {

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return &TronScanner{
		db:     db,
		Cfg:    *nodeCfg,
		client: httpClient,
	}
}

func (t *TronScanner) Chain() model.Chain {
	return model.ChainTron
}

func (t *TronScanner) GetLatestBlock() uint64 {
	return t.latestBlock.Load()
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
	log.Printf("[TronScanner] Started TRON blockchain scanner daemon (Interval: %ds)", interval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		t.scanOnce(ctx, transferChan)
		select {
		case <-ctx.Done():
			log.Printf("[TronScanner] Scanner stopped gracefully")
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (t *TronScanner) scanOnce(ctx context.Context, transferChan chan<- model.ChainTransfer) {
	t.updateLatestBlock(ctx)
	t.scanActiveTronAddresses(ctx, transferChan)
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
		return err
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

	var activeIntents []model.PaymentIntent
	err := t.db.WithContext(ctx).
		Where("status = ? AND UPPER(chain) = ?", model.StatusWatching, model.ChainTron).
		Find(&activeIntents).Error
	if err != nil || len(activeIntents) == 0 {
		return
	}

	// Address deduplication
	addressMap := make(map[string]bool)
	for _, intent := range activeIntents {
		addr := strings.TrimSpace(intent.TargetAddress)
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
	url := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?limit=20&only_to=true", baseURL, addr)
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

	if resp.StatusCode != http.StatusOK {
		return
	}

	var trcResp TronTRC20Resp
	if err := json.NewDecoder(resp.Body).Decode(&trcResp); err != nil || !trcResp.Success {
		return
	}

	for _, tx := range trcResp.Data {
		if !strings.EqualFold(tx.To, addr) {
			continue
		}

		decimals := tx.TokenInfo.Decimals
		if decimals == 0 {
			decimals = 6 // Default USDT decimals on TRON is 6
		}

		// 使用 decimal 无损高精度计算，防止大额资金丢精度
		valDec, err := decimal.NewFromString(tx.Value)
		if err != nil {
			continue
		}
		amount := valDec.Div(decimal.New(1, decimals)).InexactFloat64()

		transfer := model.ChainTransfer{
			TxHash:         tx.TransactionID,
			Chain:          model.ChainTron,
			FromAddress:    tx.From,
			TargetAddress:  tx.To,
			Amount:         amount,
			Token:          tx.TokenInfo.Symbol,
			BlockNumber:    tx.BlockNumber,
			BlockTimestamp: tx.BlockTimestamp / 1000,
		}
		select {
		case <-ctx.Done():
			return
		case transferChan <- transfer:
		}
	}
}

// SimulateTransfer 模拟触发一笔 TRC-20 充值事件（仅供本地开发联调、单测或 Webhook 验证）
func (t *TronScanner) SimulateTransfer(ctx context.Context, toAddress string, amount float64, token model.Token, transferChan chan<- model.ChainTransfer) (string, error) {
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
		log.Printf("[TronScanner] [MOCK] 已模拟充值事件: TxHash=%s, To=%s, Amount=%.4f %s",
			txHash, target, amount, token)
		return txHash, nil
	case <-time.After(3 * time.Second):
		return "", fmt.Errorf("推送模拟事件超时，transferChan 已满")
	}
}
