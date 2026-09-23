package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/logger"
	"crypdog/internal/model"
	"errors"
	"fmt"

	"strings"
	"sync"

	"gorm.io/gorm"
)

type Scanner interface {
	Chain() model.Chain
	Start(ctx context.Context, out chan<- model.ChainTransfer) error
	GetLatestBlock() uint64
	SupportedTokens() []model.TokenSpec
}

// TxVerifier 供扫描器实现二次核验链上交易有效性（防假充值与重组回滚）
type TxVerifier interface {
	VerifyTransaction(ctx context.Context, txHash string, blockNumber uint64) (bool, error)
}

type ScannerFactory func(chain model.Chain, db *gorm.DB, chainCfg config.ChainNodeConfig) (Scanner, error)

type Simulator interface {
	SimulateTransfer(toAddress string, amount float64, token string, out chan<- model.ChainTransfer) string
}
type Manager struct {
	scanners       map[string]Scanner
	mu             sync.RWMutex
	scannerDrivers map[string]ScannerFactory
}

func NewManager() *Manager {
	return &Manager{
		scanners:       make(map[string]Scanner),
		scannerDrivers: make(map[string]ScannerFactory),
	}
}

// RegisterDriver 方便未来在其他包直接注册新型链驱动（如 btc, sui 等）
func (m *Manager) RegisterDriver(chain string, factory ScannerFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scannerDrivers[chain] = factory
}

// Register 注册一个扫描器实例
func (m *Manager) Register(s Scanner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scanners[string(s.Chain())] = s
}

func (m *Manager) RegisterFromConfig(cfg *config.Config, db *gorm.DB) error {

	for chain, nodeCfg := range cfg.Chains {
		// 显式禁用的链可以安全跳过；显式启用但配置不完整必须阻止启动。
		if nodeCfg.Enabled != nil && !*nodeCfg.Enabled {
			logger.Info("chain scanner disabled", "chain", chain)
			continue
		}
		if strings.TrimSpace(nodeCfg.RPCURL) == "" {
			if nodeCfg.Enabled != nil && *nodeCfg.Enabled {
				return fmt.Errorf("链 %s 已启用但未配置 RPC URL", chain)
			}
			logger.Warn("chain scanner skipped because RPC is not configured", "chain", chain)
			continue
		}
		// 根据 Driver 查找对应的构造器，未配置时自动根据链类型推断默认驱动
		driver := strings.ToLower(strings.TrimSpace(nodeCfg.Driver))
		if driver == "" {
			driver = inferDriverByChain(chain)
		}
		factory, exists := m.scannerDrivers[driver]
		if !exists {
			return fmt.Errorf("链 %s 使用了不支持的驱动类型: %s", chain, driver)
		}
		scanner, err := factory(chain, db, nodeCfg)
		if err != nil {
			return fmt.Errorf("初始化链 %s 扫描器失败: %w", chain, err)
		}
		m.Register(scanner)
		logger.Info("chain scanner loaded", "chain", chain, "driver", driver)
	}
	return nil
}

// inferDriverByChain 根据链标识推断默认底层驱动类型
func inferDriverByChain(chain model.Chain) string {
	c := strings.ToUpper(strings.TrimSpace(string(chain)))
	switch c {
	case "TRON", "TRC20":
		return "tron"
	case "SOLANA", "SOL":
		return "solana"
	case "ETH", "ETHEREUM", "BSC", "BINANCE", "POLYGON", "MATIC", "ARBITRUM", "ARB", "OPTIMISM", "BASE", "AVAX":
		return "evm"
	default:
		return ""
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
			logger.Info("starting chain scanner", "scanner", scannerName)
			if err := scannerInstance.Start(ctx, out); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("chain scanner stopped unexpectedly", "scanner", scannerName, "error", err)
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

// GetScannersStatus 返回所有已注册扫描器的当前块高状态
func (m *Manager) GetScannersStatus() map[string]uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := make(map[string]uint64)
	for name, s := range m.scanners {
		status[name] = s.GetLatestBlock()
	}
	return status
}

// VerifyTransaction 二次核验指定链交易的有效性，若扫描器支持 TxVerifier 则调用二次核验
func (m *Manager) VerifyTransaction(ctx context.Context, chain model.Chain, txHash string, blockNumber uint64) (bool, error) {
	m.mu.RLock()
	s, ok := m.scanners[string(chain)]
	m.mu.RUnlock()
	if !ok {
		// 缺失扫描器时不能默认放行，否则会绕过交易二次核验。
		return false, fmt.Errorf("链 %s 扫描器未注册，无法验证交易", chain)
	}

	if verifier, ok := s.(TxVerifier); ok {
		return verifier.VerifyTransaction(ctx, txHash, blockNumber)
	}

	return true, nil
}

// func (m *Manager) SimulateTransfer(chain string, toAddress string, amount *big.Int, token string, out chan<- model.ChainTransfer) (string, error) {
// 	m.mu.RLock()
// 	s, exists := m.scanners[strings.ToUpper(strings.TrimSpace(chain))]
// 	m.mu.RUnlock()

// 	if !exists {
// 		return "", fmt.Errorf("chain [%s] scanner not registered or disabled", chain)
// 	}

// 	// 类型断言：检查扫描器是否支持模拟
// 	simulator, ok := s.(Simulator)
// 	if !ok {
// 		return "", fmt.Errorf("chain [%s] does not support transfer simulation", chain)
// 	}

// 	txHash := simulator.SimulateTransfer(toAddress, amount, token, out)
// 	return txHash, nil
// }

// 抽取独立的校验辅助函数，简化逻辑
func isChainNodeEnabled(nodeCfg *config.ChainNodeConfig) bool {
	if nodeCfg == nil || strings.TrimSpace(nodeCfg.RPCURL) == "" {
		return false
	}
	if nodeCfg.Enabled != nil && !*nodeCfg.Enabled {
		return false
	}
	return true
}
