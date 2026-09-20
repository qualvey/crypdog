package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypdog/internal/config"
	"crypdog/internal/engine"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"crypdog/internal/scanner"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

func setupTestRouter(t *testing.T) (*gorm.DB, http.Handler) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err)
	_ = db.AutoMigrate(&model.WalletAddress{}, &model.PaymentIntent{}, &model.ChainToken{}, &model.AdminAuditLog{})

	metrics.InitMetrics()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   "8080",
			Secret: "test-secret",
		},
		Metrics: config.MetricsConfig{
			Enabled: true,
			Path:    "/metrics",
		},
		Pprof: config.PprofConfig{
			Enabled: true,
		},
	}

	poolMgr := engine.NewMicroAmountManager(db)
	scannerMgr := scanner.NewManager()

	handler := NewHandler(db, cfg, poolMgr, scannerMgr, nil)
	r := SetupRouter(handler)
	return db, r
}

func TestObservability_HealthProbes(t *testing.T) {
	_, r := setupTestRouter(t)

	// Test Liveness Probe
	reqLive := httptest.NewRequest("GET", "/healthz/live", nil)
	wLive := httptest.NewRecorder()
	r.ServeHTTP(wLive, reqLive)

	assert.Equal(t, http.StatusOK, wLive.Code)
	var liveBody map[string]interface{}
	err := json.Unmarshal(wLive.Body.Bytes(), &liveBody)
	assert.NoError(t, err)
	assert.Equal(t, "UP", liveBody["status"])

	// Test Readiness Probe
	reqReady := httptest.NewRequest("GET", "/healthz/ready", nil)
	wReady := httptest.NewRecorder()
	r.ServeHTTP(wReady, reqReady)

	assert.Equal(t, http.StatusOK, wReady.Code)
	var readyBody map[string]interface{}
	err = json.Unmarshal(wReady.Body.Bytes(), &readyBody)
	assert.NoError(t, err)
	assert.Equal(t, "UP", readyBody["status"])
	assert.NotNil(t, readyBody["components"])
}

func TestObservability_RequestIDMiddleware(t *testing.T) {
	_, r := setupTestRouter(t)

	// Case 1: Auto-generate X-Request-ID
	req1 := httptest.NewRequest("GET", "/health", nil)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	reqID1 := w1.Header().Get("X-Request-ID")
	assert.NotEmpty(t, reqID1)

	// Case 2: Passthrough existing X-Request-ID
	customID := "custom-trace-uuid-12345"
	req2 := httptest.NewRequest("GET", "/health", nil)
	req2.Header.Set("X-Request-ID", customID)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	reqID2 := w2.Header().Get("X-Request-ID")
	assert.Equal(t, customID, reqID2)
}

func TestObservability_PrometheusMetricsEndpoint(t *testing.T) {
	_, r := setupTestRouter(t)

	// Trigger some metrics by hitting /health
	reqHealth := httptest.NewRequest("GET", "/health", nil)
	wHealth := httptest.NewRecorder()
	r.ServeHTTP(wHealth, reqHealth)

	// Query /metrics
	reqMetrics := httptest.NewRequest("GET", "/metrics", nil)
	wMetrics := httptest.NewRecorder()
	r.ServeHTTP(wMetrics, reqMetrics)

	assert.Equal(t, http.StatusOK, wMetrics.Code)
	bodyStr := wMetrics.Body.String()
	assert.True(t, strings.Contains(bodyStr, "crypdog_http_requests_total"))
	assert.True(t, strings.Contains(bodyStr, "go_goroutines"))
}

func TestObservability_PprofEndpoints(t *testing.T) {
	_, r := setupTestRouter(t)

	// 1. 无鉴权访问应被安全拦截返回 401
	reqUnauth := httptest.NewRequest("GET", "/debug/pprof/", nil)
	wUnauth := httptest.NewRecorder()
	r.ServeHTTP(wUnauth, reqUnauth)
	assert.Equal(t, http.StatusUnauthorized, wUnauth.Code)

	// 2. 带有效 Secret 鉴权访问应成功返回 200
	reqAuth := httptest.NewRequest("GET", "/debug/pprof/", nil)
	reqAuth.Header.Set("Authorization", "Bearer test-secret")
	wAuth := httptest.NewRecorder()
	r.ServeHTTP(wAuth, reqAuth)

	assert.Equal(t, http.StatusOK, wAuth.Code)
	assert.True(t, strings.Contains(wAuth.Body.String(), "Types of profiles available"))
}

func TestCancelIntentEndpoints(t *testing.T) {
	db, r := setupTestRouter(t)
	err := db.AutoMigrate(&model.PaymentIntent{})
	assert.NoError(t, err)

	intent := model.PaymentIntent{
		ID:            "intent_arb_001",
		OrderID:       "ord_cancel_001",
		Chain:         model.ChainArbitrum,
		Token:         model.TokenUSDT,
		TargetAddress: "0x7bdc49542978b16566e82c8f90db1eb03804c675",
		Status:        model.StatusWatching,
	}
	assert.NoError(t, db.Create(&intent).Error)

	// 1. 测试 POST /api/v1/watcher/intents/cancel 带 JSON body
	body := `{"orderId":"ord_cancel_001"}`
	req := httptest.NewRequest("POST", "/api/v1/watcher/intents/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, float64(200), resp["code"])

	var updated model.PaymentIntent
	assert.NoError(t, db.Where("order_id = ?", "ord_cancel_001").First(&updated).Error)
	assert.Equal(t, model.StatusCancelled, updated.Status)

	// 2. 测试 POST /api/v1/watcher/intents/:orderId/cancel 路径参数
	intent2 := model.PaymentIntent{
		ID:            "intent_arb_002",
		OrderID:       "ord_cancel_002",
		Chain:         model.ChainArbitrum,
		Token:         model.TokenUSDC,
		TargetAddress: "0x7bdc49542978b16566e82c8f90db1eb03804c675",
		Status:        model.StatusWatching,
	}
	assert.NoError(t, db.Create(&intent2).Error)

	req2 := httptest.NewRequest("POST", "/api/v1/watcher/intents/ord_cancel_002/cancel", nil)
	req2.Header.Set("Authorization", "Bearer test-secret")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusOK, w2.Code)
	var updated2 model.PaymentIntent
	assert.NoError(t, db.Where("order_id = ?", "ord_cancel_002").First(&updated2).Error)
	assert.Equal(t, model.StatusCancelled, updated2.Status)
}

