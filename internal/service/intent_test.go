package service_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"crypdog/internal/engine"
	"crypdog/internal/model"
	"crypdog/internal/service"

	"github.com/glebarez/sqlite" // 纯 Go 实现的无 CGO SQLite，也可以用 gorm.io/driver/sqlite
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupTestDB 初始化隔离的 SQLite 内存数据库
func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	// 自动迁移 PaymentIntent 表
	err = db.AutoMigrate(&model.PaymentIntent{})
	require.NoError(t, err)

	// 每次测试结束前清空表，保证测试用例互不干扰
	t.Cleanup(func() {
		db.Exec("DELETE FROM payment_intents")
	})

	return db
}

func defaultDTO() service.RegisterDTO {
	return service.RegisterDTO{
		OrderID:        "ord_test_001",
		Chain:          model.Chain("SOLANA"),
		Token:          model.Token("USDT"),
		TargetAddress:  "SolanaAddress11111111111111111111111111",
		ExpectedAmount: decimal.RequireFromString("100.000100"),
		TimeoutSeconds: 1800,
		WebhookURL:     "https://example.com/webhook",
	}
}

// 1. 测试创建全新订单
func TestRegisterOrReactivate_NewOrder(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	dto := defaultDTO()
	intent, isIdempotent, err := svc.RegisterOrReactivate(dto)

	assert.NoError(t, err)
	assert.False(t, isIdempotent)
	assert.NotNil(t, intent)
	assert.Equal(t, dto.OrderID, intent.OrderID)
	assert.Equal(t, model.StatusWatching, intent.Status)
	assert.True(t, intent.ExpectedAmount.Equal(dto.ExpectedAmount))
	assert.True(t, intent.ExpiresAt.After(time.Now()))

	// 验证确实写入了数据库
	var inDB model.PaymentIntent
	err = db.Where("order_id = ?", dto.OrderID).First(&inDB).Error
	assert.NoError(t, err)
	assert.Equal(t, intent.ID, inDB.ID)
}

// 2. 测试已支付订单无法重新注册
func TestRegisterOrReactivate_AlreadyPaid(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	// 预先插入一条已支付记录
	paidIntent := model.PaymentIntent{
		ID:             "intent_paid_1",
		OrderID:        "ord_test_paid",
		Chain:          "SOLANA",
		Token:          "USDT",
		TargetAddress:  "SolanaAddress11111111111111111111111111",
		ExpectedAmount: decimal.RequireFromString("100.000100"),
		Status:         model.StatusPaid,
	}
	require.NoError(t, db.Create(&paidIntent).Error)

	dto := defaultDTO()
	dto.OrderID = paidIntent.OrderID

	_, _, err := svc.RegisterOrReactivate(dto)
	assert.ErrorIs(t, err, service.ErrOrderAlreadyPaid)
}

// 3. 测试活跃订单的幂等重放（Watching / Confirming 状态）
func TestRegisterOrReactivate_IdempotentReplay(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	dto := defaultDTO()
	// 首次创建
	firstIntent, isIdempotent, err := svc.RegisterOrReactivate(dto)
	require.NoError(t, err)
	assert.False(t, isIdempotent)

	// 二次使用相同参数请求 -> 应该命中幂等逻辑
	secondIntent, isIdempotent, err := svc.RegisterOrReactivate(dto)
	assert.NoError(t, err)
	assert.True(t, isIdempotent)
	assert.Equal(t, firstIntent.ID, secondIntent.ID)
	assert.Equal(t, firstIntent.OrderID, secondIntent.OrderID)
}

// 4. 测试活跃订单篡改参数（报错 ErrParamMutation）
func TestRegisterOrReactivate_ParamMutation(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	dto := defaultDTO()
	_, _, err := svc.RegisterOrReactivate(dto)
	require.NoError(t, err)

	// 修改金额再次请求，应当拒绝并报错
	tamperedDTO := dto
	tamperedDTO.ExpectedAmount = decimal.RequireFromString("200.000000")

	_, _, err = svc.RegisterOrReactivate(tamperedDTO)
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrParamMutation)
}

