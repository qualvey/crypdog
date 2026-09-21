package queue

import (
	"context"
	"log"
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
			log.Println("[IntentCleaner] 收到停止信号，清理守护进程平稳退出")
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
			log.Printf("[IntentCleaner PANIC RECOVER] 清理任务执行异常: %v", r)
		}
	}()

	c.cleanExpiredIntents()
}
func (c *IntentCleaner) cleanExpiredIntents() {
	now := time.Now()
	res := c.db.Model(&model.PaymentIntent{}).
		Where("status = ? AND expires_at <= ?", model.StatusWatching, now).
		Update("status", model.StatusExpired)

	if res.Error != nil {
		log.Printf("[Cleaner] Error cleaning expired intents: %v", res.Error)
		return
	}

	if res.RowsAffected > 0 {
		log.Printf("[Cleaner] Automatically expired %d outdated payment intents", res.RowsAffected)
	}
}
