package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"crypdog/internal/config"
	"crypdog/internal/model"
	"crypdog/internal/signature"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type AdminHandler struct {
	db  *gorm.DB
	cfg *config.Config
}

func NewAdminHandler(db *gorm.DB, cfg *config.Config) *AdminHandler {
	return &AdminHandler{
		db:  db,
		cfg: cfg,
	}
}

// ==================== 1. 收款地址池管理 API ====================

type CreateWalletReq struct {
	Chain   model.Chain `json:"chain" binding:"required"`
	Address string      `json:"address" binding:"required"`
	Label   string      `json:"label"`
	Weight  int         `json:"weight"`
}

type UpdateWalletReq struct {
	Label   *string `json:"label"`
	Weight  *int    `json:"weight"`
	Enabled *bool   `json:"enabled"`
}

// ListWallets 获取收款地址列表
// GET /api/v1/admin/wallets
func (h *AdminHandler) ListWallets(c *gin.Context) {
	chain := strings.TrimSpace(c.Query("chain"))
	enabledStr := strings.TrimSpace(c.Query("enabled"))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	query := h.db.Model(&model.WalletAddress{})
	if chain != "" {
		normChain := model.NormalizeChain(chain)
		query = query.Where("chain = ? OR chain = ?", normChain, chain)
	}
	if enabledStr != "" {
		enabled := enabledStr == "true" || enabledStr == "1"
		query = query.Where("enabled = ?", enabled)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询地址数量失败"})
		return
	}

	var wallets []model.WalletAddress
	offset := (page - 1) * pageSize
	if err := query.Order("id DESC").Offset(offset).Limit(pageSize).Find(&wallets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询地址列表失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"data": gin.H{
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
			"list":     wallets,
		},
	})
}

// CreateWallet 新增收款地址
// POST /api/v1/admin/wallets
func (h *AdminHandler) CreateWallet(c *gin.Context) {
	var req CreateWalletReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": fmt.Sprintf("参数错误: %v", err)})
		return
	}

	normChain := model.NormalizeChain(string(req.Chain))
	cleanAddr := strings.TrimSpace(req.Address)

	// 1. 严格校验链上地址格式（防错误地址导致客户资产永久丢失）
	if err := signature.ValidateChainAddress(string(normChain), cleanAddr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("非法收款钱包地址: %v", err),
		})
		return
	}

	cleanAddr = normChain.NormalizeAddress(cleanAddr)

	// 2. 查重
	var count int64
	h.db.Model(&model.WalletAddress{}).
		Where("chain = ? AND (address = ? OR LOWER(address) = ?)", normChain, cleanAddr, strings.ToLower(cleanAddr)).
		Count(&count)
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"code":    409,
			"message": fmt.Sprintf("公链 [%s] 已存在收款地址 [%s]", normChain, cleanAddr),
		})
		return
	}

	weight := req.Weight
	if weight <= 0 {
		weight = 1
	}

	wallet := model.WalletAddress{
		Chain:   normChain,
		Address: cleanAddr,
		Label:   strings.TrimSpace(req.Label),
		Weight:  weight,
		Enabled: true,
	}

	if err := h.db.Create(&wallet).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "保存钱包地址失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "添加收款地址成功",
		"data":    wallet,
	})
}

// UpdateWallet 更新收款地址
// PUT /api/v1/admin/wallets/:id
func (h *AdminHandler) UpdateWallet(c *gin.Context) {
	id := c.Param("id")
	var wallet model.WalletAddress
	if err := h.db.First(&wallet, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "收款地址不存在"})
		return
	}

	var req UpdateWalletReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if req.Label != nil {
		updates["label"] = strings.TrimSpace(*req.Label)
	}
	if req.Weight != nil && *req.Weight > 0 {
		updates["weight"] = *req.Weight
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	if len(updates) > 0 {
		if err := h.db.Model(&wallet).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "更新失败"})
			return
		}
	}

	_ = h.db.First(&wallet, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "更新成功",
		"data":    wallet,
	})
}

// DeleteWallet 删除收款地址（带进行中订单安全保护）
// DELETE /api/v1/admin/wallets/:id
func (h *AdminHandler) DeleteWallet(c *gin.Context) {
	id := c.Param("id")
	var wallet model.WalletAddress
	if err := h.db.First(&wallet, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "收款地址不存在"})
		return
	}

	// 安全防范：检查该地址是否存在进行中（WATCHING / CONFIRMING）的充值意向
	var activeCount int64
	h.db.Model(&model.PaymentIntent{}).
		Where("chain = ? AND (target_address = ? OR LOWER(target_address) = ?) AND status IN ?",
			wallet.Chain, wallet.Address, strings.ToLower(wallet.Address), []model.IntentStatus{model.StatusWatching, model.StatusConfirming}).
		Count(&activeCount)

	if activeCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("该地址当前有 %d 笔进行中的待支付订单，禁止物理删除！建议在后台停用 (enabled=false)", activeCount),
		})
		return
	}

	if err := h.db.Delete(&wallet).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "删除失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "收款地址已删除",
	})
}

// ==================== 2. 代币合约白名单管理 API ====================

