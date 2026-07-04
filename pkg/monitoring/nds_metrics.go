package monitoring

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var (
	workerMetricLabelOnce sync.Once
	workerMetricLabel     string
)

func metricLabel(value string) string {
	return strconv.Quote(value)
}

func currentWorkerMetricLabel() string {
	workerMetricLabelOnce.Do(func() {
		workerMetricLabel = strings.TrimSpace(os.Getenv("CLOUD_GAME_WORKER_TAG"))
		if workerMetricLabel == "" {
			workerMetricLabel = strings.TrimSpace(os.Getenv("HOSTNAME"))
		}
		if workerMetricLabel == "" {
			if hostname, err := os.Hostname(); err == nil {
				workerMetricLabel = strings.TrimSpace(hostname)
			}
		}
		if workerMetricLabel == "" {
			workerMetricLabel = "unknown"
		}
	})
	return workerMetricLabel
}

func ObserveNDSRoomCreate(status string, duration time.Duration) {
	metrics.GetOrCreateHistogram(`cloud_game_nds_room_create_duration_seconds{status=` + metricLabel(status) + `}`).
		Update(duration.Seconds())
}

func IncNDSTokenFailure(reason string) {
	metrics.GetOrCreateCounter(`cloud_game_nds_token_failures_total{reason=` + metricLabel(reason) + `}`).Inc()
}

func IncNDSWebhookRetry(event string) {
	metrics.GetOrCreateCounter(`cloud_game_nds_webhook_retries_total{event=` + metricLabel(event) + `}`).Inc()
}

func IncNDSSaveUploadFailure(stage string) {
	metrics.GetOrCreateCounter(`cloud_game_nds_save_upload_failures_total{stage=` + metricLabel(stage) + `}`).Inc()
}

func AddNDSNetpacketStats(room string, player uint16, txPackets uint64, rxPackets uint64, droppedPackets uint64) {
	playerLabel := strconv.Itoa(int(player))
	if txPackets > 0 {
		metrics.GetOrCreateCounter(`cloud_game_nds_netpacket_packets_total{room=` + metricLabel(room) + `,player=` + metricLabel(playerLabel) + `,direction="tx"}`).
			AddInt64(int64(txPackets))
	}
	if rxPackets > 0 {
		metrics.GetOrCreateCounter(`cloud_game_nds_netpacket_packets_total{room=` + metricLabel(room) + `,player=` + metricLabel(playerLabel) + `,direction="rx"}`).
			AddInt64(int64(rxPackets))
	}
	if droppedPackets > 0 {
		metrics.GetOrCreateCounter(`cloud_game_nds_netpacket_packets_total{room=` + metricLabel(room) + `,player=` + metricLabel(playerLabel) + `,direction="dropped"}`).
			AddInt64(int64(droppedPackets))
	}
}

func SetWorkerVideoFPS(fps float64) {
	if fps < 0 {
		fps = 0
	}
	metrics.GetOrCreateGauge(`cloud_game_worker_video_fps{worker=`+metricLabel(currentWorkerMetricLabel())+`}`, nil).Set(fps)
}

func ObserveWorkerVideoEncode(duration time.Duration) {
	metrics.GetOrCreateHistogram(`cloud_game_worker_video_encode_duration_seconds{worker=` + metricLabel(currentWorkerMetricLabel()) + `}`).
		Update(duration.Seconds())
}

func IncWorkerVideoFrame() {
	metrics.GetOrCreateCounter(`cloud_game_worker_video_frames_total{worker=` + metricLabel(currentWorkerMetricLabel()) + `}`).Inc()
}
