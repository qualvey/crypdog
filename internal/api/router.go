package api

import (
	"io"
	"log"
	"net/http"
	"strings"

	"crypdog/internal/config"
	"crypdog/internal/engine"
	"crypdog/internal/model"
	"crypdog/internal/scanner"
	"crypdog/internal/service"
	"crypdog/internal/signature"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// HeightProvider 获取块高的函数签名
type HeightProvider func(chain string) int64
type TransferSimulator interface {
	SimulateTransfer(toAddress string, amount float64, token string, out chan<- model.ChainTransfer) string
}

// Handler 持有 API 业务所需的依赖
type Handler struct {
	db           *gorm.DB
	cfg          *config.Config
	poolManager  *engine.MicroAmountManager
	scannerMgr   *scanner.Manager           // 代替之前散落的具体扫描器
	transferChan chan<- model.ChainTransfer // 仅当 API 层有 /simulate 这种本地测试接口时需要
}

func NewHandler(
	db *gorm.DB,
	cfg *config.Config,
	poolManager *engine.MicroAmountManager,
	scannerMgr *scanner.Manager,
	transferChan chan<- model.ChainTransfer,
) *Handler {
	return &Handler{
		db:           db,
		cfg:          cfg,
		poolManager:  poolManager,
		scannerMgr:   scannerMgr,
		transferChan: transferChan,
	}
}
func SetupRouter(h *Handler) *gin.Engine {
	r := gin.Default()

	// CORS Middleware
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})
	intentService := service.NewIntentService(h.db, h.poolManager)
	intentHandler := NewIntentHandler(h, intentService)
	transferHandler := NewTransferHandler(h.db)

	// Health Check Endpoint
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "UP",
			"service": "CrypDog Crypto Payment Guardian",
		})
	})

	// Service Authorization Middleware
	authMiddleware := func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		expectedToken := "Bearer " + h.cfg.Server.Secret
		if authHeader == "" || authHeader != expectedToken {
			c.JSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "Unauthorized access: invalid or missing Service Secret Key",
			})
			c.Abort()
			return
		}
		c.Next()
	}

	// API v1 Routes
	v1 := r.Group("/api/v1/watcher")
	{
		// Protected endpoints
		protected := v1.Group("")
		protected.Use(authMiddleware)
		{
			protected.POST("/intents", intentHandler.RegisterIntent)
			protected.POST("/intents/allocate", intentHandler.AllocateIntent)
			protected.GET("/intents/:orderId", intentHandler.GetIntentStatus)
			protected.POST("/intents/:orderId/cancel", intentHandler.CancelIntent)
			protected.DELETE("/intents/:orderId", intentHandler.CancelIntent)
			protected.GET("/transfers", transferHandler.GetTransfers)
			protected.POST("/intents/simulate", intentHandler.SimulateOnChainTransfer)
		}
	}

	// Mock Webhook Receiver Endpoint for testing/verifying callback signatures
	r.POST("/api/v1/mock/webhook", func(c *gin.Context) {
		sig := c.GetHeader("X-Signature-SHA256")
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot read body"})
			return
		}

		// Verify HMAC signature
		isValid := signature.VerifyHMACSHA256(bodyBytes, h.cfg.Webhook.Secret, sig)
		log.Printf("==================================================")
		log.Printf("[Mock Webhook Target] Received Webhook Payload!")
		log.Printf("Header X-Signature-SHA256: %s", sig)
		log.Printf("Payload Body: %s", string(bodyBytes))
		log.Printf("HMAC Verification Result: %v (Valid: %t)", sig, isValid)
		log.Printf("==================================================")

		if !isValid {
			c.JSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "Invalid HMAC-SHA256 signature",
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"code":    200,
			"message": "Webhook received and signature verified successfully",
		})
	})

	return r
}

func getBearerToken(header string) string {
	parts := strings.Split(header, " ")
	if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
		return parts[1]
	}
	return ""
}
