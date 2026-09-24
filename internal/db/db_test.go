package db

import (
	"fmt"
	"testing"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupDBTest(t *testing.T) *gorm.DB {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, database.AutoMigrate(&model.WalletAddress{}))

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	return database
}

func TestSeedInitialWallets_NoCrossChainDisable(t *testing.T) {
	database := setupDBTest(t)

	// 1. 预设一个已有的 TRON 收款地址
	tronWallet := model.WalletAddress{
		Chain:     model.ChainTron,
		Address:   "TWLp7W7umCmLYwLxm8hwndo6B9p6tsshzh",
		Label:     "tron-hot-01",
		Enabled:   true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, database.Create(&tronWallet).Error)

	// 2. 模拟系统启动，配置中只包含了 ARBITRUM 的钱包
	arbConfigWallets := []config.InitialWallet{
		{
			Chain:   "ARBITRUM",
			Address: "0x7bdc49542978b16566e82c8f90db1eb03804c675",
			Label:   "arb-hot-01",
		},
	}

	seedInitialWallets(database, arbConfigWallets)

	// 3. 校验：ARBITRUM 钱包被成功插入并启用
	var arbWalletInDB model.WalletAddress
	require.NoError(t, database.Where("address = ?", "0x7bdc49542978b16566e82c8f90db1eb03804c675").First(&arbWalletInDB).Error)
	assert.True(t, arbWalletInDB.Enabled)

	// 4. 关键验证：已有的 TRON 钱包绝对不能被误关禁用！必须依然保持 enabled = true
	var tronWalletInDB model.WalletAddress
	require.NoError(t, database.Where("address = ?", tronWallet.Address).First(&tronWalletInDB).Error)
	assert.True(t, tronWalletInDB.Enabled, "配置只更新 Arbitrum 钱包时，TRON 的已有钱包必须保持 enabled=true")
}

func TestBackfillAllocationKeys_RestoresActiveIntents(t *testing.T) {
	database, err := gorm.Open(sqlite.Open("file:backfill-"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.PaymentIntent{}))

	intent := model.PaymentIntent{
		ID:             "intent_old_schema",
		OrderID:        "order_old_schema",
		Chain:          model.ChainArbitrum,
		Token:          model.TokenUSDC,
		TargetAddress:  "0x7bdc49542978b16566e82c8f90db1eb03804c675",
		ExpectedAmount: decimal.RequireFromString("10.0001"),
		Status:         model.StatusWatching,
	}
	require.NoError(t, database.Create(&intent).Error)

	backfillAllocationKeys(database)

	var restored model.PaymentIntent
	require.NoError(t, database.First(&restored, "id = ?", intent.ID).Error)
	require.NotNil(t, restored.AllocationKey)
	require.Equal(t, model.BuildAllocationKey(intent.Chain, intent.Token, intent.TargetAddress, intent.ExpectedAmount), *restored.AllocationKey)
}
