package main

import (
	"crypdog/internal/app"
	"crypdog/internal/config"
	"crypdog/internal/db"
	"crypdog/internal/logger"

	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.LoadConfig()
	if err != nil {
		logger.Fatal("configuration load failed", "error", err)
	}

	// 初始化统一日志系统
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath, cfg.Log.Timestamp, cfg.Log.MaxSizeMB, cfg.Log.MaxBackups)

	logger.Info("🐕 Starting CrypDog (Crypto Payment Watchdog Daemon)")

	// 1. 初始化数据库与全局上下文（必须在最前）
	database := db.InitDB(cfg)
	App := app.NewApp(cfg, database)
	if err := App.Run(); err != nil {
		logger.Fatal("application stopped with error", "error", err)
	}
	logger.Info("CrypDog stopped")
}
