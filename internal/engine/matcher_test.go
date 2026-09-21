package engine

import (
	"context"
	"errors"
	"sync"
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
	err = db.AutoMigrate(&model.PaymentIntent{}, &model.ChainTransfer{}, &model.WebhookLog{})
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

func TestMatcherEngine_ProcessTransfer_NoCrossTokenMatch(t *testing.T) {
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
	assert.Equal(t, model.StatusWatching, updatedIntent.Status, "USDT 意向不能被 USDC 充值跨币种撮合，应保持 WATCHING 状态")
	assert.Empty(t, updatedIntent.TxHash)
}

func TestMatcherEngine_LatePaymentRecovery(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	// 准备一条已经超时的 EXPIRED 订单（超时时间设为 10 分钟前）
	intent := model.PaymentIntent{
		ID:             "intent_expired_001",
		OrderID:        "ord_expired_001",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("50.0001"),
		Status:         model.StatusExpired,
		ExpiresAt:      time.Now().Add(-10 * time.Minute),
	}
	require.NoError(t, db.Create(&intent).Error)

	// 用户迟到了，转账金额吻合
	transfer := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_late_payment",
		0,
		"Unknown",
		targetAddr,
		"50000100",
		100,
	)
	transfer.Token = model.TokenUSDT
	transfer.Amount = decimal.RequireFromString("50.0001")
	transfer.BlockTimestamp = time.Now().Unix()

	err := engine.ProcessTransfer(transfer, 115)
	require.NoError(t, err)

	// 验证订单成功挽回并推进为 PAID
	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_expired_001").First(&updatedIntent).Error)
	assert.Equal(t, model.StatusPaid, updatedIntent.Status, "迟到充值应成功挽回并将订单流转为 PAID")
	assert.Equal(t, "0xhash_late_payment", updatedIntent.TxHash)
}

func TestMatcherEngine_ReprocessUnmatchedTransfer(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"

	// 1. 预先落库一笔未撮合的流水 (matched_order_id 为空)
	transfer := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_historical_unmatched",
		0,
		"0xfromUser",
		targetAddr,
		"100000100",
		100,
	)
	transfer.Token = model.TokenUSDT
	transfer.Amount = decimal.RequireFromString("100.0001")
	transfer.BlockTimestamp = time.Now().Unix()
	require.NoError(t, db.Create(&transfer).Error)

	// 2. 此时创建订单 (例如用户在转账之后才创建订单，或服务重启恢复未匹配流水)
	intent := model.PaymentIntent{
		ID:             "intent_recovery_001",
		OrderID:        "ord_recovery_001",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("100.0001"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	// 3. 执行补偿重试撮合 ReprocessUnmatchedTransfer
	err := engine.ReprocessUnmatchedTransfer(transfer, 115)
	require.NoError(t, err)

	// 4. 验证订单成功被撮合并标记为 PAID
	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_recovery_001").First(&updatedIntent).Error)
	assert.Equal(t, model.StatusPaid, updatedIntent.Status)
	assert.Equal(t, "0xhash_historical_unmatched", updatedIntent.TxHash)

	// 验证流水更新了 matched_order_id
	var updatedTransfer model.ChainTransfer
	require.NoError(t, db.Where("tx_hash = ?", transfer.TxHash).First(&updatedTransfer).Error)
	assert.Equal(t, "ord_recovery_001", updatedTransfer.MatchedOrderID)
}

func TestMatcherEngine_HistoricalTheftPrevented(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)
	engine := NewMatcherEngine(db, cfg, dispatcher)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"

	// 1. 模拟 1 小时前发生的历史未认领流水 (如他人打错款)
	historicalTransfer := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_ancient_theft_attempt",
		0,
		"0xfromAttacker",
		targetAddr,
		"100000100",
		100,
	)
	historicalTransfer.Token = model.TokenUSDT
	historicalTransfer.Amount = decimal.RequireFromString("100.0001")
	historicalTransfer.BlockTimestamp = time.Now().Add(-1 * time.Hour).Unix()
	require.NoError(t, db.Create(&historicalTransfer).Error)

	// 2. 攻击者现在创建新订单，故意使用相同的金额 100.0001 试图白嫖盗刷
	intent := model.PaymentIntent{
		ID:             "intent_theft_target",
		OrderID:        "ord_theft_target",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("100.0001"),
		Status:         model.StatusWatching,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, db.Create(&intent).Error)

	// 3. 触发撮合 (模拟流水补偿重放)
	err := engine.ReprocessUnmatchedTransfer(historicalTransfer, 120)
	require.NoError(t, err)

	// 4. 验证：由于历史流水发生时间早于订单创建时间，绝对禁止撮合，订单必须保持 WATCHING，防盗刷成功！
	var checkedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_theft_target").First(&checkedIntent).Error)
	assert.Equal(t, model.StatusWatching, checkedIntent.Status, "历史早于订单创建的流水绝不可匹配新订单")
	assert.Empty(t, checkedIntent.TxHash)
}

