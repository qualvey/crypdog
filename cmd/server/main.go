package main

import (
	"log"

	"crypdog/internal/app"
	"crypdog/internal/config"
	"crypdog/internal/db"

	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	log.Println("🐕 Starting CrypDog (Crypto Payment Watchdog Daemon)")

	// 1. 初始化数据库与全局上下文（必须在最前）
	database := db.InitDB(cfg)
	App := app.NewApp(cfg, database)
	if err := App.Run(); err != nil {
		log.Fatal(err)
	}
	log.Println("🐕 CrypDog 已安全平稳停止")
}
