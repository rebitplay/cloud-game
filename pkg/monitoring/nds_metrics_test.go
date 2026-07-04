package monitoring

import (
	"sync"
	"testing"

	"github.com/VictoriaMetrics/metrics"
)

func resetWorkerMetricLabelForTest(t *testing.T) {
	t.Helper()
	workerMetricLabelOnce = sync.Once{}
	workerMetricLabel = ""
	t.Cleanup(func() {
		workerMetricLabelOnce = sync.Once{}
		workerMetricLabel = ""
	})
}

func TestCurrentWorkerMetricLabelUsesExplicitTag(t *testing.T) {
	resetWorkerMetricLabelForTest(t)
	t.Setenv("CLOUD_GAME_WORKER_TAG", "room-123-p1")
	t.Setenv("HOSTNAME", "container-host")

	if got := currentWorkerMetricLabel(); got != "room-123-p1" {
		t.Fatalf("worker metric label = %q, want explicit tag", got)
	}
}

func TestCurrentWorkerMetricLabelFallsBackToHostname(t *testing.T) {
	resetWorkerMetricLabelForTest(t)
	t.Setenv("CLOUD_GAME_WORKER_TAG", "  ")
	t.Setenv("HOSTNAME", "container-host")

	if got := currentWorkerMetricLabel(); got != "container-host" {
		t.Fatalf("worker metric label = %q, want HOSTNAME", got)
	}
}

func TestSetWorkerVideoFPSPublishesGauge(t *testing.T) {
	resetWorkerMetricLabelForTest(t)
	t.Setenv("CLOUD_GAME_WORKER_TAG", "fps-test-worker")

	SetWorkerVideoFPS(59.826)

	got := metrics.GetOrCreateGauge(`cloud_game_worker_video_fps{worker="fps-test-worker"}`, nil).Get()
	if got != 59.826 {
		t.Fatalf("worker video fps gauge = %v, want 59.826", got)
	}

	SetWorkerVideoFPS(-1)
	got = metrics.GetOrCreateGauge(`cloud_game_worker_video_fps{worker="fps-test-worker"}`, nil).Get()
	if got != 0 {
		t.Fatalf("worker video fps gauge after negative value = %v, want 0", got)
	}
}
