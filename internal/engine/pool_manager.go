package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"crypdog/internal/lock"
	"crypdog/internal/logger"
	"crypdog/internal/metrics"
	"crypdog/internal/model"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type MicroAmountManager struct {
	db     *gorm.DB
	locker lock.Locker
}

func NewMicroAmountManager(db *gorm.DB, lockers ...lock.Locker) *MicroAmountManager {
	var l lock.Locker
	if len(lockers) > 0 && lockers[0] != nil {
		l = lockers[0]
	} else {
		l = lock.NewKeyedMutexLocker()
	}
	return &MicroAmountManager{
		db:     db,
		locker: l,
	}
}

// 定义常量/配置（使用 Decimal 类型定义）
var (
	defaultMinTail  = decimal.NewFromFloat(0.0001) // 最小尾数
	defaultMaxTail  = decimal.NewFromFloat(0.0999) // 最大尾数
	defaultTailStep = decimal.NewFromFloat(0.0001) // 步长
)

// AllocateUniqueAmount finds and reserves the smallest unassigned micro-amount for a given (chain, token, baseAmount) scope.
// If the first wallet has all tail slots occupied, it automatically falls back to subsequent available wallets in the pool.
func (m *MicroAmountManager) AllocateUniqueAmount(chain model.Chain, token model.Token, baseAmount decimal.Decimal) (string, decimal.Decimal, error) {
	normChain := model.NormalizeChain(string(chain))
	lockKey := fmt.Sprintf("alloc:%s:%s", normChain, token)
	unlock, err := m.locker.Acquire(context.Background(), lockKey, 5*time.Second)
	if err != nil {
		return "", decimal.Zero, fmt.Errorf("failed to acquire allocation lock: %w", err)
	}
	defer unlock()
	rawChain := strings.ToUpper(strings.TrimSpace(string(chain)))

	var wallets []model.WalletAddress
	err = m.db.Where("(chain = ? OR chain = ?) AND enabled = ?", normChain, rawChain, true).
		Order("last_used_at ASC"). // 轮换策略：优先使用最久未使用的地址
		Find(&wallets).Error
	if err != nil || len(wallets) == 0 {
		return "", decimal.Zero, fmt.Errorf("无可用收款地址: %w", err)
	}

	minStep := int(defaultMinTail.Div(defaultTailStep).Round(0).IntPart())
	maxStep := int(defaultMaxTail.Div(defaultTailStep).Round(0).IntPart())

	for _, wallet := range wallets {
		targetAddress := wallet.Address

		// 1. Fetch all currently active (WATCHING or CONFIRMING) intents for this target address
		// 以及在 15 分钟冷却期内（最近 EXPIRED 或 CANCELLED）的订单，彻底防止迟到充值冒领
		cooldownSince := time.Now().Add(-15 * time.Minute)
		var activeIntents []model.PaymentIntent
		err = m.db.Where(
			"((status IN (?, ?)) OR (status IN (?, ?) AND updated_at >= ?)) AND (UPPER(chain) = ? OR UPPER(chain) = ?) AND UPPER(token) = ? AND (target_address = ? OR LOWER(target_address) = ?)",
			model.StatusWatching,
			model.StatusConfirming,
			model.StatusExpired,
			model.StatusCancelled,
			cooldownSince,
			normChain,
			rawChain,
			token,
			targetAddress,
			strings.ToLower(targetAddress),
		).Find(&activeIntents).Error

		if err != nil {
			return "", decimal.Zero, fmt.Errorf("failed to query active intents: %w", err)
		}

		// 2. Build map of currently occupied amounts
		occupied := make(map[int]bool)
		for _, it := range activeIntents {
			diff := it.ExpectedAmount.Sub(baseAmount)
			if diff.GreaterThanOrEqual(defaultMinTail) && diff.LessThanOrEqual(defaultMaxTail) {
				stepIdx := int(diff.Div(defaultTailStep).Round(0).IntPart())
				occupied[stepIdx] = true
			}
		}

		// 3. Find first unoccupied tail offset
		for i := minStep; i <= maxStep; i++ {
			if !occupied[i] {
				tail := defaultTailStep.Mul(decimal.NewFromInt(int64(i)))
				// Format to 6 decimal places to prevent binary float inaccuracies
				allocated := baseAmount.Add(tail).Round(6)

				// 真正更新轮换时间戳，使多收款地址负载均衡生效
				m.db.Model(&wallet).Update("last_used_at", time.Now())

				metrics.RecordMicroAllocation(true)
				metrics.SetMicroPoolUsed(string(chain), targetAddress, len(activeIntents)+1)
				return targetAddress, allocated, nil
			}
		}
	}

	metrics.RecordMicroAllocation(false)
	return "", decimal.Zero, fmt.Errorf("all micro-amount slots in range [%s, %s] are currently occupied across all %d active wallets for chain %s",
		defaultMinTail.StringFixed(4), defaultMaxTail.StringFixed(4), len(wallets), chain)
}

// CheckCollision checks if an expected amount is already in use by another active intent
func (m *MicroAmountManager) CheckCollision(
	chain model.Chain,
	token model.Token,
	targetAddress string,
	expectedAmount decimal.Decimal,
	excludeOrderID string,
) (bool, error) {
	normChain := model.NormalizeChain(string(chain))
	normAddr := strings.ToLower(strings.TrimSpace(targetAddress))

	cooldownSince := time.Now().Add(-15 * time.Minute)
	var activeIntents []model.PaymentIntent
	query := m.db.Model(&model.PaymentIntent{}).
		Where("((status IN (?, ?)) OR (status IN (?, ?) AND updated_at >= ?))",
			model.StatusWatching, model.StatusConfirming, model.StatusExpired, model.StatusCancelled, cooldownSince).
		Where("(chain = ? OR UPPER(chain) = ?)", normChain, normChain).
		Where("UPPER(token) = ?", token.ToUpper()).
		Where("(target_address = ? OR LOWER(target_address) = ?)", targetAddress, normAddr)

	if excludeOrderID != "" {
		query = query.Where("order_id != ?", excludeOrderID)
	}

	if err := query.Find(&activeIntents).Error; err != nil {
		logger.Info("Expected Amount %v query error: %v", expectedAmount, err)
		return false, err
	}

	for _, it := range activeIntents {
		if it.ExpectedAmount.Equal(expectedAmount) {
			return true, nil
		}
	}

	return false, nil
}
