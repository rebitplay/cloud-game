package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	"github.com/giongto35/cloud-game/v3/pkg/network/httpx"
)

type fakeNDSConnection struct {
	flushBlock    <-chan struct{}
	flushStarted  chan<- string
	flushStatus   *api.NDSSaveStatus
	id            com.Uid
	lastStart     *api.StartGameRequest
	romInstallErr error
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
		if f.romInstallErr != nil {
			return nil, f.romInstallErr
		}
		req := payload.(api.NDSRomInstallRequest)
		return json.Marshal(api.NDSRomInstallResponse{Game: "Tetris DS", Path: "nds/" + req.FileName})
	case api.NDSSessionPrepare:
		return json.Marshal(api.OK)
	case api.NDSFlushSave:
		if f.flushStarted != nil {
			req := payload.(api.NDSFlushSaveRequest)
			f.flushStarted <- req.RoomID
		}
		if f.flushBlock != nil {
			<-f.flushBlock
		}
		if f.flushStatus != nil {
			return json.Marshal(f.flushStatus)
		}
		req := payload.(api.NDSFlushSaveRequest)
		return json.Marshal(api.NDSSaveStatus{RoomID: req.RoomID, Status: "unchanged"})
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
	t.Setenv("NDS_API_KEYS", "next-key, fallback-key")

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

	for _, key := range []string{"test-key", "next-key", "fallback-key"} {
		withAuth := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/capacity", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		h.requireNDSAPIKey(h.handleNDSCapacity())(withAuth, req)
		if withAuth.Code != http.StatusOK {
			t.Fatalf("capacity with auth key %q status = %d, want %d; body=%s", key, withAuth.Code, http.StatusOK, withAuth.Body.String())
		}
	}

	badAuth := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capacity", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	h.requireNDSAPIKey(h.handleNDSCapacity())(badAuth, req)
	if badAuth.Code != http.StatusUnauthorized {
		t.Fatalf("capacity with wrong auth status = %d, want %d", badAuth.Code, http.StatusUnauthorized)
	}
}

func TestNDSV1RoutesRequireAuth(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	conf := config.CoordinatorConfig{}
	coordinator := &Coordinator{hub: NewHub(conf, logger.NewConsole(false, "test", false))}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/capacity"},
		{http.MethodPost, "/v1/rooms"},
		{http.MethodGet, "/v1/rooms/room-123"},
		{http.MethodDelete, "/v1/rooms/room-123"},
		{http.MethodPost, "/v1/rooms/room-123/players/1/token"},
	} {
		rec := httptest.NewRecorder()
		coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth status = %d, want %d; body=%s", tc.method, tc.path, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}
}

func TestNDSV1CapacityIncludesDynamicSpawnableRooms(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_MAX_ROOM_COUNT", "2")
	h := testNDSHub(t, 1)
	h.ndsSpawner = &ndsSpawner{
		enabled:         true,
		maxGroups:       2,
		playersPerGroup: 4,
		nextGroup:       2,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capacity", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	h.requireNDSAPIKey(h.handleNDSCapacity())(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capacity status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.NDSCapacityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalRooms != 2 || resp.FreeRooms != 2 || resp.ByPlayers["4"] != 2 {
		t.Fatalf("dynamic capacity not included: %#v", resp)
	}
}

func TestNDSSpawnerReleasedGroupRestoresCapacity(t *testing.T) {
	spawner := &ndsSpawner{
		enabled:   true,
		maxGroups: 2,
		nextGroup: 1,
	}

	groupID, err := spawner.allocateGroupID()
	if err != nil {
		t.Fatal(err)
	}
	if groupID != "mkds-r1" {
		t.Fatalf("allocated group = %q, want mkds-r1", groupID)
	}
	if remaining := spawner.remainingSpawnCapacity(); remaining != 1 {
		t.Fatalf("remaining after allocation = %d, want 1", remaining)
	}

	spawner.releaseGroupID(groupID)
	if remaining := spawner.remainingSpawnCapacity(); remaining != 2 {
		t.Fatalf("remaining after release = %d, want 2", remaining)
	}

	reused, err := spawner.allocateGroupID()
	if err != nil {
		t.Fatal(err)
	}
	if reused != "mkds-r1" {
		t.Fatalf("reused group = %q, want mkds-r1", reused)
	}

	spawner.releaseGroupID(reused)
	spawner.releaseGroupID(reused)
	if remaining := spawner.remainingSpawnCapacity(); remaining != 2 {
		t.Fatalf("duplicate release remaining = %d, want 2", remaining)
	}
}

