package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypdog/internal/config"
	"crypdog/internal/engine"
	"crypdog/internal/metrics"
	"crypdog/internal/scanner"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

func setupTestRouter(t *testing.T) (*gorm.DB, http.Handler) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err)

	metrics.InitMetrics()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   "8080",
			Secret: "test-secret",
		},
		Metrics: config.MetricsConfig{
			Enabled: true,
			Path:    "/metrics",
		},
		Pprof: config.PprofConfig{
			Enabled: true,
		},
	}

	poolMgr := engine.NewMicroAmountManager(db)
	scannerMgr := scanner.NewManager()

	handler := NewHandler(db, cfg, poolMgr, scannerMgr, nil)
	r := SetupRouter(handler)
	return db, r
}

func TestObservability_HealthProbes(t *testing.T) {
	_, r := setupTestRouter(t)

	// Test Liveness Probe
	reqLive := httptest.NewRequest("GET", "/healthz/live", nil)
	wLive := httptest.NewRecorder()
	r.ServeHTTP(wLive, reqLive)

	assert.Equal(t, http.StatusOK, wLive.Code)
	var liveBody map[string]interface{}
	err := json.Unmarshal(wLive.Body.Bytes(), &liveBody)
	assert.NoError(t, err)
	assert.Equal(t, "UP", liveBody["status"])

	// Test Readiness Probe
	reqReady := httptest.NewRequest("GET", "/healthz/ready", nil)
	wReady := httptest.NewRecorder()
	r.ServeHTTP(wReady, reqReady)

	assert.Equal(t, http.StatusOK, wReady.Code)
	var readyBody map[string]interface{}
	err = json.Unmarshal(wReady.Body.Bytes(), &readyBody)
	assert.NoError(t, err)
	assert.Equal(t, "UP", readyBody["status"])
	assert.NotNil(t, readyBody["components"])
}

func TestObservability_RequestIDMiddleware(t *testing.T) {
	_, r := setupTestRouter(t)

	// Case 1: Auto-generate X-Request-ID
	req1 := httptest.NewRequest("GET", "/health", nil)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	reqID1 := w1.Header().Get("X-Request-ID")
	assert.NotEmpty(t, reqID1)

	// Case 2: Passthrough existing X-Request-ID
	customID := "custom-trace-uuid-12345"
	req2 := httptest.NewRequest("GET", "/health", nil)
	req2.Header.Set("X-Request-ID", customID)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	reqID2 := w2.Header().Get("X-Request-ID")
	assert.Equal(t, customID, reqID2)
}

func TestObservability_PrometheusMetricsEndpoint(t *testing.T) {
	_, r := setupTestRouter(t)

	// Trigger some metrics by hitting /health
	reqHealth := httptest.NewRequest("GET", "/health", nil)
	wHealth := httptest.NewRecorder()
	r.ServeHTTP(wHealth, reqHealth)

	// Query /metrics
	reqMetrics := httptest.NewRequest("GET", "/metrics", nil)
	wMetrics := httptest.NewRecorder()
	r.ServeHTTP(wMetrics, reqMetrics)

	assert.Equal(t, http.StatusOK, wMetrics.Code)
	bodyStr := wMetrics.Body.String()
	assert.True(t, strings.Contains(bodyStr, "crypdog_http_requests_total"))
	assert.True(t, strings.Contains(bodyStr, "go_goroutines"))
}

func TestObservability_PprofEndpoints(t *testing.T) {
	_, r := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.Contains(w.Body.String(), "Types of profiles available"))
}
