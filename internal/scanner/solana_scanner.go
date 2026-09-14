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

var DefaultSolanaTokens = []model.TokenSpec{
	{Identifier: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Symbol: model.TokenUSDT, Decimals: 6},
	{Identifier: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Symbol: model.TokenUSDC, Decimals: 6},
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
	tokensMu       sync.RWMutex
	tokens         map[string]model.TokenSpec
	ataMu          sync.RWMutex
	tokenAccounts  map[string][]string // ownerAddress -> []tokenAccountPubkeys
}

func NewSolanaScanner(db *gorm.DB, cfg *config.ChainNodeConfig) (Scanner, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	scanner := &SolanaScanner{
		db:             db,
		cfg:            cfg,
		client:         client,
		rpcURL:         cfg.RPCURL,
		lastSignatures: make(map[string]string),
		tokens:         make(map[string]model.TokenSpec),
		tokenAccounts:  make(map[string][]string),
	}
	for _, spec := range DefaultSolanaTokens {
		scanner.tokens[spec.Identifier] = spec
	}

	// 3. 外部注入：从 Cfg.Tokens 读取用户在 YAML 中自定义或覆盖的代币
	for _, t := range cfg.Tokens {
		trimmedAddr := strings.TrimSpace(t.Identifier)
		if trimmedAddr == "" {
			continue
		}
		scanner.tokens[trimmedAddr] = model.TokenSpec{
			Symbol:     t.Symbol,
			Identifier: trimmedAddr, // Solana Base58 严格区分大小写，严禁 ToLower
			Decimals:   t.Decimals,
			IsNative:   false,
		}
	}
	return scanner, nil
}
// SupportedTokens implements [Scanner].
func (s *SolanaScanner) SupportedTokens() []model.TokenSpec {
	s.tokensMu.RLock()
	defer s.tokensMu.RUnlock()
	result := make([]model.TokenSpec, 0, len(s.tokens))
	for _, spec := range s.tokens {
		result = append(result, spec)
	}
	return result
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
	// 无论是否有活跃监听地址，每个扫描周期始终必须刷新链上最新高度，防止无 WATCHING 订单时 CONFIRMING 订单确认数检测卡死
	s.refreshLatestSlot(ctx)

	addresses := s.getActiveTargetAddresses()
	if len(addresses) == 0 {
		return
	}

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
	// 原子 CAS 更新最新 Slot
	for {
		current := s.latestBlock.Load()
		if slot <= current {
			break
		}
		if s.latestBlock.CompareAndSwap(current, slot) {
			break
		}
	}
	metrics.RecordScanBlock("SOLANA", s.latestBlock.Load(), slot)
	return nil
}

// 2. 独立提取：常态化监控平台所有已启用的 SOLANA 收款钱包地址池
func (s *SolanaScanner) getActiveTargetAddresses() []string {
	if s.db == nil {
		return nil
	}
	var addrs []string
	s.db.Model(&model.WalletAddress{}).
		Where("(chain = ? OR UPPER(chain) = ?) AND enabled = ?", model.ChainSolana, "SOLANA", true).
		Distinct().
		Pluck("address", &addrs)
	return addrs
}

func (s *SolanaScanner) getAddressesToScan(ctx context.Context, owner string) []string {
	addrs := []string{owner}
	s.ataMu.RLock()
	cached, exists := s.tokenAccounts[owner]
	s.ataMu.RUnlock()

	if exists && len(cached) > 0 {
		return append(addrs, cached...)
	}

	tas, err := s.getTokenAccountsByOwner(ctx, owner)
	if err == nil && len(tas) > 0 {
		s.ataMu.Lock()
		s.tokenAccounts[owner] = tas
		s.ataMu.Unlock()
		addrs = append(addrs, tas...)
	}
	return addrs
}