type CreateTokenReq struct {
	Chain    model.Chain `json:"chain" binding:"required"`
	Symbol   model.Token `json:"symbol" binding:"required"`
	Name     string      `json:"name" binding:"required"`
	Contract string      `json:"contract"`
	Decimals int         `json:"decimals"`
	IsNative bool        `json:"isNative"`
	Icon     string      `json:"icon"`
	Badge    string      `json:"badge"`
	Priority int         `json:"priority"`
	Enabled  *bool       `json:"enabled"`
}

type UpdateTokenReq struct {
	Name     *string `json:"name"`
	Contract *string `json:"contract"`
	Decimals *int    `json:"decimals"`
	IsNative *bool   `json:"isNative"`
	Icon     *string `json:"icon"`
	Badge    *string `json:"badge"`
	Priority *int    `json:"priority"`
	Enabled  *bool   `json:"enabled"`
}

// ListTokens 获取代币白名单列表
// GET /api/v1/admin/tokens
func (h *AdminHandler) ListTokens(c *gin.Context) {
	chain := strings.TrimSpace(c.Query("chain"))
	symbol := strings.TrimSpace(c.Query("symbol"))
	enabledStr := strings.TrimSpace(c.Query("enabled"))

	query := h.db.Model(&model.ChainToken{})
	if chain != "" {
		normChain := model.NormalizeChain(chain)
		query = query.Where("chain = ? OR chain = ?", normChain, chain)
	}
	if symbol != "" {
		query = query.Where("symbol = ? OR LOWER(symbol) = ?", strings.ToUpper(symbol), strings.ToLower(symbol))
	}
	if enabledStr != "" {
		enabled := enabledStr == "true" || enabledStr == "1"
		query = query.Where("enabled = ?", enabled)
	}

	var tokens []model.ChainToken
	if err := query.Order("priority DESC, id ASC").Find(&tokens).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询代币列表失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"data": tokens,
	})
}

// CreateToken 新增代币合约
// POST /api/v1/admin/tokens
func (h *AdminHandler) CreateToken(c *gin.Context) {
	var req CreateTokenReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}

	normChain := model.NormalizeChain(string(req.Chain))
	sym := model.Token(strings.ToUpper(strings.TrimSpace(string(req.Symbol))))
	cleanContract := strings.TrimSpace(req.Contract)

	decimals := req.Decimals
	if decimals <= 0 {
		if sym == "ETH" || sym == "BNB" {
			decimals = 18
		} else if sym == "SOL" {
			decimals = 9
		} else {
			decimals = 6
		}
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// 查重：同链同币种
	var count int64
	h.db.Model(&model.ChainToken{}).Where("chain = ? AND symbol = ?", normChain, sym).Count(&count)
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"code":    409,
			"message": fmt.Sprintf("公链 [%s] 已存在代币 [%s]", normChain, sym),
		})
		return
	}

	token := model.ChainToken{
		Chain:    normChain,
		Symbol:   sym,
		Name:     strings.TrimSpace(req.Name),
		Contract: cleanContract,
		Decimals: decimals,
		IsNative: req.IsNative,
		Icon:     strings.TrimSpace(req.Icon),
		Badge:    strings.TrimSpace(req.Badge),
		Priority: req.Priority,
		Enabled:  enabled,
	}

	if err := h.db.Create(&token).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "保存代币配置失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "添加代币成功",
		"data":    token,
	})
}

// UpdateToken 更新代币合约配置
// PUT /api/v1/admin/tokens/:id
func (h *AdminHandler) UpdateToken(c *gin.Context) {
	id := c.Param("id")
	var token model.ChainToken
	if err := h.db.First(&token, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "代币不存在"})
		return
	}

	var req UpdateTokenReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = strings.TrimSpace(*req.Name)
	}
	if req.Contract != nil {
		updates["contract"] = strings.TrimSpace(*req.Contract)
	}
	if req.Decimals != nil && *req.Decimals > 0 {
		updates["decimals"] = *req.Decimals
	}
	if req.IsNative != nil {
		updates["is_native"] = *req.IsNative
	}
	if req.Icon != nil {
		updates["icon"] = strings.TrimSpace(*req.Icon)
	}
	if req.Badge != nil {
		updates["badge"] = strings.TrimSpace(*req.Badge)
	}
	if req.Priority != nil {
		updates["priority"] = *req.Priority
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	if len(updates) > 0 {
		if err := h.db.Model(&token).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "更新代币失败"})
			return
		}
	}

	_ = h.db.First(&token, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "更新成功",
		"data":    token,
	})
}

// DeleteToken 删除代币合约配置
// DELETE /api/v1/admin/tokens/:id
func (h *AdminHandler) DeleteToken(c *gin.Context) {
	id := c.Param("id")
	var token model.ChainToken
	if err := h.db.First(&token, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "代币不存在"})
		return
	}

	// 检查是否有未完成订单正在等待此代币
	var activeCount int64
	h.db.Model(&model.PaymentIntent{}).
		Where("chain = ? AND token = ? AND status IN ?", token.Chain, token.Symbol, []model.IntentStatus{model.StatusWatching, model.StatusConfirming}).
		Count(&activeCount)

	if activeCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": fmt.Sprintf("仍有 %d 笔进行中的订单等待此代币支付，禁止删除！请先在后台禁用 (enabled=false)", activeCount),
		})
		return
	}

	if err := h.db.Delete(&token).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "删除失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "代币已删除",
	})
}
