package api

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"crypdog/internal/logger"
	"crypdog/internal/metrics"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const HeaderXRequestID = "X-Request-ID"

// RequestIDMiddleware 统一注入并传递 Request ID
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := c.GetHeader(HeaderXRequestID)
		if reqID == "" {
			reqID = uuid.New().String()
		}

		c.Writer.Header().Set(HeaderXRequestID, reqID)
		c.Set(string(logger.RequestIDKey), reqID)
		c.Request = c.Request.WithContext(logger.WithRequestID(c.Request.Context(), reqID))

		c.Next()
	}
}

// AccessLogMiddleware 记录结构化 HTTP 请求日志与 Prometheus 延迟指标
func AccessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()
		method := c.Request.Method
		clientIP := c.ClientIP()

		if raw != "" {
			path = path + "?" + raw
		}

		// 为避免 Prometheus 路径标签基数爆炸，采用路由模式作为 Metric Label
		metricPath := c.FullPath()
		if metricPath == "" {
			metricPath = c.Request.URL.Path
		}

		// 记录 Prometheus HTTP 指标
		metrics.RecordHTTPRequest(method, metricPath, status, latency.Seconds())

		// 记录结构化日志
		logger.InfoContext(c.Request.Context(), "HTTP Request",
			"status", status,
			"method", method,
			"path", path,
			"ip", clientIP,
			"latency_ms", latency.Milliseconds(),
			"bytes", c.Writer.Size(),
			"user_agent", c.Request.UserAgent(),
		)
	}
}

// StructuredRecoveryMiddleware 替代 Gin 默认 Recovery，结构化记录 Panic 异常
func StructuredRecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				logger.ErrorContext(c.Request.Context(), "HTTP Panic Recovered",
					"error", fmt.Sprintf("%v", r),
					"stack", stack,
					"path", c.Request.URL.Path,
				)

				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    500,
					"message": "Internal Server Error",
				})
			}
		}()
		c.Next()
	}
}
