package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// LiveHandler 存活探针（Liveness Probe）
func LiveHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "UP",
		"service":   "CrypDog Crypto Payment Guardian",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// ReadyHandler 就绪探针（Readiness Probe）与深度健康检查
func ReadyHandler(h *Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		overallStatus := "UP"
		httpCode := http.StatusOK

		components := gin.H{}

		// 1. 检查数据库连通性
		dbComponent := gin.H{"status": "UP"}
		if h.db != nil {
			sqlDB, err := h.db.DB()
			if err != nil || sqlDB.Ping() != nil {
				dbComponent["status"] = "DOWN"
				if err != nil {
					dbComponent["error"] = err.Error()
				} else {
					dbComponent["error"] = "db ping timeout"
				}
				overallStatus = "DOWN"
				httpCode = http.StatusServiceUnavailable
			}
		} else {
			dbComponent["status"] = "DOWN"
			dbComponent["error"] = "db connection not initialized"
			overallStatus = "DOWN"
			httpCode = http.StatusServiceUnavailable
		}
		components["database"] = dbComponent

		// 2. 检查公链扫描器活跃度
		scannersComponent := gin.H{}
		if h.scannerMgr != nil {
			scannerStatuses := h.scannerMgr.GetScannersStatus()
			for chain, latestBlock := range scannerStatuses {
				scannersComponent[chain] = gin.H{
					"status":       "ACTIVE",
					"latest_block": latestBlock,
				}
			}
		}
		components["scanners"] = scannersComponent

		c.JSON(httpCode, gin.H{
			"status":     overallStatus,
			"service":    "CrypDog Crypto Payment Guardian",
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"components": components,
		})
	}
}
