package queue

import (
	"bytes"
	"context"
	"crypdog/internal/logger"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"net"
	"net/http"
	"strings"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/metrics"
	"crypdog/internal/model"
	"crypdog/internal/signature"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	WebhookEventGet     = "get"
	WebhookEventConfirm = "confirm"
)

type WebhookPayload struct {
	Event                 string          `json:"event"`
	OrderID               string          `json:"orderId"`
	Chain                 model.Chain     `json:"chain"`
	Token                 model.Token     `json:"token"`
	TargetAddress         string          `json:"targetAddress"`
	Amount                decimal.Decimal `json:"amount"`
	TxHash                string          `json:"txHash"`
	BlockTimestamp        int64           `json:"blockTimestamp"`
	Confirmations         uint64          `json:"confirmations,omitempty"`
	RequiredConfirmations uint64          `json:"requiredConfirmations,omitempty"`
	Timestamp             string          `json:"timestamp"`
}

type WebhookDispatcher struct {
	db     *gorm.DB
	cfg    *config.Config
	client *http.Client
}

func (w *WebhookDispatcher) maxAttempts() int {
	if w.cfg == nil || w.cfg.Webhook.MaxRetries <= 0 {
		return 4 // one initial attempt plus the default three retries
	}
	return 1 + w.cfg.Webhook.MaxRetries
}

func NewWebhookDispatcher(db *gorm.DB, cfg *config.Config) *WebhookDispatcher {
	timeout := 10 * time.Second
	if cfg != nil && cfg.Webhook.TimeoutSec > 0 {
		timeout = time.Duration(cfg.Webhook.TimeoutSec) * time.Second
	}

	allowLocal := cfg != nil && cfg.Webhook.AllowLocal
	requireHTTPS := cfg != nil && (strings.EqualFold(cfg.Env, "production") || strings.EqualFold(cfg.Env, "prod"))

	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			// 解析实际建立连接的目标 IP，在网络层彻底防范 SSRF 与 DNS Rebinding
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no ip addresses resolved for host: %s", host)
			}

			for _, ip := range ips {
				if !allowLocal {
					if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || strings.HasPrefix(ip.String(), "169.254.") {
						return nil, fmt.Errorf("SSRF protection: connection to restricted IP %s blocked", ip.String())
					}
				}
			}

			// 选择第一个合法 IP 建立物理 TCP 连接
			targetAddr := net.JoinHostPort(ips[0].String(), port)
			return dialer.DialContext(ctx, network, targetAddr)
		},
	}

	return &WebhookDispatcher{
		db:  db,
		cfg: cfg,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				// 校验重定向目标 URL 防 SSRF 重定向绕过
				return signature.ValidateWebhookURL(req.URL.String(), allowLocal, requireHTTPS)
			},
		},
	}
}

// StartRetryWorker 周期性扫描数据库中投递失败且待重试的 WebhookLog，防止服务重启后内存重试任务丢失
func (w *WebhookDispatcher) StartRetryWorker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("webhook retry worker stopped")
			return
		case <-ticker.C:
			w.retryPendingLogs(ctx)
		}
	}
}

func (w *WebhookDispatcher) retryPendingLogs(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic in webhook retry worker", "error", r)
		}
	}()

	now := time.Now()
	var pendingLogs []model.WebhookLog
	since := now.Add(-2 * time.Hour)
	err := w.db.WithContext(ctx).
		Where("success = ? AND attempt < ? AND next_retry_at IS NOT NULL AND next_retry_at <= ? AND created_at >= ? AND (retry_claimed_at IS NULL OR retry_claimed_at <= ?)",
			false, w.maxAttempts(), now, since, now.Add(-5*time.Minute)).
		Order("id ASC").
		Limit(20).
		Find(&pendingLogs).Error
	if err != nil || len(pendingLogs) == 0 {
		return
	}

	for _, l := range pendingLogs {
		var payload WebhookPayload
		if err := json.Unmarshal([]byte(l.Payload), &payload); err != nil {
			w.db.Model(&l).Update("next_retry_at", nil)
			continue
		}

		event := l.Event
		if event == "" {
			event = payload.Event
		}

		var successCount int64
		w.db.Model(&model.WebhookLog{}).Where("order_id = ? AND event = ? AND success = ?", l.OrderID, event, true).Count(&successCount)
		if successCount > 0 {
			w.db.Model(&l).Update("next_retry_at", nil)
			continue
		}

		// Claim with a lease instead of clearing next_retry_at. If the process
		// crashes after this point, the lease expires and the task is retried.
		claimedAt := time.Now()
		res := w.db.Model(&model.WebhookLog{}).
			Where("id = ? AND next_retry_at IS NOT NULL AND (retry_claimed_at IS NULL OR retry_claimed_at <= ?)", l.ID, now.Add(-5*time.Minute)).
			Update("retry_claimed_at", claimedAt)
		if res.RowsAffected == 0 {
			continue // 已被并发消费，跳过
		}

		logger.Info("retrying webhook delivery", "order_id", l.OrderID, "event", event, "attempt", l.Attempt+1)
		w.DeliverWithRetry(l.OrderID, l.WebhookURL, payload, l.Attempt+1)
	}
}

