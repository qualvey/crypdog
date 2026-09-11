package scanner

import (
	"math/big"
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
