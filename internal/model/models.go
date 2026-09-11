package model

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type IntentStatus string

const (
	StatusWatching    IntentStatus = "WATCHING"
	StatusConfirming  IntentStatus = "CONFIRMING"
	StatusPaid        IntentStatus = "PAID"
	StatusPartialPaid IntentStatus = "PARTIAL_PAID"
	StatusExpired     IntentStatus = "EXPIRED"
	StatusCancelled   IntentStatus = "CANCELLED"
)

// PaymentIntent represents a registered payment monitoring task
type PaymentIntent struct {
	ID            string `gorm:"primaryKey;size:64" json:"intentId"`
	OrderID       string `gorm:"uniqueIndex;size:128;not null" json:"orderId"`
	TxHash        string `gorm:"size:128;index" json:"txHash,omitempty"`
	Chain         Chain  `gorm:"size:32;not null;index:idx_collision,priority:3" json:"chain"`
	LogIndex      int64  `gorm:"default:0" json:"logIndex,omitempty"`
	Token         Token  `gorm:"size:32;not null;index:idx_collision,priority:4" json:"token"`
	TargetAddress string `gorm:"size:128;not null;index:idx_collision,priority:1" json:"targetAddress"`
	// 1. 金额改用定点数，与 DB 的 decimal(24,8) 完美对应，杜绝 float 误差
	ExpectedAmount decimal.Decimal `gorm:"type:decimal(36,18);not null;index:idx_collision,priority:2" json:"expectedAmount"`
	ReceivedAmount decimal.Decimal `gorm:"type:decimal(36,18);default:0" json:"receivedAmount"`
	TimeoutSeconds int             `gorm:"not null;default:1800" json:"timeoutSeconds"`
	WebhookURL     string          `gorm:"size:512;not null" json:"webhookUrl"`
	Status         IntentStatus    `gorm:"size:32;not null;default:'WATCHING';index" json:"status"`
	BlockNumber    uint64          `gorm:"default:0" json:"blockNumber,omitempty"`
	Confirmations  uint64          `gorm:"default:0" json:"confirmations,omitempty"`
	ExpiresAt      time.Time       `gorm:"not null;index" json:"expiresAt"`
	PaidAt         *time.Time      `gorm:"index" json:"paidAt,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// ChainTransfer records scanned on-chain token transfer transactions
type ChainTransfer struct {
	ID            uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	TxHash        string `gorm:"size:128;not null;uniqueIndex:idx_chain_tx_log,priority:2" json:"txHash"`
	Chain         Chain  `gorm:"size:32;not null;uniqueIndex:idx_chain_tx_log,priority:1" json:"chain"`
	LogIndex      int64  `gorm:"not null;uniqueIndex:idx_chain_tx_log,priority:3" json:"logIndex"`
	Token         Token  `gorm:"size:32;not null" json:"token"`
	Contract      string `gorm:"size:128;not null;index" json:"contract"` // 新增合约地址
	FromAddress   string `gorm:"size:128;not null" json:"from"`
	TargetAddress string `gorm:"size:128;not null;index" json:"targetAddress"` // 对齐 TargetAddress
	ToAddress     string `gorm:"column:to_address;size:128;default:''" json:"toAddress,omitempty"` // 兼容历史数据库列
	// 1. 业务可读金额：改用 string 或 shopspring/decimal，坚决不能用 float64（会丢分度）
	Amount decimal.Decimal `gorm:"type:varchar(64);not null" json:"amount"`
	// 2. 原始链上精度数值：必须用字符串保存 16 进制转出的十进制无损大数
	RawValue       string    `gorm:"type:varchar(78);not null" json:"rawValue"`
	BlockNumber    uint64    `gorm:"not null" json:"blockNumber"`
	BlockTimestamp int64     `gorm:"not null" json:"blockTimestamp"`
	MatchedOrderID string    `gorm:"size:128" json:"matchedOrderId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	Decimals       uint8     `json:"decimals"` // 代币精度
	Status         uint8     `json:"status"`   // 1: 成功, 2: 待最终确认 (用于大额充值二次校验)
}

