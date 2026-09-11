package engine

import (
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
}
type MatcherEngine struct {
	db         *gorm.DB
	cfg        *config.Config
	dispatcher WebhookDispatcher
	mu         sync.Mutex
}

func NewMatcherEngine(db *gorm.DB, cfg *config.Config, dispatcher *queue.WebhookDispatcher) *MatcherEngine {
	return &MatcherEngine{
		db:         db,
		cfg:        cfg,
		dispatcher: dispatcher,
	}
}

// 统一对外入口：只负责编排流程
func (e *MatcherEngine) ProcessTransfer(transfer model.ChainTransfer, currentBlockNumber uint64) error {
	if strings.TrimSpace(transfer.TxHash) == "" || transfer.Amount.LessThanOrEqual(decimal.Zero) {
		return errors.New("invalid transfer parameters")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. 流水落库与排重
	isNew, err := e.recordTransferIfNotExists(&transfer)
	if err != nil {
		return fmt.Errorf("record transfer error: %w", err)
	}
	if !isNew {
		// 显式直接返回 nil，语义清晰：已存在流水安全忽略，不再向下撮合
		return nil
	}
	// 2. 查询候选订单
	intents, err := e.queryCandidateIntents(transfer)
	if err != nil || len(intents) == 0 {
		metrics.RecordTransferMatch(string(transfer.Chain), false)
		log.Printf("[Matcher] 未找到匹配候选意向: Chain=%s, Token=%s, To=%s, Amount=%s, Tx=%s",
			transfer.Chain, transfer.Token, transfer.TargetAddress, transfer.Amount.String(), transfer.TxHash)
		return err
	}

	// 3. 纯内存计算：匹配尾数
	matchedIntent := e.findMatchedIntent(intents, transfer.Amount)
	if matchedIntent == nil {
		metrics.RecordTransferMatch(string(transfer.Chain), false)
		log.Printf("[Matcher] 金额不匹配候选意向: Tx=%s, Amount=%s, 候选订单数=%d",
			transfer.TxHash, transfer.Amount.String(), len(intents))
		return nil
	}

	metrics.RecordTransferMatch(string(transfer.Chain), true)
	log.Printf("[Matcher] 🎉 成功匹配订单: OrderID=%s, IntentID=%s, Tx=%s, Amount=%s",
		matchedIntent.OrderID, matchedIntent.ID, transfer.TxHash, transfer.Amount.String())

	// 4. 状态推进与结算（事务 + 触发通知）
	return e.settle(matchedIntent, &transfer, currentBlockNumber)
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

	// 成功且已确认，异步触发通知
	if isPaid && e.dispatcher != nil {
		e.dispatcher.DispatchAsync(intent, transfer.TxHash, transfer.BlockTimestamp)
	}

	return nil
}
func (e *MatcherEngine) queryCandidateIntents(transfer model.ChainTransfer) ([]model.PaymentIntent, error) {
	tokens := []model.Token{transfer.Token}
	// 支持 USDT 与 USDC 等价稳定币互通撮合
	if transfer.Token == model.TokenUSDT {
		tokens = append(tokens, model.TokenUSDC)
	} else if transfer.Token == model.TokenUSDC {
		tokens = append(tokens, model.TokenUSDT)
	}

	normAddr := transfer.Chain.NormalizeAddress(transfer.TargetAddress)

	var intents []model.PaymentIntent
	err := e.db.Where(
		"status IN (?, ?) AND chain = ? AND token IN (?) AND (target_address = ? OR LOWER(target_address) = ?)",
		model.StatusWatching,
		model.StatusConfirming,
		transfer.Chain,
		tokens,
		normAddr,
		strings.ToLower(normAddr),
	).Find(&intents).Error
	return intents, err
}
