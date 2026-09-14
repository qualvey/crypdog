package model

type TokenSpec struct {
	Symbol     Token  `yaml:"symbol"`        // 例如 "USDT", "ETH"
	Decimals   int    `yaml:"decimals"`      // 真实精度，例如 6, 18
	Identifier string `yaml:"token_address"` // 合约地址，原生代币为空
	IsNative   bool   `yaml:"is_native"`     // 是否为原生公链币
}