// 5. 测试已过期/已取消订单的重新激活（Reactivate）
func TestRegisterOrReactivate_ReactivateCancelledOrExpired(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	oldTime := time.Now().Add(-1 * time.Hour)
	expiredIntent := model.PaymentIntent{
		ID:             "intent_expired_1",
		OrderID:        "ord_test_expired",
		Chain:          "SOLANA",
		Token:          "USDT",
		TargetAddress:  "SolanaAddress11111111111111111111111111",
		ExpectedAmount: decimal.RequireFromString("50.000100"),
		Status:         model.StatusExpired,
		ExpiresAt:      oldTime,
		TxHash:         "old_hash_should_be_cleared",
	}
	require.NoError(t, db.Create(&expiredIntent).Error)

	dto := defaultDTO()
	dto.OrderID = expiredIntent.OrderID
	dto.ExpectedAmount = decimal.RequireFromString("60.000200") // 允许新参数

	reactivated, isIdempotent, err := svc.RegisterOrReactivate(dto)
	assert.NoError(t, err)
	assert.False(t, isIdempotent)

	// 验证状态重置成功
	assert.Equal(t, expiredIntent.ID, reactivated.ID) // 复用原 ID
	assert.Equal(t, model.StatusWatching, reactivated.Status)
	assert.Empty(t, reactivated.TxHash)
	assert.True(t, reactivated.ExpectedAmount.Equal(dto.ExpectedAmount))
	assert.True(t, reactivated.ExpiresAt.After(time.Now()))
}

// 6. 测试多订单微额碰撞（Collision Detection）
func TestRegisterOrReactivate_AmountCollision(t *testing.T) {
	db := setupTestDB(t)
	// 初始化碰撞检测管理器
	poolMgr := engine.NewMicroAmountManager(db)
	svc := service.NewIntentService(db, poolMgr)

	// 订单 A 占用了某个金额
	dtoA := defaultDTO()
	dtoA.OrderID = "ord_A"
	dtoA.ExpectedAmount = decimal.RequireFromString("100.000100")
	_, _, err := svc.RegisterOrReactivate(dtoA)
	require.NoError(t, err)

	// 订单 B 在同一链、同代币、同目标地址尝试使用完全相同的金额
	dtoB := defaultDTO()
	dtoB.OrderID = "ord_B"
	dtoB.ExpectedAmount = dtoA.ExpectedAmount

	_, _, err = svc.RegisterOrReactivate(dtoB)
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrAmountCollision)
}

func TestIntentService_CancelIntent(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	// 1. 创建订单
	dto := defaultDTO()
	intent, _, err := svc.RegisterOrReactivate(dto)
	require.NoError(t, err)

	// 2. 通过 OrderID 取消
	cancelled, err := svc.CancelIntent(dto.OrderID)
	assert.NoError(t, err)
	assert.Equal(t, model.StatusCancelled, cancelled.Status)

	// 3. 再次取消（幂等性）
	cancelledAgain, err := svc.CancelIntent(intent.ID)
	assert.NoError(t, err)
	assert.Equal(t, model.StatusCancelled, cancelledAgain.Status)

	// 4. 已支付订单不可取消
	paidIntent := model.PaymentIntent{
		ID:             "intent_paid_001",
		OrderID:        "ord_paid_001",
		Chain:          model.ChainTron,
		Token:          model.TokenUSDT,
		TargetAddress:  "T9yD14Nj9j7xXv8YmZP2K8qL4W9vR1e3S4",
		ExpectedAmount: decimal.RequireFromString("50.00"),
		Status:         model.StatusPaid,
	}
	require.NoError(t, db.Create(&paidIntent).Error)

	_, err = svc.CancelIntent(paidIntent.OrderID)
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCannotCancelPaid)

	// 5. 不存在的订单
	_, err = svc.CancelIntent("non_existent_id")
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrIntentNotFound)
}

