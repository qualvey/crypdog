package queue_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"
	"crypdog/internal/queue"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupQueueTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	err = db.AutoMigrate(&model.PaymentIntent{}, &model.ChainTransfer{}, &model.WebhookLog{})
	require.NoError(t, err)

	t.Cleanup(func() {
		db.Exec("DELETE FROM payment_intents")
		db.Exec("DELETE FROM chain_transfers")
		db.Exec("DELETE FROM webhook_logs")
	})

	return db
}

func TestConfirmationWorker_OptimisticUpdate(t *testing.T) {
	db := setupQueueTestDB(t)
	cfg := &config.Config{
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainArbitrum: {Confirmations: 3},
		},
	}

	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	heightProvider := func(chain model.Chain) uint64 {
		return 100 // 当前链高度 100
	}

	worker := queue.NewConfirmationWorker(db, cfg, dispatcher, heightProvider)

	now := time.Now()
	// 1. 正常场景：订单处于 CONFIRMING 且 BlockNumber=95 (100 - 95 + 1 = 6 >= 3)
	intent := model.PaymentIntent{
		ID:             "intent_opt_1",
		OrderID:        "ord_opt_1",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDC,
		TargetAddress:  "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		ExpectedAmount: decimal.NewFromFloat(10.0001),
		ReceivedAmount: decimal.NewFromFloat(10.0001),
		Status:         model.StatusConfirming,
		BlockNumber:    95,
		Confirmations:  1,
		TxHash:         "mock_tx_123",
		LogIndex:       0,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, db.Create(&intent).Error)

	transfer := model.ChainTransfer{
		TxHash:         "mock_tx_123",
		Chain:          model.ChainArbitrum,
		LogIndex:       0,
		TargetAddress:  "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		Amount:         decimal.NewFromFloat(10.0001),
		BlockNumber:    95,
		BlockTimestamp: now.Unix(),
	}
	require.NoError(t, db.Create(&transfer).Error)

	// 运行一次单轮检测 (可通过暴露或私有调用，safeCheckConfirmations 内部调用 checkConfirmations)
	// 在确认数满足时应成功将状态推进至 PAID
	workerStartCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	worker.StartWorker(workerStartCtx, 50*time.Millisecond)

	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("id = ?", intent.ID).First(&updatedIntent).Error)
	assert.Equal(t, model.StatusPaid, updatedIntent.Status)
	assert.NotNil(t, updatedIntent.PaidAt)

	var updatedTransfer model.ChainTransfer
	require.NoError(t, db.Where("tx_hash = ?", transfer.TxHash).First(&updatedTransfer).Error)
	assert.Equal(t, intent.OrderID, updatedTransfer.MatchedOrderID)
}

func TestWebhookDispatcher_NoDualTrigger(t *testing.T) {
	db := setupQueueTestDB(t)
	var callCount atomic.Int32

	// 创建一个返回 500 的 mock server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			Secret:     "test-secret",
			TimeoutSec: 2,
			AllowLocal: true,
		},
	}

	dispatcher := queue.NewWebhookDispatcher(db, cfg)

	payload := queue.WebhookPayload{
		Event:         "PAYMENT_SUCCESS",
		OrderID:       "ord_retry_test",
		Chain:         model.ChainArbitrum,
		Token:         model.TokenUSDC,
		TargetAddress: "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		Amount:        decimal.NewFromFloat(10.0001),
		TxHash:        "0xabc123",
	}

	// 第一次投递：应当且仅发送 1 次请求，同时在 DB 写入一条失败日志
	dispatcher.DeliverWithRetry("ord_retry_test", mockServer.URL, payload, 1)

	assert.Equal(t, int32(1), callCount.Load(), "初始失败投递应且仅发生 1 次请求")

	// 等待 2.5 秒（超过原 2s backoff），确认不会有内存 time.AfterFunc 偷偷在后台重试
	time.Sleep(2500 * time.Millisecond)
	assert.Equal(t, int32(1), callCount.Load(), "不应有内存 time.AfterFunc 异步再次触发请求")

	// 验证 DB 中记录了 next_retry_at
	var logs []model.WebhookLog
	require.NoError(t, db.Where("order_id = ?", "ord_retry_test").Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.False(t, logs[0].Success)
	assert.NotNil(t, logs[0].NextRetryAt)
}
