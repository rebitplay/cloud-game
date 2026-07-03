package coordinator

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

type Worker struct {
	AppLibrary
	Connection
	RegionalClient
	Session
	slotted

	Addr           string
	NDSGroup       string
	NDSPlayer      int
	PingServer     string
	Port           string
	ReservedRoomId string
	RoomId         string // room reference
	Tag            string
	WebRTCPort     int
	Zone           string

	Lib      []api.GameInfo
	Sessions map[string]struct{}

	hub    *Hub
	log    *logger.Logger
	rtcMux *webRTCMux
}

type RegionalClient interface {
	In(region string) bool
}

type HasUserRegistry interface {
	Find(id string) *User
}

type AppLibrary interface {
	SetLib([]api.GameInfo)
	AppNames() []api.GameInfo
}

type Session interface {
	AddSession(id string)
	// HadSession is true when an old session is found
	HadSession(id string) bool
	SetSessions(map[string]struct{})
}

type AppMeta struct {
	Alias  string
	Base   string
	Name   string
	Path   string
	System string
	Type   string
}

func NewWorker(sock *com.Connection, handshake api.ConnectionRequest[com.Uid], log *logger.Logger, rtcMux *webRTCMux, hub *Hub) *Worker {
	conn := com.NewConnection[api.PT, api.In[com.Uid], api.Out, *api.Out](sock, handshake.Id, log)
	ndsPlayer := handshake.NDSPlayer
	if ndsPlayer == 0 {
		ndsPlayer = inferNDSPlayer(handshake.Tag, handshake.Zone)
	}
	ndsGroup := handshake.NDSGroup
	if ndsGroup == "" {
		ndsGroup = inferNDSGroup(handshake.Tag, handshake.Zone)
	}
	return &Worker{
		Connection: conn,
		Addr:       handshake.Addr,
		NDSGroup:   ndsGroup,
		NDSPlayer:  ndsPlayer,
		PingServer: handshake.PingURL,
		Port:       handshake.Port,
		Tag:        handshake.Tag,
		WebRTCPort: handshake.WebRTCPort,
		Zone:       handshake.Zone,
		hub:        hub,
		log: log.Extend(log.With().
			Str(logger.ClientField, logger.MarkNone).
			Str(logger.DirectionField, logger.MarkNone).
			Str("cid", conn.Id().Short())),
		rtcMux: rtcMux,
	}
}

func (w *Worker) HandleRequests(users HasUserRegistry) chan struct{} {
	return w.ProcessPackets(func(p api.In[com.Uid]) (err error) {
		switch p.T {
		case api.RegisterRoom:
			err = api.Do(p, func(d api.RegisterRoomRequest) {
				w.log.Info().Msgf("set room [%v] = %v", w.Id(), d)
				w.HandleRegisterRoom(d)
			})
		case api.CloseRoom:
			err = api.Do(p, w.HandleCloseRoom)
		case api.WebrtcSignal:
			err = api.DoE(p, func(rq api.WebrtcSignalRequest) error {
				if rq.Ice == nil {
					return fmt.Errorf("ice candidate is missing")
				}
				return w.HandleIceCandidate(rq, users)
			})
		case api.LibNewGameList:
			err = api.DoE(p, w.HandleLibGameList)
		case api.PrevSessions:
			err = api.DoE(p, w.HandlePrevSessionList)
		case api.NDSSaveUploaded:
			err = api.DoE(p, w.HandleNDSSaveUploaded)
		default:
			w.log.Warn().Msgf("Unknown packet: %+v", p)
		}
		if err != nil && !errors.Is(err, api.ErrMalformed) {
			w.log.Error().Err(err).Send()
			err = api.ErrMalformed
		}
		return
	})
}

func (w *Worker) SetLib(list []api.GameInfo) { w.Lib = list }

func (w *Worker) AppNames() []api.GameInfo {
	return w.Lib
}

func (w *Worker) AddSession(id string) {
	// sessions can be uninitialized until the coordinator pushes them to the worker
	if w.Sessions == nil {
		return
	}

	w.Sessions[id] = struct{}{}
}

func (w *Worker) HadSession(id string) bool {
	_, ok := w.Sessions[id]
	return ok
}

func (w *Worker) SetSessions(sessions map[string]struct{}) {
	w.Sessions = sessions
}

// In say whether some worker from this region (zone).
// Empty region always returns true.
func (w *Worker) In(region string) bool { return region == "" || region == w.Zone }

// slotted used for tracking user slots and the availability.
type slotted int32

// HasSlot checks if the current worker has a free slot to start a new game.
// Workers support only one game at a time, so it returns true in case if
// there are no players in the room (worker).
func (s *slotted) HasSlot() bool { return atomic.LoadInt32((*int32)(s)) == 0 }

// TryReserve reserves the slot only when it's free.
func (s *slotted) TryReserve() bool {
	for {
		current := atomic.LoadInt32((*int32)(s))
		if current != 0 {
			return false
		}
		if atomic.CompareAndSwapInt32((*int32)(s), 0, 1) {
			return true
		}
	}
}

// UnReserve decrements user counter of the worker.
func (s *slotted) UnReserve() {
	for {
		current := atomic.LoadInt32((*int32)(s))
		if current <= 0 {
			// reset to zero
			if current < 0 {
				if atomic.CompareAndSwapInt32((*int32)(s), current, 0) {
					return
				}
				continue
			}

			return
		}

		// Regular decrement for positive values
		newVal := current - 1
		if atomic.CompareAndSwapInt32((*int32)(s), current, newVal) {
			return
		}
	}
}

func (s *slotted) FreeSlots() { atomic.StoreInt32((*int32)(s), 0) }

func (w *Worker) ReserveRoom(id string) bool {
	if !w.TryReserve() {
		return false
	}
	w.RoomId = id
	w.ReservedRoomId = id
	return true
}

func (w *Worker) ReleaseReservation(id string) bool {
	if w.ReservedRoomId != id {
		return false
	}
	w.ReservedRoomId = ""
	w.RoomId = ""
	w.FreeSlots()
	return true
}

func (w *Worker) Disconnect() {
	if w.rtcMux != nil {
		w.rtcMux.unregisterWorker(w)
	}
	w.Connection.Disconnect()
	w.RoomId = ""
	w.ReservedRoomId = ""
	w.FreeSlots()
}

func (w *Worker) PrintInfo() string {
	return fmt.Sprintf("id: %v, addr: %v, port: %v, webrtc port: %v, zone: %v, ping addr: %v, tag: %v, nds group: %v, nds player: %v",
		w.Id(), w.Addr, w.Port, w.WebRTCPort, w.Zone, w.PingServer, w.Tag, w.NDSGroup, w.NDSPlayer)
}

func inferNDSPlayer(names ...string) int {
	for _, name := range names {
		_, player, ok := splitNDSPlayerSuffix(name)
		if ok {
			return player
		}
	}
	return 0
}

func inferNDSGroup(tag string, zone string) string {
	for _, name := range []string{tag, zone} {
		group, _, ok := splitNDSPlayerSuffix(name)
		if ok && group != "" {
			return group
		}
	}
	return "default"
}

func splitNDSPlayerSuffix(name string) (string, int, bool) {
	idx := strings.LastIndex(name, "-p")
	if idx < 0 || idx == 0 || idx+2 >= len(name) {
		return "", 0, false
	}
	suffix := name[idx+2:]
	if strings.Contains(suffix, "-") {
		return "", 0, false
	}
	player, err := strconv.Atoi(suffix)
	if err != nil || player < 1 || player > 4 {
		return "", 0, false
	}
	return name[:idx], player, true
}