func TestMatcherEngine_TxVerifier_RevertBlocked(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)

	// 注入自定义 txVerifier：模拟链上查询到该交易其实执行失败 (Revert / status == 0x0)
	fakeVerifier := func(ctx context.Context, chain model.Chain, txHash string, blockNumber uint64) (bool, error) {
		return false, nil // 交易无效
	}
	engine := NewMatcherEngine(db, cfg, dispatcher, fakeVerifier)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	intent := model.PaymentIntent{
		ID:             "intent_revert_test",
		OrderID:        "ord_revert_test",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("100.000100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	transfer := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_reverted_tx",
		0,
		"0xfrom",
		targetAddr,
		"100000000",
		100,
	)
	transfer.Token = model.TokenUSDT
	transfer.Amount = decimal.RequireFromString("100.000100")
	transfer.BlockTimestamp = time.Now().Unix()

	// 当前高度 120 (满足确认数)，但由于 txVerifier 报告交易失败，必须阻断
	err := engine.ProcessTransfer(transfer, 120)
	assert.Error(t, err, "假充值/已Revert交易必须报错阻断")

	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_revert_test").First(&updatedIntent).Error)
	assert.NotEqual(t, model.StatusPaid, updatedIntent.Status, "失败交易绝不能标记为 PAID")
}

func TestMatcherEngine_TxVerifier_RPCDowngradeToConfirming(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig()
	dispatcher := queue.NewWebhookDispatcher(db, cfg)

	// 注入自定义 txVerifier：模拟 RPC 节点网络抖动或超时
	jitterVerifier := func(ctx context.Context, chain model.Chain, txHash string, blockNumber uint64) (bool, error) {
		return false, errors.New("rpc timeout or rate limited")
	}
	engine := NewMatcherEngine(db, cfg, dispatcher, jitterVerifier)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	intent := model.PaymentIntent{
		ID:             "intent_rpc_error_test",
		OrderID:        "ord_rpc_error_test",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("100.000100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	transfer := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_rpc_error_tx",
		0,
		"0xfrom",
		targetAddr,
		"100000000",
		100,
	)
	transfer.Token = model.TokenUSDT
	transfer.Amount = decimal.RequireFromString("100.000100")
	transfer.BlockTimestamp = time.Now().Unix()

	// 当前高度 120 (满足确认数)，但由于 RPC 抖动，安全降级为 CONFIRMING
	err := engine.ProcessTransfer(transfer, 120)
	require.NoError(t, err)

	var updatedIntent model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_rpc_error_test").First(&updatedIntent).Error)
	assert.Equal(t, model.StatusConfirming, updatedIntent.Status, "RPC 抖动时应安全降级为 CONFIRMING，待 Worker 重试")
}

type mockDualPhaseDispatcher struct {
	mu     sync.Mutex
	events []struct {
		Event                 string
		OrderID               string
		Confirmations         uint64
		RequiredConfirmations uint64
	}
}

func (m *mockDualPhaseDispatcher) DispatchAsync(intent *model.PaymentIntent, txHash string, timestamp int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, struct {
		Event                 string
		OrderID               string
		Confirmations         uint64
		RequiredConfirmations uint64
	}{Event: queue.WebhookEventConfirm, OrderID: intent.OrderID, Confirmations: intent.Confirmations})
}

