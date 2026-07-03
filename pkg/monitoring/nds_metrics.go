package monitoring

import (
	"strconv"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

func metricLabel(value string) string {
	return strconv.Quote(value)
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

func ObserveWorkerVideoEncode(duration time.Duration) {
	metrics.GetOrCreateHistogram("cloud_game_worker_video_encode_duration_seconds").Update(duration.Seconds())
}

func IncWorkerVideoFrame() {
	metrics.GetOrCreateCounter("cloud_game_worker_video_frames_total").Inc()
}
