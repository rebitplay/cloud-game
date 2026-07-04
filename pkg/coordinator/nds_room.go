package coordinator

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/config"
)

const (
	ndsRoomProvisioning = "provisioning"
	ndsRoomReady        = "ready"
	ndsRoomActive       = "active"
	ndsRoomClosing      = "closing"
	ndsRoomClosed       = "closed"
	ndsRoomFailed       = "failed"
)

type ndsRoomRegistry struct {
	mu    sync.RWMutex
	rooms map[string]*ndsRoomSession
}

type ndsRoomSession struct {
	mu             sync.Mutex
	closedAt       *time.Time
	closingStarted bool
	createdAt      time.Time
	idleTimeout    time.Duration
	endpoint       string
	game           string
	groupID        string
	idleTimer      *time.Timer
	joinDeadline   time.Time
	joinTimer      *time.Timer
	maxDuration    time.Duration
	maxTimer       *time.Timer
	players        map[int]*ndsSeat
	reason         string
	reserved       []reservedNDSWorker
	roomID         string
	romPath        string
	startedAt      *time.Time
	state          string
	streamedBytes  int64
	updatedAt      time.Time
}

type ndsSeat struct {
	connected       bool
	connectedAt     *time.Time
	lastSaveAt      *time.Time
	lastSeen        *time.Time
	player          int
	ref             string
	roomID          string
	saveURL         string
	saveWebhookSHA1 string
	saveSHA1        string
	saveSize        int
	saveStatus      string
	saveUploadURL   string
	user            *User
	worker          *Worker
}

type ndsUserSession struct {
	Player int
	Ref    string
	RoomID string
	Seat   *ndsSeat
}

func newNDSRoomRegistry() *ndsRoomRegistry {
	return &ndsRoomRegistry{rooms: make(map[string]*ndsRoomSession)}
}

func (r *ndsRoomRegistry) get(roomID string) *ndsRoomSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[roomID]
}

func (r *ndsRoomRegistry) put(room *ndsRoomSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rooms[room.roomID] = room
}

func (r *ndsRoomRegistry) live(roomID string) *ndsRoomSession {
	room := r.get(roomID)
	if room == nil || room.state == ndsRoomClosed || room.state == ndsRoomFailed {
		return nil
	}
	return room
}

func (r *ndsRoomRegistry) close(roomID string, now time.Time) *ndsRoomSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[roomID]
	if room == nil {
		return nil
	}
	if room.state != ndsRoomClosed {
		room.state = ndsRoomClosed
		room.closedAt = &now
		room.updatedAt = now
	}
	return room
}

func (s *ndsRoomSession) response(ice []config.IceServer, now time.Time) (api.NDSRoomV1Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	players := make([]api.NDSPlayerJoinInfo, 0, len(s.players))
	for _, seat := range s.sortedSeats() {
		token, err := makeNDSSeatToken(s.roomID, seat.player, seat.ref, now)
		if err != nil {
			return api.NDSRoomV1Response{}, err
		}
		players = append(players, api.NDSPlayerJoinInfo{
			IceServers:   ndsIceServers(ice, s.roomID, seat.player),
			Player:       seat.player,
			Ref:          seat.ref,
			SignalingURL: ndsSignalingURL(s.endpoint, token),
			Token:        token,
		})
	}
	return api.NDSRoomV1Response{
		Endpoint:     s.endpoint,
		Game:         s.game,
		JoinDeadline: s.joinDeadline,
		Players:      players,
		RoomID:       s.roomID,
		State:        s.state,
	}, nil
}

func (s *ndsRoomSession) tokenResponse(player int, ice []config.IceServer, now time.Time) (api.NDSPlayerJoinInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seat := s.players[player]
	if seat == nil {
		return api.NDSPlayerJoinInfo{}, false, nil
	}
	token, err := makeNDSSeatToken(s.roomID, seat.player, seat.ref, now)
	if err != nil {
		return api.NDSPlayerJoinInfo{}, true, err
	}
	return api.NDSPlayerJoinInfo{
		IceServers:   ndsIceServers(ice, s.roomID, seat.player),
		Player:       seat.player,
		Ref:          seat.ref,
		SignalingURL: ndsSignalingURL(s.endpoint, token),
		Token:        token,
	}, true, nil
}

func (s *ndsRoomSession) stateResponse() api.NDSRoomStateResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	players := make([]api.NDSPlayerState, 0, len(s.players))
	for _, seat := range s.sortedSeats() {
		players = append(players, api.NDSPlayerState{
			Connected:   seat.connected,
			ConnectedAt: seat.connectedAt,
			LastSaveAt:  seat.lastSaveAt,
			LastSeen:    seat.lastSeen,
			Player:      seat.player,
			Ref:         seat.ref,
			SaveStatus:  seat.saveStatus,
		})
	}
	return api.NDSRoomStateResponse{
		ClosedAt:  s.closedAt,
		CreatedAt: s.createdAt,
		Endpoint:  s.endpoint,
		Game:      s.game,
		Players:   players,
		RoomID:    s.roomID,
		StartedAt: s.startedAt,
		State:     s.state,
		UpdatedAt: s.updatedAt,
	}
}

func (s *ndsRoomSession) allDisconnectedLocked() bool {
	for _, seat := range s.players {
		if seat.connected {
			return false
		}
	}
	return true
}

func (s *ndsRoomSession) stopTimersLocked() {
	if s.joinTimer != nil {
		s.joinTimer.Stop()
		s.joinTimer = nil
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	if s.maxTimer != nil {
		s.maxTimer.Stop()
		s.maxTimer = nil
	}
}

func (s *ndsRoomSession) sortedSeats() []*ndsSeat {
	players := make([]int, 0, len(s.players))
	for player := range s.players {
		players = append(players, player)
	}
	sort.Ints(players)
	seats := make([]*ndsSeat, 0, len(players))
	for _, player := range players {
		seats = append(seats, s.players[player])
	}
	return seats
}

func ndsSignalingURL(endpoint string, token string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u = &url.URL{Scheme: "https", Host: endpoint}
	}
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else {
		u.Scheme = "wss"
	}
	u.Path = "/ws"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}

func ndsIceServers(base []config.IceServer, roomID string, player int) []api.IceServer {
	servers := make([]api.IceServer, 0, len(base)+2)
	for _, server := range base {
		servers = append(servers, api.IceServer{
			Urls:       server.Urls,
			Username:   server.Username,
			Credential: server.Credential,
		})
	}

	urls := firstNonEmptyEnv("NDS_TURN_URLS", "NDS_TURN_URL")
	secret := firstNonEmptyEnv("NDS_TURN_SECRET", "NDS_TURN_SHARED_SECRET")
	if urls == "" || secret == "" {
		return servers
	}
	username := strconv.FormatInt(time.Now().Add(ndsTokenTTL).Unix(), 10) + ":" + roomID + ":" + strconv.Itoa(player)
	credential := base64HMACSHA1(secret, username)
	for _, raw := range strings.Split(urls, ",") {
		url := strings.TrimSpace(raw)
		if url == "" {
			continue
		}
		servers = append(servers, api.IceServer{Urls: url, Username: username, Credential: credential})
	}
	return servers
}