func (s *SolanaScanner) getTokenAccountsByOwner(ctx context.Context, owner string) ([]string, error) {
	params := []interface{}{
		owner,
		map[string]interface{}{
			"programId": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
		},
		map[string]interface{}{
			"encoding": "jsonParsed",
		},
	}
	res, err := s.callRPC(ctx, "getTokenAccountsByOwner", params)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Value []struct {
			Pubkey string `json:"pubkey"`
		} `json:"value"`
	}
	if err := json.Unmarshal(res, &resp); err != nil {
		return nil, err
	}
	var accounts []string
	for _, v := range resp.Value {
		if v.Pubkey != "" {
			accounts = append(accounts, v.Pubkey)
		}
	}
	return accounts, nil
}

// 3. 独立处理单个地址及其关联 Token 账户的扫描与游标更新
func (s *SolanaScanner) scanSingleAddress(ctx context.Context, addr string, ch chan<- model.ChainTransfer) {
	queryAddrs := s.getAddressesToScan(ctx, addr)
	for _, qAddr := range queryAddrs {
		select {
		case <-ctx.Done():
			return
		default:
			s.scanAddressStream(ctx, qAddr, addr, ch)
		}
	}
}

func (s *SolanaScanner) scanAddressStream(ctx context.Context, queryAddr, targetOwner string, ch chan<- model.ChainTransfer) {
	// 1. 从内存锁中读取上次记录的游标 (string)
	s.sigMu.RLock()
	lastSig := s.lastSignatures[queryAddr]
	s.sigMu.RUnlock()

	// 2. 若内存游标为空且存在 DB，尝试从历史入库流水恢复游标
	if lastSig == "" && s.db != nil {
		var lastTransfer model.ChainTransfer
		if err := s.db.WithContext(ctx).
			Where("chain = ? AND (target_address = ? OR target_address = ?)", model.ChainSolana, targetOwner, queryAddr).
			Order("id DESC").First(&lastTransfer).Error; err == nil && lastTransfer.TxHash != "" {
			lastSig = lastTransfer.TxHash
			s.sigMu.Lock()
			s.lastSignatures[queryAddr] = lastSig
			s.sigMu.Unlock()
		}
	}

	// 3. 传入 string 类型的游标，增量拉取新签名（支持分页，避免突发 > 50 笔漏单）
	var allSigs []solSig
	before := ""
	for {
		batch, err := s.getSignaturesForAddress(ctx, queryAddr, lastSig, before)
		if err != nil {
			metrics.RecordScanError("SOLANA", "solana")
			log.Printf("[SolanaScanner] 获取地址 %s 签名失败: %v", queryAddr, err)
			return
		}
		if len(batch) == 0 {
			break
		}
		allSigs = append(allSigs, batch...)
		// 若 lastSig 为空（冷启动）或本批未满 50 条，或单次已拉取 200 条，停止防过载
		if lastSig == "" || len(batch) < 50 || len(allSigs) >= 200 {
			break
		}
		before = batch[len(batch)-1].Signature
	}

	if len(allSigs) == 0 {
		return
	}

	// 4. 冷启动保护：若此前没有任何游标（全新地址首次监听）
	var cutoff int64
	if lastSig == "" {
		// 先将最新签名置为游标水位线，确保后续周期只拉增量
		s.sigMu.Lock()
		s.lastSignatures[queryAddr] = allSigs[0].Signature
		s.sigMu.Unlock()

		// 检查该地址是否有活跃订单创建时间限制
		if s.db != nil {
			var intent model.PaymentIntent
			if err := s.db.WithContext(ctx).
				Where("status = ? AND UPPER(chain) = ? AND (target_address = ? OR target_address = ?)", model.StatusWatching, "SOLANA", targetOwner, queryAddr).
				Order("created_at ASC").First(&intent).Error; err == nil && !intent.CreatedAt.IsZero() {
				cutoff = intent.CreatedAt.Add(-60 * time.Second).Unix()
			}
		}
	}

	// 5. 按时间升序（从旧到新，即反向遍历）处理交易，并逐笔安全推进游标
	for i := len(allSigs) - 1; i >= 0; i-- {
		select {
		case <-ctx.Done():
			return
		default:
		}

		sigInfo := allSigs[i]
		if sigInfo.Err != nil {
			// 失败链上交易，跳过并记录游标
			s.sigMu.Lock()
			s.lastSignatures[queryAddr] = sigInfo.Signature
			s.sigMu.Unlock()
			continue
		}

		if err := s.processTransaction(ctx, sigInfo.Signature, targetOwner, cutoff, ch); err != nil {
			log.Printf("[SolanaScanner] 解析处理交易 %s 失败: %v", sigInfo.Signature, err)
			return // 发生错误停止继续推进，保留未处理签名以便下一周期重试
		}

		s.sigMu.Lock()
		s.lastSignatures[queryAddr] = sigInfo.Signature
		s.sigMu.Unlock()
	}
}

