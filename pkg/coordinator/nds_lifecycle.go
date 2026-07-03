package coordinator

import (
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
)

const (
	ndsCloseHost        = "host_close"
	ndsCloseIdle        = "idle"
	ndsCloseJoinTimeout = "join_timeout"
	ndsCloseMaxDuration = "max_duration"
	ndsCloseError       = "error"
)

func (h *Hub) startNDSRoomLifecycle(room *ndsRoomSession) {
	room.mu.Lock()
	if room.joinTimer != nil {
		room.joinTimer.Stop()
	}
	joinDelay := time.Until(room.joinDeadline)
	if joinDelay < 0 {
		joinDelay = 0
	}
	room.joinTimer = time.AfterFunc(joinDelay, func() {
		h.closeNDSRoom(room.roomID, ndsCloseJoinTimeout)
	})
	room.mu.Unlock()
	h.writeNDSJournal()
}

func (h *Hub) markNDSPlayerConnected(user *User) {
	if user == nil || user.nds == nil {
		return
	}
	room := h.ndsRooms.live(user.nds.RoomID)
	if room == nil {
		return
	}
	now := time.Now().UTC()
	firstStart := false
	room.mu.Lock()
	if room.state == ndsRoomReady || room.state == ndsRoomProvisioning {
		room.state = ndsRoomActive
		room.startedAt = &now
		firstStart = true
		if room.joinTimer != nil {
			room.joinTimer.Stop()
			room.joinTimer = nil
		}
		if room.maxDuration > 0 && room.maxTimer == nil {
			room.maxTimer = time.AfterFunc(room.maxDuration, func() {
				h.closeNDSRoom(room.roomID, ndsCloseMaxDuration)
			})
		}
	}
	if room.idleTimer != nil {
		room.idleTimer.Stop()
		room.idleTimer = nil
	}
	room.updatedAt = now
	room.mu.Unlock()

	h.emitNDSWebhook("player.connected", room, map[string]any{"player": user.nds.Player, "ref": user.nds.Ref})
	if firstStart {
		h.emitNDSWebhook("room.started", room, nil)
	}
	h.writeNDSJournal()
}

func (h *Hub) markNDSPlayerDisconnected(user *User) {
	if user == nil || user.nds == nil {
		return
	}
	room := h.ndsRooms.live(user.nds.RoomID)
	if room == nil {
		return
	}
	startIdle := false
	room.mu.Lock()
	if room.state == ndsRoomActive && room.allDisconnectedLocked() && room.idleTimeout > 0 && room.idleTimer == nil {
		startIdle = true
		room.idleTimer = time.AfterFunc(room.idleTimeout, func() {
			h.closeNDSRoom(room.roomID, ndsCloseIdle)
		})
	}
	room.mu.Unlock()

	h.emitNDSWebhook("player.disconnected", room, map[string]any{"player": user.nds.Player, "ref": user.nds.Ref})
	if startIdle {
		h.writeNDSJournal()
	}
}

func (h *Hub) closeNDSRoom(roomID string, reason string) *ndsRoomSession {
	room := h.ndsRooms.get(roomID)
	if room == nil {
		return nil
	}
	now := time.Now().UTC()
	room.mu.Lock()
	if room.state == ndsRoomClosed {
		room.mu.Unlock()
		return room
	}
	room.state = ndsRoomClosing
	room.reason = reason
	room.stopTimersLocked()
	users := make([]*User, 0, len(room.players))
	for _, seat := range room.players {
		if seat.user != nil {
			users = append(users, seat.user)
		}
	}
	seats := room.sortedSeats()
	room.mu.Unlock()

	for _, seat := range seats {
		status := api.NDSSaveStatus{
			Player: seat.player,
			Ref:    seat.ref,
			RoomID: seat.roomID,
			Status: "unchanged",
		}
		if seat.worker != nil {
			resp, err := seat.worker.FlushNDSSave(seat.roomID)
			if err != nil || resp == nil {
				status.Status = "failed"
			} else {
				status = *resp
				if status.Player == 0 {
					status.Player = seat.player
				}
				if status.Ref == "" {
					status.Ref = seat.ref
				}
				if status.RoomID == "" {
					status.RoomID = seat.roomID
				}
				if status.Status == "" {
					status.Status = "unchanged"
				}
			}
		}
		h.recordNDSSaveStatus(status)
	}

	for _, user := range users {
		user.Disconnect()
	}
	releaseNDSReservations(room.reserved)

	room.mu.Lock()
	room.state = ndsRoomClosed
	room.closedAt = &now
	room.updatedAt = now
	room.mu.Unlock()

	h.emitNDSWebhook("room.closed", room, h.ndsRoomClosedExtra(room, reason))
	h.writeNDSJournal()
	return room
}

