package service

import (
	"testing"

	"crypdog/internal/model"

	"github.com/stretchr/testify/assert"
)

func TestBuildPaymentOptionsUsesConsistentChainOrder(t *testing.T) {
	options := buildPaymentOptions(
		map[string]bool{
			string(model.ChainArbitrum): true,
			string(model.ChainSolana):   true,
		},
		[]model.ChainToken{
			// Intentionally interleave chains between tokens. Database priority
			// order must not determine the order inside each token's chain list.
			{Chain: model.ChainSolana, Symbol: model.TokenUSDC, Decimals: 6},
			{Chain: model.ChainArbitrum, Symbol: model.TokenUSDC, Decimals: 6},
			{Chain: model.ChainSolana, Symbol: model.TokenUSDT, Decimals: 6},
			{Chain: model.ChainArbitrum, Symbol: model.TokenUSDT, Decimals: 6},
		},
	)

	assert.Equal(t, model.ChainArbitrum, options.Chains["USDC"][0].Chain)
	assert.Equal(t, model.ChainArbitrum, options.Chains["USDT"][0].Chain)
	assert.Equal(t, model.ChainSolana, options.Chains["USDC"][1].Chain)
	assert.Equal(t, model.ChainSolana, options.Chains["USDT"][1].Chain)
}