// 4. 独立提取：纯交易解析逻辑，方便编写单元测试
func (s *SolanaScanner) processTransaction(ctx context.Context, sig, targetAddr string, cutoff int64, ch chan<- model.ChainTransfer) error {
	tx, err := s.getTransaction(ctx, sig)
	if err != nil {
		metrics.RecordScanError("SOLANA", "solana")
		return err
	}
	if tx == nil {
		return nil
	}

	if cutoff > 0 && tx.BlockTime > 0 && tx.BlockTime < cutoff {
		// 属于订单创建前的历史陈旧交易，跳过不推入队列
		return nil
	}

	// 解析出所有的入账事件
	transfers := s.extractIncomingTransfers(tx, targetAddr)

	for _, tr := range transfers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- tr:
			metrics.RecordTransferCaptured(string(model.ChainSolana), string(tr.Token))
		}
	}
	return nil
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

	// 获取交易哈希与发起方
	txHash := ""
	if len(tx.Transaction.Signatures) > 0 {
		txHash = tx.Transaction.Signatures[0]
	}

	fromAddress := "Unknown"
	if tx.Transaction.Message != nil {
		for _, ak := range tx.Transaction.Message.AccountKeys {
			if keyMap, ok := ak.(map[string]interface{}); ok {
				if isSigner, _ := keyMap["signer"].(bool); isSigner {
					if pubkey, _ := keyMap["pubkey"].(string); pubkey != "" {
						fromAddress = pubkey
						break
					}
				}
			}
		}
	}

	// 统一采用秒级时间戳，若 Solana 节点出块时间未就绪，兜底当前本地时间
	blockTs := tx.BlockTime
	if blockTs <= 0 {
		blockTs = time.Now().Unix()
	}

	s.tokensMu.RLock()
	defer s.tokensMu.RUnlock()

	// 2. 遍历 PostTokenBalances，只关注属于目标地址 targetAddr 的账户
	for _, post := range tx.Meta.PostTokenBalances {
		// 校验当前 Token 账户的所有者是否是目标地址 (Solana Base58 严格区分大小写)
		if strings.TrimSpace(post.Owner) != targetAddr {
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
			if meta, ok := s.tokens[post.Mint]; ok {
				tokenSymbol = meta.Symbol
			}

			// 计算本次转账的无损原始链上最小单位数值 (例如 15 USDT, 精度 6 -> 15000000)
			decimals := post.UiTokenAmount.Decimals
			rawVal := diff.Mul(decimal.New(1, int32(decimals))).Floor().String()

			// 直接构造完整的 ChainTransfer
			transfer := model.ChainTransfer{
				TxHash:         txHash,
				Chain:          model.ChainSolana,
				LogIndex:       int64(post.AccountIndex), // 以 accountIndex 作为 tx 内唯一索引
				Contract:       post.Mint,
				FromAddress:    fromAddress,
				TargetAddress:  targetAddr,
				Amount:         diff,
				RawValue:       rawVal,
				Token:          tokenSymbol,
				BlockNumber:    tx.Slot,
				BlockTimestamp: blockTs,
				Decimals:       uint8(decimals),
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
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
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

	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		return nil, fmt.Errorf("RPC HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf[:n])))
	}

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

func (s *SolanaScanner) getSignaturesForAddress(ctx context.Context, address, until string, before ...string) ([]solSig, error) {
	params := []interface{}{address}
	opts := map[string]interface{}{"limit": 50}
	if until != "" {
		opts["until"] = until
	}
	if len(before) > 0 && before[0] != "" {
		opts["before"] = before[0]
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
		Message    *struct {
			AccountKeys []interface{} `json:"accountKeys"`
		} `json:"message,omitempty"`
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