// WebhookLog records outgoing webhook delivery attempts and status
type WebhookLog struct {
	ID           uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	OrderID      string     `gorm:"size:128;not null;index" json:"orderId"`
	WebhookURL   string     `gorm:"size:512;not null" json:"webhookUrl"`
	Payload      string     `gorm:"type:text;not null" json:"payload"`
	Signature    string     `gorm:"size:128;not null" json:"signature"`
	StatusCode   int        `json:"statusCode"`
	ResponseBody string     `gorm:"type:text" json:"responseBody"`
	Success      bool       `json:"success"`
	Attempt      int        `json:"attempt"`
	NextRetryAt  *time.Time `json:"nextRetryAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}
type StrategyFactory struct{}

type Chain string
type Token string
type Currency string

const (
	TokenUSDT Token = "USDT"
	TokenBTC  Token = "BTC"
	TokenUSDC Token = "USDC"
	TokenETH  Token = "ETH"
	TokenBNB  Token = "BNB"
)
const (
	// 异构链
	ChainTron   Chain = "TRON"
	ChainSolana Chain = "SOLANA" // 补上 Solana

	// EVM 兼容链家族
	ChainEth      Chain = "ETH" // 建议将 ChainErc20 改为 ChainEth
	ChainBsc      Chain = "BSC"
	ChainPolygon  Chain = "POLYGON"
	ChainArbitrum Chain = "ARBITRUM"
)

const (
	CNY Currency = "CNY"
	USD Currency = "USD"
)

type AlipayConfig struct {
	Enabled            bool   `json:"enabled"`
	AppID              string `json:"appId"`
	MerchantPrivateKey string `json:"merchantPrivateKey,omitempty"`
	AlipayPublicKey    string `json:"alipayPublicKey,omitempty"`
	Sandbox            bool   `json:"sandbox"`
}

type AggregateConfig struct {
	Enabled    bool   `json:"enabled"`
	MerchantID string `json:"merchantId"`
	APIKey     string `json:"apiKey,omitempty"`
	GatewayURL string `json:"gatewayUrl"`
}

type CrypCoin struct {
	Chain            Chain
	Token            Token
	ExchangeRate     decimal.Decimal `json:"exchangeRate"`
	AutoExchangeRate bool            `json:"autoExchangeRate"`
	Provider         string          `json:"provider"`
	Decimals         int             `json:"decimals"`
	Enabled          bool            `json:"enabled"`
}

type Crypto struct {
	Enabled bool       `json:"enabled"`
	Coins   []CrypCoin `json:"coins"`
}

// 提供一个查找方法
func (c *Crypto) GetCoin(chain Chain, token Token) (*CrypCoin, bool) {
	for i := range c.Coins {
		if c.Coins[i].Chain == chain && c.Coins[i].Token == token {
			return &c.Coins[i], true
		}
	}
	return nil, false
}

type PaymentConfig struct {
	ID        uint            `gorm:"primaryKey" json:"-"`
	Alipay    AlipayConfig    `gorm:"serializer:json" json:"alipay"`
	Aggregate AggregateConfig `gorm:"serializer:json" json:"aggregate"`
	Crypto    Crypto          `gorm:"serializer:json" json:"crypto"`
}

// PublicPaymentConfig is the safe, sanitized view returned to anonymous clients (C-end)
type PublicPaymentConfig struct {
	Alipay struct {
		Enabled bool   `json:"enabled"`
		AppID   string `json:"appId"`
		Sandbox bool   `json:"sandbox"`
	} `json:"alipay"`
	Aggregate struct {
		Enabled    bool   `json:"enabled"`
		MerchantID string `json:"merchantId"`
		GatewayURL string `json:"gatewayUrl"`
	} `json:"aggregate"`

	Crypto struct {
		Coins    []CrypCoin
		Provider string `json:"provider"`
	} `json:"crypto_usdt"`
}

func (p *PaymentConfig) ToPublic() PublicPaymentConfig {
	var pub PublicPaymentConfig
	pub.Alipay.Enabled = p.Alipay.Enabled
	pub.Alipay.AppID = p.Alipay.AppID
	pub.Alipay.Sandbox = p.Alipay.Sandbox

	pub.Aggregate.Enabled = p.Aggregate.Enabled
	pub.Aggregate.MerchantID = p.Aggregate.MerchantID
	pub.Aggregate.GatewayURL = p.Aggregate.GatewayURL

	pub.Crypto.Coins = p.Crypto.Coins

	return pub
}

type ChannelType string

const (
	ChannelTypeAlipay    ChannelType = "alipay"
	ChannelTypeAggregate ChannelType = "aggregate"
	ChannelTypeCrypto    ChannelType = "crypto"
)

type CheckoutRequest struct {
	Channel   ChannelType `json:"channel" binding:"required"`
	ChainType string      `json:"chainType"`
	OrderNo   string      `json:"orderNo"`
	Amount    float64     `json:"amount"`
	Currency  Currency    `json:"currency"`
	Chain     Chain       `json:"chain"`
	Token     Token       `json:"token"`
}

type PaymentCreateResult struct {
	Channel       string    `json:"channel"`
	OrderNo       string    `json:"orderNo"`
	PayURL        string    `json:"payUrl,omitempty"`
	QRCode        string    `json:"qrCode,omitempty"`
	CryptoAddress string    `json:"cryptoAddress,omitempty"`
	CryptoAmount  float64   `json:"cryptoAmount,omitempty"`
	ChainType     Chain     `json:"chainType,omitempty"`
	TokenType     Token     `json:"tokenType,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type WalletAddress struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`

	// 资产属性
	Chain   Chain  `gorm:"size:32;not null;index:idx_chain_enabled" json:"chain"` // 如: "TRON", "ETH", "BSC"
	Address string `gorm:"size:128;not null;uniqueIndex" json:"address"`          // 链上实际收款地址
	Label   string `gorm:"size:64" json:"label,omitempty"`                        // 备注/冷钱包标签 (如 "Binance Hot 01")

	// 状态控制
	Enabled bool `gorm:"not null;default:true;index:idx_chain_enabled" json:"enabled"` // 是否启用接收新付款

	// 调度与轮换字段
	LastUsedAt time.Time `gorm:"index" json:"lastUsedAt"` // 最近一次被分配的时间（用于轮换算法）
	Weight     int       `gorm:"default:1" json:"weight"` // 轮换权重（可选）

	// 安全与审计
	Balance   float64   `gorm:"type:decimal(24,8);default:0" json:"balance"` // 链上缓存余额（方便监控归集）
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt time.Time `gorm:"not null" json:"updatedAt"`
}

// TableName 自定义表名
func (WalletAddress) TableName() string {
	return "wallet_addresses"
}

// String 实现 fmt.Stringer 接口，也能直接转原生 string
func (c Chain) String() string {
	return string(c)
}

// ToUpper 封装大写转换，业务调用更干净
func (c Chain) ToUpper() string {
	return strings.ToUpper(string(c))
}

// ToUpper 封装大写转换，业务调用更干净
func (t Token) ToUpper() string {
	return strings.ToUpper(string(t))
}

// NewChainTransfer 统一构造入口：封装所有数据清洗规则与默认值
func NewChainTransfer(
	chain Chain,
	txHash string,
	logIndex int64,
	contract string,
	targetAddress string,
	rawValue string,
	blockNumber uint64,
) ChainTransfer {
	normalizedTarget := chain.NormalizeAddress(targetAddress)
	return ChainTransfer{
		Chain:         chain,
		TxHash:        strings.TrimSpace(txHash),
		LogIndex:      logIndex,
		Contract:      chain.NormalizeAddress(contract),
		TargetAddress: normalizedTarget, // 自动纠正地址格式
		ToAddress:     normalizedTarget,
		RawValue:      rawValue,
		BlockNumber:   blockNumber,
	}
}
