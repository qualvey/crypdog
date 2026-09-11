package scanner

import (
	"context"
	"crypdog/internal/config"
	"crypdog/internal/model"
	"testing"

	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

// 虚拟的 Scanner 实现，用于测试注册逻辑
type MockScanner struct{ BaseScanner }

func (m *MockScanner) Chain() model.Chain {
	return model.ChainBsc
}
func (m *MockScanner) Start(ctx context.Context, transferChan chan<- model.ChainTransfer) error {
	return nil
}

func (m *MockScanner) GetLatestBlock() uint64 {
	return 0
}

// Ptr 返回任意类型值的指针
func Ptr[T any](v T) *T {
	return &v
}
func TestManager_RegisterFromConfig(t *testing.T) {
	// 1. Arrange（准备假环境）
	mgr := NewManager()
	calledChains := make(map[model.Chain]bool)
	mgr.RegisterDriver("evm", func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner {
		calledChains[chain] = true
		return &MockScanner{}
	})
	mgr.RegisterDriver("eth", func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner {
		calledChains[chain] = true
		return &MockScanner{}
	})
	mgr.RegisterDriver("tron", func(chain model.Chain, db *gorm.DB, cfg *config.Config) Scanner {
		calledChains[chain] = true
		return &MockScanner{}
	})
	// 记录哪些链被真正调用了 factory

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
				Driver:  "solana", // 验证未知驱动不会导致崩溃
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
	mgr.RegisterFromConfig(cfg, nil) // db 传 nil 即可，这里不需要查库

	// 3. Assert（断言分流结果）
	assert.True(t, calledChains[model.ChainBsc], "正常启用的 EVM 链必须被注册")
	assert.False(t, calledChains[model.ChainEth], "禁用的链绝不能被注册")
	assert.False(t, calledChains[model.ChainTron], "未知驱动的链必须安全跳过")
	assert.False(t, calledChains["EMPTY_RPC_CHAIN"], "RPC 为空的链必须跳过")
	assert.Equal(t, 1, len(calledChains), "最终应该恰好只注册了 1 个扫描器")
}
func TestIsChainNodeEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.ChainNodeConfig
		expected bool
	}{
		{
			name:     "合法配置：显式开启且 RPC 正常",
			cfg:      config.ChainNodeConfig{Enabled: Ptr(true), RPCURL: "https://rpc.com"},
			expected: true,
		},
		{
			name:     "合法配置：Enabled 为 nil（默认开启）且 RPC 正常",
			cfg:      config.ChainNodeConfig{Enabled: nil, RPCURL: "https://rpc.com"},
			expected: true,
		},
		{
			name:     "非法配置：显式禁用",
			cfg:      config.ChainNodeConfig{Enabled: Ptr(false), RPCURL: "https://rpc.com"},
			expected: false,
		},
		{
			name:     "非法配置：RPC 为空字符串",
			cfg:      config.ChainNodeConfig{Enabled: Ptr(true), RPCURL: ""},
			expected: false,
		},
		{
			name:     "非法配置：RPC 为纯空白字符",
			cfg:      config.ChainNodeConfig{Enabled: Ptr(true), RPCURL: "   \t\n  "},
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
