package coordinator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

func TestCoordinatorMetricsAggregatesWorkerCloudGameSamples(t *testing.T) {
	workerMetrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join([]string{
			`# HELP cloud_game_worker_video_frames_total worker frames`,
			`cloud_game_worker_video_frames_total{worker="mkds-p1"} 42`,
			`cloud_game_nds_save_upload_failures_total{stage="put"} 1`,
			`go_goroutines 12`,
			``,
		}, "\n")))
	}))
	defer workerMetrics.Close()

	h := NewHub(config.CoordinatorConfig{}, logger.NewConsole(false, "test", false))
	h.workers.Put(com.NewUid(), &Worker{MonitoringURL: workerMetrics.URL, Tag: "mkds-p1"})

	rec := httptest.NewRecorder()
	h.handleCoordinatorMetrics()(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `cloud_game_worker_video_frames_total{worker="mkds-p1"} 42`) {
		t.Fatalf("worker video metric missing from aggregated body: %s", body)
	}
	if !strings.Contains(body, `cloud_game_nds_save_upload_failures_total{stage="put",worker="mkds-p1"} 1`) {
		t.Fatalf("worker label was not added to worker NDS metric: %s", body)
	}
	if strings.Contains(body, "go_goroutines 12") {
		t.Fatalf("non-cloud worker metric leaked into coordinator body: %s", body)
	}
}

func TestWorkerMetricSampleLineFiltersAndLabels(t *testing.T) {
	line, ok := workerMetricSampleLine(`cloud_game_nds_netpacket_packets_total{room="r1",player="1",direction="tx"} 7`, "mkds-p1")
	if !ok {
		t.Fatal("expected NDS worker metric to be included")
	}
	if want := `cloud_game_nds_netpacket_packets_total{room="r1",player="1",direction="tx",worker="mkds-p1"} 7`; line != want {
		t.Fatalf("worker metric line = %q, want %q", line, want)
	}

	if _, ok := workerMetricSampleLine(`# TYPE cloud_game_nds_netpacket_packets_total counter`, "mkds-p1"); ok {
		t.Fatal("metadata comments should be skipped")
	}
	if _, ok := workerMetricSampleLine(`go_memstats_alloc_bytes 12`, "mkds-p1"); ok {
		t.Fatal("non-cloud metrics should be skipped")
	}
}
