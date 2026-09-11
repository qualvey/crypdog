package engine

import (
	"testing"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"
	"crypdog/internal/queue"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupEngineTestDB 初始化内存 SQLite 数据库
func setupEngineTestDB(t *testing.T) *gorm.DB {
	// 每个测试用例使用独立的内存数据库，防止数据污染
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	// 内存数据库建议限制单连接或保持连接活跃，防止连接释放导致内存库被清除
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	// 自动迁移
	err = db.AutoMigrate(&model.PaymentIntent{}, &model.ChainTransfer{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	return db
}

// setupMockConfig 构造测试配置：BSC 需要 3 个确认，ETH 需要 12 个
func setupMockConfig() *config.Config {
	return &config.Config{
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainBsc: {Confirmations: 3},
			model.ChainEth: {Confirmations: 12},
		},
	}
}

func TestMatcherEngine_ProcessTransfer_HappyPath_Paid(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	// dispatcher 传一个空或基础实例，若未起 worker 则 DispatchAsync 仅入队或空转
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	// 1. Arrange: 准备一条处于 WATCHING 状态的意向订单
	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	intent := model.PaymentIntent{
		ID:             "intent_test_001",
		OrderID:        "ord_test_001",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("100.000100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	// 准备链上转账流水：金额在容差内，区块高度 100
	transfer := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_normal_payment",
		intent.LogIndex,
		"Unknown",
		targetAddr,
		"100000000",
		100,
	)
	transfer.Token = model.TokenUSDT
	transfer.Amount = decimal.RequireFromString("100.000100")
	transfer.BlockTimestamp = time.Now().Unix()

	// 2. Act: 当前区块高度 115，确认数 115 - 100 + 1 = 16 (远大于默认确认数)
	err := engine.ProcessTransfer(transfer, 115)
	require.NoError(t, err)

	// 3. Assert: 验证订单状态跃迁与链上流水绑定
	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_test_001").First(&updatedIntent).Error)
	assert.Equal(t, model.StatusPaid, updatedIntent.Status, "满足确认数后状态应为 PAID")
	assert.Equal(t, "0xhash_normal_payment", updatedIntent.TxHash)
	assert.NotNil(t, updatedIntent.PaidAt)
	assert.True(t, updatedIntent.ReceivedAmount.Equal(transfer.Amount))

	// 验证流水表记录是否成功回填 matched_order_id
	var savedTransfer model.ChainTransfer
	require.NoError(t, db.Where("tx_hash = ?", transfer.TxHash).First(&savedTransfer).Error)
	assert.Equal(t, "ord_test_001", savedTransfer.MatchedOrderID)
}

func TestMatcherEngine_ProcessTransfer_WaitingConfirmations(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	// 1. Arrange: 准备订单
	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	intent := model.PaymentIntent{
		ID:             "intent_test_002",
		OrderID:        "ord_test_002",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("50.000000"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	transfer := model.ChainTransfer{
		TxHash:         "0xhash_waiting_confirm",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		Amount:         decimal.RequireFromString("50.000000"),
		BlockNumber:    100,
		BlockTimestamp: time.Now().Unix(),
	}

	// 2. Act: 当前区块 100，确认数仅为 1（通常 BSC 需 3+，ETH 需 12+）
	// 确保此时确认数小于配置要求
	err := engine.ProcessTransfer(transfer, 100)
	require.NoError(t, err)

	// 3. Assert: 状态进入 CONFIRMING，不能标记为 PAID
	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_test_002").First(&updatedIntent).Error)
	assert.Equal(t, model.StatusConfirming, updatedIntent.Status, "确认数不足时应保持 CONFIRMING")
	assert.Nil(t, updatedIntent.PaidAt, "未达到确认数时不应有 PaidAt 时间戳")
}

func TestMatcherEngine_ProcessTransfer_IdempotentDuplicateTx(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	transfer := model.ChainTransfer{
		TxHash:         "0xhash_dup_check",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		Amount:         decimal.RequireFromString("10.000000"),
		BlockNumber:    100,
		BlockTimestamp: time.Now().Unix(),
	}

	// 先往库里插入一条相同 TxHash 的已处理记录
	require.NoError(t, db.Create(&transfer).Error)

	// 再次送入相同的交易
	err := engine.ProcessTransfer(transfer, 120)
	require.NoError(t, err)

	// 验证库中该 Tx 依然只有一条记录，不会报错，直接返回
	var count int64
	db.Model(&model.ChainTransfer{}).Where("tx_hash = ?", "0xhash_dup_check").Count(&count)
	assert.Equal(t, int64(1), count)
}

func TestMatcherEngine_ProcessTransfer_AmountMismatch_Ignored(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	intent := model.PaymentIntent{
		ID:             "intent_test_003",
		OrderID:        "ord_test_003",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("100.000100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	// 金额差距较大 (比如付了 99.00)，超出容差
	transfer := model.ChainTransfer{
		TxHash:         "0xhash_wrong_amount",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		Amount:         decimal.RequireFromString("99.000000"),
		BlockNumber:    100,
		BlockTimestamp: time.Now().Unix(),
	}

	err := engine.ProcessTransfer(transfer, 120)
	require.NoError(t, err)

	// 验证订单状态未被更改，依旧保持 WATCHING
	var unchangedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_test_003").First(&unchangedIntent).Error)
	assert.Equal(t, model.StatusWatching, unchangedIntent.Status)
	assert.Empty(t, unchangedIntent.TxHash)
}

func TestMatcherEngine_ProcessTransfer_UsdtUsdcCrossMatch(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	targetAddr := "0x7bdc49542978b16566e82c8f90db1eb03804c675"
	// 订单约定币种是 USDT，金额 0.1501
	intent := model.PaymentIntent{
		ID:             "intent_cross_001",
		OrderID:        "ord_cross_001",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("0.150100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	// 链上实际支付的是 USDC，金额 0.1501
	transfer := model.ChainTransfer{
		TxHash:         "0xhash_cross_usdc_payment",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDC,
		TargetAddress:  targetAddr,
		Amount:         decimal.RequireFromString("0.150100"),
		BlockNumber:    100,
		BlockTimestamp: time.Now().Unix(),
	}

	err := engine.ProcessTransfer(transfer, 115)
	require.NoError(t, err)

	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_cross_001").First(&updatedIntent).Error)
	assert.Equal(t, model.StatusPaid, updatedIntent.Status, "USDC 充值应能成功撮合 USDT 等价订单")
	assert.Equal(t, "0xhash_cross_usdc_payment", updatedIntent.TxHash)
}

