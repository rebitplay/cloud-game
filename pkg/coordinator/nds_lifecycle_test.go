package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

func TestNDSJoinTimeoutClosesRoom(t *testing.T) {
	h := NewHub(config.CoordinatorConfig{}, logger.NewConsole(false, "test", false))
	room := &ndsRoomSession{
		createdAt:    time.Now().UTC(),
		joinDeadline: time.Now().UTC().Add(10 * time.Millisecond),
		players:      map[int]*ndsSeat{},
		roomID:       "room-123",
		state:        ndsRoomReady,
		updatedAt:    time.Now().UTC(),
	}
	h.ndsRooms.put(room)
	h.startNDSRoomLifecycle(room)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.ndsRooms.get("room-123").state == ndsRoomClosed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("room state = %s, want closed", h.ndsRooms.get("room-123").state)
}

func TestNDSWebhookSignatureAndDelivery(t *testing.T) {
	t.Setenv("NDS_WEBHOOK_SECRET", "hook-secret")
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("NDS_WEBHOOK_URL", server.URL)

	h := NewHub(config.CoordinatorConfig{}, logger.NewConsole(false, "test", false))
	room := &ndsRoomSession{roomID: "room-123", state: ndsRoomReady}
	h.emitNDSWebhook("room.ready", room, nil)

	select {
	case header := <-received:
		sig := header.Get("X-NDS-Signature")
		if !strings.HasPrefix(sig, "sha256=") {
			t.Fatalf("signature header = %q", sig)
		}
		if header.Get("X-NDS-Event") != "room.ready" || header.Get("X-NDS-Delivery") == "" {
			t.Fatalf("unexpected webhook headers: %#v", header)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for webhook")
	}

	body, _ := json.Marshal(ndsWebhookPayload{Event: "room.ready", RoomID: "room-123", State: ndsRoomReady})
	if got := ndsWebhookSignature("hook-secret", "2026-07-03T00:00:00Z", body); got == "" {
		t.Fatal("empty signature")
	}
}

func TestNDSJournalWritesLiveRooms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNTIME_DIR", dir)
	h := NewHub(config.CoordinatorConfig{}, logger.NewConsole(false, "test", false))
	h.ndsRooms.put(&ndsRoomSession{roomID: "room-123", state: ndsRoomReady})
	h.writeNDSJournal()

	path := filepath.Join(dir, "room-journal.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal not written: %v", err)
	}
	h.closeNDSRoom("room-123", ndsCloseHost)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal still exists after close: %v", err)
	}
}
