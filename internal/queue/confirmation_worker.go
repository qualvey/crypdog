package queue

import (
	"context"
	"crypdog/internal/logger"
	"errors"

	"strings"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"gorm.io/gorm"
)

type HeightProviderFunc func(chain model.Chain) uint64
type TxVerifierFunc func(ctx context.Context, chain model.Chain, txHash string, blockNumber uint64) (bool, error)

type ConfirmationWorker struct {
	db             *gorm.DB
	cfg            *config.Config
	dispatcher     *WebhookDispatcher
	heightProvider HeightProviderFunc
	txVerifier     TxVerifierFunc
}

func NewConfirmationWorker(
	db *gorm.DB,
	cfg *config.Config,
	dispatcher *WebhookDispatcher,
	heightProvider HeightProviderFunc,
	txVerifier ...TxVerifierFunc,
) *ConfirmationWorker {
	var verifier TxVerifierFunc
	if len(txVerifier) > 0 {
		verifier = txVerifier[0]
	}
	return &ConfirmationWorker{
		db:             db,
		cfg:            cfg,
		dispatcher:     dispatcher,
		heightProvider: heightProvider,
		txVerifier:     verifier,
	}
}

// StartWorker runs background loop to track confirming intents and advance them to PAID
func (w *ConfirmationWorker) StartWorker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("confirmation worker stopped")
			return
		case <-ticker.C:
			w.safeCheckConfirmations()
		}
	}
}
func (w *ConfirmationWorker) safeCheckConfirmations() {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic in confirmation worker", "error", r)
		}
	}()

	w.checkConfirmations()
}

func (w *ConfirmationWorker) checkConfirmations() {
	var confirmingIntents []model.PaymentIntent
	err := w.db.Where("status = ? AND tx_hash != ''", model.StatusConfirming).Find(&confirmingIntents).Error
	if err != nil || len(confirmingIntents) == 0 {
		return
	}

	now := time.Now()

	for _, intent := range confirmingIntents {
		currentBlock := w.heightProvider(intent.Chain)

		if currentBlock <= 0 || currentBlock < intent.BlockNumber {
			continue
		}

		confirmations := currentBlock - intent.BlockNumber + 1
		required := w.cfg.GetRequiredConfirmations(intent.Chain)

		if confirmations >= required {
			// 二次链上核验防假充值与重组
			if w.txVerifier != nil && !strings.HasPrefix(intent.TxHash, "mock_") {
				verifyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				valid, err := w.txVerifier(verifyCtx, intent.Chain, intent.TxHash, intent.BlockNumber)
				cancel()
				if err != nil {
					logger.Warn("transaction verification failed; payment remains confirming", "tx_hash", intent.TxHash, "chain", intent.Chain, "error", err)
					continue
				}
				if !valid {
					logger.Error("reverted or invalid transaction blocked", "order_id", intent.OrderID, "tx_hash", intent.TxHash, "chain", intent.Chain)
					continue
				}
			}

			// 乐观锁条件原子更新：必须当前状态仍为 CONFIRMING
			res := w.db.Model(&model.PaymentIntent{}).
				Where("id = ? AND status = ?", intent.ID, model.StatusConfirming).
				Updates(map[string]interface{}{
					"status":         model.StatusPaid,
					"allocation_key": nil,
					"confirmations":  confirmations,
					"paid_at":        &now,
					"updated_at":     now,
				})
			if res.Error != nil {
				logger.Error("payment status update failed", "order_id", intent.OrderID, "error", res.Error)
				continue
			}
			if res.RowsAffected == 0 {
				logger.Warn("payment status changed concurrently; refusing paid transition", "order_id", intent.OrderID)
				continue
			}

			intent.Status = model.StatusPaid
			intent.Confirmations = confirmations
			intent.PaidAt = &now
			intent.UpdatedAt = now

			// Mark transfer as matched (精准使用 chain + tx_hash + log_index 避免误伤同 tx 其他转账)
			w.db.Model(&model.ChainTransfer{}).
				Where("chain = ? AND tx_hash = ? AND log_index = ?", intent.Chain, intent.TxHash, intent.LogIndex).
				Update("matched_order_id", intent.OrderID)

			logger.Info("payment confirmed", "order_id", intent.OrderID, "tx_hash", intent.TxHash, "confirmations", confirmations, "required_confirmations", required, "chain", intent.Chain)

			// Fetch transfer timestamp if available
			var transfer model.ChainTransfer
			var blockTs int64 = now.Unix()
			if err := w.db.Where("chain = ? AND tx_hash = ? AND log_index = ?", intent.Chain, intent.TxHash, intent.LogIndex).First(&transfer).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					logger.Error("related transfer lookup failed", "tx_hash", intent.TxHash, "error", err)
				}
				// 找不到才合理降级为 now
			} else {
				blockTs = transfer.BlockTimestamp
			}

			// Trigger Webhook callback: 次段 confirm
			w.dispatcher.DispatchEventAsync(WebhookEventConfirm, &intent, intent.TxHash, blockTs, confirmations, required)
		} else if confirmations != intent.Confirmations {
			// Update intermediate confirmation count (仅当状态仍为 CONFIRMING 时更新)
			w.db.Model(&model.PaymentIntent{}).
				Where("id = ? AND status = ?", intent.ID, model.StatusConfirming).
				Updates(map[string]interface{}{
					"confirmations": confirmations,
					"updated_at":    now,
				})

			logger.Info("payment confirmations updated", "order_id", intent.OrderID, "confirmations", confirmations, "required_confirmations", required, "chain", intent.Chain)
		}
	}
}
