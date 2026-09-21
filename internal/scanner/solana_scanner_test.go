package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	err = db.AutoMigrate(&model.PaymentIntent{}, &model.WalletAddress{})
	require.NoError(t, err)
	return db
}

func TestNewSolanaScanner(t *testing.T) {
	cfg := &config.ChainNodeConfig{
		RPCURL: "https://api.mainnet-beta.solana.com",
		Tokens: []model.TokenSpec{
			{
				Symbol:     model.Token("BONK"),
				Identifier: "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263",
				Decimals:   5,
			},
		},
	}

	sc, err := NewSolanaScanner(nil, cfg)
	require.NoError(t, err)
	assert.Equal(t, model.ChainSolana, sc.Chain())
	assert.Equal(t, uint64(0), sc.GetLatestBlock())

	tokens := sc.SupportedTokens()
	// 默认 2 个 (USDT, USDC) + 自定义 1 个 (BONK) = 3
	assert.Equal(t, 3, len(tokens))

	tokenMap := make(map[string]model.TokenSpec)
	for _, tk := range tokens {
		tokenMap[tk.Identifier] = tk
	}
	assert.Equal(t, model.TokenUSDT, tokenMap["Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"].Symbol)
	assert.Equal(t, model.TokenUSDC, tokenMap["EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"].Symbol)
	assert.Equal(t, model.Token("BONK"), tokenMap["DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"].Symbol)
}

func TestSolanaScanner_ExtractIncomingTransfers(t *testing.T) {
	targetAddr := "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"
	usdtMint := "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	usdcMint := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	unknownMint := "CustomTokenMint111111111111111111111111111"

	scanner := &SolanaScanner{
		tokens: map[string]model.TokenSpec{
			usdtMint: {Identifier: usdtMint, Symbol: model.TokenUSDT, Decimals: 6},
			usdcMint: {Identifier: usdcMint, Symbol: model.TokenUSDC, Decimals: 6},
		},
	}

	t.Run("nil交易对象返回空", func(t *testing.T) {
		transfers := scanner.extractIncomingTransfers(nil, targetAddr)
		assert.Nil(t, transfers)
	})

	t.Run("正常充值入账增量计算", func(t *testing.T) {
		tx := &solTx{
			Slot:      250000000,
			BlockTime: 1700000000,
		}
		tx.Transaction.Signatures = []string{"5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUc"}
		// Pre: 10.5 USDT
		tx.Meta.PreTokenBalances = []tokenBalance{
			{
				AccountIndex: 1,
				Mint:         usdtMint,
				Owner:        targetAddr,
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "10500000",
					Decimals:       6,
					UiAmountString: "10.5",
				},
			},
		}
		// Post: 25.5 USDT (充值 15 USDT)
		tx.Meta.PostTokenBalances = []tokenBalance{
			{
				AccountIndex: 1,
				Mint:         usdtMint,
				Owner:        targetAddr,
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "25500000",
					Decimals:       6,
					UiAmountString: "25.5",
				},
			},
		}

		transfers := scanner.extractIncomingTransfers(tx, targetAddr)
		require.Len(t, transfers, 1)

		tr := transfers[0]
		assert.Equal(t, tx.Transaction.Signatures[0], tr.TxHash)
		assert.Equal(t, model.ChainSolana, tr.Chain)
		assert.Equal(t, int64(1), tr.LogIndex)
		assert.Equal(t, usdtMint, tr.Contract)
		assert.Equal(t, targetAddr, tr.TargetAddress)
		assert.Equal(t, model.TokenUSDT, tr.Token)
		assert.True(t, tr.Amount.Equal(decimal.NewFromFloat(15.0)))
		assert.Equal(t, "15000000", tr.RawValue)
		assert.Equal(t, uint64(250000000), tr.BlockNumber)
		assert.Equal(t, int64(1700000000), tr.BlockTimestamp)
		assert.Equal(t, uint8(6), tr.Decimals)
	})

	t.Run("首次入账（Pre 中无记录，从无到有）", func(t *testing.T) {
		tx := &solTx{
			Slot:      250000001,
			BlockTime: 1700000001,
		}
		tx.Transaction.Signatures = []string{"sig_first_time"}
		tx.Meta.PostTokenBalances = []tokenBalance{
			{
				AccountIndex: 2,
				Mint:         usdcMint,
				Owner:        targetAddr,
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "100000000",
					Decimals:       6,
					UiAmountString: "100",
				},
			},
		}

		transfers := scanner.extractIncomingTransfers(tx, targetAddr)
		require.Len(t, transfers, 1)
		assert.Equal(t, model.TokenUSDC, transfers[0].Token)
		assert.True(t, transfers[0].Amount.Equal(decimal.NewFromFloat(100.0)))
	})

	t.Run("非目标地址和转出交易被过滤", func(t *testing.T) {
		tx := &solTx{
			Slot: 250000002,
		}
		tx.Transaction.Signatures = []string{"sig_filtered"}
		tx.Meta.PreTokenBalances = []tokenBalance{
			{
				AccountIndex: 1,
				Mint:         usdtMint,
				Owner:        targetAddr,
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "50000000",
					Decimals:       6,
					UiAmountString: "50",
				},
			},
		}
		tx.Meta.PostTokenBalances = []tokenBalance{
			// 1. 他人账户充值
			{
				AccountIndex: 0,
				Mint:         usdtMint,
				Owner:        "OtherAddress1111111111111111111111111111111",
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "999000000",
					Decimals:       6,
					UiAmountString: "999",
				},
			},
			// 2. 目标账户转出（50 -> 20，减少）
			{
				AccountIndex: 1,
				Mint:         usdtMint,
				Owner:        targetAddr,
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "20000000",
					Decimals:       6,
					UiAmountString: "20",
				},
			},
		}

		transfers := scanner.extractIncomingTransfers(tx, targetAddr)
		assert.Empty(t, transfers, "他人充值和目标账户转出不应产生入账记录")
	})

	t.Run("未知 Token Mint 自动回退为 Mint 字符串", func(t *testing.T) {
		tx := &solTx{
			Slot: 250000003,
		}
		tx.Transaction.Signatures = []string{"sig_unknown"}
		tx.Meta.PostTokenBalances = []tokenBalance{
			{
				AccountIndex: 3,
				Mint:         unknownMint,
				Owner:        targetAddr,
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "500",
					Decimals:       2,
					UiAmountString: "5.0",
				},
			},
		}

		transfers := scanner.extractIncomingTransfers(tx, targetAddr)
		require.Len(t, transfers, 1)
		assert.Equal(t, model.Token(unknownMint), transfers[0].Token)
		assert.True(t, transfers[0].Amount.Equal(decimal.NewFromFloat(5.0)))
	})

	t.Run("Base58地址大小写严格区分（不同大小写不应匹配）", func(t *testing.T) {
		tx := &solTx{
			Slot: 250000004,
		}
		tx.Transaction.Signatures = []string{"sig_case_test"}
		tx.Meta.PostTokenBalances = []tokenBalance{
			{
				AccountIndex: 1,
				Mint:         usdtMint,
				// 仅将 targetAddr 变更为小写（在 Base58 中属于不同地址）
				Owner: strings.ToLower(targetAddr),
				UiTokenAmount: struct {
					Amount         string `json:"amount"`
					Decimals       int    `json:"decimals"`
					UiAmountString string `json:"uiAmountString"`
				}{
					Amount:         "15000000",
					Decimals:       6,
					UiAmountString: "15.0",
				},
			},
		}

		transfers := scanner.extractIncomingTransfers(tx, targetAddr)
		assert.Empty(t, transfers, "Base58 区分大小写，不同大小写的地址不应匹配")
	})
}