func TestIntentService_AllocateOrReactivate(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.WalletAddress{}))

	// 创建可用收款地址
	wallet := model.WalletAddress{
		Chain:   model.ChainArbitrum,
		Address: "0x7bdc49542978b16566e82c8f90db1eb03804c675",
		Enabled: true,
	}
	require.NoError(t, db.Create(&wallet).Error)

	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	allocDTO := service.AllocateDTO{
		OrderID:        "ord_alloc_001",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDC,
		BaseAmount:     decimal.NewFromInt(10),
		TimeoutSeconds: 1800,
		WebhookURL:     "https://example.com/webhook",
	}

	// 1. 全新分配
	intent, tail, isIdempotent, err := svc.AllocateOrReactivate(allocDTO)
	require.NoError(t, err)
	assert.False(t, isIdempotent)
	assert.Equal(t, model.StatusWatching, intent.Status)
	assert.True(t, intent.ExpectedAmount.GreaterThan(allocDTO.BaseAmount))
	assert.True(t, tail.GreaterThan(decimal.Zero))

	// 2. 幂等重复调用：返回已有订单
	intentSame, tailSame, isIdempotentSame, err := svc.AllocateOrReactivate(allocDTO)
	require.NoError(t, err)
	assert.True(t, isIdempotentSame)
	assert.Equal(t, intent.ID, intentSame.ID)
	assert.Equal(t, tail.String(), tailSame.String())

	// 3. 将订单取消后，再次通过 AllocateOrReactivate 进行分配（验证重激活能力，不报 UNIQUE 约束冲突）
	intent.Status = model.StatusCancelled
	require.NoError(t, db.Save(intent).Error)

	reactivated, _, isIdempotentReactivated, err := svc.AllocateOrReactivate(allocDTO)
	require.NoError(t, err)
	assert.False(t, isIdempotentReactivated)
	assert.Equal(t, intent.ID, reactivated.ID)
	assert.Equal(t, model.StatusWatching, reactivated.Status)

	// 4. 将订单设为已支付后，再次 Allocate 应当被拒绝
	reactivated.Status = model.StatusPaid
	require.NoError(t, db.Save(reactivated).Error)

	_, _, _, err = svc.AllocateOrReactivate(allocDTO)
	assert.ErrorIs(t, err, service.ErrOrderAlreadyPaid)
}

// 8. 测试高并发下的微数分配安全性（防撞车竞争条件）
func TestAllocateOrReactivate_ConcurrentSafety(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.WalletAddress{}))

	wallet := model.WalletAddress{
		Chain:     model.ChainArbitrum,
		Address:   "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		Enabled:   true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, db.Create(&wallet).Error)

	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	concurrency := 20
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	amounts := make(chan decimal.Decimal, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(orderIdx int) {
			defer wg.Done()
			dto := service.AllocateDTO{
				OrderID:        fmt.Sprintf("ord_concurrent_%d", orderIdx),
				Chain:          model.ChainArbitrum,
				Token:          model.TokenUSDC,
				BaseAmount:     decimal.NewFromInt(10),
				TimeoutSeconds: 1800,
				WebhookURL:     "https://example.com/webhook",
			}
			intent, _, _, err := svc.AllocateOrReactivate(dto)
			if err != nil {
				errs <- err
				return
			}
			amounts <- intent.ExpectedAmount
		}(i)
	}

	wg.Wait()
	close(errs)
	close(amounts)

	for err := range errs {
		require.NoError(t, err)
	}

	allocatedMap := make(map[string]bool)
	count := 0
	for amt := range amounts {
		count++
		assert.False(t, allocatedMap[amt.String()], "Amount %s should not be duplicated across concurrent allocations", amt.String())
		allocatedMap[amt.String()] = true
	}
	assert.Equal(t, concurrency, count)
}

func TestCancelIntent_Restrictions(t *testing.T) {
	db := setupTestDB(t)
	svc := service.NewIntentService(db, engine.NewMicroAmountManager(db))

	// 1. Confirming 订单禁止取消
	confirmingIntent := model.PaymentIntent{
		ID:             "intent_confirming_1",
		OrderID:        "ord_confirming_1",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDC,
		TargetAddress:  "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		ExpectedAmount: decimal.RequireFromString("10.000100"),
		Status:         model.StatusConfirming,
	}
	require.NoError(t, db.Create(&confirmingIntent).Error)

	_, err := svc.CancelIntent("ord_confirming_1")
	assert.ErrorIs(t, err, service.ErrCannotCancelConfirming)

	// 2. Paid 订单禁止取消
	paidIntent := model.PaymentIntent{
		ID:             "intent_paid_2",
		OrderID:        "ord_paid_2",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDC,
		TargetAddress:  "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		ExpectedAmount: decimal.RequireFromString("10.000200"),
		Status:         model.StatusPaid,
	}
	require.NoError(t, db.Create(&paidIntent).Error)

	_, err = svc.CancelIntent("ord_paid_2")
	assert.ErrorIs(t, err, service.ErrCannotCancelPaid)

	// 3. Watching 订单允许取消
	watchingIntent := model.PaymentIntent{
		ID:             "intent_watching_3",
		OrderID:        "ord_watching_3",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDC,
		TargetAddress:  "0x7BDc49542978B16566e82c8f90DB1EB03804C675",
		ExpectedAmount: decimal.RequireFromString("10.000300"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&watchingIntent).Error)

	cancelled, err := svc.CancelIntent("ord_watching_3")
	assert.NoError(t, err)
	assert.Equal(t, model.StatusCancelled, cancelled.Status)
}

