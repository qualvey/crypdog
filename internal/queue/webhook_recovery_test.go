package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupWebhookRecoveryDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file:webhook-recovery-"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.WebhookLog{}))
	return db
}

func TestWebhookRetry_ReclaimsLeaseAfterCrash(t *testing.T) {
	db := setupWebhookRecoveryDB(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	claimedAt := time.Now().Add(-6 * time.Minute)
	retryAt := time.Now().Add(-time.Minute)
	log := model.WebhookLog{
		OrderID:        "order-crashed-before-delivery",
		Event:          WebhookEventConfirm,
		WebhookURL:     server.URL,
		Payload:        `{"event":"confirm","orderId":"order-crashed-before-delivery"}`,
		Signature:      "signature",
		Attempt:        1,
		Success:        false,
		NextRetryAt:    &retryAt,
		RetryClaimedAt: &claimedAt,
		CreatedAt:      time.Now().Add(-2 * time.Minute),
	}
	require.NoError(t, db.Create(&log).Error)

	dispatcher := NewWebhookDispatcher(db, &config.Config{
		Webhook: config.WebhookConfig{Secret: "test-secret", AllowLocal: true, TimeoutSec: 2},
	})
	dispatcher.retryPendingLogs(context.Background())

	require.Equal(t, 1, calls)
	var successful int64
	require.NoError(t, db.Model(&model.WebhookLog{}).
		Where("order_id = ? AND success = ?", log.OrderID, true).
		Count(&successful).Error)
	require.Equal(t, int64(1), successful)
}
