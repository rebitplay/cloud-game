package worker

import (
	"strings"
	"testing"

	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/config"
)

func TestWorkerMonitoringURLUsesLoopbackForWildcardAddress(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = true
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"

	got := workerMonitoringURL(conf, "[::]:9001")
	if got != "http://127.0.0.1:6621/worker/metrics" {
		t.Fatalf("worker monitoring URL = %q", got)
	}
}

func TestWorkerMonitoringURLUsesLoopbackForZonedLocalhost(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = true
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"

	got := workerMonitoringURL(conf, "mkds-p1.localhost:9001")
	if got != "http://127.0.0.1:6621/worker/metrics" {
		t.Fatalf("worker monitoring URL = %q", got)
	}
}

func TestWorkerMonitoringURLDisabledWhenMetricsOff(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = false
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"

	if got := workerMonitoringURL(conf, "127.0.0.1:9001"); got != "" {
		t.Fatalf("worker monitoring URL = %q, want empty", got)
	}
}

func TestBuildConnQueryAdvertisesMonitoringURL(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = true
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"
	conf.Network.PingEndpoint = "/echo"

	raw, err := buildConnQuery(com.NewUid(), conf, 8701, "127.0.0.1:9001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"monitoring_url":"http://127.0.0.1:6621/worker/metrics"`) {
		t.Fatalf("handshake did not include monitoring URL: %s", raw)
	}
}