func TestCoordinatorPublicMetricsRoute(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_PUBLIC_METRICS", "false")
	t.Setenv("CLOUD_GAME_COORDINATOR_PUBLIC_METRICS", "false")
	conf := config.CoordinatorConfig{}
	coordinator := &Coordinator{hub: NewHub(conf, logger.NewConsole(false, "test", false))}

	disabled := httptest.NewRecorder()
	coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("metrics status without opt-in = %d, want %d", disabled.Code, http.StatusNotFound)
	}

	t.Setenv("NDS_PUBLIC_METRICS", "true")
	noAuth := httptest.NewRecorder()
	coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(noAuth, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("metrics status without auth = %d, want %d", noAuth.Code, http.StatusUnauthorized)
	}

	enabled := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(enabled, req)
	if enabled.Code != http.StatusOK {
		t.Fatalf("metrics status with opt-in = %d, want %d; body=%s", enabled.Code, http.StatusOK, enabled.Body.String())
	}
	if !strings.Contains(enabled.Body.String(), "cloud_game_nds_rooms") {
		t.Fatalf("NDS metrics missing from /metrics body: %s", enabled.Body.String())
	}
}

func TestCoordinatorBuildzRouteReportsFrontendAndEnv(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TURN_URLS", "turn:turn.example.com:3478?transport=udp")
	t.Setenv("NDS_TURN_SECRET", "turn-secret")
	t.Setenv("WEBRTC_MUX_ENABLED", "true")
	t.Setenv("WEBRTC_PUBLIC_IP", "109.224.230.118")
	t.Setenv("WEBRTC_PUBLIC_PORT", "8641")
	t.Setenv("NDS_PUBLIC_METRICS", "true")
	BuildVersion = "test-version"
	t.Cleanup(func() { BuildVersion = "?" })

	conf := config.CoordinatorConfig{}
	coordinator := &Coordinator{hub: NewHub(conf, logger.NewConsole(false, "test", false))}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/buildz", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("buildz status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp buildzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Version != "test-version" {
		t.Fatalf("version = %q", resp.Version)
	}
	if resp.WebRTCAsset != "webrtc.js?v=10" {
		t.Fatalf("webrtc asset = %q, want v10; body=%s", resp.WebRTCAsset, rec.Body.String())
	}
	if !resp.WebRTCFirefoxRelayPolicy {
		t.Fatalf("Firefox relay policy not detected; body=%s", rec.Body.String())
	}
	if !resp.NDSRestTURNConfigured || !resp.WebRTCMuxEnabled || !resp.MetricsEnabled {
		t.Fatalf("expected deploy flags missing; body=%s", rec.Body.String())
	}
	if resp.WebRTCPublicIP != "109.224.230.118" || resp.WebRTCPublicPort != "8641" {
		t.Fatalf("public WebRTC fields wrong; body=%s", rec.Body.String())
	}

	noAuth := httptest.NewRecorder()
	coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(noAuth, httptest.NewRequest(http.MethodGet, "/buildz", nil))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("buildz without auth status = %d, want %d", noAuth.Code, http.StatusUnauthorized)
	}
}

