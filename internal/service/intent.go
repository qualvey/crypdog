package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"crypdog/internal/engine"
	"crypdog/internal/logger"
	"crypdog/internal/metrics"
	"crypdog/internal/model"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var (
	ErrOrderAlreadyPaid = errors.New("order has already been paid")
	ErrAmountCollision  = errors.New("amount collision detected on target address")
	ErrParamMutation    = errors.New("active order cannot mutate parameters")
	ErrIntentNotFound   = errors.New("payment intent not found")
	ErrCannotCancelPaid = errors.New("cannot cancel payment intent because it is already paid")
)

type IntentService struct {
	db          *gorm.DB
	poolManager *engine.MicroAmountManager
}

func NewIntentService(db *gorm.DB, pool *engine.MicroAmountManager) *IntentService {
	return &IntentService{db: db, poolManager: pool}
}

type RegisterDTO struct {
	OrderID        string
	Chain          model.Chain
	Token          model.Token
	TargetAddress  string
	ExpectedAmount decimal.Decimal
	TimeoutSeconds int
	WebhookURL     string
}

// RegisterOrReactivate 封装完整的订单创建、重激活与幂等状态机
func (s *IntentService) RegisterOrReactivate(dto RegisterDTO) (*model.PaymentIntent, bool, error) {
	var existing model.PaymentIntent
	err := s.db.Where("order_id = ?", dto.OrderID).First(&existing).Error

	if err == nil {
		switch existing.Status {
		case model.StatusPaid:
			logger.Info("paid %s", dto.OrderID)
			return nil, false, ErrOrderAlreadyPaid

		case model.StatusWatching, model.StatusConfirming:
			if !isIdempotentMatch(&existing, &dto) {
				return nil, false, fmt.Errorf("%w: cannot change params for %s order", ErrParamMutation, existing.Status)
			}
			return &existing, true, nil // 第二个参数 true 表示走的是幂等返回

		case model.StatusCancelled, model.StatusExpired:
			if err := s.checkCollision(dto); err != nil {
				return nil, false, err
			}
			s.resetExistingIntent(&existing, dto)
			if err := s.db.Save(&existing).Error; err != nil {
				return nil, false, err
			}
			return &existing, false, nil
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	// 全新订单
	if err := s.checkCollision(dto); err != nil {
		return nil, false, err
	}

	intent := s.buildNewIntent(dto)
	//落库
	if err := s.db.Create(&intent).Error; err != nil {
		return nil, false, err
	}
	return &intent, false, nil
}

// 辅助方法全部私有化，核心流转一目了然
func (s *IntentService) checkCollision(dto RegisterDTO) error {
	if s.poolManager == nil {
		logger.Error("poolmanager is nil")
		return fmt.Errorf("poolmanager was nil")
	}
	targetAddress := dto.Chain.NormalizeAddress(dto.TargetAddress)
	collided, err := s.poolManager.CheckCollision(dto.Chain, dto.Token, targetAddress, dto.ExpectedAmount, dto.OrderID)
	if err != nil {
		logger.Error("CheckCollision Failed")
		return fmt.Errorf("check collision failed: %w", err)
	}
	if collided {
		return ErrAmountCollision
	}
	return nil
}

func isIdempotentMatch(existing *model.PaymentIntent, dto *RegisterDTO) bool {
	// 1. 金额使用 decimal 严格比对数值等价性
	if !existing.ExpectedAmount.Equal(dto.ExpectedAmount) {
		return false
	}

	// 2. 链与代币类型比对
	if existing.Chain != dto.Chain || existing.Token != dto.Token {
		return false
	}

	// 3. 地址比对（先去空格）
	addr1 := strings.TrimSpace(existing.TargetAddress)
	addr2 := strings.TrimSpace(dto.TargetAddress)

	// TRON 与 SOLANA (Base58) 严格区分大小写
	if strings.EqualFold(string(dto.Chain), "TRON") || strings.EqualFold(string(dto.Chain), "SOLANA") {
		return addr1 == addr2
	}

	// EVM 体系统一忽略大小写比对
	return strings.EqualFold(addr1, addr2)
}

func (s *IntentService) resetExistingIntent(intent *model.PaymentIntent, dto RegisterDTO) {
	now := time.Now()
	intent.Chain = dto.Chain
	intent.Token = dto.Token
	intent.TargetAddress = dto.TargetAddress
	intent.ExpectedAmount = dto.ExpectedAmount
	intent.ReceivedAmount = decimal.New(0, 0)
	intent.WebhookURL = dto.WebhookURL
	intent.Status = model.StatusWatching
	intent.TxHash = ""
	intent.BlockNumber = 0
	intent.Confirmations = 0
	intent.TimeoutSeconds = dto.TimeoutSeconds
	intent.ExpiresAt = now.Add(time.Duration(dto.TimeoutSeconds) * time.Second)
	intent.PaidAt = nil
	intent.UpdatedAt = now
}

func (s *IntentService) buildNewIntent(dto RegisterDTO) model.PaymentIntent {
	now := time.Now()

	return model.PaymentIntent{
		ID:             fmt.Sprintf("intent_%s_%s", strings.ToLower(dto.Chain.String()), uuid.New().String()[:8]),
		OrderID:        dto.OrderID,
		Chain:          dto.Chain,
		Token:          dto.Token,
		TargetAddress:  dto.Chain.NormalizeAddress(dto.TargetAddress),
		ExpectedAmount: dto.ExpectedAmount,
		TimeoutSeconds: dto.TimeoutSeconds,
		WebhookURL:     dto.WebhookURL,
		Status:         model.StatusWatching,
		ExpiresAt:      now.Add(time.Duration(dto.TimeoutSeconds) * time.Second),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// CancelIntent 取消指定意向（支持通过 orderId 或 intentId 取消），并释放微额占用
func (s *IntentService) CancelIntent(idOrOrderID string) (*model.PaymentIntent, error) {
	id := strings.TrimSpace(idOrOrderID)
	if id == "" {
		return nil, errors.New("missing orderId or intentId")
	}

	var intent model.PaymentIntent
	if err := s.db.Where("order_id = ? OR id = ?", id, id).First(&intent).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIntentNotFound
		}
		return nil, err
	}

	if intent.Status == model.StatusPaid {
		return nil, fmt.Errorf("%w: %s", ErrCannotCancelPaid, id)
	}

	if intent.Status == model.StatusCancelled || intent.Status == model.StatusExpired {
		return &intent, nil
	}

	intent.Status = model.StatusCancelled
	intent.UpdatedAt = time.Now()
	if err := s.db.Save(&intent).Error; err != nil {
		return nil, fmt.Errorf("failed to save cancelled intent: %w", err)
	}

	metrics.RecordIntentStatus(string(intent.Chain), string(intent.Token), string(model.StatusCancelled))
	logger.Info("[IntentService] 成功取消支付意向: OrderID=%s, IntentID=%s", intent.OrderID, intent.ID)
	return &intent, nil
}

