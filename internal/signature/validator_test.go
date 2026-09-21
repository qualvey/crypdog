package signature

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateWebhookURL(t *testing.T) {
	// 1. Valid URLs
	assert.NoError(t, ValidateWebhookURL("https://api.example.com/webhook", false))
	assert.NoError(t, ValidateWebhookURL("http://api.example.com:8080/v1/webhook", false))

	// 2. Disallowed schemes
	assert.ErrorIs(t, ValidateWebhookURL("ftp://api.example.com/webhook", false), ErrDisallowedScheme)
	assert.ErrorIs(t, ValidateWebhookURL("file:///etc/passwd", false), ErrDisallowedScheme)
	assert.ErrorIs(t, ValidateWebhookURL("gopher://example.com", false), ErrDisallowedScheme)

	// 3. Metadata IPs
	assert.Error(t, ValidateWebhookURL("http://169.254.169.254/latest/meta-data/", false))
	assert.Error(t, ValidateWebhookURL("http://169.254.169.254/latest/meta-data/", true))

	// 4. Unspecified / 0.0.0.0
	assert.Error(t, ValidateWebhookURL("http://0.0.0.0:8080/webhook", false))

	// 5. Localhost with allowLocal flag
	assert.Error(t, ValidateWebhookURL("http://127.0.0.1:8080/webhook", false))
	assert.NoError(t, ValidateWebhookURL("http://127.0.0.1:8080/webhook", true))
	assert.Error(t, ValidateWebhookURL("http://localhost:8080/webhook", false))
	assert.NoError(t, ValidateWebhookURL("http://localhost:8080/webhook", true))

	// 6. Private IP subnets (RFC 1918)
	assert.Error(t, ValidateWebhookURL("http://10.0.0.5:8080/webhook", false))
	assert.Error(t, ValidateWebhookURL("http://172.16.1.100/webhook", false))
	assert.Error(t, ValidateWebhookURL("http://192.168.1.1/admin", false))
}

func TestValidateChainAddress(t *testing.T) {
	// EVM: valid
	assert.NoError(t, ValidateChainAddress("ETH", "0x7BDc49542978B16566e82c8f90DB1EB03804C675"))
	assert.NoError(t, ValidateChainAddress("BSC", "0x71C83664204CB356015E178d721978E2b0C8976F"))
	assert.NoError(t, ValidateChainAddress("ARBITRUM", "0xaf88d065e77c8cc2239327c5edb3a432268e5831"))
	// EVM: invalid
	assert.Error(t, ValidateChainAddress("BSC", "0x123"))
	assert.Error(t, ValidateChainAddress("BSC", "71C83664204CB356015E178d721978E2b0C8976F"))
	assert.Error(t, ValidateChainAddress("BSC", "0x71C83664204CB356015E178d721978E2b0C8976Z")) // non-hex

	// TRON: valid (USDT and USDC contract addresses & real wallet address)
	assert.NoError(t, ValidateChainAddress("TRON", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"))
	assert.NoError(t, ValidateChainAddress("TRON", "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"))
	assert.NoError(t, ValidateChainAddress("TRC20", "TWLp7W7umCmLYwLxm8hwndo6B9p6tsshzh"))
	// TRON: invalid
	assert.Error(t, ValidateChainAddress("TRON", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6x")) // bad checksum
	assert.Error(t, ValidateChainAddress("TRON", "AR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")) // not starting with T
	assert.Error(t, ValidateChainAddress("TRON", "TR7NHqjeKQxGTCi8"))                   // short

	// SOLANA: valid
	assert.NoError(t, ValidateChainAddress("SOLANA", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"))
	assert.NoError(t, ValidateChainAddress("SOL", "v9nSdKzovPHyTY2AbtSZotM6qStdDvQ4Jt1ht5S9BW1"))
	// SOLANA: invalid
	assert.Error(t, ValidateChainAddress("SOLANA", "short"))
	assert.Error(t, ValidateChainAddress("SOLANA", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNY0000000000000000000000000000")) // too long
}
