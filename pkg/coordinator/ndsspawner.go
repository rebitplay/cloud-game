package coordinator

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

const (
	defaultNDSDynamicMaxGroups = 8
	defaultNDSPlayersPerGroup  = 4
	defaultNDSSpawnTimeout     = 30 * time.Second
)

type ndsSpawner struct {
	enabled         bool
	maxGroups       int
	playersPerGroup int
	nextGroup       int
	workerPath      string
	coordinatorHost string
	hubAddr         string
	display         string
	runtimeDir      string
	videoCodec      string
	includeLoopback string
	iceIPMap        string
	webrtcBasePort  int
	workerBasePort  int
	monitorBasePort int
	timeout         time.Duration

	log       *logger.Logger
	mu        sync.Mutex
	processes map[string][]*exec.Cmd
}

func newNDSSpawnerFromEnv(log *logger.Logger) *ndsSpawner {
	if !envBool("NDS_AUTO_SPAWN", false) && !envBool("NDS_DYNAMIC_WORKERS", false) {
		return nil
	}

	defaultWebRTCBasePort := 8640
	if envBool("WEBRTC_MUX_ENABLED", false) || envBool("CLOUD_GAME_WEBRTC_MUX_ENABLED", false) {
		defaultWebRTCBasePort = 8700
	}
	warmGroups := envInt("ROOM_COUNT", envInt("WARM_GROUPS", envInt("NDS_WARM_GROUPS", envInt("NDS_ROOM_COUNT", 0))))
	maxGroups := envInt("NDS_MAX_ROOM_COUNT", defaultNDSDynamicMaxGroups)
	if maxGroups < warmGroups {
		maxGroups = warmGroups
	}

	spawner := &ndsSpawner{
		enabled:         true,
		maxGroups:       maxGroups,
		playersPerGroup: envInt("NDS_PLAYERS_PER_ROOM", envInt("PLAYER_COUNT", defaultNDSPlayersPerGroup)),
		nextGroup:       warmGroups + 1,
		workerPath:      envString("NDS_WORKER_PATH", "./worker"),
		coordinatorHost: envString("COORDINATOR_HOST", "127.0.0.1:"+envString("PORT", "8000")),
		hubAddr:         envString("HUB_ADDR", "127.0.0.1:55355"),
		display:         envString("DISPLAY", ":99"),
		runtimeDir:      envString("RUNTIME_DIR", "/tmp/cloud-game/nds-lan"),
		videoCodec:      envString("CLOUD_GAME_ENCODER_VIDEO_CODEC", "vp8"),
		includeLoopback: envString("CLOUD_GAME_WEBRTC_INCLUDELOOPBACKCANDIDATE", "false"),
		iceIPMap:        envString("CLOUD_GAME_WEBRTC_ICEIPMAP", envString("BUNNY_ANYCAST_IP", "")),
		webrtcBasePort:  envInt("WEBRTC_WORKER_BASE_PORT", envInt("WEBRTC_BASE_PORT", defaultWebRTCBasePort)),
		workerBasePort:  envInt("WORKER_BASE_PORT", 9000),
		monitorBasePort: envInt("MONITORING_BASE_PORT", 6620),
		timeout:         envDuration("NDS_SPAWN_TIMEOUT", defaultNDSSpawnTimeout),
		log:             log,
		processes:       make(map[string][]*exec.Cmd),
	}
	if spawner.playersPerGroup < 1 {
		spawner.playersPerGroup = 1
	}
	if spawner.playersPerGroup > 4 {
		spawner.playersPerGroup = 4
	}
	return spawner
}

func (s *ndsSpawner) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for group, commands := range s.processes {
		for _, cmd := range commands {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
		delete(s.processes, group)
	}
}

func (h *Hub) spawnNDSGroup(players int) error {
	if h.ndsSpawner == nil || !h.ndsSpawner.enabled {
		return apiConflict("not enough free NDS groups")
	}
	return h.ndsSpawner.spawnGroup(h, players)
}