func TestIntentHandler_GetPaymentOptions(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err)
	assert.NoError(t, db.AutoMigrate(&model.WalletAddress{}, &model.PaymentIntent{}, &model.ChainToken{}))

	falseVal := false
	trueVal := true
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   "8080",
			Secret: "test-secret",
		},
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainTron: {
				Enabled: &falseVal, // 显式禁用
			},
			model.ChainArbitrum: {
				Enabled: &trueVal, // 显式启用
			},
		},
	}

	poolMgr := engine.NewMicroAmountManager(db)
	scannerMgr := scanner.NewManager()
	handler := NewHandler(db, cfg, poolMgr, scannerMgr, nil)
	r := SetupRouter(handler)

	// 1. 先插入两个钱包地址：TRON (但节点被禁用) 和 ARBITRUM (节点启用)
	db.Create(&model.WalletAddress{
		Chain:   model.ChainTron,
		Address: "TWLp7W7umCmLYwLxm8hwndo6B9p6tsshzh",
		Enabled: true,
	})
	db.Create(&model.WalletAddress{
		Chain:   model.ChainArbitrum,
		Address: "0x7bdc49542978b16566e82c8f90db1eb03804c675",
		Enabled: true,
	})

	req := httptest.NewRequest("GET", "/api/v1/watcher/options", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Code int                        `json:"code"`
		Data model.CryptoPaymentOptions `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 200, resp.Code)

	// 校验 tokens 是否包含 USDT
	hasUSDT := false
	for _, tok := range resp.Data.Tokens {
		if tok.Symbol == "USDT" {
			hasUSDT = true
			break
		}
	}
	assert.True(t, hasUSDT)

	// ARBITRUM 应该在 USDT 的 chains 列表中，而 TRON 不应该在（因为 config 中 enabled=false）
	usdtChains := resp.Data.Chains["USDT"]
	var chainNames []string
	for _, c := range usdtChains {
		chainNames = append(chainNames, string(c.Chain))
	}
	assert.Contains(t, chainNames, "ARBITRUM")
	assert.NotContains(t, chainNames, "TRON")
	assert.Equal(t, "USDT", resp.Data.DefaultToken)
	assert.Equal(t, "ARBITRUM", resp.Data.DefaultChain)
}

func TestRegisterIntent_InvalidAddress(t *testing.T) {
	_, r := setupTestRouter(t)

	// 1. 测试 EVM 地址非法（非 0x 开头或非 40 hex）
	badEvmBody := `{
		"orderId": "ord_bad_addr_001",
		"chain": "BSC",
		"token": "USDT",
		"targetAddress": "0xInvalidHexAddress123",
		"expectedAmount": 10.0001,
		"timeoutSeconds": 1800,
		"webhookUrl": "https://example.com/webhook"
	}`
	req := httptest.NewRequest("POST", "/api/v1/watcher/intents", strings.NewReader(badEvmBody))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid targetAddress")

	// 2. 测试 TRON 地址非法（校验和不匹配）
	badTronBody := `{
		"orderId": "ord_bad_addr_002",
		"chain": "TRON",
		"token": "USDT",
		"targetAddress": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6x",
		"expectedAmount": 10.0001,
		"timeoutSeconds": 1800,
		"webhookUrl": "https://example.com/webhook"
	}`
	reqTron := httptest.NewRequest("POST", "/api/v1/watcher/intents", strings.NewReader(badTronBody))
	reqTron.Header.Set("Authorization", "Bearer test-secret")
	reqTron.Header.Set("Content-Type", "application/json")
	wTron := httptest.NewRecorder()
	r.ServeHTTP(wTron, reqTron)

	assert.Equal(t, http.StatusBadRequest, wTron.Code)
	assert.Contains(t, wTron.Body.String(), "Invalid targetAddress")
}

func TestRegisterIntent_RejectNonPlatformWallet(t *testing.T) {
	db, r := setupTestRouter(t)

	// 预置系统启用的 ARBITRUM 官方收款钱包
	db.Create(&model.WalletAddress{
		Chain:   model.ChainArbitrum,
		Address: "0x7bdc49542978b16566e82c8f90db1eb03804c675",
		Enabled: true,
	})

	// 传入合规的 EVM 地址格式，但并不属于平台的收款钱包
	unauthorizedWalletBody := `{
		"orderId": "ord_unauth_001",
		"chain": "ARBITRUM",
		"token": "USDC",
		"targetAddress": "0x1111111111111111111111111111111111111111",
		"expectedAmount": 10.0001,
		"timeoutSeconds": 1800,
		"webhookUrl": "https://example.com/webhook"
	}`

	req := httptest.NewRequest("POST", "/api/v1/watcher/intents", strings.NewReader(unauthorizedWalletBody))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not in the configured platform wallet pool")
}
