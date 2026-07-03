package coordinator

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/pion/stun/v3"
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
	raw := `{"type":"offer","sdp":"v=0\r\na=ice-ufrag:worker123\r\na=ice-pwd:workerpwd456\r\na=candidate:123 1 udp 2122260223 10.0.0.2 8701 typ host\r\na=end-of-candidates\r\n"}`

	rewritten, ufrag := rewriteRTCSessionSDP(raw, "203.0.113.10", 8641)
	if ufrag != "worker123" {
		t.Fatalf("ufrag mismatch: %q", ufrag)
	}
	if pwd := extractRTCSessionSDPPwd(raw); pwd != "workerpwd456" {
		t.Fatalf("pwd mismatch: %q", pwd)
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

func TestRewriteSTUNBindingSuccessXORMappedAddress(t *testing.T) {
	const pwd = "workerpwd456"
	original := stun.MustBuild(
		stun.TransactionID,
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: net.ParseIP("127.0.0.1").To4(), Port: 9881},
		stun.NewShortTermIntegrity(pwd),
		stun.Fingerprint,
	)
	dst := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40000}

	rewritten := rewriteSTUNBindingSuccess(original.Raw, dst, pwd)

	msg := &stun.Message{Raw: rewritten}
	if err := msg.Decode(); err != nil {
		t.Fatalf("decode rewritten stun: %v", err)
	}
	if err := stun.NewShortTermIntegrity(pwd).Check(msg); err != nil {
		t.Fatalf("rewritten integrity check: %v", err)
	}
	if err := stun.Fingerprint.Check(msg); err != nil {
		t.Fatalf("rewritten fingerprint check: %v", err)
	}

	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(msg); err != nil {
		t.Fatalf("mapped address: %v", err)
	}
	if got := mapped.String(); got != "198.51.100.2:40000" {
		t.Fatalf("mapped address = %q, want 198.51.100.2:40000", got)
	}
}

func TestRewriteSTUNBindingSuccessRequiresWorkerPassword(t *testing.T) {
	original := stun.MustBuild(
		stun.TransactionID,
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: net.ParseIP("127.0.0.1").To4(), Port: 9881},
		stun.NewShortTermIntegrity("workerpwd456"),
		stun.Fingerprint,
	)
	dst := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40000}

	rewritten := rewriteSTUNBindingSuccess(original.Raw, dst, "")

	if string(rewritten) != string(original.Raw) {
		t.Fatalf("packet was rewritten without worker password")
	}
}

func TestBuildSTUNBindingSuccessForBrowserRequest(t *testing.T) {
	const pwd = "workerpwd456"
	request := stun.MustBuild(
		stun.TransactionID,
		stun.BindingRequest,
		stun.NewUsername("worker123:browser456"),
		stun.NewShortTermIntegrity(pwd),
		stun.Fingerprint,
	)
	dst := &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40000}

	reply := buildSTUNBindingSuccess(request.Raw, dst, pwd)
	if reply == nil {
		t.Fatal("expected STUN binding success reply")
	}

	msg := &stun.Message{Raw: reply}
	if err := msg.Decode(); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if msg.Type != stun.BindingSuccess {
		t.Fatalf("reply type = %s, want %s", msg.Type, stun.BindingSuccess)
	}
	if msg.TransactionID != request.TransactionID {
		t.Fatalf("transaction ID mismatch")
	}
	if err := stun.NewShortTermIntegrity(pwd).Check(msg); err != nil {
		t.Fatalf("reply integrity check: %v", err)
	}
	if err := stun.Fingerprint.Check(msg); err != nil {
		t.Fatalf("reply fingerprint check: %v", err)
	}

	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(msg); err != nil {
		t.Fatalf("mapped address: %v", err)
	}
	if got := mapped.String(); got != "198.51.100.2:40000" {
		t.Fatalf("mapped address = %q, want 198.51.100.2:40000", got)
	}
}

func TestBrowserDestsReturnsAllLearnedBrowserAddrs(t *testing.T) {
	mux := &webRTCMux{}
	route := &webRTCMuxRoute{
		browserAddr: &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 40000},
		browserAddrs: map[string]*net.UDPAddr{
			"198.51.100.2:40000": {IP: net.ParseIP("198.51.100.2"), Port: 40000},
			"198.51.100.2:40001": {IP: net.ParseIP("198.51.100.2"), Port: 40001},
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
