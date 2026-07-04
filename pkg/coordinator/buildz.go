package coordinator

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giongto35/cloud-game/v3/pkg/config"
)

var (
	BuildVersion       = "?"
	webrtcAssetPattern = regexp.MustCompile(`webrtc\.js\?v=[0-9]+`)
)

type buildzResponse struct {
	Version                  string `json:"version"`
	WebRTCAsset              string `json:"webrtc_asset,omitempty"`
	WebRTCFirefoxRelayPolicy bool   `json:"webrtc_firefox_relay_policy"`
	NDSRestTURNConfigured    bool   `json:"nds_rest_turn_configured"`
	ConfigTURNCount          int    `json:"config_turn_count"`
	WebRTCMuxEnabled         bool   `json:"webrtc_mux_enabled"`
	WebRTCPublicIP           string `json:"webrtc_public_ip,omitempty"`
	WebRTCPublicPort         string `json:"webrtc_public_port,omitempty"`
	MetricsEnabled           bool   `json:"metrics_enabled"`
	AssetError               string `json:"asset_error,omitempty"`
}

func handleBuildz(conf config.CoordinatorConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}

		asset, relayPolicy, assetErr := detectWebRTCFrontend()
		resp := buildzResponse{
			Version:                  BuildVersion,
			WebRTCAsset:              asset,
			WebRTCFirefoxRelayPolicy: relayPolicy,
			NDSRestTURNConfigured:    ndsRESTTURNConfigured(),
			ConfigTURNCount:          countConfigTURN(conf.Webrtc.IceServers),
			WebRTCMuxEnabled: envBool("WEBRTC_MUX_ENABLED", false) ||
				envBool("CLOUD_GAME_WEBRTC_MUX_ENABLED", false),
			WebRTCPublicIP:   firstNonEmptyEnv("WEBRTC_PUBLIC_IP", "BUNNY_ANYCAST_IP"),
			WebRTCPublicPort: firstNonEmptyEnv("WEBRTC_PUBLIC_PORT"),
			MetricsEnabled:   coordinatorPublicMetricsEnabled(conf),
		}
		if assetErr != nil {
			resp.AssetError = assetErr.Error()
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func detectWebRTCFrontend() (asset string, firefoxRelayPolicy bool, err error) {
	networkJS, err := readWebAsset("js/network/network.js")
	if err != nil {
		return "", false, err
	}
	if match := webrtcAssetPattern.FindString(string(networkJS)); match != "" {
		asset = match
	}

	webrtcJS, err := readWebAsset("js/network/webrtc.js")
	if err != nil {
		return asset, false, err
	}
	firefoxRelayPolicy = strings.Contains(string(webrtcJS), `iceTransportPolicy = "relay"`) &&
		strings.Contains(string(webrtcJS), "Firefox detected")
	return asset, firefoxRelayPolicy, nil
}

func readWebAsset(rel string) ([]byte, error) {
	root := firstNonEmptyEnv("CLOUD_GAME_WEB_ROOT")
	if root == "" {
		root = "./web"
	}
	candidates := []string{
		filepath.Join(root, rel),
		filepath.Join("..", "..", "web", rel),
		filepath.Join("/usr/local/share/cloud-game/web", rel),
	}
	var lastErr error
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func ndsRESTTURNConfigured() bool {
	return firstNonEmptyEnv("NDS_TURN_URLS", "NDS_TURN_URL") != "" &&
		firstNonEmptyEnv("NDS_TURN_SECRET", "NDS_TURN_SHARED_SECRET") != ""
}

func countConfigTURN(servers []config.IceServer) int {
	count := 0
	for _, server := range servers {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(server.Urls)), "turn:") ||
			strings.HasPrefix(strings.ToLower(strings.TrimSpace(server.Urls)), "turns:") {
			count++
		}
	}
	return count
}
