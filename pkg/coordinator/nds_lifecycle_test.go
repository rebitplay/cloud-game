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

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
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

func TestNDSRoomCloseFlushesSavesAndReportsStatuses(t *testing.T) {
	events := make(chan ndsWebhookPayload, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload ndsWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook: %v", err)
		}
		events <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("NDS_WEBHOOK_URL", server.URL)

	h := NewHub(config.CoordinatorConfig{}, logger.NewConsole(false, "test", false))
	startedAt := time.Now().UTC().Add(-5 * time.Second)
	flushedAt := time.Now().UTC()
	internalRoomID := "room-123-p1___Tetris DS"
	worker := &Worker{Connection: &fakeNDSConnection{
		id: com.NewUid(),
		flushStatus: &api.NDSSaveStatus{
			FlushedAt: flushedAt,
			Player:    1,
			Ref:       "user_1",
			RoomID:    internalRoomID,
			SHA1:      "0123456789abcdef0123456789abcdef01234567",
			Size:      12,
			Status:    "uploaded",
		},
	}}
	room := &ndsRoomSession{
		createdAt: time.Now().UTC().Add(-10 * time.Second),
		players: map[int]*ndsSeat{
			1: {player: 1, ref: "user_1", roomID: internalRoomID, saveStatus: "unchanged", worker: worker},
		},
		roomID:    "room-123",
		startedAt: &startedAt,
		state:     ndsRoomActive,
		updatedAt: time.Now().UTC(),
	}
	h.ndsRooms.put(room)

	h.closeNDSRoom("room-123", ndsCloseHost)

	var closed ndsWebhookPayload
	deadline := time.After(time.Second)
	for closed.Event != "room.closed" {
		select {
		case event := <-events:
			if event.Event == "room.closed" {
				closed = event
			}
		case <-deadline:
			t.Fatal("timed out waiting for room.closed webhook")
		}
	}
	extra := closed.Extra
	if extra["reason"] != ndsCloseHost {
		t.Fatalf("reason = %#v, want %q", extra["reason"], ndsCloseHost)
	}
	players, ok := extra["players"].([]any)
	if !ok || len(players) != 1 {
		t.Fatalf("players extra = %#v", extra["players"])
	}
	player, ok := players[0].(map[string]any)
	if !ok || player["save_status"] != "uploaded" {
		t.Fatalf("player save status = %#v", players[0])
	}
	if got := h.ndsRooms.get("room-123").players[1].saveStatus; got != "uploaded" {
		t.Fatalf("stored save status = %q, want uploaded", got)
	}
}

func TestNDSRoomCloseRecyclesWorkersAndRuntimeDir(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")
	runtimeDir := t.TempDir()
	t.Setenv("RUNTIME_DIR", runtimeDir)

	h := testNDSHub(t, 1)
	create := postNDSRoom(t, h, testNDSCreateBody("room-123", 2))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body=%s", create.Code, http.StatusCreated, create.Body.String())
	}

	groupDir := filepath.Join(runtimeDir, "mkds-r1")
	saveFile := filepath.Join(groupDir, "p1", "save", "leak.srm")
	if err := os.MkdirAll(filepath.Dir(saveFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(saveFile, []byte("old-save"), 0600); err != nil {
		t.Fatal(err)
	}
	if len(h.ndsWorkersForGroup("mkds-r1")) != 4 {
		t.Fatalf("test setup worker count = %d, want 4", len(h.ndsWorkersForGroup("mkds-r1")))
	}

	h.closeNDSRoom("room-123", ndsCloseHost)

	if len(h.ndsWorkersForGroup("mkds-r1")) != 0 {
		t.Fatalf("workers were not recycled: %d still registered", len(h.ndsWorkersForGroup("mkds-r1")))
	}
	if _, err := os.Stat(groupDir); !os.IsNotExist(err) {
		t.Fatalf("group runtime dir still exists after recycle: %v", err)
	}
	if got := h.ndsRooms.get("room-123").state; got != ndsRoomClosed {
		t.Fatalf("room state = %s, want %s", got, ndsRoomClosed)
	}
}
