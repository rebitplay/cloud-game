package coordinator

import (
	"fmt"
	"net/url"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/monitoring"
)

func (h *Hub) resolveNDSUserSession(q url.Values) (*ndsUserSession, error) {
	token := q.Get("token")
	if token == "" {
		roomID := q.Get(api.RoomIdQueryParam)
		if h.ndsRooms.live(ndsRoomBaseFromWorkerRoom(roomID)) != nil {
			monitoring.IncNDSTokenFailure("missing")
			return nil, fmt.Errorf("token required for NDS room")
		}
		return nil, nil
	}

	claims, err := validateNDSSeatToken(token, time.Now().UTC())
	if err != nil {
		monitoring.IncNDSTokenFailure("invalid")
		return nil, err
	}
	room := h.ndsRooms.live(claims.RID)
	if room == nil {
		monitoring.IncNDSTokenFailure("room_not_found")
		return nil, fmt.Errorf("room not found")
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	seat := room.players[claims.P]
	if seat == nil || seat.ref != claims.Ref {
		monitoring.IncNDSTokenFailure("seat_not_found")
		return nil, fmt.Errorf("seat not found")
	}
	return &ndsUserSession{RoomID: claims.RID, Player: claims.P, Ref: claims.Ref, Seat: seat, ExpiresAt: time.Unix(claims.Exp, 0)}, nil
}

func (h *Hub) attachNDSUser(user *User) {
	if user == nil || user.nds == nil || user.nds.Seat == nil {
		return
	}
	room := h.ndsRooms.live(user.nds.RoomID)
	if room == nil {
		return
	}

	now := time.Now().UTC()
	var old *User
	room.mu.Lock()
	seat := user.nds.Seat
	old = seat.user
	seat.user = user
	seat.connected = true
	seat.connectedAt = &now
	seat.lastSeen = &now
	room.updatedAt = now
	room.mu.Unlock()

	if old != nil && old != user {
		old.Notify(api.ErrNoFreeSlots, "")
		old.Disconnect()
		h.users.Remove(old)
	}
	h.markNDSPlayerConnected(user)
}

func (h *Hub) detachNDSUser(user *User) {
	if user == nil || user.nds == nil || user.nds.Seat == nil {
		return
	}
	room := h.ndsRooms.live(user.nds.RoomID)
	if room == nil {
		return
	}
	now := time.Now().UTC()
	room.mu.Lock()
	seat := user.nds.Seat
	if seat.user == user {
		seat.user = nil
		seat.connected = false
		seat.lastSeen = &now
		room.updatedAt = now
	}
	room.mu.Unlock()
	h.markNDSPlayerDisconnected(user)
}
