package scanner

import (
	"sync"
	"time"

	"crypdog/internal/logger"
	"crypdog/internal/model"
)

func maxScannerHealthAge(interval time.Duration) time.Duration {
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	return interval * 3
}

// HealthStatus is the observable health state shared by all built-in scanners.
type HealthStatus struct {
	Healthy             bool       `json:"healthy"`
	LatestBlock         uint64     `json:"latest_block"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures uint64     `json:"consecutive_failures,omitempty"`
}

// ScannerHealth exposes detailed health information without changing Scanner.
type ScannerHealth interface {
	IsHealthy() bool
	HealthStatus() HealthStatus
}

// HealthTracker centralizes health state, failure logging and recovery logging.
// It is intentionally mutex-based because health is low-frequency state and the
// snapshot must be internally consistent.
type HealthTracker struct {
	mu                  sync.RWMutex
	chain               model.Chain
	interval            time.Duration
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
	lastError           string
	consecutiveFailures uint64
	lastFailureLogAt    time.Time
}

func NewHealthTracker(chain model.Chain, interval time.Duration) *HealthTracker {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return &HealthTracker{chain: chain, interval: interval}
}

func (h *HealthTracker) MarkSuccess() {
	now := time.Now().UTC()
	h.mu.Lock()
	wasFailing := h.consecutiveFailures > 0
	failures := h.consecutiveFailures
	lastFailure := h.lastFailureAt
	h.lastSuccessAt = now
	h.lastFailureAt = time.Time{}
	h.lastError = ""
	h.consecutiveFailures = 0
	h.mu.Unlock()

	if wasFailing {
		downtime := now.Sub(lastFailure).Milliseconds()
		logger.Info("chain scanner recovered", "chain", h.chain, "downtime_ms", downtime, "previous_failures", failures)
	}
}

func (h *HealthTracker) MarkFailure(err error) {
	if err == nil {
		return
	}
	now := time.Now().UTC()
	h.mu.Lock()
	h.lastFailureAt = now
	h.lastError = err.Error()
	h.consecutiveFailures++
	shouldLog := h.consecutiveFailures == 1 || now.Sub(h.lastFailureLogAt) >= 30*time.Second
	if shouldLog {
		h.lastFailureLogAt = now
	}
	failures := h.consecutiveFailures
	h.mu.Unlock()

	if shouldLog {
		logger.Error("chain scanner health check failed", "chain", h.chain, "error", err, "consecutive_failures", failures)
	}
}

func (h *HealthTracker) IsHealthy() bool {
	h.mu.RLock()
	last := h.lastSuccessAt
	lastFailure := h.lastFailureAt
	interval := h.interval
	h.mu.RUnlock()
	return scannerHealthy(last, lastFailure, interval)
}

func (h *HealthTracker) Snapshot(latestBlock uint64) HealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	status := HealthStatus{
		Healthy:             scannerHealthy(h.lastSuccessAt, h.lastFailureAt, h.interval),
		LatestBlock:         latestBlock,
		LastError:           h.lastError,
		ConsecutiveFailures: h.consecutiveFailures,
	}
	if !h.lastSuccessAt.IsZero() {
		value := h.lastSuccessAt
		status.LastSuccessAt = &value
	}
	if !h.lastFailureAt.IsZero() {
		value := h.lastFailureAt
		status.LastFailureAt = &value
	}
	return status
}

func scannerHealthy(lastSuccess, lastFailure time.Time, interval time.Duration) bool {
	if lastSuccess.IsZero() || (!lastFailure.IsZero() && !lastFailure.Before(lastSuccess)) {
		return false
	}
	return time.Since(lastSuccess) <= maxScannerHealthAge(interval)
}
