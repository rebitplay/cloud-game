package coordinator

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var ndsMetricsHub atomic.Pointer[Hub]
var ndsMetricsOnce sync.Once

func registerNDSMetrics(h *Hub) {
	ndsMetricsHub.Store(h)
	ndsMetricsOnce.Do(func() {
		for _, state := range []string{
			ndsRoomProvisioning,
			ndsRoomReady,
			ndsRoomActive,
			ndsRoomClosing,
			ndsRoomClosed,
			ndsRoomFailed,
		} {
			state := state
			metrics.GetOrCreateGauge(fmt.Sprintf(`cloud_game_nds_rooms{state="%s"}`, state), func() float64 {
				hub := ndsMetricsHub.Load()
				if hub == nil {
					return 0
				}
				return float64(hub.countNDSRoomsByState(state))
			})
		}

		metrics.GetOrCreateGauge("cloud_game_nds_seats_connected", func() float64 {
			hub := ndsMetricsHub.Load()
			if hub == nil {
				return 0
			}
			return float64(hub.countNDSConnectedSeats())
		})
	})
}

func (h *Hub) countNDSRoomsByState(state string) int {
	if h == nil || h.ndsRooms == nil {
		return 0
	}
	count := 0
	h.ndsRooms.mu.RLock()
	defer h.ndsRooms.mu.RUnlock()
	for _, room := range h.ndsRooms.rooms {
		room.mu.Lock()
		if room.state == state {
			count++
		}
		room.mu.Unlock()
	}
	return count
}

func (h *Hub) countNDSConnectedSeats() int {
	if h == nil || h.ndsRooms == nil {
		return 0
	}
	count := 0
	h.ndsRooms.mu.RLock()
	defer h.ndsRooms.mu.RUnlock()
	for _, room := range h.ndsRooms.rooms {
		room.mu.Lock()
		for _, seat := range room.players {
			if seat.connected {
				count++
			}
		}
		room.mu.Unlock()
	}
	return count
}

func (h *Hub) auditNDSRoomCreate(reqPlayers int, room *ndsRoomSession, status int, duration time.Duration) {
	if h == nil || room == nil {
		return
	}
	room.mu.Lock()
	refs := make([]string, 0, len(room.players))
	for _, seat := range room.sortedSeats() {
		refs = append(refs, seat.ref)
	}
	roomID := room.roomID
	game := room.game
	groupID := room.groupID
	state := room.state
	room.mu.Unlock()

	h.log.Info().
		Str("audit", "nds_room_create").
		Str("room", roomID).
		Str("game", game).
		Str("group", groupID).
		Str("state", state).
		Strs("refs", refs).
		Int("players", reqPlayers).
		Int("status", status).
		Dur("create_latency", duration).
		Msg("NDS room audit create")
}

func (h *Hub) auditNDSRoomClose(room *ndsRoomSession, reason string) {
	if h == nil || room == nil {
		return
	}
	room.mu.Lock()
	refs := make([]string, 0, len(room.players))
	saveBytes := int64(0)
	for _, seat := range room.sortedSeats() {
		refs = append(refs, seat.ref)
		saveBytes += int64(seat.saveSize)
	}
	roomID := room.roomID
	game := room.game
	groupID := room.groupID
	var duration time.Duration
	if room.startedAt != nil && room.closedAt != nil {
		duration = room.closedAt.Sub(*room.startedAt)
	}
	bytesStreamed := room.streamedBytes
	room.mu.Unlock()

	h.log.Info().
		Str("audit", "nds_room_close").
		Str("room", roomID).
		Str("game", game).
		Str("group", groupID).
		Str("reason", reason).
		Strs("refs", refs).
		Int64("duration_sec", int64(duration.Seconds())).
		Int64("bytes_streamed", bytesStreamed).
		Int64("save_bytes", saveBytes).
		Msg("NDS room audit close")
}