// DispatchEventAsync enqueues an asynchronous webhook delivery attempt with a specific event type (e.g., "get" or "confirm")
func (w *WebhookDispatcher) DispatchEventAsync(event string, intent *model.PaymentIntent, txHash string, blockTimestamp int64, confirmations, requiredConfirmations uint64) {
	go func() {
		payload := WebhookPayload{
			Event:                 event,
			OrderID:               intent.OrderID,
			Chain:                 intent.Chain,
			Token:                 intent.Token,
			TargetAddress:         intent.TargetAddress,
			Amount:                intent.ReceivedAmount,
			TxHash:                txHash,
			BlockTimestamp:        blockTimestamp,
			Confirmations:         confirmations,
			RequiredConfirmations: requiredConfirmations,
			Timestamp:             time.Now().UTC().Format(time.RFC3339),
		}

		w.DeliverWithRetry(intent.OrderID, intent.WebhookURL, payload, 1)
	}()
}

// DispatchAsync enqueues an asynchronous webhook delivery attempt (defaults to "confirm" event for backward compatibility)
func (w *WebhookDispatcher) DispatchAsync(intent *model.PaymentIntent, txHash string, blockTimestamp int64) {
	required := uint64(0)
	if w.cfg != nil {
		required = w.cfg.GetRequiredConfirmations(intent.Chain)
	}
	w.DispatchEventAsync(WebhookEventConfirm, intent, txHash, blockTimestamp, intent.Confirmations, required)
}

// DeliverWithRetry sends the payload and registers retry schedule in DB (handled exclusively by StartRetryWorker)
func (w *WebhookDispatcher) DeliverWithRetry(orderID, webhookURL string, payload WebhookPayload, attempt int) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		logger.Error("marshal webhook payload failed", "order_id", orderID, "error", err)
		return
	}
	allowLocal := w.cfg != nil && w.cfg.Webhook.AllowLocal
	requireHTTPS := w.cfg != nil && (strings.EqualFold(w.cfg.Env, "production") || strings.EqualFold(w.cfg.Env, "prod"))
	if err := signature.ValidateWebhookURL(webhookURL, allowLocal, requireHTTPS); err != nil {
		logger.Warn("webhook target rejected", "order_id", orderID, "error", err)
		return
	}

	sig := signature.GenerateHMACSHA256(jsonBytes, w.cfg.Webhook.Secret)

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		logger.Error("create webhook request failed", "order_id", orderID, "error", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature-SHA256", sig)
	req.Header.Set("User-Agent", "CrypDog-Webhook-Guardian/1.0")

	logger.Info("sending webhook notification", "order_id", orderID, "event", payload.Event, "attempt", attempt, "target_host", webhookURL)

	startTime := time.Now()
	resp, err := w.client.Do(req)
	durationSec := time.Since(startTime).Seconds()

	webhookLog := model.WebhookLog{
		OrderID:    orderID,
		Event:      payload.Event,
		WebhookURL: webhookURL,
		Payload:    string(jsonBytes),
		Signature:  sig,
		Attempt:    attempt,
	}

	success := false
	if err == nil {
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(resp.Body)
		webhookLog.StatusCode = resp.StatusCode
		webhookLog.ResponseBody = string(bodyBytes)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			success = true
		}
	} else {
		webhookLog.ResponseBody = err.Error()
	}

	webhookLog.Success = success

	var nextDelay time.Duration
	if !success && attempt < w.maxAttempts() {
		backoffDurations := []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second, 2 * time.Minute}
		nextDelay = backoffDurations[attempt-1]
		nextRetryAt := time.Now().Add(nextDelay)
		webhookLog.NextRetryAt = &nextRetryAt
	}

	w.db.Create(&webhookLog)

	if success {
		metrics.RecordWebhookDispatch("success", durationSec)
		logger.Info("webhook delivered", "order_id", orderID, "event", payload.Event, "attempt", attempt)
		return
	}

	logger.Warn("webhook delivery failed", "order_id", orderID, "status", webhookLog.StatusCode, "attempt", attempt, "error", err)

	// 重试彻底收拢至 StartRetryWorker 单一持久化队列驱动，严禁在此启动 time.AfterFunc 导致双重触发
	if attempt < w.maxAttempts() {
		metrics.RecordWebhookDispatch("retry", durationSec)
		logger.Info("webhook delivery scheduled for retry", "order_id", orderID, "delay", nextDelay, "attempt", attempt+1)
	} else {
		metrics.RecordWebhookDispatch("max_retried", durationSec)
		logger.Error("webhook delivery exceeded max retries", "order_id", orderID, "attempt", attempt)
	}
}