func TestSolanaScanner_GetActiveTargetAddresses(t *testing.T) {
	db := setupTestDB(t)

	// 插入多种状态和链的 WalletAddress
	wallets := []model.WalletAddress{
		{
			Chain:   model.ChainSolana,
			Address: "SolanaAddressA",
			Enabled: true,
		},
		{
			Chain:   model.ChainSolana,
			Address: "SolanaAddressB",
			Enabled: true,
		},
		{
			Chain:   model.ChainSolana,
			Address: "SolanaAddressC",
			Enabled: false, // 已禁用，不应被检索
		},
		{
			Chain:   model.ChainEth, // 非 Solana 链
			Address: "EthAddressD",
			Enabled: true,
		},
	}
	for i := range wallets {
		require.NoError(t, db.Create(&wallets[i]).Error)
	}
	require.NoError(t, db.Model(&model.WalletAddress{}).Where("address = ?", "SolanaAddressC").Update("enabled", false).Error)

	scanner := &SolanaScanner{db: db}
	addrs := scanner.getActiveTargetAddresses()

	assert.ElementsMatch(t, []string{"SolanaAddressA", "SolanaAddressB"}, addrs)
}

func TestSolanaScanner_RPCMethods(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "getSlot":
			resp := rpcRes{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`265123456`),
			}
			json.NewEncoder(w).Encode(resp)

		case "getSignaturesForAddress":
			resp := rpcRes{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`[
					{"signature": "sig1", "slot": 265123455, "err": null},
					{"signature": "sig2", "slot": 265123450, "err": {"InstructionError": [0, "Custom"]}}
				]`),
			}
			json.NewEncoder(w).Encode(resp)

		case "getTransaction":
			params, _ := req.Params.([]interface{})
			sig, _ := params[0].(string)
			if sig == "sig_null" {
				resp := rpcRes{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  json.RawMessage(`null`),
				}
				json.NewEncoder(w).Encode(resp)
				return
			}

			txJSON := `{
				"transaction": {
					"signatures": ["sig1"]
				},
				"slot": 265123455,
				"blockTime": 1710000000,
				"meta": {
					"preTokenBalances": [],
					"postTokenBalances": [
						{
							"accountIndex": 1,
							"mint": "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
							"owner": "TargetSolAddr",
							"uiTokenAmount": {
								"amount": "10000000",
								"decimals": 6,
								"uiAmountString": "10"
							}
						}
					]
				}
			}`
			resp := rpcRes{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(txJSON),
			}
			json.NewEncoder(w).Encode(resp)

		case "errorMethod":
			resp := rpcRes{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &rpcErr{
					Code:    -32000,
					Message: "Node is behind",
				},
			}
			json.NewEncoder(w).Encode(resp)

		default:
			http.Error(w, "method not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	scanner := &SolanaScanner{
		client: server.Client(),
		rpcURL: server.URL,
	}
	ctx := context.Background()

	t.Run("getSlot 成功返回 slot", func(t *testing.T) {
		slot, err := scanner.getSlot(ctx)
		require.NoError(t, err)
		assert.Equal(t, uint64(265123456), slot)
	})

	t.Run("refreshLatestSlot 更新 latestBlock", func(t *testing.T) {
		err := scanner.refreshLatestSlot(ctx)
		require.NoError(t, err)
		assert.Equal(t, uint64(265123456), scanner.GetLatestBlock())
	})

	t.Run("getSignaturesForAddress 成功解析", func(t *testing.T) {
		sigs, err := scanner.getSignaturesForAddress(ctx, "TargetSolAddr", "sig_until")
		require.NoError(t, err)
		require.Len(t, sigs, 2)
		assert.Equal(t, "sig1", sigs[0].Signature)
		assert.Nil(t, sigs[0].Err)
		assert.Equal(t, "sig2", sigs[1].Signature)
		assert.NotNil(t, sigs[1].Err)
	})

	t.Run("getTransaction 成功解析与 null 容错", func(t *testing.T) {
		tx, err := scanner.getTransaction(ctx, "sig1")
		require.NoError(t, err)
		require.NotNil(t, tx)
		assert.Equal(t, uint64(265123455), tx.Slot)
		assert.Equal(t, "sig1", tx.Transaction.Signatures[0])

		nullTx, err := scanner.getTransaction(ctx, "sig_null")
		require.NoError(t, err)
		assert.Nil(t, nullTx)
	})

	t.Run("callRPC 错误响应处理", func(t *testing.T) {
		_, err := scanner.callRPC(ctx, "errorMethod", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "RPC error: Node is behind")
	})
}

