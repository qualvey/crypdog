package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func setupAdminTestRouter(t *testing.T) (*gorm.DB, http.Handler) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err)
	assert.NoError(t, db.AutoMigrate(&model.WalletAddress{}, &model.PaymentIntent{}, &model.ChainToken{}, &model.AdminAuditLog{}))

	metrics.InitMetrics()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:        "8080",
			Secret:      "service-test-secret-123456",
			AdminSecret: "admin-test-secret-123456",
		},
	}

	poolMgr := engine.NewMicroAmountManager(db)
	scannerMgr := scanner.NewManager()

	handler := NewHandler(db, cfg, poolMgr, scannerMgr, nil)
	r := SetupRouter(handler)
	return db, r
}

func TestAdminAuth_Unauthorized(t *testing.T) {
	_, r := setupAdminTestRouter(t)

	// 没有 Bearer Token
	req := httptest.NewRequest("GET", "/api/v1/admin/wallets", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAdminAuth_UsesSeparateSecret(t *testing.T) {
	_, r := setupAdminTestRouter(t)
	req := httptest.NewRequest("GET", "/api/v1/admin/wallets", nil)
	req.Header.Set("Authorization", "Bearer service-test-secret-123456")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAdmin_WalletCRUD(t *testing.T) {
	db, r := setupAdminTestRouter(t)

	// 1. Create Wallet with invalid address -> 400
	invalidBody, _ := json.Marshal(map[string]interface{}{
		"chain":   "ARBITRUM",
		"address": "invalid-address-format",
		"label":   "Arb Wallet",
	})
	req := httptest.NewRequest("POST", "/api/v1/admin/wallets", bytes.NewBuffer(invalidBody))
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// 2. Create Wallet with valid address -> 200
	validBody, _ := json.Marshal(map[string]interface{}{
		"chain":   "ARBITRUM",
		"address": "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		"label":   "Arbitrum Hot 1",
		"weight":  10,
	})
	req = httptest.NewRequest("POST", "/api/v1/admin/wallets", bytes.NewBuffer(validBody))
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var createdResp struct {
		Code int                 `json:"code"`
		Data model.WalletAddress `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &createdResp))
	assert.Equal(t, "ARBITRUM", string(createdResp.Data.Chain))
	walletID := createdResp.Data.ID

	// 3. List Wallets -> 200
	req = httptest.NewRequest("GET", "/api/v1/admin/wallets?chain=ARBITRUM", nil)
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 4. Update Wallet -> 200
	falseVal := false
	newWeight := 50
	newLabel := "Updated Arb Hot 1"
	updateBody, _ := json.Marshal(map[string]interface{}{
		"label":   newLabel,
		"weight":  newWeight,
		"enabled": falseVal,
	})
	req = httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/admin/wallets/%d", walletID), bytes.NewBuffer(updateBody))
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 5. Delete Wallet -> 200 (no active intents)
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/admin/wallets/%d", walletID), nil)
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify deleted
	var count int64
	db.Model(&model.WalletAddress{}).Where("id = ?", walletID).Count(&count)
	assert.Equal(t, int64(0), count)
}

func TestAdmin_TokenCRUD(t *testing.T) {
	db, r := setupAdminTestRouter(t)

	// 1. Create Token -> 200
	body, _ := json.Marshal(map[string]interface{}{
		"chain":    "ARBITRUM",
		"symbol":   "USDT",
		"name":     "Tether USD",
		"contract": "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9",
		"decimals": 6,
		"priority": 100,
	})
	req := httptest.NewRequest("POST", "/api/v1/admin/tokens", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var createdTokenResp struct {
		Code int              `json:"code"`
		Data model.ChainToken `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &createdTokenResp))
	tokenID := createdTokenResp.Data.ID

	// 2. List Tokens -> 200
	req = httptest.NewRequest("GET", "/api/v1/admin/tokens?chain=ARBITRUM", nil)
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 3. Update Token -> 200
	updateBody, _ := json.Marshal(map[string]interface{}{
		"priority": 200,
		"badge":    "热门推荐",
	})
	req = httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/admin/tokens/%d", tokenID), bytes.NewBuffer(updateBody))
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 4. Delete Token -> 200
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/admin/tokens/%d", tokenID), nil)
	req.Header.Set("Authorization", "Bearer admin-test-secret-123456")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var count int64
	db.Model(&model.ChainToken{}).Where("id = ?", tokenID).Count(&count)
	assert.Equal(t, int64(0), count)
}
