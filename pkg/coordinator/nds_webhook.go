package coordinator

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/xid"
)

type ndsWebhookPayload struct {
	Event     string         `json:"event"`
	Extra     map[string]any `json:"extra,omitempty"`
	RoomID    string         `json:"room_id"`
	State     string         `json:"state"`
	Timestamp time.Time      `json:"timestamp"`
}

func (h *Hub) emitNDSWebhook(event string, room *ndsRoomSession, extra map[string]any) {
	url := firstNonEmptyEnv("NDS_WEBHOOK_URL")
	if url == "" || room == nil {
		return
	}
	payload := ndsWebhookPayload{
		Event:     event,
		Extra:     extra,
		RoomID:    room.roomID,
		State:     room.state,
		Timestamp: time.Now().UTC(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		h.log.Warn().Err(err).Str("event", event).Msg("NDS webhook marshal failed")
		return
	}
	go h.postNDSWebhook(url, event, body)
}

func (h *Hub) postNDSWebhook(rawURL string, event string, body []byte) {
	backoff := time.Second
	for attempt := 0; attempt < 5; attempt++ {
		if err := postNDSWebhookOnce(rawURL, event, body); err != nil {
			h.log.Warn().Err(err).Str("event", event).Int("attempt", attempt+1).Msg("NDS webhook delivery failed")
			if attempt < 4 {
				time.Sleep(backoff)
				backoff *= 2
				if backoff > 2*time.Minute {
					backoff = 2 * time.Minute
				}
			}
			continue
		}
		return
	}
}

func postNDSWebhookOnce(rawURL string, event string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NDS-Delivery", xid.New().String())
	req.Header.Set("X-NDS-Event", event)
	req.Header.Set("X-NDS-Timestamp", timestamp)
	if secret := firstNonEmptyEnv("NDS_WEBHOOK_SECRET"); secret != "" {
		req.Header.Set("X-NDS-Signature", "sha256="+ndsWebhookSignature(secret, timestamp, body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

func ndsWebhookSignature(secret string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Hub) writeNDSJournal() {
	path := ndsJournalPath()
	if path == "" || h.ndsRooms == nil {
		return
	}
	var rooms []string
	h.ndsRooms.mu.RLock()
	for roomID, room := range h.ndsRooms.rooms {
		if room.state != ndsRoomClosed && room.state != ndsRoomFailed {
			rooms = append(rooms, roomID)
		}
	}
	h.ndsRooms.mu.RUnlock()
	if len(rooms) == 0 {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		h.log.Warn().Err(err).Msg("NDS journal mkdir failed")
		return
	}
	body, _ := json.Marshal(rooms)
	if err := os.WriteFile(path, body, 0600); err != nil {
		h.log.Warn().Err(err).Msg("NDS journal write failed")
	}
}

func (h *Hub) emitNDSOrphanJournal() {
	path := ndsJournalPath()
	if path == "" {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rooms []string
	if err := json.Unmarshal(body, &rooms); err != nil {
		_ = os.Remove(path)
		return
	}
	for _, roomID := range rooms {
		room := &ndsRoomSession{roomID: roomID, state: ndsRoomClosed, reason: ndsCloseError}
		h.emitNDSWebhook("room.closed", room, map[string]any{"reason": ndsCloseError})
	}
	_ = os.Remove(path)
}

func ndsJournalPath() string {
	if path := firstNonEmptyEnv("NDS_ROOM_JOURNAL"); path != "" {
		return path
	}
	runtimeDir := firstNonEmptyEnv("RUNTIME_DIR")
	if runtimeDir == "" {
		return ""
	}
	return filepath.Join(runtimeDir, "room-journal.json")
}
