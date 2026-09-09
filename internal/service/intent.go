package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"crypdog/internal/engine"
	"crypdog/internal/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrOrderAlreadyPaid = errors.New("order has already been paid")
	ErrAmountCollision  = errors.New("amount collision detected on target address")
	ErrParamMutation    = errors.New("active order cannot mutate parameters")
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
	ExpectedAmount float64
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
	if err := s.db.Create(&intent).Error; err != nil {
		return nil, false, err
	}
	return &intent, false, nil
}

// 辅助方法全部私有化，核心流转一目了然
func (s *IntentService) checkCollision(dto RegisterDTO) error {
	if s.poolManager != nil {
		if collided, _ := s.poolManager.CheckCollision(dto.Chain, dto.Token, dto.TargetAddress, dto.ExpectedAmount, dto.OrderID); collided {
			return ErrAmountCollision
		}
	}
	return nil
}

func isIdempotentMatch(existing *model.PaymentIntent, dto *RegisterDTO) bool {
	const epsilon = 0.000001
	if math.Abs(existing.ExpectedAmount-dto.ExpectedAmount) > epsilon ||
		existing.Chain != dto.Chain ||
		existing.Token != dto.Token {
		return false
	}
	if dto.Chain == "TRON" || dto.Chain == "SOLANA" {
		return existing.TargetAddress == dto.TargetAddress
	}
	return strings.EqualFold(existing.TargetAddress, dto.TargetAddress)
}

func (s *IntentService) resetExistingIntent(intent *model.PaymentIntent, dto RegisterDTO) {
	now := time.Now()
	intent.Chain = dto.Chain
	intent.Token = dto.Token
	intent.TargetAddress = dto.TargetAddress
	intent.ExpectedAmount = dto.ExpectedAmount
	intent.ReceivedAmount = 0
	intent.TimeoutSeconds = dto.TimeoutSeconds
	intent.WebhookURL = dto.WebhookURL
	intent.Status = model.StatusWatching
	intent.TxHash = ""
	intent.BlockNumber = 0
	intent.Confirmations = 0
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
		TargetAddress:  dto.TargetAddress,
		ExpectedAmount: dto.ExpectedAmount,
		TimeoutSeconds: dto.TimeoutSeconds,
		WebhookURL:     dto.WebhookURL,
		Status:         model.StatusWatching,
		ExpiresAt:      now.Add(time.Duration(dto.TimeoutSeconds) * time.Second),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}
