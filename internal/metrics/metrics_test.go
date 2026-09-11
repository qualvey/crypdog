package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsRegistrationAndHelpers(t *testing.T) {
	InitMetrics()

	// Test RecordScanBlock
	RecordScanBlock("ARBITRUM", 100, 105)

	// Test RecordScanError
	RecordScanError("ARBITRUM", "evm")

	// Test RecordTransferCaptured
	RecordTransferCaptured("ARBITRUM", "USDT")

	// Test RecordTransferMatch
	RecordTransferMatch("ARBITRUM", true)
	RecordTransferMatch("ARBITRUM", false)

	// Test RecordIntentStatus
	RecordIntentStatus("ARBITRUM", "USDT", "PAID")

	// Test RecordMicroAllocation
	RecordMicroAllocation(true)
	RecordMicroAllocation(false)

	// Test SetMicroPoolUsed
	SetMicroPoolUsed("ARBITRUM", "0x123", 5)

	// Test SetPipelineQueueLength
	SetPipelineQueueLength(12)

	// Test RecordWebhookDispatch
	RecordWebhookDispatch("success", 0.123)

	// Test RecordHTTPRequest
	RecordHTTPRequest("GET", "/health", 200, 0.005)

	_ = prometheus.DefaultRegisterer
}
