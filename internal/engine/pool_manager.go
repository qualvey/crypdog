package engine

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"crypdog/internal/model"

	"gorm.io/gorm"
)

type MicroAmountManager struct {
	db *gorm.DB
	mu sync.Mutex
}

func NewMicroAmountManager(db *gorm.DB) *MicroAmountManager {
	return &MicroAmountManager{
		db: db,
	}
}

const (
	defaultMinTail  = 0.0001
	defaultMaxTail  = 0.0999
	defaultTailStep = 0.0001
	floatEpsilon    = 0.000001
)

// AllocateUniqueAmount finds and reserves the smallest unassigned micro-amount for a given (chain, token, targetAddress, baseAmount) scope
func (m *MicroAmountManager) AllocateUniqueAmount(chain model.Chain, token model.Token, baseAmount float64) (string,float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var wallet model.WalletAddress
	err := m.db.Where("chain = ? AND enabled = ?", strings.ToUpper(string(chain)), true).
		Order("last_used_at ASC"). // 轮换策略：优先使用最久未使用的地址
		First(&wallet).Error
	if err != nil {
		return "", 0, fmt.Errorf("无可用收款地址: %w", err)
	}
	targetAddress := wallet.Address
	// 1. Fetch all currently active (WATCHING or CONFIRMING) intents for this target address
	var activeIntents []model.PaymentIntent
	err = m.db.Where("status IN (?, ?) AND UPPER(chain) = ? AND UPPER(token) = ?",
		model.StatusWatching,
		model.StatusConfirming,
		chain,
		token,
	).Find(&activeIntents).Error

	if err != nil {
		return "", 0, fmt.Errorf("failed to query active intents: %w", err)
	}

	// 2. Build map of currently occupied amounts
	occupied := make(map[int]bool)
	for _, it := range activeIntents {
		diff := it.ExpectedAmount - baseAmount
		if diff >= (defaultMinTail-floatEpsilon) && diff <= (defaultMaxTail+floatEpsilon) {
			// convert offset to index (e.g. 0.0001 -> 1, 0.0002 -> 2)
			stepIdx := int(math.Round(diff / defaultTailStep))
			occupied[stepIdx] = true
		}
	}

	// 3. Find first unoccupied tail offset
	maxSteps := int(math.Round((defaultMaxTail - defaultMinTail) / defaultTailStep)) + 1
	for i := 1; i <= maxSteps; i++ {
		if !occupied[i] {
			tail := float64(i) * defaultTailStep
			// Format to 6 decimal places to prevent binary float inaccuracies
			allocated := math.Round((baseAmount+tail)*1000000) / 1000000
			return targetAddress,allocated, nil
		}
	}

	return "",0, fmt.Errorf("all micro-amount slots in range [%.4f, %.4f] are currently occupied for address %s",
		defaultMinTail, defaultMaxTail, targetAddress)
}

// CheckCollision checks if an expected amount is already in use by another active intent
func (m *MicroAmountManager) CheckCollision(chain model.Chain, token model.Token, targetAddress string, expectedAmount float64, excludeOrderID string) (bool, error) {
	targetAddress = strings.ToLower(strings.TrimSpace(targetAddress))

	var activeIntents []model.PaymentIntent
	query := m.db.Where("status IN (?, ?) AND UPPER(chain) = ? AND UPPER(token) = ? AND LOWER(target_address) = ?",
		model.StatusWatching,
		model.StatusConfirming,
		chain,
		token,
		targetAddress,
	)

	if excludeOrderID != "" {
		query = query.Where("order_id != ?", excludeOrderID)
	}

	if err := query.Find(&activeIntents).Error; err != nil {
		return false, err
	}

	for _, it := range activeIntents {
		if math.Abs(it.ExpectedAmount-expectedAmount) < floatEpsilon {
			return true, nil // Collision detected!
		}
	}

	return false, nil
}
