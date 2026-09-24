package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/model"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTronScanner_RejectFakeUSDTToken(t *testing.T) {
	// 构造 Mock TronGrid API
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := TronTRC20Resp{
			Success: true,
			Data: []TronTRC20Tx{
				// 1. 假代币攻击交易：Symbol 叫 "USDT"，但合约地址是伪造的
				{
					TransactionID: "0x_fake_usdt_tx",
					TokenInfo: struct {
						Symbol   model.Token `json:"symbol"`
						Address  string      `json:"address"`
						Decimals int32       `json:"decimals"`
						Name     string      `json:"name"`
					}{
						Symbol:   model.TokenUSDT,
						Address:  "TFakeContractAddress1234567890abcdef",
						Decimals: 6,
						Name:     "Tether USD (Scam)",
					},
					BlockTimestamp: 1700000000000,
					BlockNumber:    60000001,
					From:           "TAttackerAddress123456",
					To:             "TTargetMerchantAddress123456",
					Type:           "Transfer",
					Value:          "100000000", // 100 USDT
				},
				// 2. 正版 USDT 交易：官方合约 TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
				{
					TransactionID: "0x_real_usdt_tx",
					TokenInfo: struct {
						Symbol   model.Token `json:"symbol"`
						Address  string      `json:"address"`
						Decimals int32       `json:"decimals"`
						Name     string      `json:"name"`
					}{
						Symbol:   model.TokenUSDT,
						Address:  "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
						Decimals: 6,
						Name:     "Tether USD",
					},
					BlockTimestamp: 1700000010000,
					BlockNumber:    60000002,
					From:           "TGoodUserAddress123456",
					To:             "TTargetMerchantAddress123456",
					Type:           "Transfer",
					Value:          "100000000", // 100 USDT
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	cfg := &config.ChainNodeConfig{
		RPCURL: mockServer.URL,
	}

	sc, err := NewTronScanner(nil, cfg)
	require.NoError(t, err)

	tronScanner := sc.(*TronScanner)
	transferChan := make(chan model.ChainTransfer, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tronScanner.scanSingleAddress(ctx, mockServer.URL, "TTargetMerchantAddress123456", transferChan)

	// 验证：假代币应当被拦截过滤，只有 1 笔真实 USDT 入账
	assert.Equal(t, 1, len(transferChan))
	captured := <-transferChan
	assert.Equal(t, "0x_real_usdt_tx", captured.TxHash)
	assert.Equal(t, model.TokenUSDT, captured.Token)
	assert.Equal(t, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", captured.Contract)
	assert.True(t, captured.Amount.Equal(decimal.NewFromInt(100)))
}

func TestTronScanner_FollowsPagination(t *testing.T) {
	var requests int
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("fingerprint") == "next-page" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data": []map[string]interface{}{{
					"transaction_id":  "tx-page-51",
					"block_timestamp": int64(1700000051000),
					"block_number":    60000051,
					"from":            "TFrom",
					"to":              "TTarget",
					"value":           "100000000",
					"token_info": map[string]interface{}{
						"symbol": "USDT", "address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "decimals": 6,
					},
				}},
			})
			return
		}

		data := make([]map[string]interface{}, 50)
		for i := range data {
			data[i] = map[string]interface{}{
				"transaction_id":  fmt.Sprintf("tx-page-%d", i),
				"block_timestamp": int64(1700000000000 + i*1000),
				"block_number":    60000000 + i,
				"from":            "TFrom",
				"to":              "TTarget",
				"value":           "100000000",
				"token_info": map[string]interface{}{
					"symbol": "USDT", "address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "decimals": 6,
				},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    data,
			"meta":    map[string]string{"fingerprint": "next-page"},
		})
	}))
	defer mockServer.Close()

	sc, err := NewTronScanner(nil, &config.ChainNodeConfig{RPCURL: mockServer.URL})
	require.NoError(t, err)
	transferChan := make(chan model.ChainTransfer, 51)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sc.(*TronScanner).scanSingleAddress(ctx, mockServer.URL, "TTarget", transferChan)
	assert.Equal(t, 2, requests)
	assert.Len(t, transferChan, 51)
}
