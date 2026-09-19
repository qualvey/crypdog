package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"crypdog/internal/queue"

	"github.com/shopspring/decimal"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type WebhookDispatcher interface {
	DispatchAsync(intent *model.PaymentIntent, txHash string, timestamp int64)
	DispatchEventAsync(event string, intent *model.PaymentIntent, txHash string, timestamp int64, confirmations, requiredConfirmations uint64)
}

type TxVerifierFunc func(ctx context.Context, chain model.Chain, txHash string, blockNumber uint64) (bool, error)

type MatcherEngine struct {
	db         *gorm.DB
	cfg        *config.Config
	dispatcher WebhookDispatcher
	txVerifier TxVerifierFunc
	mu         sync.Mutex
}

func NewMatcherEngine(db *gorm.DB, cfg *config.Config, dispatcher WebhookDispatcher, txVerifier ...TxVerifierFunc) *MatcherEngine {
	var verifier TxVerifierFunc
	if len(txVerifier) > 0 {
		verifier = txVerifier[0]
	}
	return &MatcherEngine{
		db:         db,
		cfg:        cfg,
		dispatcher: dispatcher,
		txVerifier: verifier,
	}
}

// ProcessTransfer 统一对外入口：只负责编排新扫描到的链上流水
func (e *MatcherEngine) ProcessTransfer(transfer model.ChainTransfer, currentBlockNumber uint64) error {
	if strings.TrimSpace(transfer.TxHash) == "" || transfer.Amount.LessThanOrEqual(decimal.Zero) {
		return errors.New("invalid transfer parameters")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. 流水落库与排重：兼容 Scanner 预落库架构
	// 若该流水已存在且已经成功撮合过订单 (matched_order_id != "")，则安全忽略
	var existing model.ChainTransfer
	err := e.db.Where("chain = ? AND tx_hash = ? AND log_index = ?", transfer.Chain, transfer.TxHash, transfer.LogIndex).First(&existing).Error
	if err == nil {
		if existing.MatchedOrderID != "" {
			// 已被撮合结算过的流水，幂等忽略
			return nil
		}
		// 数据库中已有流水（如 Scanner 提前持久化），但尚未撮合订单，使用数据库主键 ID 继续撮合
		transfer.ID = existing.ID
	} else {
		// 尚无此流水记录，落库保存
		if err := e.db.Create(&transfer).Error; err != nil {
			return fmt.Errorf("record transfer error: %w", err)
		}
	}

	return e.matchAndSettle(&transfer, currentBlockNumber)
}

// ReprocessUnmatchedTransfer 专门用于服务启动时或补偿扫描已落库但未撮合的历史流水
func (e *MatcherEngine) ReprocessUnmatchedTransfer(transfer model.ChainTransfer, currentBlockNumber uint64) error {
	if strings.TrimSpace(transfer.TxHash) == "" || transfer.Amount.LessThanOrEqual(decimal.Zero) {
		return errors.New("invalid transfer parameters")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	return e.matchAndSettle(&transfer, currentBlockNumber)
}

func (e *MatcherEngine) matchAndSettle(transfer *model.ChainTransfer, currentBlockNumber uint64) error {
	// 2. 查询候选订单 (仅 WATCHING 状态订单，CONFIRMING 不受新流水抢占)
	intents, err := e.queryCandidateIntents(*transfer)
	var matchedIntent *model.PaymentIntent
	if err == nil && len(intents) > 0 {
		matchedIntent = e.findMatchedIntent(intents, transfer.Amount)
	}

	// 2.1 迟到入账挽回 (Late Payment Recovery)：若活跃订单未匹配，查找近期超时的 EXPIRED 订单
	if matchedIntent == nil {
		expiredIntents, expErr := e.queryExpiredCandidateIntents(*transfer)
		if expErr == nil && len(expiredIntents) > 0 {
			matchedIntent = e.findMatchedIntent(expiredIntents, transfer.Amount)
			if matchedIntent != nil {
				log.Printf("[Matcher] ⏰ 捕获超时迟到充值订单: OrderID=%s, IntentID=%s, Tx=%s, Amount=%s",
					matchedIntent.OrderID, matchedIntent.ID, transfer.TxHash, transfer.Amount.String())
			}
		}
	}

	if matchedIntent == nil {
		metrics.RecordTransferMatch(string(transfer.Chain), false)
		log.Printf("[Matcher] 未找到匹配候选意向: Chain=%s, Token=%s, To=%s, Amount=%s, Tx=%s",
			transfer.Chain, transfer.Token, transfer.TargetAddress, transfer.Amount.String(), transfer.TxHash)
		return nil
	}

	metrics.RecordTransferMatch(string(transfer.Chain), true)
	log.Printf("[Matcher] 🎉 成功匹配订单: OrderID=%s, IntentID=%s, Tx=%s, Amount=%s",
		matchedIntent.OrderID, matchedIntent.ID, transfer.TxHash, transfer.Amount.String())

	// 4. 状态推进与结算（事务 + 触发通知）
	return e.settle(matchedIntent, transfer, currentBlockNumber)
}
func (e *MatcherEngine) recordTransferIfNotExists(transfer *model.ChainTransfer) (bool, error) {
	// 依赖 DB 复合唯一索引: idx_chain_tx_log (chain, tx_hash, log_index)
	result := e.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "chain"},
			{Name: "tx_hash"},
			{Name: "log_index"},
		},
		DoNothing: true, // 遇到冲突静默跳过
	}).Create(transfer)

	if result.Error != nil {
		return false, result.Error
	}

	// RowsAffected == 0 说明发生冲突，记录已存在
	if result.RowsAffected == 0 {
		return false, nil
	}

	return true, nil
}
func (e *MatcherEngine) findMatchedIntent(intents []model.PaymentIntent, amount decimal.Decimal) *model.PaymentIntent {
	epsilon := decimal.NewFromFloat(0.000001)
	for i := range intents {
		if intents[i].ExpectedAmount.Sub(amount).Abs().LessThanOrEqual(epsilon) {
			return &intents[i]
		}
	}
	return nil
}

