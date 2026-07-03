package config

import "testing"

func TestWebRTCApplyEnvOverridesAddsTurnServers(t *testing.T) {
	t.Setenv("WEBRTC_TURN_URLS", "turn:turn.example.com:3478?transport=udp, turns:turn.example.com:5349")
	t.Setenv("WEBRTC_TURN_USERNAME", "user")
	t.Setenv("WEBRTC_TURN_CREDENTIAL", "secret")

	conf := Webrtc{IceServers: []IceServer{{Urls: "stun:stun.l.google.com:19302"}}}
	conf.ApplyEnvOverrides()

	want := []IceServer{
		{Urls: "stun:stun.l.google.com:19302"},
		{Urls: "turn:turn.example.com:3478?transport=udp", Username: "user", Credential: "secret"},
		{Urls: "turns:turn.example.com:5349", Username: "user", Credential: "secret"},
	}
	if len(conf.IceServers) != len(want) {
		t.Fatalf("ICE server count = %d, want %d: %#v", len(conf.IceServers), len(want), conf.IceServers)
	}
	for i := range want {
		if conf.IceServers[i] != want[i] {
			t.Fatalf("ICE server %d = %#v, want %#v", i, conf.IceServers[i], want[i])
		}
	}
}

func TestWebRTCApplyEnvOverridesSkipsDuplicateTurnServer(t *testing.T) {
	t.Setenv("CLOUD_GAME_WEBRTC_TURN_URL", "turn:turn.example.com:3478?transport=tcp")
	t.Setenv("CLOUD_GAME_WEBRTC_TURN_USERNAME", "user")
	t.Setenv("CLOUD_GAME_WEBRTC_TURN_PASSWORD", "secret")

	conf := Webrtc{IceServers: []IceServer{
		{Urls: "turn:turn.example.com:3478?transport=tcp", Username: "user", Credential: "secret"},
	}}
	conf.ApplyEnvOverrides()

	if len(conf.IceServers) != 1 {
		t.Fatalf("ICE server count = %d, want 1: %#v", len(conf.IceServers), conf.IceServers)
	}
}
