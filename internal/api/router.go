package api

import (
	"crypdog/internal/logger"
	"crypto/subtle"
	"io"

	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/engine"
	"crypdog/internal/model"
	"crypdog/internal/scanner"
	"crypdog/internal/service"
	"crypdog/internal/signature"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	r := gin.New()

	// 统一注册可观测性与防御中间件
	r.Use(StructuredRecoveryMiddleware())
	r.Use(RequestIDMiddleware())
	r.Use(AccessLogMiddleware())
	r.Use(RateLimitMiddleware(h.cfg.Server.RateLimitPerMin))

	// Limit request bodies before JSON binding to prevent memory exhaustion.
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})

	// CORS Middleware. Empty allow-list means no browser CORS access.
	r.Use(func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		allowed := false
		for _, configured := range h.cfg.Server.AllowedOrigins {
			if strings.TrimSpace(configured) == origin {
				allowed = true
				break
			}
		}
		if allowed {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Vary", "Origin")
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Request-ID")
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

	// Health Check Endpoints (存活探针与深度就绪探针)
	r.GET("/health", ReadyHandler(h))
	r.GET("/healthz/live", LiveHandler)
	r.GET("/healthz/ready", ReadyHandler(h))

	// Prometheus Metrics Endpoint
	if h.cfg.Metrics.Enabled {
		metricsPath := h.cfg.Metrics.Path
		if metricsPath == "" {
			metricsPath = "/metrics"
		}
		metricsHandler := gin.WrapH(promhttp.Handler())
		if h.cfg.Metrics.Secret != "" {
			r.GET(metricsPath, func(c *gin.Context) {
				expected := "Bearer " + h.cfg.Metrics.Secret
				provided := c.GetHeader("Authorization")
				if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				}
				metricsHandler(c)
			})
		} else {
			r.GET(metricsPath, metricsHandler)
		}
	}

	// Service Authorization Middleware (使用常量时间比对防时序侧信道反推)
	authMiddleware := func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		expectedToken := "Bearer " + h.cfg.Server.Secret
		if len(authHeader) != len(expectedToken) || subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedToken)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "Unauthorized access: invalid or missing Service Secret Key",
			})
			c.Abort()
			return
		}
		c.Next()
	}
	adminAuthMiddleware := func(c *gin.Context) {
		secret := h.cfg.Server.AdminSecret
		if secret == "" {
			secret = h.cfg.Server.Secret // development/test compatibility
		}
		expectedToken := "Bearer " + secret
		provided := c.GetHeader("Authorization")
		if len(provided) != len(expectedToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "Unauthorized admin access"})
			c.Abort()
			return
		}
		c.Next()
	}
	adminAuditMiddleware := func(c *gin.Context) {
		c.Next()
		if h.db == nil {
			return
		}
		requestID := c.Writer.Header().Get(HeaderXRequestID)
		_ = h.db.Create(&model.AdminAuditLog{
			RequestID:  requestID,
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			ClientIP:   c.ClientIP(),
			StatusCode: c.Writer.Status(),
			Success:    c.Writer.Status() >= 200 && c.Writer.Status() < 400,
			CreatedAt:  time.Now(),
		}).Error
	}

	// pprof Profiling 调试探针（必须增加鉴权保护）
	if h.cfg.Pprof.Enabled {
		pprofGroup := r.Group("/debug/pprof")
		pprofGroup.Use(authMiddleware)
		{
			pprofGroup.GET("/", gin.WrapF(pprof.Index))
			pprofGroup.GET("/cmdline", gin.WrapF(pprof.Cmdline))
			pprofGroup.GET("/profile", gin.WrapF(pprof.Profile))
			pprofGroup.POST("/symbol", gin.WrapF(pprof.Symbol))
			pprofGroup.GET("/symbol", gin.WrapF(pprof.Symbol))
			pprofGroup.GET("/trace", gin.WrapF(pprof.Trace))
			pprofGroup.GET("/allocs", gin.WrapH(pprof.Handler("allocs")))
			pprofGroup.GET("/block", gin.WrapH(pprof.Handler("block")))
			pprofGroup.GET("/goroutine", gin.WrapH(pprof.Handler("goroutine")))
			pprofGroup.GET("/heap", gin.WrapH(pprof.Handler("heap")))
			pprofGroup.GET("/mutex", gin.WrapH(pprof.Handler("mutex")))
			pprofGroup.GET("/threadcreate", gin.WrapH(pprof.Handler("threadcreate")))
		}
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
			protected.POST("/intents/cancel", intentHandler.CancelIntent)
			protected.DELETE("/intents/:orderId", intentHandler.CancelIntent)
			protected.GET("/transfers", transferHandler.GetTransfers)
			protected.POST("/intents/simulate", intentHandler.SimulateOnChainTransfer)
			protected.GET("/options", intentHandler.GetPaymentOptions)

		}
	}

	// Admin Control Plane Routes (收款钱包池与代币白名单管理)
	adminHandler := NewAdminHandler(h.db, h.cfg)
	admin := r.Group("/api/v1/admin")
	admin.Use(adminAuthMiddleware, adminAuditMiddleware)
	{
		// 收款地址池 CRUD
		admin.GET("/wallets", adminHandler.ListWallets)
		admin.POST("/wallets", adminHandler.CreateWallet)
		admin.PUT("/wallets/:id", adminHandler.UpdateWallet)
		admin.DELETE("/wallets/:id", adminHandler.DeleteWallet)

		// 代币合约白名单 CRUD
		admin.GET("/tokens", adminHandler.ListTokens)
		admin.POST("/tokens", adminHandler.CreateToken)
		admin.PUT("/tokens/:id", adminHandler.UpdateToken)
		admin.DELETE("/tokens/:id", adminHandler.DeleteToken)
	}

	// Mock Webhook Receiver Endpoint 仅在本地开发调试（AllowLocal 为 true）时挂载
	if h.cfg.Webhook.AllowLocal {
		r.POST("/api/v1/mock/webhook", func(c *gin.Context) {
			sig := c.GetHeader("X-Signature-SHA256")
			bodyBytes, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot read body"})
				return
			}

			// Verify HMAC signature
			isValid := signature.VerifyHMACSHA256(bodyBytes, h.cfg.Webhook.Secret, sig)
			logger.Info("mock webhook received", "signature_present", sig != "", "payload_bytes", len(bodyBytes), "signature_valid", isValid)

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
	}

	return r
}

func getBearerToken(header string) string {
	parts := strings.Split(header, " ")
	if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
		return parts[1]
	}
	return ""
}