func (e *MatcherEngine) settle(intent *model.PaymentIntent, transfer *model.ChainTransfer, currentBlock uint64) error {
	// 防 uint64 下溢计算确认数
	var confirmations uint64 = 1
	if currentBlock >= transfer.BlockNumber {
		confirmations = currentBlock - transfer.BlockNumber + 1
	}

	required := e.cfg.GetRequiredConfirmations(transfer.Chain)
	now := time.Now()

	intent.TxHash = transfer.TxHash
	intent.BlockNumber = transfer.BlockNumber
	intent.Confirmations = confirmations
	intent.ReceivedAmount = transfer.Amount
	intent.LogIndex = transfer.LogIndex
	intent.UpdatedAt = now

	isPaid := confirmations >= required
	if isPaid && e.txVerifier != nil && !strings.HasPrefix(transfer.TxHash, "mock_") {
		verifyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		valid, err := e.txVerifier(verifyCtx, transfer.Chain, transfer.TxHash, transfer.BlockNumber)
		cancel()
		if err != nil {
			log.Printf("[Matcher] ⚠️ 快速确认二次核验交易失败(RPC抖动暂不推进为PAID，交由ConfirmationWorker重试): tx=%s, err=%v", transfer.TxHash, err)
			isPaid = false
		} else if !valid {
			log.Printf("[Matcher ALARM] 🚨 拦截链上假充值/已Revert交易: Order=%s, Tx=%s, Chain=%s", intent.OrderID, transfer.TxHash, transfer.Chain)
			return fmt.Errorf("transaction verification failed: invalid or reverted tx %s", transfer.TxHash)
		}
	}

	if intent.DetectedAt == nil {
		intent.DetectedAt = &now
	}

	if isPaid {
		intent.Status = model.StatusPaid
		intent.PaidAt = &now
		transfer.MatchedOrderID = intent.OrderID
		metrics.RecordIntentStatus(string(intent.Chain), string(intent.Token), string(model.StatusPaid))
	} else {
		intent.Status = model.StatusConfirming
		metrics.RecordIntentStatus(string(intent.Chain), string(intent.Token), string(model.StatusConfirming))
	}

	// 事务保证两张表强一致
	err := e.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(intent).Error; err != nil {
			return err
		}
		if isPaid {
			return tx.Model(transfer).Update("matched_order_id", intent.OrderID).Error
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 双段 Hook 触发：
	// 1. 首次匹配到充值流水，立即分发 "get" 事件
	// 2. 若当前已满足所需确认数 (isPaid)，紧随其后分发 "confirm" 事件
	if e.dispatcher != nil {
		e.dispatcher.DispatchEventAsync(queue.WebhookEventGet, intent, transfer.TxHash, transfer.BlockTimestamp, confirmations, required)
		if isPaid {
			go func() {
				// 略微让出时间片，确保网络层接收端按时序先收到 get 再收到 confirm
				time.Sleep(200 * time.Millisecond)
				e.dispatcher.DispatchEventAsync(queue.WebhookEventConfirm, intent, transfer.TxHash, transfer.BlockTimestamp, confirmations, required)
			}()
		}
	}

	return nil
}

func (e *MatcherEngine) queryCandidateIntents(transfer model.ChainTransfer) ([]model.PaymentIntent, error) {
	normAddr := transfer.Chain.NormalizeAddress(transfer.TargetAddress)

	// 时间戳安全约束：链上转账只能撮合创建时间早于或相近于该交易的订单（允许 180s 时钟容差）
	// 彻底杜绝使用刚创建的新订单白嫖以前的历史未认领流水 (Time-travel / Theft attack)
	maxCreatedAt := time.Now().Add(180 * time.Second)
	if transfer.BlockTimestamp > 0 {
		maxCreatedAt = time.Unix(transfer.BlockTimestamp, 0).Add(180 * time.Second)
	}

	var intents []model.PaymentIntent
	err := e.db.Where(
		"status = ? AND chain = ? AND token = ? AND (target_address = ? OR LOWER(target_address) = ?) AND created_at <= ?",
		model.StatusWatching,
		transfer.Chain,
		transfer.Token,
		normAddr,
		strings.ToLower(normAddr),
		maxCreatedAt,
	).Find(&intents).Error
	return intents, err
}

func (e *MatcherEngine) queryExpiredCandidateIntents(transfer model.ChainTransfer) ([]model.PaymentIntent, error) {
	normAddr := transfer.Chain.NormalizeAddress(transfer.TargetAddress)

	// 仅查找 24 小时内超时的订单，避免无限期匹配历史老单
	since := time.Now().Add(-24 * time.Hour)
	maxCreatedAt := time.Now().Add(180 * time.Second)
	if transfer.BlockTimestamp > 0 {
		maxCreatedAt = time.Unix(transfer.BlockTimestamp, 0).Add(180 * time.Second)
	}

	var intents []model.PaymentIntent
	err := e.db.Where(
		"status = ? AND chain = ? AND token = ? AND (target_address = ? OR LOWER(target_address) = ?) AND expires_at >= ? AND created_at <= ?",
		model.StatusExpired,
		transfer.Chain,
		transfer.Token,
		normAddr,
		strings.ToLower(normAddr),
		since,
		maxCreatedAt,
	).Find(&intents).Error
	return intents, err
}

