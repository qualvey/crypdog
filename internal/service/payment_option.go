package service

import (
	"context"
	"sort"

	"crypdog/internal/config"
	"crypdog/internal/model"
	"crypdog/internal/scanner"

	"gorm.io/gorm"
)

// PaymentOptionService 负责计算前端可用的支付 Token 与 Chain。
// Handler 不直接参与数据库、配置和 Scanner 状态判断。
type PaymentOptionService struct {
	db         *gorm.DB
	cfg        *config.Config
	scannerMgr *scanner.Manager
}

func NewPaymentOptionService(
	db *gorm.DB,
	cfg *config.Config,
	scannerMgr *scanner.Manager,
) *PaymentOptionService {
	return &PaymentOptionService{
		db:         db,
		cfg:        cfg,
		scannerMgr: scannerMgr,
	}
}

func (s *PaymentOptionService) GetOptions(ctx context.Context) (model.CryptoPaymentOptions, error) {
	activeChains, err := s.getActiveWalletChains(ctx)
	if err != nil {
		return model.CryptoPaymentOptions{}, err
	}

	availableChains := s.getAvailablePaymentChains(activeChains)
	if len(availableChains) == 0 {
		return emptyPaymentOptions(), nil
	}

	tokens, err := s.getEnabledPaymentTokens(ctx)
	if err != nil {
		return model.CryptoPaymentOptions{}, err
	}
	return buildPaymentOptions(availableChains, tokens), nil
}

func emptyPaymentOptions() model.CryptoPaymentOptions {
	return model.CryptoPaymentOptions{
		Tokens: []model.CryptoTokenOption{},
		Chains: make(map[string][]model.CryptoChainOption),
	}
}

func (s *PaymentOptionService) getActiveWalletChains(ctx context.Context) (map[string]bool, error) {
	var wallets []model.WalletAddress
	if err := s.db.WithContext(ctx).
		Where("enabled = ?", true).
		Find(&wallets).Error; err != nil {
		return nil, err
	}

	chains := make(map[string]bool, len(wallets))
	for _, wallet := range wallets {
		chains[string(model.NormalizeChain(string(wallet.Chain)))] = true
	}
	return chains, nil
}

func (s *PaymentOptionService) getAvailablePaymentChains(activeChains map[string]bool) map[string]bool {
	available := make(map[string]bool)
	if s.cfg == nil || len(s.cfg.Chains) == 0 {
		for chain := range activeChains {
			available[chain] = true
		}
	} else {
		for chain, nodeCfg := range s.cfg.Chains {
			if nodeCfg.Enabled != nil && !*nodeCfg.Enabled {
				continue
			}
			normalized := string(model.NormalizeChain(string(chain)))
			if activeChains[normalized] {
				available[normalized] = true
			}
		}
	}

	// 配置和钱包状态都满足时，还必须确认 Scanner 最近成功完成过 RPC 检查。
	if s.scannerMgr != nil && s.scannerMgr.HasScanners() {
		healthyChains := s.scannerMgr.GetAvailableChains()
		for chain := range available {
			if !healthyChains[chain] {
				delete(available, chain)
			}
		}
	}
	return available
}

func (s *PaymentOptionService) getEnabledPaymentTokens(ctx context.Context) ([]model.ChainToken, error) {
	var tokens []model.ChainToken
	if err := s.db.WithContext(ctx).
		Where("enabled = ?", true).
		Order("priority DESC, id ASC").
		Find(&tokens).Error; err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return model.GetDefaultChainTokens(), nil
	}
	return tokens, nil
}

func buildPaymentOptions(availableChains map[string]bool, dbTokens []model.ChainToken) model.CryptoPaymentOptions {
	options := emptyPaymentOptions()
	seenTokens := make(map[string]bool)

	for _, token := range dbTokens {
		if !availableChains[string(model.NormalizeChain(string(token.Chain)))] {
			continue
		}

		symbol := string(token.Symbol)
		chainMeta := model.GetChainDisplayMeta(token.Chain)
		chainName, chainBadge := chainMeta.Name, chainMeta.Badge
		if chainName == "" {
			chainName = string(token.Chain)
		}
		if token.Badge != "" {
			chainBadge = token.Badge
		}
		options.Chains[symbol] = append(options.Chains[symbol], model.CryptoChainOption{
			Chain: token.Chain, Name: chainName, Badge: chainBadge, Decimals: token.Decimals,
		})

		if !seenTokens[symbol] {
			seenTokens[symbol] = true
			options.Tokens = append(options.Tokens, paymentTokenOption(token))
		}
	}

	// Each token must expose chains in the same canonical order. The database
	// order is token-oriented (priority/id), so it cannot be used directly here.
	for symbol := range options.Chains {
		sort.SliceStable(options.Chains[symbol], func(i, j int) bool {
			left := model.GetChainDisplayMeta(options.Chains[symbol][i].Chain)
			right := model.GetChainDisplayMeta(options.Chains[symbol][j].Chain)
			leftOrder, rightOrder := left.Order, right.Order
			if leftOrder == 0 {
				leftOrder = int(^uint(0) >> 1)
			}
			if rightOrder == 0 {
				rightOrder = int(^uint(0) >> 1)
			}
			if leftOrder != rightOrder {
				return leftOrder < rightOrder
			}
			return options.Chains[symbol][i].Chain < options.Chains[symbol][j].Chain
		})
	}

	setDefaultPaymentOption(&options)
	return options
}

func paymentTokenOption(token model.ChainToken) model.CryptoTokenOption {
	option := model.GetTokenDisplayMeta(token.Symbol)
	option.Symbol = string(token.Symbol)
	if token.Name != "" {
		option.Name = token.Name
	}
	if token.Icon != "" {
		option.Icon = token.Icon
	}
	if option.Name == "" {
		option.Name = string(token.Symbol)
	}
	if option.Icon == "" {
		option.Icon = "fa-solid fa-coins"
	}
	return option
}

func setDefaultPaymentOption(options *model.CryptoPaymentOptions) {
	if len(options.Tokens) == 0 {
		return
	}
	options.DefaultToken = options.Tokens[0].Symbol
	for _, token := range options.Tokens {
		if token.Symbol == "USDT" {
			options.DefaultToken = token.Symbol
			break
		}
	}
	if chains := options.Chains[options.DefaultToken]; len(chains) > 0 {
		options.DefaultChain = string(chains[0].Chain)
	}
}
