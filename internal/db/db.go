package db

import (
	"crypdog/internal/logger"
	"fmt"

	"strings"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var DB *gorm.DB

func InitDB(cfg *config.Config) *gorm.DB {
	var err error
	if cfg.Database.Driver == "postgres" {
		DB, err = gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
	} else {
		// Pure-Go SQLite driver for zero-CGO local development and testing
		// 自动追加 WAL 模式、5000ms busy_timeout 与 NORMAL synchronous，彻底解决多协程并发写入时的 database is locked
		dsn := cfg.Database.DSN
		if !strings.Contains(dsn, "_pragma") {
			delimiter := "?"
			if strings.Contains(dsn, "?") {
				delimiter = "&"
			}
			dsn = fmt.Sprintf("%s%s_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", dsn, delimiter)
		}
		logger.Warn("SQLite database in use; PostgreSQL is recommended for production or multi-replica deployments", "driver", cfg.Database.Driver)
		DB, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	}

	if err != nil {
		logger.Fatal("database connection failed", "driver", cfg.Database.Driver, "error", err)
	}

	logger.Info("database connected", "driver", cfg.Database.Driver)

	// Auto migrate tables
	err = DB.AutoMigrate(
		&model.PaymentIntent{},
		&model.ChainTransfer{},
		&model.WebhookLog{},
		&model.AdminAuditLog{},
		&model.WalletAddress{},
		&model.ScanProgress{},
		&model.ChainToken{},
	)
	if err != nil {
		logger.Fatal("database migration failed", "error", err)
	}

	logger.Info("database migration completed")
	backfillAllocationKeys(DB)

	// 播种初始默认代币与收款钱包
	seedInitialTokens(DB)
	seedInitialWallets(DB, cfg.InitialWallets)

	sqlDB, err := DB.DB()
	if err != nil {
		logger.Fatal("get generic database object failed", "error", err)
	}

	// 设置最大空闲连接数
	sqlDB.SetMaxIdleConns(10)
	// 设置最大打开连接数
	sqlDB.SetMaxOpenConns(100)
	// 设置连接最大生命周期（防止连接被防火墙/云数据库单方面切断）
	sqlDB.SetConnMaxLifetime(time.Hour)
	return DB
}

// backfillAllocationKeys upgrades databases created before the allocation
// uniqueness key existed. A duplicate active key intentionally fails loudly
// during startup instead of allowing ambiguous payment matching.
func backfillAllocationKeys(database *gorm.DB) {
	var intents []model.PaymentIntent
	if err := database.Where("allocation_key IS NULL AND status IN ?", []model.IntentStatus{
		model.StatusWatching, model.StatusConfirming,
	}).Find(&intents).Error; err != nil {
		logger.Fatal("allocation key backfill query failed", "error", err)
	}
	for i := range intents {
		key := model.BuildAllocationKey(intents[i].Chain, intents[i].Token, intents[i].TargetAddress, intents[i].ExpectedAmount)
		if err := database.Model(&model.PaymentIntent{}).
			Where("id = ? AND allocation_key IS NULL", intents[i].ID).
			Update("allocation_key", key).Error; err != nil {
			logger.Fatal("allocation key backfill failed", "order_id", intents[i].OrderID, "error", err)
		}
	}
}

func seedInitialTokens(database *gorm.DB) {
	defaultTokens := model.GetDefaultChainTokens()

	for _, t := range defaultTokens {
		t.CreatedAt = time.Now()
		t.UpdatedAt = time.Now()
		database.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "chain"}, {Name: "symbol"}},
			DoNothing: true,
		}).Create(&t)
	}
}

func seedInitialWallets(database *gorm.DB, wallets []config.InitialWallet) {
	if len(wallets) == 0 {
		return
	}

	for _, w := range wallets {
		addr := strings.TrimSpace(w.Address)
		if addr == "" || strings.TrimSpace(w.Chain) == "" {
			continue
		}
		normChain := model.NormalizeChain(w.Chain)

		wallet := model.WalletAddress{
			Chain:     normChain,
			Address:   addr,
			Label:     strings.TrimSpace(w.Label),
			Enabled:   true,
			UpdatedAt: time.Now(),
		}
		database.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "chain"}, {Name: "address"}},
			DoUpdates: clause.AssignmentColumns([]string{"enabled", "label", "updated_at"}),
		}).Create(&wallet)
	}
}
