package queue

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"crypdog/internal/config"
	"crypdog/internal/model"
	"crypdog/internal/signature"

	"gorm.io/gorm"
)

type WebhookPayload struct {
	Event          string      `json:"event"`
	OrderID        string      `json:"orderId"`
	Chain          model.Chain `json:"chain"`
	Token          model.Token `json:"token"`
	TargetAddress  string      `json:"targetAddress"`
	Amount         float64     `json:"amount"`
	TxHash         string      `json:"txHash"`
	BlockTimestamp int64       `json:"blockTimestamp"`
	Timestamp      string      `json:"timestamp"`
}

type WebhookDispatcher struct {
	db     *gorm.DB
	cfg    *config.Config
	client *http.Client
}

func NewWebhookDispatcher(db *gorm.DB, cfg *config.Config) *WebhookDispatcher {
	return &WebhookDispatcher{
		db:  db,
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// DispatchAsync enqueues an asynchronous webhook delivery attempt
func (w *WebhookDispatcher) DispatchAsync(intent *model.PaymentIntent, txHash string, blockTimestamp int64) {
	go func() {
		payload := WebhookPayload{
			Event:          "PAYMENT_SUCCESS",
			OrderID:        intent.OrderID,
			Chain:          intent.Chain,
			Token:          intent.Token,
			TargetAddress:  intent.TargetAddress,
			Amount:         intent.ReceivedAmount,
			TxHash:         txHash,
			BlockTimestamp: blockTimestamp,
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
		}

		w.DeliverWithRetry(intent.OrderID, intent.WebhookURL, payload, 1)
	}()
}

// DeliverWithRetry sends the payload and retries up to 5 times if necessary
func (w *WebhookDispatcher) DeliverWithRetry(orderID, webhookURL string, payload WebhookPayload, attempt int) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[Webhook] Marshal error for order %s: %v", orderID, err)
		return
	}

	sig := signature.GenerateHMACSHA256(jsonBytes, w.cfg.Webhook.Secret)

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		log.Printf("[Webhook] Failed to create request for order %s: %v", orderID, err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature-SHA256", sig)
	req.Header.Set("User-Agent", "CrypDog-Webhook-Guardian/1.0")

	log.Printf("[Webhook] Sending notification to %s (Order: %s, Attempt: %d)", webhookURL, orderID, attempt)

	resp, err := w.client.Do(req)

	webhookLog := model.WebhookLog{
		OrderID:    orderID,
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
	w.db.Create(&webhookLog)

	if success {
		log.Printf("[Webhook] Successfully delivered webhook for Order: %s", orderID)
		return
	}

	log.Printf("[Webhook] Delivery failed for Order: %s (Status: %d, Attempt: %d)", orderID, webhookLog.StatusCode, attempt)

	// Retry logic (Exponential backoff: 2s, 10s, 30s, 2m)
	if attempt < 5 {
		backoffDurations := []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second, 2 * time.Minute}
		nextDelay := backoffDurations[attempt-1]

		nextRetryAt := time.Now().Add(nextDelay)
		webhookLog.NextRetryAt = &nextRetryAt

		time.AfterFunc(nextDelay, func() {
			w.DeliverWithRetry(orderID, webhookURL, payload, attempt+1)
		})
	} else {
		log.Printf("[Webhook] Exceeded max retries for Order: %s", orderID)
	}
}
