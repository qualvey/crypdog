//go:build integration
// +build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"crypdog/internal/api"
	"crypdog/internal/config"
	"crypdog/internal/engine"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"crypdog/internal/queue"
	"crypdog/internal/scanner"
	"crypdog/internal/signature"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// webhookRecord captures incoming webhook calls sent to the mock webhook server.
type webhookRecord struct {
	Signature string
	Payload   queue.WebhookPayload
	RawBody   []byte
}

type integrationEnv struct {
	db           *gorm.DB
	cfg          *config.Config
	router       http.Handler
	matcher      *engine.MatcherEngine
	transferChan chan model.ChainTransfer
	dispatcher   *queue.WebhookDispatcher
	serverSecret string
	webhookLock  sync.Mutex
	webhookCalls []webhookRecord
	cancel       context.CancelFunc
}

func setupIntegrationEnv(t *testing.T) *integrationEnv {
	metrics.InitMetrics()

	// 1. Initialize SQLite in-memory database
	dsn := fmt.Sprintf("file:integration_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(
		&model.PaymentIntent{},
		&model.ChainTransfer{},
		&model.WebhookLog{},
		&model.WalletAddress{},
		&model.ChainToken{},
		&model.AdminAuditLog{},
	)
	require.NoError(t, err)

	serverSecret := "test-integration-server-secret-12345"
	webhookSecret := "test-integration-webhook-secret-67890"

	cfg := &config.Config{
		Env: "development",
		Server: config.ServerConfig{
			Port:             "8080",
			Secret:           serverSecret,
			AdminSecret:      serverSecret,
			EnableSimulation: true,
			RateLimitPerMin:  1000,
		},
		Webhook: config.WebhookConfig{
			Secret:     webhookSecret,
			AllowLocal: true,
			TimeoutSec: 5,
		},
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainBsc: {
				Confirmations: 1,
			},
			model.ChainEth: {
				Confirmations: 3,
			},
		},
	}

	// 2. Pre-seed wallet address
	wallet := model.WalletAddress{
		Chain:   model.ChainBsc,
		Address: "0x1111111111111111111111111111111111111111",
		Enabled: true,
		Weight:  10,
	}
	require.NoError(t, db.Create(&wallet).Error)

	tokenUSDT := model.ChainToken{
		Chain:    model.ChainBsc,
		Symbol:   model.TokenUSDT,
		Name:     "Tether USD",
		Decimals: 18,
		Enabled:  true,
	}
	require.NoError(t, db.Create(&tokenUSDT).Error)

	env := &integrationEnv{
		db:           db,
		cfg:          cfg,
		serverSecret: serverSecret,
		transferChan: make(chan model.ChainTransfer, 100),
	}

	// 3. Webhook Dispatcher
	env.dispatcher = queue.NewWebhookDispatcher(db, cfg)

	// 4. Matcher Engine
	env.matcher = engine.NewMatcherEngine(db, cfg, env.dispatcher)

	// 5. Start async pipeline runner
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case tr, ok := <-env.transferChan:
				if !ok {
					return
				}
				_ = env.matcher.ProcessTransfer(tr, tr.BlockNumber+10)
			}
		}
	}()

	poolManager := engine.NewMicroAmountManager(db)
	scannerMgr := scanner.NewManager()

	handler := api.NewHandler(db, cfg, poolManager, scannerMgr, env.transferChan)
	env.router = api.SetupRouter(handler)

	t.Cleanup(func() {
		cancel()
		_ = sqlDB.Close()
	})

	return env
}

