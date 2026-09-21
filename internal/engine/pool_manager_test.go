package engine

import (
	"fmt"
	"testing"
	"time"

	"crypdog/internal/model"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupPoolTestDB(t *testing.T) *gorm.DB {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(&model.PaymentIntent{}, &model.WalletAddress{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	return db
}

func TestMicroAmountManager_AllocateUniqueAmount_TRC20Alias(t *testing.T) {
	db := setupPoolTestDB(t)
	mgr := NewMicroAmountManager(db)

	// 插入 TRON 地址
	wallet := model.WalletAddress{
		Chain:     model.ChainTron,
		Address:   "TWLp7W7umCmLYwLxm8hwndo6B9p6tsshzh",
		Label:     "trc20",
		Enabled:   true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, db.Create(&wallet).Error)

	// 使用 "TRC20" 申请微额
	targetAddr, allocated, err := mgr.AllocateUniqueAmount(model.Chain("TRC20"), model.TokenUSDT, decimal.NewFromFloat(100))
	require.NoError(t, err)
	assert.Equal(t, "TWLp7W7umCmLYwLxm8hwndo6B9p6tsshzh", targetAddr)
	assert.True(t, allocated.GreaterThan(decimal.NewFromFloat(100)))

	// 模拟将该意向订单保存入库（占用该金额微额槽位）
	intent := model.PaymentIntent{
		ID:             "intent_test_pool_1",
		OrderID:        "ord_pool_1",
		Chain:          model.ChainTron,
		Token:          model.TokenUSDT,
		TargetAddress:  targetAddr,
		ExpectedAmount: allocated,
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent).Error)

	// 再次使用 "TRON" 申请微额，确保同一地址上不冲突并成功递增
	targetAddr2, allocated2, err := mgr.AllocateUniqueAmount(model.ChainTron, model.TokenUSDT, decimal.NewFromFloat(100))
	require.NoError(t, err)
	assert.Equal(t, "TWLp7W7umCmLYwLxm8hwndo6B9p6tsshzh", targetAddr2)
	assert.NotEqual(t, allocated.String(), allocated2.String())
	assert.True(t, allocated2.GreaterThan(allocated))
}

func TestMicroAmountManager_AddressRotation(t *testing.T) {
	db := setupPoolTestDB(t)
	mgr := NewMicroAmountManager(db)

	// 插入两个 Arbitrum 地址，初始化不同 last_used_at
	w1 := model.WalletAddress{
		Chain:      model.ChainArbitrum,
		Address:    "0x1111111111111111111111111111111111111111",
		Enabled:    true,
		LastUsedAt: time.Now().Add(-10 * time.Minute),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	w2 := model.WalletAddress{
		Chain:      model.ChainArbitrum,
		Address:    "0x2222222222222222222222222222222222222222",
		Enabled:    true,
		LastUsedAt: time.Now().Add(-5 * time.Minute),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(&w1).Error)
	require.NoError(t, db.Create(&w2).Error)

	// 第 1 次分配：应选取最久未使用的 w1
	addr1, _, err := mgr.AllocateUniqueAmount(model.ChainArbitrum, model.TokenUSDC, decimal.NewFromInt(10))
	require.NoError(t, err)
	assert.Equal(t, w1.Address, addr1)

	// 第 2 次分配：由于 w1 的 last_used_at 已经更新为当前时间，本次应当轮换到 w2！
	addr2, _, err := mgr.AllocateUniqueAmount(model.ChainArbitrum, model.TokenUSDC, decimal.NewFromInt(10))
	require.NoError(t, err)
	assert.Equal(t, w2.Address, addr2)

	// 第 3 次分配：由于 w2 的 last_used_at 更新了，轮换回到 w1！
	addr3, _, err := mgr.AllocateUniqueAmount(model.ChainArbitrum, model.TokenUSDC, decimal.NewFromInt(10))
	require.NoError(t, err)
	assert.Equal(t, w1.Address, addr3)
}

func TestMicroAmountManager_IndependentPoolPerAddress(t *testing.T) {
	db := setupPoolTestDB(t)
	mgr := NewMicroAmountManager(db)

	w1 := model.WalletAddress{
		Chain:      model.ChainArbitrum,
		Address:    "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Enabled:    true,
		LastUsedAt: time.Now().Add(-10 * time.Minute),
	}
	w2 := model.WalletAddress{
		Chain:      model.ChainArbitrum,
		Address:    "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Enabled:    true,
		LastUsedAt: time.Now().Add(-5 * time.Minute),
	}
	require.NoError(t, db.Create(&w1).Error)
	require.NoError(t, db.Create(&w2).Error)

	// 在 w1 上创建订单，占用 10.0001
	intent1 := model.PaymentIntent{
		ID:             "intent_w1",
		OrderID:        "ord_w1",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDT,
		TargetAddress:  w1.Address,
		ExpectedAmount: decimal.RequireFromString("10.0001"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, db.Create(&intent1).Error)

	// 此时向 w2 分配 10 USDT：由于是不同收款地址，w2 的 10.0001 应当是空闲可用的！
	// (修复前：由于未限定 target_address，w2 也被认为占用了 10.0001 而跳到 10.0002)
	db.Model(&w1).Update("last_used_at", time.Now()) // 让 w2 成为最久未使用
	addr, alloc, err := mgr.AllocateUniqueAmount(model.ChainArbitrum, model.TokenUSDT, decimal.NewFromInt(10))
	require.NoError(t, err)
	assert.Equal(t, w2.Address, addr)
	assert.Equal(t, "10.0001", alloc.StringFixed(4), "不同收款地址应该具有独立的微额尾数池空间")
}

func TestMicroAmountManager_CooldownProtection(t *testing.T) {
	db := setupPoolTestDB(t)
	mgr := NewMicroAmountManager(db)

	wallet := model.WalletAddress{
		Chain:      model.ChainArbitrum,
		Address:    "0x1111111111111111111111111111111111111111",
		Enabled:    true,
		LastUsedAt: time.Now().Add(-1 * time.Hour),
	}
	require.NoError(t, db.Create(&wallet).Error)

	// 1. 插入一个 5 分钟前刚超时的订单，占用 10.0001 (处于 15 分钟冷却期内)
	expiredIntent := model.PaymentIntent{
		ID:             "intent_cooldown_1",
		OrderID:        "ord_cooldown_1",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDT,
		TargetAddress:  wallet.Address,
		ExpectedAmount: decimal.RequireFromString("10.0001"),
		Status:         model.StatusExpired,
		UpdatedAt:      time.Now().Add(-5 * time.Minute),
	}
	require.NoError(t, db.Create(&expiredIntent).Error)

	// 2. 插入一个 30 分钟前超时的老订单，占用 10.0002 (已过 15 分钟冷却期)
	oldExpiredIntent := model.PaymentIntent{
		ID:             "intent_cooldown_2",
		OrderID:        "ord_cooldown_2",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDT,
		TargetAddress:  wallet.Address,
		ExpectedAmount: decimal.RequireFromString("10.0002"),
		Status:         model.StatusExpired,
		UpdatedAt:      time.Now().Add(-30 * time.Minute),
	}
	require.NoError(t, db.Create(&oldExpiredIntent).Error)

	// 3. 此时请求分配 10 USDT：
	// 10.0001 处于 15 分钟冷却期内，不能被分配！
	// 10.0002 已经过了 15 分钟冷却期，应该被优先重用！
	_, allocated, err := mgr.AllocateUniqueAmount(model.ChainArbitrum, model.TokenUSDT, decimal.NewFromInt(10))
	require.NoError(t, err)
	assert.Equal(t, "10.0002", allocated.StringFixed(4), "处于冷却期内的尾数不可分配，已过冷却期的尾数应被回收复用")

	// 4. 再次分配，10.0001 仍然冷却中，10.0002 已刚分配(WATCHING)，应跳到 10.0003
	intentJustAllocated := model.PaymentIntent{
		ID:             "intent_new_alloc",
		OrderID:        "ord_new_alloc",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDT,
		TargetAddress:  wallet.Address,
		ExpectedAmount: allocated,
		Status:         model.StatusWatching,
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, db.Create(&intentJustAllocated).Error)

	_, nextAllocated, err := mgr.AllocateUniqueAmount(model.ChainArbitrum, model.TokenUSDT, decimal.NewFromInt(10))
	require.NoError(t, err)
	assert.Equal(t, "10.0003", nextAllocated.StringFixed(4))
}