func TestNDSDemoRouteIsNotRegistered(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	conf := config.CoordinatorConfig{}
	coordinator := &Coordinator{hub: NewHub(conf, logger.NewConsole(false, "test", false))}

	rec := httptest.NewRecorder()
	coordinator.registerRoutes(conf, httpx.NewServeMux("")).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/nds/rooms", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("demo route status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCoordinatorMetricsIncludeNDSRoomCreateDuration(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")

	h := testNDSHub(t, 1)
	create := postNDSRoom(t, h, testNDSCreateBody("room-metrics", 2))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body=%s", create.Code, http.StatusCreated, create.Body.String())
	}

	metrics := httptest.NewRecorder()
	h.handleCoordinatorMetrics()(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metrics.Body.String()
	if !strings.Contains(body, "cloud_game_nds_room_create_duration_seconds") || !strings.Contains(body, `status="201"`) {
		t.Fatalf("room create duration metric missing from /metrics body: %s", body)
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

func TestNDSV1CreateRejectsDisallowedRemoteHost(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")
	t.Setenv("NDS_DOWNLOAD_ALLOWED_HOSTS", "cdn.rebitplay.com")

	h := testNDSHub(t, 1)
	var req api.NDSRoomCreateRequest
	if err := json.Unmarshal(testNDSCreateBody("room-123", 2), &req); err != nil {
		t.Fatal(err)
	}
	req.Rom.URL = "https://evil.example/rom.nds"
	body, _ := json.Marshal(req)
	resp := postNDSRoom(t, h, body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("bad host status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	var errResp api.NDSAPIError
	if err := json.Unmarshal(resp.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Code != "invalid_rom_url" {
		t.Fatalf("bad host code = %q, want invalid_rom_url", errResp.Code)
	}
}

func TestNDSAPIRemoteURLSSRFGuard(t *testing.T) {
	oldLookup := ndsAPILookupIP
	t.Cleanup(func() { ndsAPILookupIP = oldLookup })
	ndsAPILookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) {
		switch host {
		case "cdn.rebitplay.com", "roms.b-cdn.net":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		case "private.rebitplay.com":
			return []net.IP{net.ParseIP("10.0.0.2")}, nil
		default:
			return nil, fmt.Errorf("unexpected host %s", host)
		}
	}
	t.Setenv("NDS_DOWNLOAD_ALLOWED_HOSTS", "*.b-cdn.net,cdn.rebitplay.com,private.rebitplay.com,10.0.0.1")

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{name: "allowlisted exact host", rawURL: "https://cdn.rebitplay.com/rom.nds"},
		{name: "allowlisted Bunny wildcard host", rawURL: "https://roms.b-cdn.net/rom.nds"},
		{name: "literal private IP rejected", rawURL: "https://10.0.0.1/rom.nds", wantErr: true},
		{name: "allowlisted host resolving private rejected", rawURL: "https://private.rebitplay.com/rom.nds", wantErr: true},
		{name: "disallowed host rejected", rawURL: "https://evil.example/rom.nds", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNDSAPIRemoteURL(tt.rawURL, "rom_url")
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNDSAPIRemoteURLAllowsPrivateWhenExplicitlyEnabled(t *testing.T) {
	oldLookup := ndsAPILookupIP
	t.Cleanup(func() { ndsAPILookupIP = oldLookup })
	ndsAPILookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) {
		if host != "host.containers.internal" {
			return nil, fmt.Errorf("unexpected host %s", host)
		}
		return []net.IP{net.ParseIP("169.254.1.2")}, nil
	}
	t.Setenv("NDS_DOWNLOAD_ALLOWED_HOSTS", "host.containers.internal")
	t.Setenv("NDS_ALLOW_PRIVATE_REMOTE_URLS", "true")

	if err := validateNDSAPIRemoteURL("http://host.containers.internal:18080/save.srm", "save_upload_url"); err != nil {
		t.Fatalf("expected private local test URL to be allowed: %v", err)
	}
}

func TestNDSV1AllowsBuiltinROMAndOptionalSaveUploadURL(t *testing.T) {
	req := api.NDSRoomCreateRequest{
		Room:    "room-123",
		Players: 2,
		Rom: api.NDSRomCreate{
			Name: "Tetris-DS-(USA).nds",
			SHA1: "13eb2e7e5357a6e31f94ea238826c111c965bc9b",
			URL:  "builtin:Tetris-DS-(USA).nds",
		},
		PlayerSlots: []api.NDSPlayerSlotCreate{
			{Player: 1, Ref: "user_1"},
			{Player: 2, Ref: "user_2"},
		},
	}
	if err := validateNDSRoomCreate(req); err != nil {
		t.Fatalf("builtin ROM with optional save upload rejected: %v", err)
	}

	req.PlayerSlots[0].SaveURL = "builtin:save.srm"
	err := validateNDSRoomCreate(req)
	var apiErr apiError
	if !errors.As(err, &apiErr) || apiErr.code != "invalid_save_url" {
		t.Fatalf("save URL should still require http(s), got %v", err)
	}
}

func TestNDSDemoCreateBuildsTokenizedStreamURLs(t *testing.T) {
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "http://127.0.0.1:8000")

	h := testNDSHub(t, 1)
	req := httptest.NewRequest(http.MethodPost, "/api/nds/rooms", nil)
	resp, status, err := h.createNDSDemoRoom(req, ndsDemoRoomCreateRequest{
		BaseURL: "http://play.example",
		Players: 3,
		RomURL:  "builtin:Tetris-DS-(USA).nds",
		Room:    "Tetris Demo!",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Room != "tetris-demo" {
		t.Fatalf("room = %q, want tetris-demo", resp.Room)
	}
	if len(resp.Players) != 3 {
		t.Fatalf("players = %d, want 3", len(resp.Players))
	}
	for _, player := range resp.Players {
		u, err := url.Parse(player.URL)
		if err != nil {
			t.Fatalf("stream URL parse: %v", err)
		}
		q := u.Query()
		if u.Scheme != "http" || u.Host != "play.example" || q.Get("token") == "" {
			t.Fatalf("stream URL missing base/token: %s", player.URL)
		}
		if q.Get("id") == "" || q.Get("view") != "stream" || q.Get("client") != "v9" {
			t.Fatalf("stream URL missing app params: %s", player.URL)
		}
		if q.Get("player") != strconv.Itoa(player.Player) {
			t.Fatalf("stream URL player = %q, want %d: %s", q.Get("player"), player.Player, player.URL)
		}
		if q.Get("game") != resp.Game {
			t.Fatalf("stream URL game = %q, want %q: %s", q.Get("game"), resp.Game, player.URL)
		}
	}
}

func TestNDSV1CreateProvisioningFailureEmitsWebhook(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")
	events := make(chan ndsWebhookPayload, 1)
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

	h := testNDSHub(t, 1)
	for worker := range h.workers.Values() {
		if worker.NDSGroup == "mkds-r1" && worker.NDSPlayer == 1 {
			worker.Connection = &fakeNDSConnection{id: worker.Id(), romInstallErr: errors.New("install failed")}
			break
		}
	}

	resp := postNDSRoom(t, h, testNDSCreateBody("room-123", 2))
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want %d; body=%s", resp.Code, http.StatusInternalServerError, resp.Body.String())
	}
	select {
	case event := <-events:
		if event.Event != "room.failed" || event.RoomID != "room-123" {
			t.Fatalf("unexpected webhook: %#v", event)
		}
		if event.Extra["reason"] != "rom_install_failed" {
			t.Fatalf("failure reason = %#v", event.Extra["reason"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for room.failed webhook")
	}
	if got := h.ndsRooms.get("room-123").state; got != ndsRoomFailed {
		t.Fatalf("room state = %s, want %s", got, ndsRoomFailed)
	}
}

func TestNDSV1GetRetainsClosedRoomForTenMinutes(t *testing.T) {
	h := testNDSHub(t, 0)
	now := time.Now().UTC()
	h.ndsRooms.put(&ndsRoomSession{
		closedAt:  &now,
		createdAt: now.Add(-time.Minute),
		players:   map[int]*ndsSeat{},
		roomID:    "room-123",
		state:     ndsRoomClosed,
		updatedAt: now,
	})

	retained := httptest.NewRecorder()
	h.handleNDSRoomGet(retained, "room-123")
	if retained.Code != http.StatusOK {
		t.Fatalf("retained closed room status = %d, want %d; body=%s", retained.Code, http.StatusOK, retained.Body.String())
	}

	oldClosedAt := now.Add(-ndsClosedRetention - time.Second)
	h.ndsRooms.get("room-123").mu.Lock()
	h.ndsRooms.get("room-123").closedAt = &oldClosedAt
	h.ndsRooms.get("room-123").mu.Unlock()

	expired := httptest.NewRecorder()
	h.handleNDSRoomGet(expired, "room-123")
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired closed room status = %d, want %d; body=%s", expired.Code, http.StatusNotFound, expired.Body.String())
	}
}

func TestNDSV1CreateRateLimitPerAPIKey(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")

	h := testNDSHub(t, 1)
	body := testNDSCreateBody("room-123", 2)
	for i := 0; i < 30; i++ {
		resp := postNDSRoom(t, h, body)
		if resp.Code != http.StatusCreated && resp.Code != http.StatusOK {
			t.Fatalf("create %d status = %d; body=%s", i+1, resp.Code, resp.Body.String())
		}
	}
	limited := postNDSRoom(t, h, body)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limited status = %d, want %d; body=%s", limited.Code, http.StatusTooManyRequests, limited.Body.String())
	}
	var errResp api.NDSAPIError
	if err := json.Unmarshal(limited.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Code != "rate_limited" || errResp.RetryAfterSec == 0 {
		t.Fatalf("unexpected rate limit body: %#v", errResp)
	}
}

func TestNDSV1CreateRejectedWhileDraining(t *testing.T) {
	t.Setenv("NDS_API_KEY", "test-key")
	t.Setenv("NDS_TOKEN_SECRET", "token-secret")
	t.Setenv("NDS_PUBLIC_ENDPOINT", "https://sg-1.nds.rebitplay.com")

	h := testNDSHub(t, 1)
	h.draining.Store(true)
	resp := postNDSRoom(t, h, testNDSCreateBody("room-123", 2))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining status = %d, want %d; body=%s", resp.Code, http.StatusServiceUnavailable, resp.Body.String())
	}
	var errResp api.NDSAPIError
	if err := json.Unmarshal(resp.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Code != "service_draining" || errResp.RetryAfterSec == 0 {
		t.Fatalf("unexpected draining body: %#v", errResp)
	}
}

func TestNDSV1DeleteReturnsAcceptedBeforeFlushCompletes(t *testing.T) {
	h := testNDSHub(t, 0)
	blockFlush := make(chan struct{})
	internalRoomID := "room-123-p1___Tetris DS"
	worker := &Worker{Connection: &fakeNDSConnection{id: com.NewUid(), flushBlock: blockFlush}}
	h.ndsRooms.put(&ndsRoomSession{
		createdAt: time.Now().UTC(),
		players: map[int]*ndsSeat{
			1: {player: 1, ref: "user_1", roomID: internalRoomID, saveStatus: "unchanged", worker: worker},
		},
		roomID:    "room-123",
		state:     ndsRoomActive,
		updatedAt: time.Now().UTC(),
	})

	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.handleNDSRoomDelete(rr, "room-123")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		close(blockFlush)
		t.Fatal("DELETE blocked on final save flush")
	}
	if rr.Code != http.StatusAccepted {
		close(blockFlush)
		t.Fatalf("delete status = %d, want %d; body=%s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	if got := h.ndsRooms.get("room-123").state; got != ndsRoomClosing {
		close(blockFlush)
		t.Fatalf("room state after accepted delete = %s, want %s", got, ndsRoomClosing)
	}

	close(blockFlush)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.ndsRooms.get("room-123").state == ndsRoomClosed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("room state = %s, want %s", h.ndsRooms.get("room-123").state, ndsRoomClosed)
}

func TestNDSWSRateLimitPerIP(t *testing.T) {
	h := testNDSHub(t, 0)
	handler := h.handleUserConnection()
	for i := 0; i < 10; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ws?token=bad", nil)
		req.RemoteAddr = "198.51.100.9:12345"
		handler(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("ws attempt %d status = %d, want %d", i+1, rr.Code, http.StatusUnauthorized)
		}
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws?token=bad", nil)
	req.RemoteAddr = "198.51.100.9:12345"
	handler(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("ws rate limited status = %d, want %d", rr.Code, http.StatusTooManyRequests)
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
