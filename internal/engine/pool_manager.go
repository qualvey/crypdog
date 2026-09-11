package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"crypdog/internal/logger"
	"crypdog/internal/metrics"
	"crypdog/internal/model"

	"github.com/shopspring/decimal"
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

// 定义常量/配置（使用 Decimal 类型定义）
var (
	defaultMinTail  = decimal.NewFromFloat(0.0001) // 最小尾数
	defaultMaxTail  = decimal.NewFromFloat(0.0999) // 最大尾数
	defaultTailStep = decimal.NewFromFloat(0.0001) // 步长
)

// AllocateUniqueAmount finds and reserves the smallest unassigned micro-amount for a given (chain, token, targetAddress, baseAmount) scope
func (m *MicroAmountManager) AllocateUniqueAmount(chain model.Chain, token model.Token, baseAmount decimal.Decimal) (string, decimal.Decimal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	normChain := model.NormalizeChain(string(chain))
	rawChain := strings.ToUpper(strings.TrimSpace(string(chain)))

	var wallet model.WalletAddress
	err := m.db.Where("(chain = ? OR chain = ?) AND enabled = ?", normChain, rawChain, true).
		Order("last_used_at ASC"). // 轮换策略：优先使用最久未使用的地址
		First(&wallet).Error
	if err != nil {
		return "", decimal.New(0, 10), fmt.Errorf("无可用收款地址: %w", err)
	}
	targetAddress := wallet.Address
	// 1. Fetch all currently active (WATCHING or CONFIRMING) intents for this target address
	var activeIntents []model.PaymentIntent
	err = m.db.Where("status IN (?, ?) AND (UPPER(chain) = ? OR UPPER(chain) = ?) AND UPPER(token) = ?",
		model.StatusWatching,
		model.StatusConfirming,
		normChain,
		rawChain,
		token,
	).Find(&activeIntents).Error

	if err != nil {
		return "", decimal.New(0, 10), fmt.Errorf("failed to query active intents: %w", err)
	}

	// 2. Build map of currently occupied amounts
	occupied := make(map[int]bool)
	for _, it := range activeIntents {
		diff := it.ExpectedAmount.Sub(baseAmount)
		if diff.GreaterThanOrEqual(defaultMinTail) && diff.LessThanOrEqual(defaultMaxTail) {
			// convert offset to index (e.g. 0.0001 -> 1, 0.0002 -> 2)
			stepIdx := int(diff.Div(defaultTailStep).Round(0).IntPart())
			occupied[stepIdx] = true
		}

	}

	// 3. Find first unoccupied tail offset
	minStep := int(defaultMinTail.Div(defaultTailStep).Round(0).IntPart())
	maxStep := int(defaultMaxTail.Div(defaultTailStep).Round(0).IntPart())
	for i := minStep; i <= maxStep; i++ {
		if !occupied[i] {
			tail := defaultTailStep.Mul(decimal.NewFromInt(int64(i)))
			// Format to 6 decimal places to prevent binary float inaccuracies
			allocated := baseAmount.Add(tail).Round(6)
			metrics.RecordMicroAllocation(true)
			metrics.SetMicroPoolUsed(string(chain), targetAddress, len(activeIntents)+1)
			return targetAddress, allocated, nil
		}
	}
	// 真正更新轮换时间戳
	m.db.Model(&wallet).Update("last_used_at", time.Now())

	metrics.RecordMicroAllocation(false)
	return "", decimal.New(0, 10), fmt.Errorf("all micro-amount slots in range [%s, %s] are currently occupied for address %s",
		defaultMinTail.StringFixed(4), defaultMaxTail.StringFixed(4), targetAddress)
}

// CheckCollision checks if an expected amount is already in use by another active intent
func (m *MicroAmountManager) CheckCollision(
	chain model.Chain,
	token model.Token,
	targetAddress string,
	expectedAmount decimal.Decimal,
	excludeOrderID string,
) (bool, error) {
	var count int64
	query := m.db.Model(&model.PaymentIntent{}).
		Where("status IN (?,?)", model.StatusWatching, model.StatusConfirming).
		Where("chain = ?", chain).
		Where("token = ?", token).
		Where("target_address = ?", targetAddress).
		//Sqlite不支持decimal,用string才能精确匹配
		Where("expected_amount = ?", expectedAmount.String())

	// if excludeOrderID != "" {
	// 	query = query.Where("order_id != ?", excludeOrderID)
	// }
	// 只要找到 1 条记录即代表碰撞
	if err := query.Limit(1).Count(&count).Error; err != nil {
		logger.Info("Expected Amount %v", expectedAmount)
		return false, err
	}

	return count > 0, nil
}
