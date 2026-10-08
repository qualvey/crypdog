package scanner

import (
	"errors"
	"testing"
	"time"

	"crypdog/internal/model"
	"github.com/stretchr/testify/require"
)

func TestHealthTrackerFailureAndRecovery(t *testing.T) {
	tracker := NewHealthTracker(model.ChainTron, time.Second)

	status := tracker.Snapshot(0)
	require.False(t, status.Healthy)
	require.Nil(t, status.LastSuccessAt)

	tracker.MarkFailure(errors.New("rpc unavailable"))
	status = tracker.Snapshot(123)
	require.False(t, status.Healthy)
	require.Equal(t, uint64(123), status.LatestBlock)
	require.Equal(t, "rpc unavailable", status.LastError)
	require.Equal(t, uint64(1), status.ConsecutiveFailures)
	require.NotNil(t, status.LastFailureAt)

	tracker.MarkFailure(errors.New("rpc still unavailable"))
	status = tracker.Snapshot(123)
	require.Equal(t, uint64(2), status.ConsecutiveFailures)
	require.Equal(t, "rpc still unavailable", status.LastError)

	tracker.MarkSuccess()
	status = tracker.Snapshot(124)
	require.True(t, status.Healthy)
	require.Equal(t, uint64(124), status.LatestBlock)
	require.Empty(t, status.LastError)
	require.Zero(t, status.ConsecutiveFailures)
	require.NotNil(t, status.LastSuccessAt)
}
