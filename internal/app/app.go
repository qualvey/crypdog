package app

import (
	"context"
	"crypdog/internal/api"
	"crypdog/internal/config"
	"crypdog/internal/engine"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"crypdog/internal/queue"
	"crypdog/internal/scanner"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"
)

type App struct {
	cfg           *config.Config
	db            *gorm.DB
	scannerMgr    *scanner.Manager
	dispatcher    *queue.WebhookDispatcher
	matcher       *engine.MatcherEngine
	confirmWorker *queue.ConfirmationWorker
	cleaner       *queue.IntentCleaner
	srv           *http.Server

	transferChan chan model.ChainTransfer
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func NewApp(cfg *config.Config, db *gorm.DB) *App {
	return &App{
		cfg: cfg,
		db:  db,
	}
}

func (a *App) Run() error {
	// 初始化指标收集系统
	metrics.InitMetrics()

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	defer cancel()
	a.transferChan = make(chan model.ChainTransfer, 500)
	a.dispatcher = queue.NewWebhookDispatcher(a.db, a.cfg)
	// 3. 扫描器（细节放到 scanner 包）
	a.scannerMgr = scanner.NewManager()

	a.scannerMgr.RegisterDriver("evm", func(chain model.Chain, db *gorm.DB, chainCfg config.ChainNodeConfig) (scanner.Scanner, error) {
		return scanner.NewEvmScanner(chain, db, &chainCfg)
	})
	a.scannerMgr.RegisterDriver("tron", func(chain model.Chain, db *gorm.DB, chainCfg config.ChainNodeConfig) (scanner.Scanner, error) {
		return scanner.NewTronScanner(db, &chainCfg)
	})
	a.scannerMgr.RegisterDriver("solana", func(chain model.Chain, db *gorm.DB, chainCfg config.ChainNodeConfig) (scanner.Scanner, error) {
		return scanner.NewSolanaScanner(db, &chainCfg)
	})

	a.scannerMgr.RegisterFromConfig(a.cfg, a.db) // 关键：注册逻辑内聚到 scanner
	// 4. 撮合引擎（注入链上收据二次核验，拦截假充值）
	a.matcher = engine.NewMatcherEngine(a.db, a.cfg, a.dispatcher, a.scannerMgr.VerifyTransaction)
	go a.runPipeline(ctx)

	a.scannerMgr.StartAll(ctx, a.transferChan)

	a.confirmWorker = queue.NewConfirmationWorker(a.db, a.cfg, a.dispatcher, a.scannerMgr.GetLatestBlock, a.scannerMgr.VerifyTransaction)
	go a.confirmWorker.StartWorker(ctx, 2*time.Second)
	a.cleaner = queue.NewIntentCleaner(a.db)
	go a.cleaner.StartCleaner(ctx, 10*time.Second)
	go a.dispatcher.StartRetryWorker(ctx, 3*time.Second)
	// 5. HTTP
	if err := a.startHTTP(); err != nil {
		return err
	}
	// 6. 等停机信号
	return a.waitForShutdown(ctx)
}

func (a *App) runPipeline(ctx context.Context) {

	// 关联 App 内部的 WaitGroup（需在 struct 中声明 a.wg sync.WaitGroup）
	a.wg.Add(1)
	defer a.wg.Done()

	log.Println("[Pipeline] 交易处理消费者已就绪")
	a.recoverUnmatchedTransfers(ctx)

	for {
		select {
		case <-ctx.Done():
			// 停机阶段：排空 Channel 剩余积压交易，防止丢单
			log.Println("[Pipeline] 收到退出信号，开始排空队列...")
			for {
				select {
				case t, ok := <-a.transferChan:
					if !ok {
						log.Println("[Pipeline] 队列已关闭，消费者退出")
						return
					}
					a.safeProcess(t)
				default:
					// 缓冲区当前已无积压数据，正常退出
					log.Println("[Pipeline] 队列积压消费完毕，消费者安全退出")
					return
				}
			}

		case transfer, ok := <-a.transferChan:
			if !ok {
				log.Println("[Pipeline] Channel 关闭，消费者退出")
				return
			}
			metrics.SetPipelineQueueLength(len(a.transferChan))
			a.safeProcess(transfer)
		}
	}
}

func (a *App) recoverUnmatchedTransfers(ctx context.Context) {
	if a.db == nil {
		return
	}
	var unmatched []model.ChainTransfer
	since := time.Now().Add(-48 * time.Hour)
	err := a.db.WithContext(ctx).
		Where("(matched_order_id = '' OR matched_order_id IS NULL) AND created_at >= ?", since).
		Order("id ASC").
		Limit(200).
		Find(&unmatched).Error

	if err != nil || len(unmatched) == 0 {
		return
	}

	log.Printf("[Pipeline Recovery] 发现 %d 笔历史未撮合流水，开始自动重试撮合...", len(unmatched))
	for _, tr := range unmatched {
		a.safeReprocess(tr)
	}
}

func (a *App) safeReprocess(t model.ChainTransfer) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] 重试撮合历史转账失败: %v, 数据: %+v", r, t)
		}
	}()

	currentBlock := a.scannerMgr.GetLatestBlock(t.Chain)
	if currentBlock < t.BlockNumber {
		currentBlock = t.BlockNumber
	}
	if err := a.matcher.ReprocessUnmatchedTransfer(t, currentBlock); err != nil {
		log.Printf("[Matcher Recovery] 重试撮合历史转账失败: %v, Tx=%s", err, t.TxHash)
	}
}

func (a *App) safeProcess(t model.ChainTransfer) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] 处理转账失败: %v, 数据: %+v", r, t)
		}
	}()

	currentBlock := a.scannerMgr.GetLatestBlock(t.Chain)
	if currentBlock < t.BlockNumber {
		currentBlock = t.BlockNumber
	}
	if err := a.matcher.ProcessTransfer(t, currentBlock); err != nil {
		log.Printf("[Matcher] 处理转账失败: %v, Tx=%s", err, t.TxHash)
	}
}

func (a *App) startHTTP() error {
	// 7. 启动 HTTP API 服务
	poolManager := engine.NewMicroAmountManager(a.db)
	handler := api.NewHandler(a.db, a.cfg, poolManager, a.scannerMgr, a.transferChan)
	ginHandler := api.SetupRouter(handler)

	a.srv = &http.Server{
		Addr:    fmt.Sprintf(":%s", a.cfg.Server.Port),
		Handler: ginHandler,
	}

	go func() {
		log.Printf("🚀 CrypDog running on HTTP :%s", a.cfg.Server.Port)
		if err := a.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()
	return nil

}

func (a *App) waitForShutdown(ctx context.Context) error {
	// 1. 监听系统中断信号（Ctrl+C / kill 命令）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// 阻塞在此处，直到外界发来停止信号
	sig := <-quit
	log.Printf("接收到系统信号 [%v]，正在执行平滑退出...", sig)

	// 2. 优先关闭 HTTP 服务（设置 8s 超时），拒接新的外部请求
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shutdownCancel()
	if a.srv != nil {
		if err := a.srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[HTTP] 停机超时或出错: %v", err)
		} else {
			log.Println("[HTTP] 服务已优雅停止")
		}
	}

	// 3. 级联通知所有后台协程（Scanners、Workers、Pipeline 等）准备停机
	if a.cancel != nil {
		a.cancel()
	}

	// 4. 阻塞等待所有已纳管的后台 Goroutine（如 Pipeline 排空）彻底收尾
	a.wg.Wait()

	log.Println("🐕 CrypDog 已安全平稳停止")
	return nil
}
