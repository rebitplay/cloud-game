package coordinator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

type fakeNDSConnection struct {
	id        com.Uid
	lastStart *api.StartGameRequest
}

func (f *fakeNDSConnection) Disconnect()        {}
func (f *fakeNDSConnection) Id() com.Uid        { return f.id }
func (f *fakeNDSConnection) Notify(api.PT, any) {}
func (f *fakeNDSConnection) ProcessPackets(func(api.In[com.Uid]) error) chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (f *fakeNDSConnection) Send(t api.PT, payload any) ([]byte, error) {
	switch t {
	case api.NDSRomInstall:
		req := payload.(api.NDSRomInstallRequest)
		return json.Marshal(api.NDSRomInstallResponse{Game: "Tetris DS", Path: "nds/" + req.FileName})
	case api.NDSSessionPrepare:
		return json.Marshal(api.OK)
	case api.StartGame:
		req := payload.(api.StartGameRequest)
		f.lastStart = &req
		return json.Marshal(api.StartGameResponse{
			Room:    api.Room{Rid: req.Rid},
			Pointer: true,
		})
	default:
		return json.Marshal(api.OK)
	}
}

func TestNDSV1AuthAndHealthz(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")

	h := testNDSHub(t, 1)

	health := httptest.NewRecorder()
	h.handleHealthz()(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("healthz status = %d, want %d", health.Code, http.StatusNoContent)
	}

	noAuth := httptest.NewRecorder()
	h.requireNDSAPIKey(h.handleNDSCapacity())(noAuth, httptest.NewRequest(http.MethodGet, "/v1/capacity", nil))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("capacity without auth status = %d, want %d", noAuth.Code, http.StatusUnauthorized)
	}

	withAuth := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capacity", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	h.requireNDSAPIKey(h.handleNDSCapacity())(withAuth, req)
	if withAuth.Code != http.StatusOK {
		t.Fatalf("capacity with auth status = %d, want %d; body=%s", withAuth.Code, http.StatusOK, withAuth.Body.String())
	}
}

func TestNDSV1CreateIsIdempotentAndUsesPublicEndpoint(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")

	h := testNDSHub(t, 1)
	body := testNDSCreateBody("room-123", 3)

	first := postNDSRoom(t, h, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d; body=%s", first.Code, http.StatusCreated, first.Body.String())
	}
	var firstResp api.NDSRoomV1Response
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatal(err)
	}
	if firstResp.Endpoint != "https://sg-1.nds.rebitplay.com" {
		t.Fatalf("endpoint = %q", firstResp.Endpoint)
	}
	if len(firstResp.Players) != 3 {
		t.Fatalf("players = %d, want 3", len(firstResp.Players))
	}
	if !strings.HasPrefix(firstResp.Players[0].SignalingURL, "wss://sg-1.nds.rebitplay.com/ws?") {
		t.Fatalf("signaling_url not built from NDS_PUBLIC_ENDPOINT: %q", firstResp.Players[0].SignalingURL)
	}

	second := postNDSRoom(t, h, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second create status = %d, want %d; body=%s", second.Code, http.StatusOK, second.Body.String())
	}
	var secondResp api.NDSRoomV1Response
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatal(err)
	}
	if secondResp.RoomID != firstResp.RoomID || secondResp.Players[0].Token == firstResp.Players[0].Token {
		t.Fatalf("idempotent replay should keep room and mint fresh token")
	}
}