func (s *ndsSpawner) spawnGroup(h *Hub, players int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nextGroup > s.maxGroups {
		return apiConflict(fmt.Sprintf("dynamic NDS room capacity exhausted: max groups %d", s.maxGroups))
	}

	groupID := fmt.Sprintf("mkds-r%d", s.nextGroup)
	s.nextGroup++

	s.log.Info().
		Str("group", groupID).
		Int("workers", s.playersPerGroup).
		Int("requested_players", players).
		Msg("spawning NDS worker group")

	commands := make([]*exec.Cmd, 0, s.playersPerGroup)
	for player := 1; player <= s.playersPerGroup; player++ {
		cmd, err := s.startWorker(groupID, player)
		if err != nil {
			for _, started := range commands {
				if started.Process != nil {
					_ = started.Process.Kill()
				}
			}
			return err
		}
		commands = append(commands, cmd)
	}
	s.processes[groupID] = commands

	deadline := time.Now().Add(s.timeout)
	for time.Now().Before(deadline) {
		if h.ndsGroupCanHost(groupID, players) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	s.killGroup(groupID)
	return fmt.Errorf("timed out waiting for NDS worker group %s", groupID)
}

func (s *ndsSpawner) startWorker(groupID string, player int) (*exec.Cmd, error) {
	slot := ((groupNumber(groupID) - 1) * s.playersPerGroup) + player
	workerPort := s.workerBasePort + slot
	monitorPort := s.monitorBasePort + slot
	webrtcPort := s.webrtcBasePort + slot
	zone := fmt.Sprintf("%s-p%d", groupID, player)
	netplayClientID := player - 1
	macAddress := fmt.Sprintf("00:08:BF:%02X:00:%02X", groupNumber(groupID), player)

	saveDir := filepath.Join(s.runtimeDir, groupID, fmt.Sprintf("p%d", player), "save")
	localDir := filepath.Join(s.runtimeDir, groupID, fmt.Sprintf("p%d", player), "libretro")
	for _, dir := range []string{saveDir, localDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, err
		}
	}

	cmd := exec.Command(
		s.workerPath,
		"-address", net.JoinHostPort("", strconv.Itoa(workerPort)),
		"-monitoring.port", strconv.Itoa(monitorPort),
		"-coordinatorhost", s.coordinatorHost,
		"-zone", zone,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withEnv(os.Environ(), map[string]string{
		"DISPLAY":                                    s.display,
		"MESA_GL_VERSION_OVERRIDE":                   envString("MESA_GL_VERSION_OVERRIDE", "4.5"),
		"CLOUD_GAME_WORKER_DEBUG":                    envString("CLOUD_GAME_WORKER_DEBUG", "true"),
		"CLOUD_GAME_WORKER_TAG":                      zone,
		"CLOUD_GAME_WORKER_NDS_GROUP":                groupID,
		"CLOUD_GAME_WORKER_NDS_PLAYER":               strconv.Itoa(player),
		"CLOUD_GAME_ENCODER_VIDEO_CODEC":             s.videoCodec,
		"CLOUD_GAME_WEBRTC_SINGLEPORT":               strconv.Itoa(webrtcPort),
		"CLOUD_GAME_WEBRTC_INCLUDELOOPBACKCANDIDATE": s.includeLoopback,
		"CLOUD_GAME_WEBRTC_ICEIPMAP":                 s.iceIPMap,
		"CLOUD_GAME_EMULATOR_STORAGE":                saveDir,
		"CLOUD_GAME_EMULATOR_LOCALPATH":              localDir,
		"MELONDS_NETPLAY_HUB":                        s.hubAddr,
		"MELONDS_NETPLAY_ROOM":                       groupID,
		"MELONDS_NETPLAY_CLIENT_ID":                  strconv.Itoa(netplayClientID),
		"MELONDS_MAC_ADDRESS":                        macAddress,
		"LIBRETRO_USERNAME":                          zone,
	})

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			s.log.Warn().Err(err).Str("group", groupID).Int("player", player).Msg("spawned NDS worker exited")
		}
	}()
	return cmd, nil
}

func (s *ndsSpawner) killGroup(groupID string) {
	commands := s.processes[groupID]
	for _, cmd := range commands {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	delete(s.processes, groupID)
}

func (h *Hub) ndsGroupCanHost(groupID string, players int) bool {
	for _, group := range h.ndsWorkerGroups() {
		if group.id == groupID {
			return group.canHost(players)
		}
	}
	return false
}

func groupNumber(groupID string) int {
	idx := strings.LastIndex(groupID, "-r")
	if idx < 0 || idx+2 >= len(groupID) {
		return 1
	}
	n, err := strconv.Atoi(groupID[idx+2:])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func withEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]struct{}, len(overrides))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if value, replace := overrides[key]; replace {
			out = append(out, key+"="+value)
			seen[key] = struct{}{}
			continue
		}
		out = append(out, item)
	}
	for key, value := range overrides {
		if _, ok := seen[key]; !ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func envString(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}
