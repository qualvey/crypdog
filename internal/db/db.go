package db

import (
	"fmt"
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
		// 自动追加 WAL 模式、5000ms busy_timeout 与 NORMAL synchronous，彻底解决多协程并发写入时的 database is locked
		dsn := cfg.Database.DSN
		if !strings.Contains(dsn, "_pragma") {
			delimiter := "?"
			if strings.Contains(dsn, "?") {
				delimiter = "&"
			}
			dsn = fmt.Sprintf("%s%s_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", dsn, delimiter)
		}
		log.Println("⚠️ [Database Notice] 当前使用 SQLite。生产环境或多副本集群部署建议切换为 PostgreSQL。已自动配置 WAL 模式与 5s 忙等待防锁。")
		DB, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
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

	chainAddrsMap := make(map[model.Chain][]string)
	for _, w := range wallets {
		addr := strings.TrimSpace(w.Address)
		if addr == "" || strings.TrimSpace(w.Chain) == "" {
			continue
		}
		normChain := model.NormalizeChain(w.Chain)
		chainAddrsMap[normChain] = append(chainAddrsMap[normChain], addr)

		// 1. 配置中存在的地址：更新或插入，确保 enabled = true
		wallet := model.WalletAddress{
			Chain:     normChain,
			Address:   addr,
			Label:     strings.TrimSpace(w.Label),
			Enabled:   true,
			UpdatedAt: time.Now(),
		}
		// 使用 OnConflict 在 (chain, address) 已存在时恢复 enabled = true 并更新 label
		database.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "chain"}, {Name: "address"}},
			DoUpdates: clause.AssignmentColumns([]string{"enabled", "label", "updated_at"}),
		}).Create(&wallet)
	}

	// 2. 关键：仅针对本次配置中涉及的公链，不在配置列表里的旧地址执行软下线
	// 避免配置了某条链钱包却导致已有的其他链收款地址被误伤全量禁用
	for chain, addrs := range chainAddrsMap {
		if len(addrs) > 0 {
			database.Model(&model.WalletAddress{}).
				Where("chain = ? AND address NOT IN ?", chain, addrs).
				Update("enabled", false)
		}
	}
}