func TestNDSV1CreateValidationAndCapacityErrors(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")

	h := testNDSHub(t, 0)
	tooManyPlayers := postNDSRoom(t, h, testNDSCreateBody("room-123", 5))
	if tooManyPlayers.Code != http.StatusBadRequest {
		t.Fatalf("bad players status = %d, want %d", tooManyPlayers.Code, http.StatusBadRequest)
	}

	noCapacity := postNDSRoom(t, h, testNDSCreateBody("room-124", 2))
	if noCapacity.Code != http.StatusServiceUnavailable {
		t.Fatalf("no capacity status = %d, want %d; body=%s", noCapacity.Code, http.StatusServiceUnavailable, noCapacity.Body.String())
	}
	var errResp api.NDSAPIError
	if err := json.Unmarshal(noCapacity.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Code != "no_capacity" || errResp.RetryAfterSec == 0 {
		t.Fatalf("unexpected no_capacity body: %#v", errResp)
	}
}

func TestNDSSeatTokenValidation(t *testing.T) {
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	now := time.Now().UTC()
	token, err := makeNDSSeatToken("room-123", 2, "user_2", now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := validateNDSSeatToken(token, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.RID != "room-123" || claims.P != 2 || claims.Ref != "user_2" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	if _, err := validateNDSSeatToken(token, now.Add(ndsTokenTTL+time.Second)); err == nil {
		t.Fatal("expired token validated successfully")
	}
}

func TestResolveNDSUserSessionRejectsLegacyJoinForV1Room(t *testing.T) {
	h := testNDSHub(t, 1)
	h.ndsRooms.put(&ndsRoomSession{
		roomID: "room-123",
		state:  ndsRoomReady,
		players: map[int]*ndsSeat{
			1: {player: 1, ref: "user_1"},
		},
	})

	if session, err := h.resolveNDSUserSession(url.Values{api.RoomIdQueryParam: []string{"room-123-p1___Tetris DS"}}); err == nil || session != nil {
		t.Fatalf("legacy join resolved session=%#v err=%v, want token-required error", session, err)
	}
}

func TestAttachNDSUserTakesOverSeat(t *testing.T) {
	h := testNDSHub(t, 0)
	seat := &ndsSeat{player: 1, ref: "user_1"}
	h.ndsRooms.put(&ndsRoomSession{
		roomID:  "room-123",
		state:   ndsRoomReady,
		players: map[int]*ndsSeat{1: seat},
	})

	oldUser := &User{Connection: &fakeNDSConnection{id: com.NewUid()}, nds: &ndsUserSession{RoomID: "room-123", Player: 1, Ref: "user_1", Seat: seat}}
	newUser := &User{Connection: &fakeNDSConnection{id: com.NewUid()}, nds: &ndsUserSession{RoomID: "room-123", Player: 1, Ref: "user_1", Seat: seat}}
	h.attachNDSUser(oldUser)
	h.users.Add(oldUser)
	h.attachNDSUser(newUser)

	if seat.user != newUser || !seat.connected {
		t.Fatalf("seat was not taken over by new user")
	}
	if h.users.Find(oldUser.Id().String()) != nil {
		t.Fatalf("old user still present after takeover")
	}
}

func TestNDSGameStartIgnoresForgedClientFields(t *testing.T) {
	conn := &fakeNDSConnection{id: com.NewUid()}
	worker := &Worker{Connection: conn}
	internalRoomID := "room-123-p2___Tetris DS"
	if !worker.ReserveRoom(internalRoomID) {
		t.Fatal("reserve worker")
	}
	seat := &ndsSeat{player: 2, ref: "user_2", roomID: internalRoomID, worker: worker}
	user := &User{
		Connection: &fakeNDSConnection{id: com.NewUid()},
		nds:        &ndsUserSession{RoomID: "room-123", Player: 2, Ref: "user_2", Seat: seat},
		log:        logger.NewConsole(false, "test", false),
		w:          worker,
	}

	user.HandleStartGame(api.GameStartUserRequest{
		GameName:    "Forged Game",
		RoomId:      "other-room-p4___Other Game",
		PlayerIndex: 4,
	}, config.CoordinatorConfig{})

	if conn.lastStart == nil {
		t.Fatal("worker did not receive StartGame")
	}
	if conn.lastStart.Rid != internalRoomID || conn.lastStart.PlayerIndex != 2 || conn.lastStart.Game != "" {
		t.Fatalf("start request used forged fields: %#v", conn.lastStart)
	}
}

func testNDSHub(t *testing.T, groups int) *Hub {
	t.Helper()
	h := NewHub(config.CoordinatorConfig{}, logger.NewConsole(false, "test", false))
	for group := 1; group <= groups; group++ {
		groupID := "mkds-r" + strconv.Itoa(group)
		for player := 1; player <= 4; player++ {
			worker := &Worker{
				Connection: &fakeNDSConnection{id: com.NewUid()},
				NDSGroup:   groupID,
				NDSPlayer:  player,
				Zone:       groupID + "-p" + strconv.Itoa(player),
			}
			h.workers.Add(worker)
		}
	}
	return h
}

func testNDSCreateBody(room string, players int) []byte {
	req := api.NDSRoomCreateRequest{
		Room:    room,
		Players: players,
		Rom: api.NDSRomCreate{
			Name: "Tetris DS (USA).nds",
			SHA1: "0123456789abcdef0123456789abcdef01234567",
			URL:  "https://cdn.rebitplay.com/roms/tetris.nds?token=x",
		},
	}
	for player := 1; player <= players && player <= 4; player++ {
		req.PlayerSlots = append(req.PlayerSlots, api.NDSPlayerSlotCreate{
			Player:        player,
			Ref:           "user_" + strconv.Itoa(player),
			SaveUploadURL: "https://cdn.rebitplay.com/saves/u" + strconv.Itoa(player) + ".srm?token=x",
		})
	}
	body, _ := json.Marshal(req)
	return body
}

func postNDSRoom(t *testing.T, h *Hub, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", bytes.NewReader(body))
	req.Host = "attacker.example"
	req.Header.Set("Authorization", "Bearer test-key")
	h.requireNDSAPIKey(h.handleNDSRooms())(rr, req)
	return rr
}
