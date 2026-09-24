package api

import (
	"crypdog/internal/logger"
	"crypdog/internal/model"
	"crypdog/internal/service"
	"crypdog/internal/signature"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type IntentHandler struct {
	*Handler
	intentService        *service.IntentService
	paymentOptionService *service.PaymentOptionService
}

type AllocateIntentReq struct {
	OrderID        string          `json:"orderId" binding:"required"`
	Chain          model.Chain     `json:"chain" binding:"required"`
	Token          model.Token     `json:"token" binding:"required"`
	BaseAmount     decimal.Decimal `json:"baseAmount" binding:"required"`
	TimeoutSeconds int             `json:"timeoutSeconds" binding:"required"`
	WebhookURL     string          `json:"webhookUrl" binding:"required"`
}

func NewIntentHandler(h *Handler, i *service.IntentService, p *service.PaymentOptionService) *IntentHandler {
	return &IntentHandler{
		Handler:              h,
		intentService:        i,
		paymentOptionService: p,
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

	// 校验 Webhook URL 防御 SSRF（生产环境默认严禁私网/回环地址）
	allowLocal := h.cfg != nil && h.cfg.Webhook.AllowLocal
	requireHTTPS := h.cfg != nil && (strings.EqualFold(h.cfg.Env, "production") || strings.EqualFold(h.cfg.Env, "prod"))
	if err := signature.ValidateWebhookURL(req.WebhookURL, allowLocal, requireHTTPS); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("Invalid webhookUrl: %v", err),
		})
		return
	}

	// 校验目标收款钱包地址合法性（EVM, TRON, Solana 等）
	if err := signature.ValidateChainAddress(string(req.Chain), req.TargetAddress); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("Invalid targetAddress: %v", err),
		})
		return
	}

	normChain := model.NormalizeChain(string(req.Chain))

	// 校验收款地址是否属于本微服务已配置并启用的平台收款钱包池
	if h.db != nil {
		cleanTarget := normChain.NormalizeAddress(req.TargetAddress)
		var totalWallets int64
		_ = h.db.Model(&model.WalletAddress{}).Where("chain = ? OR chain = ?", normChain, req.Chain).Count(&totalWallets)
		if totalWallets > 0 {
			var walletCount int64
			err := h.db.Model(&model.WalletAddress{}).
				Where("(chain = ? OR chain = ?) AND enabled = ? AND (address = ? OR LOWER(address) = ?)",
					normChain, req.Chain, true, cleanTarget, strings.ToLower(cleanTarget)).
				Count(&walletCount).Error
			if err == nil && walletCount == 0 {
				c.JSON(http.StatusBadRequest, gin.H{
					"code":    400,
					"message": fmt.Sprintf("Target address '%s' is not in the configured platform wallet pool for chain %s", req.TargetAddress, req.Chain),
				})
				return
			}
		}
	}

	// 基础参数兜底
	dto := service.RegisterDTO{
		OrderID:        strings.TrimSpace(req.OrderID),
		Chain:          normChain,
		Token:          req.Token,
		TargetAddress:  strings.TrimSpace(req.TargetAddress),
		ExpectedAmount: req.ExpectedAmount,
		TimeoutSeconds: normalizeTimeout(req.TimeoutSeconds),
		WebhookURL:     strings.TrimSpace(req.WebhookURL),
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

	// 校验 Webhook URL 防御 SSRF（生产环境默认严禁私网/回环地址）
	allowLocal := h.cfg != nil && h.cfg.Webhook.AllowLocal
	requireHTTPS := h.cfg != nil && (strings.EqualFold(h.cfg.Env, "production") || strings.EqualFold(h.cfg.Env, "prod"))
	if err := signature.ValidateWebhookURL(req.WebhookURL, allowLocal, requireHTTPS); err != nil {
		logger.WarnContext(c.Request.Context(), "webhook target rejected",
			"error_code", "WEBHOOK_TARGET_INVALID",
			"validation_reason", err.Error(),
			"allow_local", allowLocal,
		)
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("Invalid webhookUrl: %v", err),
		})
		return
	}

	dto := service.AllocateDTO{
		OrderID:        strings.TrimSpace(req.OrderID),
		Chain:          req.Chain,
		Token:          req.Token,
		BaseAmount:     req.BaseAmount,
		TimeoutSeconds: normalizeTimeout(req.TimeoutSeconds),
		WebhookURL:     strings.TrimSpace(req.WebhookURL),
	}

	intent, tailOffset, isIdempotent, err := h.intentService.AllocateOrReactivate(dto)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrOrderAlreadyPaid):
			c.JSON(http.StatusConflict, gin.H{
				"code":    409,
				"message": fmt.Sprintf("Order ID '%s' has already been PAID.", req.OrderID),
			})
		default:
			c.JSON(http.StatusConflict, gin.H{
				"code":    409,
				"message": fmt.Sprintf("Failed to allocate micro-amount: %v", err),
			})
		}
		return
	}

	msg := "Payment intent allocated and watching successfully"
	if isIdempotent {
		msg = "Payment intent already active (Idempotent response)"
	}

	responseData := gin.H{
		"intentId":       intent.ID,
		"orderId":        intent.OrderID,
		"baseAmount":     req.BaseAmount,
		"tailOffset":     tailOffset,
		"expectedAmount": intent.ExpectedAmount,
		"targetAddress":  intent.TargetAddress,
		"status":         intent.Status,
		"expiresAt":      intent.ExpiresAt,
	}

	logger.InfoContext(c.Request.Context(), "接受到订单意向",
		"order_id", intent.OrderID,
		"intent_id", intent.ID,
		"chain", req.Chain,
		"token", req.Token,
		"is_idempotent", isIdempotent,
	)

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": msg,
		"data":    responseData,
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
	TargetAddress string          `json:"targetAddress" binding:"required"`
	Amount        decimal.Decimal `json:"amount" binding:"required"`
	Chain         string          `json:"chain"`
	Token         string          `json:"token"`
}

