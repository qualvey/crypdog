package scanner

import (
	"context"
	"errors"
	"testing"

	"crypdog/internal/config"
	"crypdog/internal/model"

	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

// Ptr 返回任意类型值的指针
func Ptr[T any](v T) *T {
	return &v
}

// MockScanner 实现 Scanner 接口用于单元测试
type MockScanner struct {
	chain       model.Chain
	latestBlock uint64
	startErr    error
}

func (m *MockScanner) Chain() model.Chain {
	return m.chain
}

func (m *MockScanner) Start(ctx context.Context, out chan<- model.ChainTransfer) error {
	return m.startErr
}

func (m *MockScanner) GetLatestBlock() uint64 {
	return m.latestBlock
}

func (m *MockScanner) SupportedTokens() []model.TokenSpec {
	return nil
}

func TestManager_RegisterFromConfig(t *testing.T) {
	// 1. Arrange（准备假环境）
	mgr := NewManager()
	calledChains := make(map[model.Chain]bool)
	mgr.RegisterDriver("evm", func(chain model.Chain, db *gorm.DB, cfg config.ChainNodeConfig) (Scanner, error) {
		calledChains[chain] = true
		return &MockScanner{chain: chain}, nil
	})
	mgr.RegisterDriver("tron", func(chain model.Chain, db *gorm.DB, cfg config.ChainNodeConfig) (Scanner, error) {
		calledChains[chain] = true
		return &MockScanner{chain: chain}, nil
	})

	// 构造具有代表性的 4 种边界配置
	cfg := &config.Config{
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainBsc: {
				Enabled: Ptr(true),
				Driver:  " EVM ", // 验证大小写与空格容错
				RPCURL:  "https://mock-rpc",
			},
			model.ChainEth: {
				Enabled: Ptr(false), // 验证禁用逻辑
				Driver:  "evm",
				RPCURL:  "https://mock-rpc",
			},
			model.ChainTron: {
				Enabled: Ptr(true),
				Driver:  "solana", // 验证未知驱动（solana 未注册）不会导致崩溃且安全跳过
				RPCURL:  "https://mock-rpc",
			},
			// 4. 边界：RPC 为空 -> 应跳过
			"EMPTY_RPC_CHAIN": {
				Enabled: Ptr(true),
				Driver:  "evm",
				RPCURL:  "   ",
			},
		},
	}

	// 2. Act（执行）
	err := mgr.RegisterFromConfig(cfg, nil) // db 传 nil 即可，这里不需要查库
	assert.NoError(t, err)

	// 3. Assert（断言分流结果）
	assert.True(t, calledChains[model.ChainBsc], "正常启用的 EVM 链必须被注册")
	assert.False(t, calledChains[model.ChainEth], "禁用的链绝不能被注册")
	assert.False(t, calledChains[model.ChainTron], "未知驱动的链必须安全跳过")
	assert.False(t, calledChains["EMPTY_RPC_CHAIN"], "RPC 为空的链必须跳过")
	assert.Equal(t, 1, len(calledChains), "最终应该恰好只注册了 1 个扫描器")
}

func TestManager_RegisterFromConfig_InferDriver(t *testing.T) {
	mgr := NewManager()
	calledChains := make(map[model.Chain]bool)
	mgr.RegisterDriver("evm", func(chain model.Chain, db *gorm.DB, cfg config.ChainNodeConfig) (Scanner, error) {
		calledChains[chain] = true
		return &MockScanner{chain: chain}, nil
	})

	cfg := &config.Config{
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainEth: {
				Enabled: Ptr(true),
				Driver:  "", // 未指定驱动，应根据 Chain 推断为 evm
				RPCURL:  "https://eth-rpc",
			},
		},
	}

	err := mgr.RegisterFromConfig(cfg, nil)
	assert.NoError(t, err)
	assert.True(t, calledChains[model.ChainEth], "未指定 driver 时应自动推断并成功注册")
}

func TestManager_RegisterFromConfig_FactoryError(t *testing.T) {
	mgr := NewManager()
	mgr.RegisterDriver("evm", func(chain model.Chain, db *gorm.DB, cfg config.ChainNodeConfig) (Scanner, error) {
		return nil, errors.New("factory init error")
	})

	cfg := &config.Config{
		Chains: map[model.Chain]config.ChainNodeConfig{
			model.ChainBsc: {
				Enabled: Ptr(true),
				Driver:  "evm",
				RPCURL:  "https://mock-rpc",
			},
		},
	}

	err := mgr.RegisterFromConfig(cfg, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "初始化链 BSC 扫描器失败")
}

func TestManager_GetLatestBlockAndStatus(t *testing.T) {
	mgr := NewManager()
	s := &MockScanner{chain: model.ChainBsc, latestBlock: 12345}
	mgr.Register(s)

	assert.Equal(t, uint64(12345), mgr.GetLatestBlock(model.ChainBsc))
	assert.Equal(t, uint64(0), mgr.GetLatestBlock(model.ChainEth))

	status := mgr.GetScannersStatus()
	assert.Equal(t, uint64(12345), status[string(model.ChainBsc)])
}

func TestManager_StartAll(t *testing.T) {
	mgr := NewManager()
	s := &MockScanner{chain: model.ChainBsc}
	mgr.Register(s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan model.ChainTransfer)
	mgr.StartAll(ctx, out)
}

func TestInferDriverByChain(t *testing.T) {
	assert.Equal(t, "evm", inferDriverByChain(model.ChainEth))
	assert.Equal(t, "evm", inferDriverByChain(model.ChainBsc))
	assert.Equal(t, "tron", inferDriverByChain(model.ChainTron))
	assert.Equal(t, "solana", inferDriverByChain(model.ChainSolana))
	assert.Equal(t, "", inferDriverByChain("UNKNOWN_CHAIN"))
}

func TestIsChainNodeEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.ChainNodeConfig
		expected bool
	}{
		{
			name:     "合法配置：显式开启且 RPC 正常",
			cfg:      &config.ChainNodeConfig{Enabled: Ptr(true), RPCURL: "https://rpc.com"},
			expected: true,
		},
		{
			name:     "合法配置：Enabled 为 nil（默认开启）且 RPC 正常",
			cfg:      &config.ChainNodeConfig{Enabled: nil, RPCURL: "https://rpc.com"},
			expected: true,
		},
		{
			name:     "非法配置：显式禁用",
			cfg:      &config.ChainNodeConfig{Enabled: Ptr(false), RPCURL: "https://rpc.com"},
			expected: false,
		},
		{
			name:     "非法配置：RPC 为空字符串",
			cfg:      &config.ChainNodeConfig{Enabled: Ptr(true), RPCURL: ""},
			expected: false,
		},
		{
			name:     "非法配置：RPC 为纯空白字符",
			cfg:      &config.ChainNodeConfig{Enabled: Ptr(true), RPCURL: "   \t\n  "},
			expected: false,
		},
		{
			name:     "非法配置：nodeCfg 为 nil",
			cfg:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isChainNodeEnabled(tt.cfg)
			assert.Equal(t, tt.expected, got)
		})
	}
}
