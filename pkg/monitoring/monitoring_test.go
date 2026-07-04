package monitoring

import "testing"

func TestMonitoringNoopsWithoutServer(t *testing.T) {
	var m *Monitoring
	if m.GetMetricsPublicAddress() != "" {
		t.Fatalf("metrics address = %q, want empty for failed server", m.GetMetricsPublicAddress())
	}
	if m.GetProfilingAddress() != "" {
		t.Fatalf("profiling address = %q, want empty for failed server", m.GetProfilingAddress())
	}
	m.Run()
	if err := m.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
}
