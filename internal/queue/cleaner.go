package queue

import (
	"context"
	"crypdog/internal/logger"

	"time"

	"crypdog/internal/model"

	"gorm.io/gorm"
)

type IntentCleaner struct {
	db *gorm.DB
}

func NewIntentCleaner(db *gorm.DB) *IntentCleaner {
	return &IntentCleaner{db: db}
}

// StartCleaner runs a ticker that periodic checks for expired payment intents
func (c *IntentCleaner) StartCleaner(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("intent cleaner stopped")
			return
		case <-ticker.C:
			c.safeCleanExpiredIntents()
		}
	}
}

// safeCleanExpiredIntents 捕获单次清理异常，防止 Goroutine 退出
func (c *IntentCleaner) safeCleanExpiredIntents() {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic in intent cleaner", "error", r)
		}
	}()

	c.cleanExpiredIntents()
}
func (c *IntentCleaner) cleanExpiredIntents() {
	now := time.Now()
	res := c.db.Model(&model.PaymentIntent{}).
		Where("status = ? AND expires_at <= ?", model.StatusWatching, now).
		Updates(map[string]interface{}{
			"status":         model.StatusExpired,
			"allocation_key": nil,
			"updated_at":     now,
		})

	if res.Error != nil {
		logger.Error("cleaning expired intents failed", "error", res.Error)
		return
	}

	if res.RowsAffected > 0 {
		logger.Info("expired outdated payment intents", "count", res.RowsAffected)
	}
}
