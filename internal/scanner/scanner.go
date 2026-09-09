package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/model"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

type Scanner interface {
	Chain() model.Chain
	Start(ctx context.Context, out chan<- model.ChainTransfer) error
	GetLatestBlock() uint64
}
type ScannerFactory func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner

// BaseScanner 提供通用的连接句柄与状态缓存
type BaseScanner struct {
	db          *gorm.DB
	cfg         *config.Config
	client      *http.Client
	mu          sync.RWMutex
	latestBlock int64
}

// 2. 维护 Driver 与构造器的映射（可通过 Init 动态扩展）
var scannerDrivers = map[string]ScannerFactory{
	"evm": func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner {
		return NewEvmScanner(chain, db, cfg)
	},
	"tron": func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner {
		nodeCfg := cfg.Chains[chain]
		return NewTronScanner(db, &nodeCfg)
	},
	"solana": func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner {
		nodeCfg := cfg.Chains[chain]
		return NewSolanaScanner(db, &nodeCfg)
	},
}

// RegisterDriver 方便未来在其他包直接注册新型链驱动（如 btc, sui 等）
func RegisterDriver(driver string, factory ScannerFactory) {
	scannerDrivers[driver] = factory
}
func NewBaseScanner(db *gorm.DB, cfg *config.Config, timeout time.Duration) BaseScanner {
	return BaseScanner{
		db:  db,
		cfg: cfg,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// 统一提供线程安全的块高读取与更新
func (b *BaseScanner) GetLatestBlock() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.latestBlock
}

func (b *BaseScanner) SetLatestBlock(block int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.latestBlock = block
}

type Simulator interface {
	SimulateTransfer(toAddress string, amount float64, token string, out chan<- model.ChainTransfer) string
}
type Manager struct {
	scanners map[string]Scanner
	mu       sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{
		scanners: make(map[string]Scanner),
	}
}

// Register 注册一个扫描器实例
func (m *Manager) Register(s Scanner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scanners[string(s.Chain())] = s
}

func (m *Manager) RegisterFromConfig(cfg *config.Config, db *gorm.DB) {

	for chain, nodeCfg := range cfg.Chains {
		// 校验是否启用
		if !isChainNodeEnabled(nodeCfg) {
			log.Printf("[Init] 跳过链扫描器: %s (已禁用或未配置 RPC)", chain)
			continue
		}
		// 根据 Driver 查找对应的构造器
		driver := strings.ToLower(strings.TrimSpace(nodeCfg.Driver))
		factory, exists := scannerDrivers[driver]
		if !exists {
			log.Printf("[Init] 跳过链扫描器: %s (不支持的驱动类型: %s)", chain, nodeCfg.Driver)
			continue
		}

		log.Printf("[Init] 注册链扫描器: %s [%s] (%s)", chain, driver, nodeCfg.RPCURL)
		m.Register(factory(chain, db, cfg))

	}
}

// StartAll 并发拉起所有已启用的扫描器
func (m *Manager) StartAll(ctx context.Context, out chan<- model.ChainTransfer) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for name, s := range m.scanners {
		scannerInstance := s
		scannerName := name

		go func() {
			log.Printf("[Scanner] 正在拉起扫描引擎: %s", scannerName)
			if err := scannerInstance.Start(ctx, out); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[Scanner Error] 扫描器 %s 异常退出: %v", scannerName, err)
			}
		}()
	}
}

// GetLatestBlock 统一提供块高查询（代替原先零散的 heightProvider 闭包）
func (m *Manager) GetLatestBlock(chain model.Chain) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if s, ok := m.scanners[string(chain)]; ok {
		return s.GetLatestBlock()
	}
	return 0
}

func (m *Manager) SimulateTransfer(chain string, toAddress string, amount float64, token string, out chan<- model.ChainTransfer) (string, error) {
	m.mu.RLock()
	s, exists := m.scanners[strings.ToUpper(strings.TrimSpace(chain))]
	m.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("chain [%s] scanner not registered or disabled", chain)
	}

	// 类型断言：检查扫描器是否支持模拟
	simulator, ok := s.(Simulator)
	if !ok {
		return "", fmt.Errorf("chain [%s] does not support transfer simulation", chain)
	}

	txHash := simulator.SimulateTransfer(toAddress, amount, token, out)
	return txHash, nil
}

// 抽取独立的校验辅助函数，简化逻辑
func isChainNodeEnabled(nodeCfg config.ChainNodeConfig) bool {
	if strings.TrimSpace(nodeCfg.RPCURL) == "" {
		return false
	}
	if nodeCfg.Enabled != nil && !*nodeCfg.Enabled {
		return false
	}
	return true
}
