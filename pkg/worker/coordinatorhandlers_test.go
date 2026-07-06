package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	cagedapp "github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/room"
)

type testWorkerSession struct{}

func (s *testWorkerSession) Disconnect()                     {}
func (s *testWorkerSession) SendAudio([]byte, time.Duration) {}
func (s *testWorkerSession) SendVideo([]byte, time.Duration) {}
func (s *testWorkerSession) SendData([]byte)                 {}

type testWorkerApp struct {
	closed bool
}

func (a *testWorkerApp) AudioSampleRate() int            { return 44100 }
func (a *testWorkerApp) AspectRatio() float32            { return 1 }
func (a *testWorkerApp) AspectEnabled() bool             { return false }
func (a *testWorkerApp) Flipped() bool                   { return false }
func (a *testWorkerApp) Init() error                     { return nil }
func (a *testWorkerApp) ViewportSize() (int, int)        { return 256, 192 }
func (a *testWorkerApp) Scale() (float64, string)        { return 1, "" }
func (a *testWorkerApp) Rotation() uint                  { return 0 }
func (a *testWorkerApp) PixFormat() uint32               { return 0 }
func (a *testWorkerApp) Start()                          {}
func (a *testWorkerApp) Close()                          { a.closed = true }
func (a *testWorkerApp) SetAudioCb(func(cagedapp.Audio)) {}
func (a *testWorkerApp) SetVideoCb(func(cagedapp.Video)) {}
func (a *testWorkerApp) SetDataCb(func([]byte))          {}
func (a *testWorkerApp) Input(int, byte, []byte)         {}
func (a *testWorkerApp) KbMouseSupport() bool            { return false }
func (a *testWorkerApp) PointerSupport() bool            { return false }

func TestWorkerMonitoringURLUsesLoopbackForWildcardAddress(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = true
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"

	got := workerMonitoringURL(conf, "[::]:9001")
	if got != "http://127.0.0.1:6621/worker/metrics" {
		t.Fatalf("worker monitoring URL = %q", got)
	}
}

func TestWorkerMonitoringURLUsesLoopbackForZonedLocalhost(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = true
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"

	got := workerMonitoringURL(conf, "mkds-p1.localhost:9001")
	if got != "http://127.0.0.1:6621/worker/metrics" {
		t.Fatalf("worker monitoring URL = %q", got)
	}
}

func TestWorkerMonitoringURLDisabledWhenMetricsOff(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = false
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"

	if got := workerMonitoringURL(conf, "127.0.0.1:9001"); got != "" {
		t.Fatalf("worker monitoring URL = %q, want empty", got)
	}
}

func TestBuildConnQueryAdvertisesMonitoringURL(t *testing.T) {
	var conf config.Worker
	conf.Monitoring.MetricEnabled = true
	conf.Monitoring.Port = 6621
	conf.Monitoring.URLPrefix = "/worker"
	conf.Network.PingEndpoint = "/echo"

	raw, err := buildConnQuery(com.NewUid(), conf, 8701, "127.0.0.1:9001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"monitoring_url":"http://127.0.0.1:6621/worker/metrics"`) {
		t.Fatalf("handshake did not include monitoring URL: %s", raw)
	}
}

func TestNDSFirmwareNameAllowsSpacesAndCapsLength(t *testing.T) {
	got := ndsFirmwareName("  meo Coa Long Name  ")
	if got != "meo Coa Lo" {
		t.Fatalf("nds firmware name = %q, want capped name with space", got)
	}

	got = ndsFirmwareName("\n\t", "Player2")
	if got != "Player2" {
		t.Fatalf("nds firmware fallback name = %q, want Player2", got)
	}
}

func TestPreparedNDSSessionKeepsPlayerNameUntilGameStart(t *testing.T) {
	w := &Worker{}
	w.prepared.sessions = map[string]preparedNDSSession{}
	roomID := "room-123-p2___Tetris DS"

	w.markPreparedSession(roomID, preparedNDSSession{Name: "meo Coa", Player: 2, Ref: "user_2"})

	prepared, ok := w.consumePreparedSession(roomID)
	if !ok {
		t.Fatal("prepared NDS session was not found")
	}
	if prepared.Name != "meo Coa" || prepared.Player != 2 || prepared.Ref != "user_2" {
		t.Fatalf("prepared session = %#v", prepared)
	}
	if _, ok := w.consumePreparedSession(roomID); ok {
		t.Fatal("prepared NDS session should be consumed only once")
	}
}

func TestRemoveUserFromActiveNDSRoomKeepsEmulatorRunning(t *testing.T) {
	w, user, app := testWorkerWithRoom("room-123-p1___Tetris DS")
	w.markActiveNDSRoom(user.RoomId, preparedNDSSession{Player: 1, Ref: "user_1"})

	keepRoom := removeUserFromRoom(w, user)
	removeUserFromRouter(w, user, keepRoom)

	if !keepRoom {
		t.Fatal("removeUserFromRoom did not mark active NDS room for retention")
	}
	if app.closed {
		t.Fatal("active NDS emulator was closed after one browser disconnected")
	}
	if got := w.router.FindRoom("room-123-p1___Tetris DS"); got == nil {
		t.Fatal("active NDS worker room was removed after one browser disconnected")
	}
}

func TestExplicitQuitClosesActiveNDSRoom(t *testing.T) {
	w, user, app := testWorkerWithRoom("room-123-p1___Tetris DS")
	w.markActiveNDSRoom(user.RoomId, preparedNDSSession{Player: 1, Ref: "user_1"})

	out := (&coordinator{}).HandleQuitGame(api.GameQuitRequest{Rid: user.RoomId}, w)

	if out.Payload != api.OK {
		t.Fatalf("explicit close response = %#v, want %q", out.Payload, api.OK)
	}
	if !app.closed {
		t.Fatal("explicit NDS room close did not close emulator")
	}
	if got := w.router.FindRoom("room-123-p1___Tetris DS"); got != nil {
		t.Fatal("explicit NDS room close left worker room registered")
	}
	if w.isActiveNDSRoom("room-123-p1___Tetris DS") {
		t.Fatal("explicit NDS room close left active NDS marker")
	}
}

func testWorkerWithRoom(roomID string) (*Worker, *room.GameSession, *testWorkerApp) {
	router := room.NewGameRouter()
	users := com.NewNetMap[room.SessionKey, *room.GameSession]()
	app := &testWorkerApp{}
	r := room.NewRoom(roomID, app, &users, nil)
	user := room.NewGameSession("user-1", &testWorkerSession{})
	user.RoomId = roomID
	r.AddUser(user)
	router.SetRoom(r)
	router.AddUser(user)

	return &Worker{router: router}, user, app
}
