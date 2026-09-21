package api

import (
	"net/http"
	"strconv"
	"strings"

	"crypdog/internal/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TransferHandler struct {
	db *gorm.DB
}

func NewTransferHandler(db *gorm.DB) *TransferHandler {
	return &TransferHandler{db: db}
}

// GetTransfers handles GET /api/v1/watcher/transfers?address=...&token=...&chain=...&limit=50
func (h *TransferHandler) GetTransfers(c *gin.Context) {
	address := strings.TrimSpace(c.Query("address"))
	token := strings.TrimSpace(c.Query("token"))
	chain := strings.TrimSpace(c.Query("chain"))
	limitStr := c.DefaultQuery("limit", "50")

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	query := h.db.Model(&model.ChainTransfer{})

	if address != "" {
		// 统一使用 LOWER 过滤，兼容 EVM 的 Hex 地址大小写
		query = query.Where("LOWER(target_address) = ?", strings.ToLower(address))
	}
	if token != "" {
		query = query.Where("UPPER(token) = ?", strings.ToUpper(token))
	}
	if chain != "" {
		query = query.Where("UPPER(chain) = ?", strings.ToUpper(chain))
	}

	var transfers []model.ChainTransfer
	if err := query.Order("id desc").Limit(limit).Find(&transfers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "Failed to fetch transfer logs",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    200,
		"message": "success",
		"data": gin.H{
			"count":     len(transfers),
			"transfers": transfers,
		},
	})
}
