package coordinator

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestRewriteCandidateJSON(t *testing.T) {
	raw := `{"candidate":"candidate:123 1 udp 2122260223 10.0.0.2 8701 typ host","sdpMid":"0","sdpMLineIndex":0,"usernameFragment":"worker123"}`

	rewritten := rewriteCandidateJSON(raw, "203.0.113.10", 8641)

	var payload map[string]any
	if err := json.Unmarshal([]byte(rewritten), &payload); err != nil {
		t.Fatalf("candidate json: %v", err)
	}
	if got := payload["candidate"].(string); got != "candidate:123 1 udp 2122260223 203.0.113.10 8641 typ host" {
		t.Fatalf("candidate mismatch: %q", got)
	}
	if got := candidateJSONUfrag(rewritten); got != "worker123" {
		t.Fatalf("ufrag mismatch: %q", got)
	}
}

func TestRewriteCandidateJSONLeavesRelayCandidatesUntouched(t *testing.T) {
	raw := `{"candidate":"candidate:456 1 udp 1677729535 203.0.113.55 49200 typ relay raddr 0.0.0.0 rport 0","sdpMid":"0","sdpMLineIndex":0,"usernameFragment":"browser123"}`

	rewritten := rewriteCandidateJSON(raw, "203.0.113.10", 8641)

	if rewritten != raw {
		t.Fatalf("relay candidate was rewritten:\n got: %s\nwant: %s", rewritten, raw)
	}
}

func TestRewriteRTCSessionSDP(t *testing.T) {
	raw := `{"type":"offer","sdp":"v=0\r\na=ice-ufrag:worker123\r\na=candidate:123 1 udp 2122260223 10.0.0.2 8701 typ host\r\na=end-of-candidates\r\n"}`

	rewritten, ufrag := rewriteRTCSessionSDP(raw, "203.0.113.10", 8641)
	if ufrag != "worker123" {
		t.Fatalf("ufrag mismatch: %q", ufrag)
	}
	if !strings.Contains(rewritten, "candidate:123 1 udp 2122260223 203.0.113.10 8641 typ host") {
		t.Fatalf("candidate was not rewritten: %s", rewritten)
	}
}

func TestStunUsername(t *testing.T) {
	username := "worker123:browser456"
	packet := make([]byte, 20+4+len(username))
	binary.BigEndian.PutUint16(packet[0:2], 0x0001)
	binary.BigEndian.PutUint16(packet[2:4], uint16(4+len(username)))
	binary.BigEndian.PutUint32(packet[4:8], stunMagicCookie)
	binary.BigEndian.PutUint16(packet[20:22], stunAttrUsername)
	binary.BigEndian.PutUint16(packet[22:24], uint16(len(username)))
	copy(packet[24:], username)

	got, ok := stunUsername(packet)
	if !ok || got != username {
		t.Fatalf("username mismatch: %q ok=%v", got, ok)
	}
	first, second := splitICEUsername(got)
	if first != "worker123" || second != "browser456" {
		t.Fatalf("split mismatch: %q %q", first, second)
	}
}

func TestBrowserDestsReturnsAllLearnedBrowserAddrs(t *testing.T) {
	mux := &webRTCMux{}
	route := &webRTCMuxRoute{
		browserAddr: &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40000},
		browserAddrs: map[string]*net.UDPAddr{
			"198.51.100.2:40000": &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40000},
			"198.51.100.2:40001": &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40001},
		},
	}

	dests := mux.browserDests(route)
	if len(dests) != 2 {
		t.Fatalf("browser dest count = %d, want 2: %#v", len(dests), dests)
	}
	if got := dests[0].String(); got != "198.51.100.2:40000" {
		t.Fatalf("preferred dest = %q, want 198.51.100.2:40000", got)
	}
	seen := map[string]bool{}
	for _, dst := range dests {
		seen[dst.String()] = true
	}
	if !seen["198.51.100.2:40000"] || !seen["198.51.100.2:40001"] {
		t.Fatalf("browser dests missing learned addresses: %#v", seen)
	}
}
