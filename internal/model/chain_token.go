package model

import "time"

// ChainToken 代币合约白名单与显示元数据模型
type ChainToken struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Chain     Chain     `gorm:"size:32;not null;uniqueIndex:idx_token_chain_symbol,priority:1;index:idx_token_chain_enabled,priority:1" json:"chain"`
	Symbol    Token     `gorm:"size:32;not null;uniqueIndex:idx_token_chain_symbol,priority:2" json:"symbol"`  // 如: "USDT", "USDC", "ETH", "SOL"
	Name      string    `gorm:"size:64;not null" json:"name"`                                                  // 如: "Tether USD", "USD Coin"
	Contract  string    `gorm:"size:128;index" json:"contract"`                                                // 智能合约地址，原生公链代币为空字符串
	Decimals  int       `gorm:"not null;default:6" json:"decimals"`                                            // 链上代币真实精度（如 6, 18, 9）
	IsNative  bool      `gorm:"not null;default:false" json:"isNative"`                                        // 是否为主网原生币（如 ETH, SOL, BNB）
	Icon      string    `gorm:"size:128" json:"icon,omitempty"`                                                // 前端图标 CSS class 或 URL
	Badge     string    `gorm:"size:64" json:"badge,omitempty"`                                                // 前端标签 (如 "低手续费 / 推荐")
	Priority  int       `gorm:"default:0" json:"priority"`                                                     // 展示排序权重（数值越大越靠前）
	Enabled   bool      `gorm:"not null;default:true;index:idx_token_chain_enabled,priority:2" json:"enabled"` // 是否启用监听与接收
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt time.Time `gorm:"not null" json:"updatedAt"`
}

func (ChainToken) TableName() string {
	return "chain_tokens"
}

// GetDefaultChainTokens 提供平台初始预设代币清单
func GetDefaultChainTokens() []ChainToken {
	return []ChainToken{
		// TRON
		{Chain: ChainTron, Symbol: TokenUSDT, Name: "Tether USD", Contract: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "低手续费 / 推荐", Priority: 100, Enabled: true},
		{Chain: ChainTron, Symbol: TokenUSDC, Name: "USD Coin", Contract: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Priority: 90, Enabled: true},

		// ARBITRUM
		{Chain: ChainArbitrum, Symbol: TokenUSDC, Name: "USD Coin", Contract: "0xaf88d065e77c8cc2239327c5edb3a432268e5831", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "极速 / 低Gas", Priority: 100, Enabled: true},
		{Chain: ChainArbitrum, Symbol: TokenUSDT, Name: "Tether USD", Contract: "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "极速 / 低Gas", Priority: 95, Enabled: true},
		{Chain: ChainArbitrum, Symbol: TokenETH, Name: "Ethereum", Contract: "", Decimals: 18, IsNative: true, Icon: "fa-brands fa-ethereum", Priority: 80, Enabled: true},

		// BSC
		{Chain: ChainBsc, Symbol: TokenUSDT, Name: "Tether USD", Contract: "0x55d398326f99059ff775485246999027b3197955", Decimals: 18, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "高吞吐", Priority: 100, Enabled: true},
		{Chain: ChainBsc, Symbol: TokenUSDC, Name: "USD Coin", Contract: "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", Decimals: 18, Icon: "fa-solid fa-circle-dollar-to-slot", Priority: 90, Enabled: true},
		{Chain: ChainBsc, Symbol: TokenBNB, Name: "BNB", Contract: "", Decimals: 18, IsNative: true, Icon: "fa-solid fa-coins", Priority: 80, Enabled: true},

		// ETH
		{Chain: ChainEth, Symbol: TokenUSDT, Name: "Tether USD", Contract: "0xdac17f958d2ee523a2206206994597c13d831ec7", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "主网原生", Priority: 100, Enabled: true},
		{Chain: ChainEth, Symbol: TokenUSDC, Name: "USD Coin", Contract: "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Priority: 90, Enabled: true},
		{Chain: ChainEth, Symbol: TokenETH, Name: "Ethereum", Contract: "", Decimals: 18, IsNative: true, Icon: "fa-brands fa-ethereum", Priority: 80, Enabled: true},

		// POLYGON
		{Chain: ChainPolygon, Symbol: TokenUSDT, Name: "Tether USD", Contract: "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "低费率", Priority: 100, Enabled: true},
		{Chain: ChainPolygon, Symbol: TokenUSDC, Name: "USD Coin", Contract: "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Priority: 90, Enabled: true},

		// SOLANA
		{Chain: ChainSolana, Symbol: TokenUSDT, Name: "Tether USD", Contract: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "极速", Priority: 100, Enabled: true},
		{Chain: ChainSolana, Symbol: TokenUSDC, Name: "USD Coin", Contract: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Decimals: 6, Icon: "fa-solid fa-circle-dollar-to-slot", Badge: "极速", Priority: 95, Enabled: true},
		{Chain: ChainSolana, Symbol: "SOL", Name: "Solana", Contract: "", Decimals: 9, IsNative: true, Icon: "fa-solid fa-sun", Priority: 80, Enabled: true},
	}
}