func (m *mockDualPhaseDispatcher) DispatchEventAsync(event string, intent *model.PaymentIntent, txHash string, timestamp int64, confirmations, requiredConfirmations uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, struct {
		Event                 string
		OrderID               string
		Confirmations         uint64
		RequiredConfirmations uint64
	}{Event: event, OrderID: intent.OrderID, Confirmations: confirmations, RequiredConfirmations: requiredConfirmations})
}

func (m *mockDualPhaseDispatcher) GetEvents() []struct {
	Event                 string
	OrderID               string
	Confirmations         uint64
	RequiredConfirmations uint64
} {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]struct {
		Event                 string
		OrderID               string
		Confirmations         uint64
		RequiredConfirmations uint64
	}, len(m.events))
	copy(copied, m.events)
	return copied
}

func TestMatcherEngine_DualPhaseHook_FirstMatchGet_ThenConfirm(t *testing.T) {
	db := setupEngineTestDB(t)
	cfg := setupMockConfig() // BSC requires 3 confirmations
	mockDisp := &mockDualPhaseDispatcher{}
	engine := NewMatcherEngine(db, cfg, mockDisp)

	targetAddr := "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"

	// Case 1: First match with insufficient confirmations -> triggers "get" only
	intent1 := model.PaymentIntent{
		ID:             "intent_dual_001",
		OrderID:        "ord_dual_001",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("20.000100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent1).Error)

	transfer1 := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_dual_1",
		0,
		"0xpayer",
		targetAddr,
		"20000100",
		100,
	)
	transfer1.Token = model.TokenUSDT
	transfer1.Amount = decimal.RequireFromString("20.000100")
	transfer1.BlockTimestamp = time.Now().Unix()

	// Block height 100 -> confirmations = 100 - 100 + 1 = 1 (required = 3)
	err := engine.ProcessTransfer(transfer1, 100)
	require.NoError(t, err)

	events1 := mockDisp.GetEvents()
	require.Len(t, events1, 1, "未达确认数首次匹配时应且仅分发 1 个 get 事件")
	assert.Equal(t, queue.WebhookEventGet, events1[0].Event)
	assert.Equal(t, "ord_dual_001", events1[0].OrderID)
	assert.Equal(t, uint64(1), events1[0].Confirmations)
	assert.Equal(t, uint64(3), events1[0].RequiredConfirmations)

	var savedIntent1 model.PaymentIntent
	require.NoError(t, db.Where("order_id = ?", "ord_dual_001").First(&savedIntent1).Error)
	assert.Equal(t, model.StatusConfirming, savedIntent1.Status)
	assert.NotNil(t, savedIntent1.DetectedAt)

	// Case 2: First match with already sufficient confirmations -> triggers "get" then "confirm"
	intent2 := model.PaymentIntent{
		ID:             "intent_dual_002",
		OrderID:        "ord_dual_002",
		Chain:          model.ChainBsc,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: decimal.RequireFromString("30.000100"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent2).Error)

	transfer2 := model.NewChainTransfer(
		model.ChainBsc,
		"0xhash_dual_2",
		0,
		"0xpayer",
		targetAddr,
		"30000100",
		100,
	)
	transfer2.Token = model.TokenUSDT
	transfer2.Amount = decimal.RequireFromString("30.000100")
	transfer2.BlockTimestamp = time.Now().Unix()

	// Block height 110 -> confirmations = 110 - 100 + 1 = 11 (required = 3)
	err = engine.ProcessTransfer(transfer2, 110)
	require.NoError(t, err)

	// Wait briefly for goroutine sending confirm after get
	time.Sleep(300 * time.Millisecond)

	events2 := mockDisp.GetEvents()
	// events1 has 1, so events2 should have 1 + 2 = 3
	require.Len(t, events2, 3, "即刻满足确认数时应先后触发 get 与 confirm 两个事件")
	assert.Equal(t, queue.WebhookEventGet, events2[1].Event)
	assert.Equal(t, "ord_dual_002", events2[1].OrderID)
	assert.Equal(t, queue.WebhookEventConfirm, events2[2].Event)
	assert.Equal(t, "ord_dual_002", events2[2].OrderID)
}