// SimulateOnChainTransfer handles POST /api/v1/watcher/intents/simulate
func (h *IntentHandler) SimulateOnChainTransfer(c *gin.Context) {
	if h.cfg == nil || !h.cfg.Server.EnableSimulation {
		c.JSON(http.StatusForbidden, gin.H{
			"code":    403,
			"message": "Simulation endpoint is disabled in this environment (set server.enable_simulation: true to enable)",
		})
		return
	}

	var req SimulateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}

	chain := model.NormalizeChain(req.Chain)
	token := model.Token(strings.ToUpper(strings.TrimSpace(req.Token)))
	if token == "" {
		token = model.TokenUSDT
	}

	txHash := fmt.Sprintf("mock_tx_%x%d", rand.Uint64(), time.Now().UnixNano())
	latestBlock := h.scannerMgr.GetLatestBlock(chain)
	if latestBlock == 0 {
		latestBlock = 10000000
	}
	// 模拟流水不经过真实节点，因此不能等待链上新区块推进确认数。
	// 让模拟交易落在“已满足确认数”的历史区块上，确保本地测试可以完整走通
	// WATCHING -> CONFIRMING/PAID 以及 Webhook 派发链路。
	requiredConfirmations := h.cfg.GetRequiredConfirmations(chain)
	if requiredConfirmations > 0 && latestBlock >= requiredConfirmations {
		latestBlock -= requiredConfirmations - 1
	}

	transfer := model.ChainTransfer{
		TxHash:         txHash,
		Chain:          chain,
		FromAddress:    "0xSimulatedPayerWalletAddress1234567890",
		TargetAddress:  chain.NormalizeAddress(req.TargetAddress),
		Amount:         req.Amount,
		Token:          token,
		BlockNumber:    latestBlock,
		BlockTimestamp: time.Now().Unix(),
		Decimals:       6,
	}

	if h.transferChan == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "Transfer channel not initialized"})
		return
	}

	select {
	case h.transferChan <- transfer:
		c.JSON(http.StatusOK, gin.H{
			"code":    200,
			"message": "Simulated transfer broadcasted to scanner pipeline",
			"data": gin.H{
				"txHash": txHash,
				"chain":  chain,
				"amount": req.Amount,
				"token":  token,
			},
		})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "Pipeline transfer queue full"})
	}
}

type CancelIntentReq struct {
	OrderID  string `json:"orderId"`
	IntentID string `json:"intentId"`
}

// CancelIntent handles:
// - POST /api/v1/watcher/intents/cancel (JSON body: orderId or intentId)
// - POST /api/v1/watcher/intents/:orderId/cancel (path param: orderId or intentId)
// - DELETE /api/v1/watcher/intents/:orderId (path param: orderId or intentId)
func (h *IntentHandler) CancelIntent(c *gin.Context) {
	var id string
	if c.Request.Body != nil && c.Request.ContentLength > 0 {
		var req CancelIntentReq
		if err := c.ShouldBindJSON(&req); err == nil {
			if req.OrderID != "" {
				id = strings.TrimSpace(req.OrderID)
			} else if req.IntentID != "" {
				id = strings.TrimSpace(req.IntentID)
			}
		}
	}
	if id == "" {
		id = strings.TrimSpace(c.Param("orderId"))
	}
	if id == "" {
		id = strings.TrimSpace(c.Query("orderId"))
	}
	if id == "" {
		id = strings.TrimSpace(c.Query("intentId"))
	}
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Missing orderId or intentId"})
		return
	}

	intent, err := h.intentService.CancelIntent(id)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrIntentNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": err.Error()})
		case errors.Is(err, service.ErrCannotCancelPaid), errors.Is(err, service.ErrCannotCancelConfirming):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": fmt.Sprintf("Payment intent for order '%s' cancelled successfully", intent.OrderID),
		"data": gin.H{
			"intentId": intent.ID,
			"orderId":  intent.OrderID,
			"status":   intent.Status,
		},
	})
}

// GetPaymentOptions 根据当前启用的公链配置与可用收款地址池，动态产出可供前端展示的 Tokens 与 Chains
func (h *IntentHandler) GetPaymentOptions(c *gin.Context) {
	options, err := h.paymentOptionService.GetOptions(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询支付选项失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"data": options,
	})
}
