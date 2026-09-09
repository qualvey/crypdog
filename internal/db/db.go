package db

import (
	"log"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database schema: %v", err)
	}

	log.Println("Database auto-migration completed successfully")
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
