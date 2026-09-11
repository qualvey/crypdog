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
