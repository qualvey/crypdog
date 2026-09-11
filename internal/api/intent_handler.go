package api

import (
	"crypdog/internal/model"
	"crypdog/internal/service"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type IntentHandler struct {
	*Handler
	intentService *service.IntentService
}

type AllocateIntentReq struct {
	OrderID        string          `json:"orderId" binding:"required"`
	Chain          model.Chain     `json:"chain" binding:"required"`
	Token          model.Token     `json:"token" binding:"required"`
	BaseAmount     decimal.Decimal `json:"baseAmount" binding:"required"`
	TimeoutSeconds int             `json:"timeoutSeconds" binding:"required"`
	WebhookURL     string          `json:"webhookUrl" binding:"required"`
}

func NewIntentHandler(h *Handler, i *service.IntentService) *IntentHandler {
	return &IntentHandler{
		h,
		i,
	}
}

func normalizeTimeout(sec int) int {
	if sec < 180 {
		return 1800 // 默认 30 分钟
	}
	if sec > 86400 {
		return 86400
	}
	return sec
}

type RegisterIntentReq struct {
	OrderID        string          `json:"orderId" binding:"required"`
	Chain          model.Chain     `json:"chain" binding:"required"`
	Token          model.Token     `json:"token" binding:"required"`
	TargetAddress  string          `json:"targetAddress" binding:"required"`
	ExpectedAmount decimal.Decimal `json:"expectedAmount" binding:"required"`
	TimeoutSeconds int             `json:"timeoutSeconds" binding:"required"`
	WebhookURL     string          `json:"webhookUrl" binding:"required"`
}

// RegisterIntent handles POST /api/v1/watcher/intents
func (h *IntentHandler) RegisterIntent(c *gin.Context) {
	var req RegisterIntentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("Invalid parameters: %v", err),
		})
		return
	}
	// 基础参数兜底
	dto := service.RegisterDTO{
		OrderID:        strings.TrimSpace(req.OrderID),
		Chain:          model.NormalizeChain(string(req.Chain)),
		Token:          req.Token,
		TargetAddress:  strings.TrimSpace(req.TargetAddress),
		ExpectedAmount: req.ExpectedAmount,
		TimeoutSeconds: normalizeTimeout(req.TimeoutSeconds),
		WebhookURL:     req.WebhookURL,
	}

	intent, isIdempotent, err := h.intentService.RegisterOrReactivate(dto)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrOrderAlreadyPaid), errors.Is(err, service.ErrAmountCollision):
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": err.Error()})
		case errors.Is(err, service.ErrParamMutation):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		}
		return
	}

	msg := "Payment intent registered successfully"
	if isIdempotent {
		msg = "Payment intent already active (Idempotent response)"
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": msg,
		"data": gin.H{
			"intentId":  intent.ID,
			"status":    intent.Status,
			"expiresAt": intent.ExpiresAt,
		},
	})
}

// AllocateIntent handles POST /api/v1/watcher/intents/allocate
// Automatically calculates and assigns an unoccupied micro-amount tail for the order
func (h *IntentHandler) AllocateIntent(c *gin.Context) {
	var req AllocateIntentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("Invalid parameters: %v", err),
		})
		return
	}

	if h.poolManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "MicroAmountManager pool is not configured",
		})
		return
	}

	// 1. Check if orderId already exists
	var existing model.PaymentIntent
	if err := h.db.Where("order_id = ?", req.OrderID).First(&existing).Error; err == nil {
		if existing.Status == model.StatusPaid {
			c.JSON(http.StatusConflict, gin.H{
				"code":    409,
				"message": fmt.Sprintf("Order ID '%s' has already been PAID.", req.OrderID),
			})
			return
		}

		if existing.Status == model.StatusWatching || existing.Status == model.StatusConfirming {
			tail := existing.ExpectedAmount.Sub(req.BaseAmount)
			c.JSON(http.StatusOK, gin.H{
				"code":    200,
				"message": "Payment intent already active (Idempotent response)",
				"data": gin.H{
					"intentId":       existing.ID,
					"orderId":        existing.OrderID,
					"baseAmount":     req.BaseAmount,
					"tailOffset":     tail,
					"expectedAmount": existing.ExpectedAmount,
					"status":         existing.Status,
					"expiresAt":      existing.ExpiresAt,
				},
			})
			return
		}
	}

	normChain := model.NormalizeChain(string(req.Chain))

	// 2. Allocate unique micro-amount
	targetAddress, allocatedAmount, err := h.poolManager.AllocateUniqueAmount(normChain, model.Token(req.Token), req.BaseAmount)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"code":    409,
			"message": fmt.Sprintf("Failed to allocate micro-amount: %v", err),
		})
		return
	}

	tailOffset := allocatedAmount.Sub(req.BaseAmount)

	intentID := fmt.Sprintf("intent_%s_%s", normChain, uuid.New().String()[:8])
	now := time.Now()
	timeoutSec := normalizeTimeout(req.TimeoutSeconds)
	expiresAt := now.Add(time.Duration(timeoutSec) * time.Second)

	intent := model.PaymentIntent{
		ID:             intentID,
		OrderID:        req.OrderID,
		Chain:          normChain,
		Token:          req.Token,
		TargetAddress:  normChain.NormalizeAddress(targetAddress),
		ExpectedAmount: allocatedAmount,
		TimeoutSeconds: timeoutSec,
		WebhookURL:     req.WebhookURL,
		Status:         model.StatusWatching,
		ExpiresAt:      expiresAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := h.db.Create(&intent).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": fmt.Sprintf("Failed to create allocated payment intent: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "Payment intent allocated and watching successfully",
		"data": gin.H{
			"intentId":       intent.ID,
			"orderId":        intent.OrderID,
			"baseAmount":     req.BaseAmount,
			"tailOffset":     tailOffset,
			"expectedAmount": allocatedAmount,
			"targetAddress":  intent.TargetAddress,
			"status":         intent.Status,
			"expiresAt":      intent.ExpiresAt,
		},
	})
}