func (h *Hub) closeAllNDSRooms(reason string) {
	if h.ndsRooms == nil {
		return
	}
	var roomIDs []string
	h.ndsRooms.mu.RLock()
	for roomID, room := range h.ndsRooms.rooms {
		if room.state != ndsRoomClosed && room.state != ndsRoomFailed {
			roomIDs = append(roomIDs, roomID)
		}
	}
	h.ndsRooms.mu.RUnlock()
	for _, roomID := range roomIDs {
		h.closeNDSRoom(roomID, reason)
	}
}

func (h *Hub) recordNDSSaveStatus(status api.NDSSaveStatus) {
	roomID := ndsRoomBaseFromWorkerRoom(status.RoomID)
	if roomID == "" {
		return
	}
	room := h.ndsRooms.get(roomID)
	if room == nil {
		return
	}

	var seat *ndsSeat
	room.mu.Lock()
	if status.Player > 0 {
		seat = room.players[status.Player]
	}
	if seat == nil {
		for _, candidate := range room.players {
			if candidate.roomID == status.RoomID {
				seat = candidate
				break
			}
		}
	}
	if seat == nil {
		room.mu.Unlock()
		return
	}
	if status.Status == "" {
		status.Status = "unchanged"
	}
	if status.Status == "unchanged" && seat.saveStatus == "uploaded" {
		status.Status = "uploaded"
		status.SHA1 = seat.saveSHA1
		status.Size = seat.saveSize
	}
	seat.saveStatus = status.Status
	seat.saveSHA1 = status.SHA1
	seat.saveSize = status.Size
	if !status.FlushedAt.IsZero() {
		flushedAt := status.FlushedAt
		seat.lastSaveAt = &flushedAt
	}
	if status.Player == 0 {
		status.Player = seat.player
	}
	if status.Ref == "" {
		status.Ref = seat.ref
	}
	room.updatedAt = time.Now().UTC()
	room.mu.Unlock()

	if status.Status == "uploaded" && status.SHA1 != "" {
		h.emitNDSWebhook("save.uploaded", room, map[string]any{
			"flushed_at": status.FlushedAt,
			"player":     status.Player,
			"ref":        status.Ref,
			"sha1":       status.SHA1,
			"size":       status.Size,
		})
	}
}

func (h *Hub) ndsRoomClosedExtra(room *ndsRoomSession, reason string) map[string]any {
	room.mu.Lock()
	defer room.mu.Unlock()

	players := make([]map[string]any, 0, len(room.players))
	for _, seat := range room.sortedSeats() {
		status := seat.saveStatus
		if status == "" {
			status = "unchanged"
		}
		players = append(players, map[string]any{
			"player":      seat.player,
			"ref":         seat.ref,
			"save_status": status,
		})
	}
	durationSec := int64(0)
	if room.startedAt != nil && room.closedAt != nil {
		durationSec = int64(room.closedAt.Sub(*room.startedAt).Seconds())
	}
	return map[string]any{
		"duration_sec": durationSec,
		"players":      players,
		"reason":       reason,
	}
}