// TestIntegration_FullPaymentFlow tests the end-to-end payment lifecycle:
// 1. Client creates PaymentIntent (POST /api/v1/watcher/intents/allocate)
// 2. Client triggers simulated deposit (POST /api/v1/watcher/intents/simulate)
// 3. MatcherEngine processes transfer asynchronously -> transitions intent to PAID
// 4. Client polls GET /api/v1/watcher/intents/:orderId -> verifies status is PAID
// 5. Webhook listener receives event and verifies signature
func TestIntegration_FullPaymentFlow(t *testing.T) {
	env := setupIntegrationEnv(t)

	// Start a local test HTTP server to act as merchant webhook endpoint
	var receivedLock sync.Mutex
	var receivedPayloads []queue.WebhookPayload
	var receivedSignatures []string

	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sig := r.Header.Get("X-Signature-SHA256")
		valid := signature.VerifyHMACSHA256(body, env.cfg.Webhook.Secret, sig)
		if !valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var payload queue.WebhookPayload
		_ = json.Unmarshal(body, &payload)

		receivedLock.Lock()
		receivedSignatures = append(receivedSignatures, sig)
		receivedPayloads = append(receivedPayloads, payload)
		receivedLock.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200,"message":"received"}`))
	}))
	defer webhookServer.Close()

	orderID := fmt.Sprintf("order_e2e_%d", time.Now().UnixNano())

	// Step 1: Allocate Intent
	allocateBody := map[string]interface{}{
		"orderId":        orderID,
		"chain":          "BSC",
		"token":          "USDT",
		"baseAmount":     10.5,
		"timeoutSeconds": 1800,
		"webhookUrl":     webhookServer.URL,
	}
	bodyBytes, _ := json.Marshal(allocateBody)
	req := httptest.NewRequest("POST", "/api/v1/watcher/intents/allocate", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+env.serverSecret)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "AllocateIntent should return 200: %s", w.Body.String())

	var allocResp struct {
		Code int `json:"code"`
		Data struct {
			OrderID        string          `json:"orderId"`
			ExpectedAmount decimal.Decimal `json:"expectedAmount"`
			TargetAddress  string          `json:"targetAddress"`
			Status         string          `json:"status"`
		} `json:"data"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &allocResp)
	require.NoError(t, err)
	assert.Equal(t, orderID, allocResp.Data.OrderID)
	assert.Equal(t, string(model.StatusWatching), allocResp.Data.Status)
	assert.NotEmpty(t, allocResp.Data.TargetAddress)
	assert.True(t, allocResp.Data.ExpectedAmount.GreaterThan(decimal.Zero))

	// Step 2: Simulate Payment Deposit via OrderID
	simulateBody := map[string]interface{}{
		"orderId": orderID,
	}
	simBytes, _ := json.Marshal(simulateBody)
	simReq := httptest.NewRequest("POST", "/api/v1/watcher/intents/simulate", bytes.NewReader(simBytes))
	simReq.Header.Set("Authorization", "Bearer "+env.serverSecret)
	simReq.Header.Set("Content-Type", "application/json")
	simW := httptest.NewRecorder()
	env.router.ServeHTTP(simW, simReq)

	require.Equal(t, http.StatusOK, simW.Code, "Simulate should return 200: %s", simW.Body.String())

	// Step 3 & 4: Poll status until PAID
	require.Eventually(t, func() bool {
		statusReq := httptest.NewRequest("GET", "/api/v1/watcher/intents/"+orderID, nil)
		statusReq.Header.Set("Authorization", "Bearer "+env.serverSecret)
		statusW := httptest.NewRecorder()
		env.router.ServeHTTP(statusW, statusReq)

		if statusW.Code != http.StatusOK {
			return false
		}

		var statusResp struct {
			Data struct {
				Status string `json:"status"`
				TxHash string `json:"txHash"`
			} `json:"data"`
		}
		if err := json.Unmarshal(statusW.Body.Bytes(), &statusResp); err != nil {
			return false
		}
		return statusResp.Data.Status == string(model.StatusPaid) && statusResp.Data.TxHash != ""
	}, 5*time.Second, 100*time.Millisecond, "Order status should become PAID")

	// Step 5: Verify Webhook was dispatched and signature verified
	require.Eventually(t, func() bool {
		receivedLock.Lock()
		defer receivedLock.Unlock()
		return len(receivedPayloads) >= 2 // Both "get" and "confirm" events received
	}, 5*time.Second, 100*time.Millisecond, "Merchant webhook should receive both get and confirm notifications")

	receivedLock.Lock()
	defer receivedLock.Unlock()
	assert.Equal(t, orderID, receivedPayloads[0].OrderID)
	assert.NotEmpty(t, receivedSignatures[0])
	assert.Equal(t, orderID, receivedPayloads[1].OrderID)
	assert.NotEmpty(t, receivedSignatures[1])
}

// TestIntegration_SimulateDeposit_DuplicateIdempotency tests sending duplicate simulated transfers
// to verify that the system handles duplicate deposits idempotently.
func TestIntegration_SimulateDeposit_DuplicateIdempotency(t *testing.T) {
	env := setupIntegrationEnv(t)

	orderID := fmt.Sprintf("order_idem_%d", time.Now().UnixNano())

	// 1. Allocate Intent
	allocateBody := map[string]interface{}{
		"orderId":        orderID,
		"chain":          "BSC",
		"token":          "USDT",
		"baseAmount":     20.0,
		"timeoutSeconds": 1800,
		"webhookUrl":     "https://example.com/webhook",
	}
	bodyBytes, _ := json.Marshal(allocateBody)
	req := httptest.NewRequest("POST", "/api/v1/watcher/intents/allocate", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+env.serverSecret)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// 2. First Deposit with custom TxHash
	customTx := fmt.Sprintf("tx_custom_%d", time.Now().UnixNano())
	simBody1 := map[string]interface{}{
		"orderId": orderID,
		"txHash":  customTx,
	}
	b1, _ := json.Marshal(simBody1)
	req1 := httptest.NewRequest("POST", "/api/v1/watcher/intents/simulate", bytes.NewReader(b1))
	req1.Header.Set("Authorization", "Bearer "+env.serverSecret)
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	env.router.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusOK, w1.Code)

	// Wait for status to become PAID
	require.Eventually(t, func() bool {
		var intent model.PaymentIntent
		if err := env.db.Where("order_id = ?", orderID).First(&intent).Error; err == nil {
			return intent.Status == model.StatusPaid
		}
		return false
	}, 3*time.Second, 100*time.Millisecond)

	// 3. Second identical Deposit (same TxHash)
	req2 := httptest.NewRequest("POST", "/api/v1/watcher/intents/simulate", bytes.NewReader(b1))
	req2.Header.Set("Authorization", "Bearer "+env.serverSecret)
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	env.router.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	// Verify DB state remains consistent: exactly one matched transfer record
	var transfers []model.ChainTransfer
	err := env.db.Where("tx_hash = ?", customTx).Find(&transfers).Error
	require.NoError(t, err)
	assert.Len(t, transfers, 1, "There should only be 1 transfer stored for duplicate txHash")
}
