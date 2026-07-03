package config

import (
	"os"
	"strings"
)

type Webrtc struct {
	DisableDefaultInterceptors bool
	DtlsRole                   byte
	IceServers                 []IceServer
	IcePorts                   struct {
		Min uint16
		Max uint16
	}
	IceIpMap                 string
	IceLite                  bool
	IpFilter                 []string
	IpFilterRemote           []string
	IncludeLoopbackCandidate bool
	SinglePort               int
	LogLevel                 int
}

type IceServer struct {
	Urls       string `json:"urls,omitempty"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
}

func (w *Webrtc) HasDtlsRole() bool   { return w.DtlsRole > 0 }
func (w *Webrtc) HasPortRange() bool  { return w.IcePorts.Min > 0 && w.IcePorts.Max > 0 }
func (w *Webrtc) HasSinglePort() bool { return w.SinglePort > 0 }
func (w *Webrtc) HasIceIpMap() bool   { return w.IceIpMap != "" }

func (w *Webrtc) ApplyEnvOverrides() {
	urls := firstEnv(
		"CLOUD_GAME_WEBRTC_TURN_URLS",
		"CLOUD_GAME_WEBRTC_TURN_URL",
		"WEBRTC_TURN_URLS",
		"WEBRTC_TURN_URL",
	)
	if urls == "" {
		return
	}

	username := firstEnv("CLOUD_GAME_WEBRTC_TURN_USERNAME", "WEBRTC_TURN_USERNAME")
	credential := firstEnv(
		"CLOUD_GAME_WEBRTC_TURN_CREDENTIAL",
		"CLOUD_GAME_WEBRTC_TURN_PASSWORD",
		"WEBRTC_TURN_CREDENTIAL",
		"WEBRTC_TURN_PASSWORD",
	)
	for _, raw := range strings.Split(urls, ",") {
		url := strings.TrimSpace(raw)
		if url == "" {
			continue
		}
		server := IceServer{Urls: url, Username: username, Credential: credential}
		if !hasIceServer(w.IceServers, server) {
			w.IceServers = append(w.IceServers, server)
		}
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func hasIceServer(servers []IceServer, server IceServer) bool {
	for _, existing := range servers {
		if existing == server {
			return true
		}
	}
	return false
}
