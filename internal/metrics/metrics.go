package metrics

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	once sync.Once

	// Scanner 指标
	ScannerBlockHeight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crypdog",
			Subsystem: "scanner",
			Name:      "block_height",
			Help:      "Current scanned block height per chain",
		},
		[]string{"chain"},
	)

	ScannerNetworkBlockHeight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crypdog",
			Subsystem: "scanner",
			Name:      "network_block_height",
			Help:      "Latest block height on public network per chain",
		},
		[]string{"chain"},
	)

	ScannerBlockLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crypdog",
			Subsystem: "scanner",
			Name:      "block_lag",
			Help:      "Block height lag between network and local scanner",
		},
		[]string{"chain"},
	)

	ScannerErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "scanner",
			Name:      "errors_total",
			Help:      "Total scanner errors encountered",
		},
		[]string{"chain", "driver"},
	)

	ChainTransfersCapturedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "scanner",
			Name:      "transfers_captured_total",
			Help:      "Total blockchain transfer events captured",
		},
		[]string{"chain", "token"},
	)

	// Engine & Payment 指标
	PaymentIntentsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "engine",
			Name:      "payment_intents_total",
			Help:      "Total payment intents created or transitioned by status",
		},
		[]string{"chain", "token", "status"},
	)

	MicroAmountPoolUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crypdog",
			Subsystem: "engine",
			Name:      "micro_amount_pool_used",
			Help:      "Current count of active micro amounts allocated",
		},
		[]string{"chain", "address"},
	)

	MicroAmountAllocationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "engine",
			Name:      "micro_amount_allocations_total",
			Help:      "Total micro amount allocation attempts by outcome",
		},
		[]string{"status"}, // "success", "collision"
	)

	TransfersMatchedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "engine",
			Name:      "transfers_matched_total",
			Help:      "Total transfers matched against intents",
		},
		[]string{"chain", "status"}, // "matched", "unmatched"
	)

	// Queue & Webhook 指标
	PipelineQueueLength = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "crypdog",
			Subsystem: "pipeline",
			Name:      "queue_length",
			Help:      "Current number of transfers pending in pipeline buffer channel",
		},
	)

	WebhookDispatchesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "webhook",
			Name:      "dispatches_total",
			Help:      "Total webhook dispatch attempts by status",
		},
		[]string{"status"}, // "success", "failed", "max_retried"
	)

	WebhookDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "crypdog",
			Subsystem: "webhook",
			Name:      "duration_seconds",
			Help:      "Webhook HTTP response duration in seconds",
			Buckets:   prometheus.DefBuckets,
		},
	)

	// HTTP API 指标
	HttpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crypdog",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total HTTP requests received",
		},
		[]string{"method", "path", "status"},
	)

	HttpRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "crypdog",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency distributions in seconds",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "path"},
	)
)

// InitMetrics 注册所有指标到 Prometheus 全局注册表
func InitMetrics() {
	once.Do(func() {
		prometheus.MustRegister(
			ScannerBlockHeight,
			ScannerNetworkBlockHeight,
			ScannerBlockLag,
			ScannerErrorsTotal,
			ChainTransfersCapturedTotal,
			PaymentIntentsTotal,
			MicroAmountPoolUsed,
			MicroAmountAllocationsTotal,
			TransfersMatchedTotal,
			PipelineQueueLength,
			WebhookDispatchesTotal,
			WebhookDurationSeconds,
			HttpRequestsTotal,
			HttpRequestDurationSeconds,
		)
	})
}

// 辅助方法：快速更新指标

func RecordScanBlock(chain string, currentBlock uint64, networkBlock uint64) {
	ScannerBlockHeight.WithLabelValues(chain).Set(float64(currentBlock))
	if networkBlock >= currentBlock {
		ScannerNetworkBlockHeight.WithLabelValues(chain).Set(float64(networkBlock))
		ScannerBlockLag.WithLabelValues(chain).Set(float64(networkBlock - currentBlock))
	}
}

func RecordScanError(chain string, driver string) {
	ScannerErrorsTotal.WithLabelValues(chain, driver).Inc()
}

func RecordTransferCaptured(chain string, token string) {
	ChainTransfersCapturedTotal.WithLabelValues(chain, token).Inc()
}

func RecordTransferMatch(chain string, matched bool) {
	status := "unmatched"
	if matched {
		status = "matched"
	}
	TransfersMatchedTotal.WithLabelValues(chain, status).Inc()
}

func RecordIntentStatus(chain string, token string, status string) {
	PaymentIntentsTotal.WithLabelValues(chain, token, status).Inc()
}

func RecordMicroAllocation(success bool) {
	status := "collision"
	if success {
		status = "success"
	}
	MicroAmountAllocationsTotal.WithLabelValues(status).Inc()
}

func SetMicroPoolUsed(chain string, address string, count int) {
	MicroAmountPoolUsed.WithLabelValues(chain, address).Set(float64(count))
}

func SetPipelineQueueLength(length int) {
	PipelineQueueLength.Set(float64(length))
}

func RecordWebhookDispatch(status string, durationSec float64) {
	WebhookDispatchesTotal.WithLabelValues(status).Inc()
	if durationSec > 0 {
		WebhookDurationSeconds.Observe(durationSec)
	}
}

func RecordHTTPRequest(method, path string, status int, durationSec float64) {
	statusStr := strconv.Itoa(status)
	HttpRequestsTotal.WithLabelValues(method, path, statusStr).Inc()
	HttpRequestDurationSeconds.WithLabelValues(method, path).Observe(durationSec)
}
