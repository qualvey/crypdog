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
	// 1. 查询数据库中启用的收款地址
	var wallets []model.WalletAddress
	if err := h.db.Where("enabled = ?", true).Find(&wallets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询收款地址失败"})
		return
	}

	// 统计拥有有效收款钱包的公链
	activeChainsWithWallet := make(map[string]bool)
	for _, w := range wallets {
		norm := model.NormalizeChain(string(w.Chain))
		activeChainsWithWallet[string(norm)] = true
	}

	// 2. 结合 config.yaml 中的节点启用状态过滤
	availableChains := make(map[string]bool)
	if h.cfg != nil && len(h.cfg.Chains) > 0 {
		for chain, nodeCfg := range h.cfg.Chains {
			if nodeCfg.Enabled == nil || *nodeCfg.Enabled {
				norm := string(model.NormalizeChain(string(chain)))
				if activeChainsWithWallet[norm] {
					availableChains[norm] = true
				}
			}
		}
	} else {
		availableChains = activeChainsWithWallet
	}

	// 3. 产出结构化响应
	options := model.CryptoPaymentOptions{
		Tokens: []model.CryptoTokenOption{},
		Chains: make(map[string][]model.CryptoChainOption),
	}

	if len(availableChains) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"code": 200,
			"data": options,
		})
		return
	}

	// 3. 从数据库查询启用的代币白名单 (支持按 priority 倒序排序)
	var dbTokens []model.ChainToken
	if err := h.db.Where("enabled = ?", true).Order("priority DESC, id ASC").Find(&dbTokens).Error; err != nil || len(dbTokens) == 0 {
		dbTokens = model.GetDefaultChainTokens()
	}

	tokenMeta := map[string]model.CryptoTokenOption{
		"USDT": {Symbol: "USDT", Name: "Tether USD", Icon: "fa-solid fa-circle-dollar-to-slot"},
		"USDC": {Symbol: "USDC", Name: "USD Coin", Icon: "fa-solid fa-circle-dollar-to-slot"},
		"BTC":  {Symbol: "BTC", Name: "Bitcoin", Icon: "fa-brands fa-bitcoin"},
		"ETH":  {Symbol: "ETH", Name: "Ethereum", Icon: "fa-brands fa-ethereum"},
		"BNB":  {Symbol: "BNB", Name: "BNB", Icon: "fa-solid fa-coins"},
		"SOL":  {Symbol: "SOL", Name: "Solana", Icon: "fa-solid fa-sun"},
	}

	chainMeta := map[model.Chain]struct {
		name  string
		badge string
	}{
		model.ChainTron:     {name: "TRC20 (Tron)", badge: "低手续费 / 推荐"},
		model.ChainArbitrum: {name: "Arbitrum One (L2)", badge: "极速 / 低Gas"},
		model.ChainBsc:      {name: "BNB Smart Chain", badge: "高吞吐"},
		model.ChainEth:      {name: "ERC20 (Ethereum)", badge: "主网原生"},
		model.ChainPolygon:  {name: "Polygon (Matic)", badge: "低费率"},
		model.ChainSolana:   {name: "Solana", badge: "极速"},
	}

	seenTokens := make(map[string]bool)
	for _, dt := range dbTokens {
		normChain := string(model.NormalizeChain(string(dt.Chain)))
		if !availableChains[normChain] {
			continue
		}

		sym := string(dt.Symbol)
		cName := string(dt.Chain)
		cBadge := dt.Badge
		if meta, exists := chainMeta[dt.Chain]; exists {
			if meta.name != "" {
				cName = meta.name
			}
			if cBadge == "" {
				cBadge = meta.badge
			}
		}

		cOpt := model.CryptoChainOption{
			Chain:    dt.Chain,
			Name:     cName,
			Badge:    cBadge,
			Decimals: dt.Decimals,
		}

		options.Chains[sym] = append(options.Chains[sym], cOpt)

		if !seenTokens[sym] {
			seenTokens[sym] = true
			tName := dt.Name
			tIcon := dt.Icon
			if tIcon == "" {
				if meta, ok := tokenMeta[sym]; ok {
					tIcon = meta.Icon
					if tName == "" {
						tName = meta.Name
					}
				} else {
					tIcon = "fa-solid fa-coins"
				}
			}
			if tName == "" {
				tName = sym
			}
			options.Tokens = append(options.Tokens, model.CryptoTokenOption{
				Symbol: sym,
				Name:   tName,
				Icon:   tIcon,
			})
		}
	}

	// 计算默认选中的 Token 与 Chain
	if len(options.Tokens) > 0 {
		defaultToken := options.Tokens[0].Symbol
		for _, t := range options.Tokens {
			if t.Symbol == "USDT" {
				defaultToken = "USDT"
				break
			}
		}
		options.DefaultToken = defaultToken

		if chains, ok := options.Chains[defaultToken]; ok && len(chains) > 0 {
			options.DefaultChain = string(chains[0].Chain)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"data": options,
	})
}