// GetIntentStatus handles GET /api/v1/watcher/intents/:orderId
func (h *IntentHandler) GetIntentStatus(c *gin.Context) {
	orderID := c.Param("orderId")
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Missing orderId"})
		return
	}

	var intent model.PaymentIntent
	if err := h.db.Where("order_id = ?", orderID).First(&intent).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Payment intent not found",
		})
		return
	}

	data := gin.H{
		"orderId":        intent.OrderID,
		"chain":          intent.Chain,
		"token":          intent.Token,
		"targetAddress":  intent.TargetAddress,
		"status":         intent.Status,
		"expectedAmount": intent.ExpectedAmount,
		"receivedAmount": intent.ReceivedAmount,
		"expiresAt":      intent.ExpiresAt,
	}

	if intent.TxHash != "" {
		data["txHash"] = intent.TxHash
		data["blockNumber"] = intent.BlockNumber
		data["confirmations"] = intent.Confirmations
	}
	if intent.PaidAt != nil {
		data["paidAt"] = intent.PaidAt
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "success",
		"data":    data,
	})
}

type SimulateReq struct {
	TargetAddress string  `json:"targetAddress" binding:"required"`
	Amount        float64 `json:"amount" binding:"required"`
	Chain         string  `json:"chain"`
	Token         string  `json:"token"`
}

// SimulateOnChainTransfer handles POST /api/v1/watcher/intents/simulate
func (h *IntentHandler) SimulateOnChainTransfer(c *gin.Context) {
	var req SimulateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}

	chain := strings.ToUpper(req.Chain)
	if chain == "" {
		chain = "TRON"
	}

	var txHash string
	txHash, err := h.scannerMgr.SimulateTransfer(chain, req.TargetAddress, req.Amount, req.Token, h.transferChan)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "Simulated transfer broadcasted to scanner pipeline",
		"data": gin.H{
			"txHash": txHash,
			"chain":  chain,
			"amount": req.Amount,
		},
	})
}

// CancelIntent handles POST /api/v1/watcher/intents/:orderId/cancel or DELETE /api/v1/watcher/intents/:orderId
func (h *IntentHandler) CancelIntent(c *gin.Context) {
	orderID := c.Param("orderId")
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Missing orderId"})
		return
	}

	var intent model.PaymentIntent
	if err := h.db.Where("order_id = ?", orderID).First(&intent).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Payment intent not found",
		})
		return
	}

	if intent.Status == model.StatusPaid {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("Cannot cancel payment intent '%s' because it is already PAID", orderID),
		})
		return
	}

	if intent.Status == model.StatusCancelled || intent.Status == model.StatusExpired {
		c.JSON(http.StatusOK, gin.H{
			"code":    200,
			"message": fmt.Sprintf("Payment intent '%s' is already %s", orderID, intent.Status),
			"data": gin.H{
				"orderId": intent.OrderID,
				"status":  intent.Status,
			},
		})
		return
	}

	// Update status to CANCELLED
	intent.Status = model.StatusCancelled
	h.db.Save(&intent)

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": fmt.Sprintf("Payment intent '%s' cancelled successfully", orderID),
		"data": gin.H{
			"orderId": intent.OrderID,
			"status":  intent.Status,
		},
	})
}
