package websocket

import (
	"net/http"
	"testing"
)

func TestNewUpgraderAllowsAnyOriginWhenConfigured(t *testing.T) {
	upgrader := NewUpgrader("*")
	request := &http.Request{Header: http.Header{"Origin": []string{"https://play.example.test"}}}

	if upgrader.Origin != "*" {
		t.Fatalf("expected origin header value to be preserved, got %q", upgrader.Origin)
	}
	if !upgrader.CheckOrigin(request) {
		t.Fatal("expected wildcard origin to allow user websocket from another host")
	}
}

func TestNewUpgraderChecksSpecificOrigin(t *testing.T) {
	upgrader := NewUpgrader("https://play.example.test")

	if !upgrader.CheckOrigin(&http.Request{Header: http.Header{"Origin": []string{"https://play.example.test"}}}) {
		t.Fatal("expected matching origin to be allowed")
	}
	if upgrader.CheckOrigin(&http.Request{Header: http.Header{"Origin": []string{"https://other.example.test"}}}) {
		t.Fatal("expected non-matching origin to be rejected")
	}
}
