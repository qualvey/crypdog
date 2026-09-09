package queue

import (
	"context"
	"log"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"gorm.io/gorm"
)

type HeightProviderFunc func(chain model.Chain) int64

type ConfirmationWorker struct {
	db             *gorm.DB
	cfg            *config.Config
	dispatcher     *WebhookDispatcher
	heightProvider HeightProviderFunc
}

func NewConfirmationWorker(
	db *gorm.DB,
	cfg *config.Config,
	dispatcher *WebhookDispatcher,
	heightProvider HeightProviderFunc,
) *ConfirmationWorker {
	return &ConfirmationWorker{
		db:             db,
		cfg:            cfg,
		dispatcher:     dispatcher,
		heightProvider: heightProvider,
	}
}

// StartWorker runs background loop to track confirming intents and advance them to PAID
func (w *ConfirmationWorker) StartWorker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[ConfirmationWorker] 收到退出信号，停止确认度检测")
			return
		case <-ticker.C:
			w.safeCheckConfirmations()
		}
	}
}
func (w *ConfirmationWorker) safeCheckConfirmations() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ConfirmationWorker PANIC RECOVER] 执行确认度检测异常: %v", r)
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

		confirmations := int(currentBlock - intent.BlockNumber + 1)
		required := w.cfg.GetRequiredConfirmations(intent.Chain)

		if confirmations >= required {
			intent.Status = model.StatusPaid
			intent.Confirmations = confirmations
			intent.PaidAt = &now
			intent.UpdatedAt = now

			w.db.Save(&intent)

			// Mark transfer as matched
			w.db.Model(&model.ChainTransfer{}).
				Where("tx_hash = ?", intent.TxHash).
				Update("matched_order_id", intent.OrderID)

			log.Printf("[ConfirmationWorker] 🎊 Order CONFIRMED! Order: %s | TxHash: %s | Confirmations: %d/%d",
				intent.OrderID, intent.TxHash, confirmations, required)

			// Fetch transfer timestamp if available
			var transfer model.ChainTransfer
			var blockTs int64 = now.UnixMilli()
			if err := w.db.Where("tx_hash = ?", intent.TxHash).First(&transfer).Error; err == nil {
				blockTs = transfer.BlockTimestamp
			}

			// Trigger Webhook callback
			w.dispatcher.DispatchAsync(&intent, intent.TxHash, blockTs)
		} else if confirmations != intent.Confirmations {
			// Update intermediate confirmation count
			intent.Confirmations = confirmations
			intent.UpdatedAt = now
			w.db.Save(&intent)

			log.Printf("[ConfirmationWorker] Order %s confirmation updated: %d/%d",
				intent.OrderID, confirmations, required)
		}
	}
}
