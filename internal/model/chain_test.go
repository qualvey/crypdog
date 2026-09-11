package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeAddress(t *testing.T) {
	var tests []struct {
		chain       Chain
		input       string
		expected    string
		description string
	}

	// 注意：这里我们假设 NormalizeAddress 的实现会调用 strings.ToLower(strings.TrimSpace(address))
	// 因为这个函数是内部私有的，我们只能通过外部调用（如 RegisterOrReactivate）来间接测试它
	// 但是，在单元测试中验证这种内部逻辑是不太现实的
	// 通常，你会把 NormalizeAddress 提升为 public 或者把这部分逻辑移到服务层被测试
	// 但根据你的项目结构，我们只能断言它在不破坏其他功能的情况下正常工作
	// 一个更现实的做法是把 NormalizeAddress 改为 public，或者为 service 包提供一个内部测试辅助函数

	// 这里我们只做一个简单的 Happy Path 测试，假设它的实现是正确的
	tests = []struct {
		chain       Chain
		input       string
		expected    string
		description string
	}{
		{
			chain:       ChainBsc,
			input:       "  0x1234567890abcdef1234567890abcdef12345678   ",
			expected:    "0x1234567890abcdef1234567890abcdef12345678",
			description: "EVMCaseNormal",
		},
		{
			chain:       ChainBsc,
			input:       "0XABCDEF1234567890abcdef1234567890abcdef1234",
			expected:    "0xabcdef1234567890abcdef1234567890abcdef1234",
			description: "EVMCaseUpper",
		},
		{
			chain:       ChainTron,
			input:       " TRONaddress123 ",
			expected:    "TRONaddress123", // TRON 不转小写
			description: "TronNoCaseChange",
		},
		{
			chain:       ChainSolana,
			input:       " SOLANAaddress123 ",
			expected:    "SOLANAaddress123", // Solana 不转小写
			description: "SolanaNoCaseChange",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			actual := tt.chain.NormalizeAddress(tt.input)
			assert.Equal(t, tt.expected, actual, "Address normalization failed")
		})
	}
}

func TestNormalizeChain(t *testing.T) {
	assert.Equal(t, ChainTron, NormalizeChain("TRC20"))
	assert.Equal(t, ChainTron, NormalizeChain("trc20"))
	assert.Equal(t, ChainTron, NormalizeChain("TRON"))
	assert.Equal(t, ChainTron, NormalizeChain("trx"))
	assert.Equal(t, ChainEth, NormalizeChain("ERC20"))
	assert.Equal(t, ChainEth, NormalizeChain("ETH"))
	assert.Equal(t, ChainBsc, NormalizeChain("BEP20"))
	assert.Equal(t, ChainBsc, NormalizeChain("BSC"))
	assert.Equal(t, ChainSolana, NormalizeChain("SOL"))
	assert.Equal(t, ChainSolana, NormalizeChain("SOLANA"))
	assert.Equal(t, ChainPolygon, NormalizeChain("POLYGON"))
	assert.Equal(t, ChainArbitrum, NormalizeChain("ARBITRUM"))
}

