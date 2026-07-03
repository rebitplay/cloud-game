package coordinator

import (
	"time"
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
	room.mu.Unlock()

	for _, user := range users {
		user.Disconnect()
	}
	releaseNDSReservations(room.reserved)

	room.mu.Lock()
	room.state = ndsRoomClosed
	room.closedAt = &now
	room.updatedAt = now
	room.mu.Unlock()

	h.emitNDSWebhook("room.closed", room, map[string]any{"reason": reason})
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
