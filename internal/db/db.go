package db

import (
	"log"
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
		DB, err = gorm.Open(sqlite.Open(cfg.Database.DSN), &gorm.Config{})
	}

	if err != nil {
		log.Fatalf("Failed to connect to database (%s): %v", cfg.Database.Driver, err)
	}

	log.Printf("Successfully connected to database (%s)", cfg.Database.Driver)

	// Auto migrate tables
	err = DB.AutoMigrate(
		&model.PaymentIntent{},
		&model.ChainTransfer{},
		&model.WebhookLog{},
		&model.WalletAddress{},
		&model.ScanProgress{},
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database schema: %v", err)
	}

	log.Println("Database auto-migration completed successfully")

	// Seed initial wallets if configured
	seedInitialWallets(DB, cfg.InitialWallets)

	sqlDB, err := DB.DB()
	if err != nil {
		log.Fatalf("Failed to get generic database object: %v", err)
	}

	// 设置最大空闲连接数
	sqlDB.SetMaxIdleConns(10)
	// 设置最大打开连接数
	sqlDB.SetMaxOpenConns(100)
	// 设置连接最大生命周期（防止连接被防火墙/云数据库单方面切断）
	sqlDB.SetConnMaxLifetime(time.Hour)
	return DB
}

func seedInitialWallets(database *gorm.DB, wallets []config.InitialWallet) {
	if len(wallets) == 0 {
		return
	}

	var activeAddrs []string
	for _, w := range wallets {
		addr := strings.TrimSpace(w.Address)
		if addr == "" || strings.TrimSpace(w.Chain) == "" {
			continue
		}
		activeAddrs = append(activeAddrs, addr)

		// 1. 配置中存在的地址：更新或插入，确保 enabled = true
		wallet := model.WalletAddress{
			Chain:     model.NormalizeChain(w.Chain),
			Address:   addr,
			Label:     strings.TrimSpace(w.Label),
			Enabled:   true,
			UpdatedAt: time.Now(),
		}
		// 使用 OnConflict 在地址已存在时恢复 enabled = true 并更新 label
		database.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "address"}},
			DoUpdates: clause.AssignmentColumns([]string{"enabled", "label", "chain", "updated_at"}),
		}).Create(&wallet)
	}

	// 2. 关键：不在当前配置列表里的旧地址，自动软下线 (enabled = false)
	// 既不破坏历史订单的外键和数据审计，新订单也不会再分配到被剔除的旧地址
	if len(activeAddrs) > 0 {
		database.Model(&model.WalletAddress{}).
			Where("address NOT IN ?", activeAddrs).
			Update("enabled", false)
	}
}
