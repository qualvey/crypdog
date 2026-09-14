package scanner

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypdog/internal/model"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func TestParseAddressFromTopic(t *testing.T) {
	topic := "0x0000000000000000000000007bdc49542978b16566e82c8f90db1eb03804c675"
	expected := "0x7bdc49542978b16566e82c8f90db1eb03804c675"
	assert.Equal(t, expected, parseAddressFromTopic(topic))
}

func TestEvmLogParsing_KnownTokensAndDecimals(t *testing.T) {
	chain := "ARBITRUM"
	usdcContract := "0xaf88d065e77c8cc2239327c5edb3a432268e5831"

	chainTokens, hasChain := knownTokens[chain]
	assert.True(t, hasChain)

	tokenMeta, isKnown := chainTokens[strings.ToLower(usdcContract)]
	assert.True(t, isKnown)
	assert.Equal(t, model.TokenUSDC, tokenMeta.Symbol)
	assert.Equal(t, 6, tokenMeta.Decimals)

	// Case 1: 真实支付金额 150100 rawValue (16进制 0x24a54) -> 0.1501 USDC
	amountBig := new(big.Int)
	amountBig.SetString("24a54", 16)
	assert.Equal(t, int64(150100), amountBig.Int64())

	valDec, _ := decimal.NewFromString(amountBig.String())
	readableAmount := valDec.Div(decimal.New(1, int32(tokenMeta.Decimals)))
	assert.Equal(t, "0.1501", readableAmount.String())

	// Case 2: 投毒微小金额 15 rawValue (16进制 0xf) -> 0.000015 USDC
	dustBig := new(big.Int)
	dustBig.SetString("f", 16)
	dustDec, _ := decimal.NewFromString(dustBig.String())
	dustAmount := dustDec.Div(decimal.New(1, int32(tokenMeta.Decimals)))
	assert.Equal(t, "0.000015", dustAmount.String())

	// Case 3: 未知合约直接拒绝
	unknownContract := "0x1111111111111111111111111111111111111111"
	_, isUnknownMatched := chainTokens[unknownContract]
	assert.False(t, isUnknownMatched)
}

func TestEvmRPCClient_GetBlockTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		assert.NoError(t, err)

		if req.Method == "eth_getBlockByNumber" {
			// 模拟返回区块时间戳 1700000000 (0x6553f100)
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"timestamp": "0x6553f100",
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := NewEvmRPCClient(server.URL)
	ts, err := client.GetBlockTimestamp(context.Background(), 123456)
	assert.NoError(t, err)
	assert.Equal(t, int64(1700000000), ts)
}

func TestEvmScanner_BlockTimestampCaching(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Method == "eth_getBlockByNumber" {
			callCount++
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"timestamp": "0x6553f100", // 1700000000
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	scanner := &EvmScanner{
		chain:          model.ChainArbitrum,
		client:         *NewEvmRPCClient(server.URL),
		blockTimeCache: make(map[uint64]int64),
	}

	// 第一次调用：缓存未命中，调用 RPC
	ts1 := scanner.getBlockTimestamp(context.Background(), 99999)
	assert.Equal(t, int64(1700000000), ts1)
	assert.Equal(t, 1, callCount)

	// 第二次调用同高度：命中缓存，不应调用 RPC
	ts2 := scanner.getBlockTimestamp(context.Background(), 99999)
	assert.Equal(t, int64(1700000000), ts2)
	assert.Equal(t, 1, callCount, "同一个区块时间戳应走缓存，不应重复请求 RPC")
}