func TestSolanaScanner_ScanSingleAddress(t *testing.T) {
	targetAddr := "TargetSolAddr"
	usdtMint := "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case "getSignaturesForAddress":
			resp := rpcRes{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`[
					{"signature": "sig_valid", "slot": 100, "err": null},
					{"signature": "sig_failed", "slot": 99, "err": {"InstructionError": [0, "Custom"]}}
				]`),
			}
			json.NewEncoder(w).Encode(resp)

		case "getTransaction":
			params, _ := req.Params.([]interface{})
			sig, _ := params[0].(string)
			if sig == "sig_valid" {
				txJSON := `{
					"transaction": {"signatures": ["sig_valid"]},
					"slot": 100,
					"blockTime": 1710000000,
					"meta": {
						"preTokenBalances": [],
						"postTokenBalances": [
							{
								"accountIndex": 1,
								"mint": "` + usdtMint + `",
								"owner": "` + targetAddr + `",
								"uiTokenAmount": {
									"amount": "20000000",
									"decimals": 6,
									"uiAmountString": "20"
								}
							}
						]
					}
				}`
				resp := rpcRes{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  json.RawMessage(txJSON),
				}
				json.NewEncoder(w).Encode(resp)
				return
			}
			resp := rpcRes{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`null`)}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer server.Close()

	scanner := &SolanaScanner{
		client:         server.Client(),
		rpcURL:         server.URL,
		lastSignatures: make(map[string]string),
		tokens: map[string]model.TokenSpec{
			usdtMint: {Identifier: usdtMint, Symbol: model.TokenUSDT, Decimals: 6},
		},
	}

	transferChan := make(chan model.ChainTransfer, 10)
	ctx := context.Background()

	scanner.scanSingleAddress(ctx, targetAddr, transferChan)

	// 1. 验证最新的签名位点被记录
	assert.Equal(t, "sig_valid", scanner.lastSignatures[targetAddr])

	// 2. 验证有效交易被投递到 channel
	select {
	case tr := <-transferChan:
		assert.Equal(t, "sig_valid", tr.TxHash)
		assert.Equal(t, targetAddr, tr.TargetAddress)
		assert.True(t, tr.Amount.Equal(decimal.NewFromFloat(20.0)))
	case <-time.After(1 * time.Second):
		t.Fatal("未在规定时间内收到转账事件")
	}

	// 3. 失败交易 sig_failed 应被跳过，channel 中不应有额外事件
	assert.Equal(t, 0, len(transferChan))
}

func TestSolanaScanner_Start_ContextCancel(t *testing.T) {
	cfg := &config.ChainNodeConfig{
		ScanIntervalSec: 1,
		RPCURL:          "http://127.0.0.1:9999",
	}
	db := setupTestDB(t)

	sc, err := NewSolanaScanner(db, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	// 立即或短暂延时后取消
	time.AfterFunc(100*time.Millisecond, cancel)

	transferChan := make(chan model.ChainTransfer, 1)
	err = sc.Start(ctx, transferChan)
	assert.ErrorIs(t, err, context.Canceled)
}
