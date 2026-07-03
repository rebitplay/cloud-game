package coordinator

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

const (
	defaultWebRTCMuxPort = 8641
	stunMagicCookie      = 0x2112A442
	stunAttrUsername     = 0x0006
)

type webRTCMux struct {
	conn       *net.UDPConn
	publicHost string
	publicPort int
	workerHost string
	listenPort int
	trace      bool
	log        *logger.Logger

	done chan struct{}

	mu              sync.RWMutex
	routesByBrowser map[string]*webRTCMuxRoute
	routesBySession map[string]*webRTCMuxRoute
	routesByUfrag   map[string]*webRTCMuxRoute
	routesByWorker  map[string]*webRTCMuxRoute
}

type webRTCMuxRoute struct {
	sessionID    string
	workerID     string
	workerUfrag  string
	workerAddr   *net.UDPAddr
	browserAddr  *net.UDPAddr
	browserAddrs map[string]*net.UDPAddr
	updatedAt    time.Time
}

func newWebRTCMuxFromEnv(log *logger.Logger) (*webRTCMux, error) {
	if !envBool("WEBRTC_MUX_ENABLED", false) && !envBool("CLOUD_GAME_WEBRTC_MUX_ENABLED", false) {
		return nil, nil
	}

	publicPort := envInt("WEBRTC_PUBLIC_PORT", envInt("WEBRTC_MUX_PORT", defaultWebRTCMuxPort))
	listenAddr := envString("WEBRTC_MUX_LISTEN_ADDR", net.JoinHostPort("0.0.0.0", strconv.Itoa(publicPort)))
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve WebRTC mux listen addr: %w", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen WebRTC mux on %s: %w", listenAddr, err)
	}

	local, _ := conn.LocalAddr().(*net.UDPAddr)
	listenPort := publicPort
	if local != nil && local.Port != 0 {
		listenPort = local.Port
	}

	mux := &webRTCMux{
		conn:       conn,
		publicHost: envString("WEBRTC_PUBLIC_IP", envString("CLOUD_GAME_WEBRTC_ICEIPMAP", envString("BUNNY_ANYCAST_IP", ""))),
		publicPort: publicPort,
		workerHost: envString("WEBRTC_MUX_WORKER_HOST", "127.0.0.1"),
		listenPort: listenPort,
		trace:      envBool("WEBRTC_MUX_TRACE", false),
		log:        log,
		done:       make(chan struct{}),

		routesByBrowser: make(map[string]*webRTCMuxRoute),
		routesBySession: make(map[string]*webRTCMuxRoute),
		routesByUfrag:   make(map[string]*webRTCMuxRoute),
		routesByWorker:  make(map[string]*webRTCMuxRoute),
	}

	go mux.run()

	log.Info().
		Str("addr", conn.LocalAddr().String()).
		Str("public_ip", mux.publicHost).
		Int("public_port", mux.publicPort).
		Msg("WebRTC UDP mux started")

	return mux, nil
}

func (m *webRTCMux) stop() {
	if m == nil {
		return
	}
	_ = m.conn.Close()
	<-m.done
}

func (m *webRTCMux) run() {
	defer close(m.done)

	buf := make([]byte, 64*1024)
	for {
		n, src, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			m.log.Warn().Err(err).Msg("WebRTC mux read failed")
			continue
		}
		packet := make([]byte, n)
		copy(packet, buf[:n])
		m.forward(packet, src)
	}
}

func (m *webRTCMux) forward(packet []byte, src *net.UDPAddr) {
	route, fromWorker := m.lookupRoute(src, packet)
	if route == nil {
		m.tracePacket("drop", packet, src, nil, false)
		return
	}

	if fromWorker {
		m.tracePacket("worker->browser", packet, src, route, true)
		dsts := m.browserDests(route)
		if len(dsts) == 0 {
			return
		}
		for _, dst := range dsts {
			if _, err := m.conn.WriteToUDP(packet, dst); err != nil {
				m.log.Debug().Err(err).Str("dst", dst.String()).Msg("WebRTC mux write to browser failed")
			}
		}
		return
	}

	m.rememberBrowser(route, src)
	m.tracePacket("browser->worker", packet, src, route, false)
	if _, err := m.conn.WriteToUDP(packet, route.workerAddr); err != nil {
		m.log.Debug().Err(err).Str("dst", route.workerAddr.String()).Msg("WebRTC mux write to worker failed")
	}
}

func (m *webRTCMux) tracePacket(direction string, packet []byte, src *net.UDPAddr, route *webRTCMuxRoute, fromWorker bool) {
	if !m.trace {
		return
	}

	event := m.log.Debug().
		Str("direction", direction).
		Str("src", src.String()).
		Str("kind", muxPacketKind(packet)).
		Int("bytes", len(packet))
	if route != nil {
		event = event.
			Str("session", route.sessionID).
			Str("worker", route.workerAddr.String())
		if route.browserAddr != nil {
			event = event.Str("browser", route.browserAddr.String())
		}
	}
	event.Bool("from_worker", fromWorker).Msg("WebRTC mux packet")
}

func (m *webRTCMux) lookupRoute(src *net.UDPAddr, packet []byte) (*webRTCMuxRoute, bool) {
	srcKey := src.String()

	m.mu.RLock()
	if route := m.routesByWorker[srcKey]; route != nil {
		m.mu.RUnlock()
		return route, true
	}
	if route := m.routesByBrowser[srcKey]; route != nil {
		m.mu.RUnlock()
		return route, false
	}
	m.mu.RUnlock()

	username, ok := stunUsername(packet)
	if !ok {
		return nil, false
	}

	first, second := splitICEUsername(username)
	m.mu.RLock()
	route := m.routesByUfrag[first]
	if route == nil && second != "" {
		route = m.routesByUfrag[second]
	}
	m.mu.RUnlock()
	if route == nil {
		return nil, false
	}

	return route, src.String() == route.workerAddr.String()
}

func (m *webRTCMux) rememberBrowser(route *webRTCMuxRoute, addr *net.UDPAddr) {
	m.mu.Lock()
	defer m.mu.Unlock()

	addrKey := addr.String()
	if route.browserAddrs == nil {
		route.browserAddrs = make(map[string]*net.UDPAddr)
	}
	if _, ok := route.browserAddrs[addrKey]; ok {
		if preferBrowserAddr(route.browserAddr, route.browserAddrs[addrKey]) {
			route.browserAddr = route.browserAddrs[addrKey]
		}
		route.updatedAt = time.Now()
		return
	}
	copied := *addr
	route.browserAddrs[addrKey] = &copied
	if preferBrowserAddr(route.browserAddr, route.browserAddrs[addrKey]) {
		route.browserAddr = route.browserAddrs[addrKey]
	}
	route.updatedAt = time.Now()
	m.routesByBrowser[addrKey] = route
	m.log.Debug().
		Str("session", route.sessionID).
		Str("browser", route.browserAddr.String()).
		Str("worker", route.workerAddr.String()).
		Msg("WebRTC mux learned browser address")
}

func (m *webRTCMux) browserDests(route *webRTCMuxRoute) []*net.UDPAddr {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if route.browserAddr == nil && len(route.browserAddrs) == 0 {
		return nil
	}
	dests := make([]*net.UDPAddr, 0, len(route.browserAddrs)+1)
	seen := map[string]struct{}{}
	if route.browserAddr != nil {
		dests = append(dests, route.browserAddr)
		seen[route.browserAddr.String()] = struct{}{}
	}
	for key, addr := range route.browserAddrs {
		if addr == nil {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		dests = append(dests, addr)
	}
	return dests
}

func preferBrowserAddr(current *net.UDPAddr, next *net.UDPAddr) bool {
	if next == nil {
		return false
	}
	if current == nil {
		return true
	}
	if current.IP.IsLoopback() && !next.IP.IsLoopback() {
		return true
	}
	if !current.IP.IsLoopback() && next.IP.IsLoopback() {
		return false
	}
	return true
}

func (m *webRTCMux) rewriteWorkerSDP(sessionID string, w *Worker, raw string) string {
	rewritten, ufrag := rewriteRTCSessionSDP(raw, m.publicHost, m.publicPort)
	if ufrag != "" {
		m.registerWorkerSession(sessionID, w, ufrag)
	}
	return rewritten
}

func (m *webRTCMux) rewriteUserSDP(raw string) string {
	rewritten, _ := rewriteRTCSessionSDP(raw, m.workerHost, m.listenPort)
	return rewritten
}

func (m *webRTCMux) rewriteWorkerICE(sessionID string, w *Worker, raw string) string {
	if raw == "" {
		return raw
	}
	if ufrag := candidateJSONUfrag(raw); ufrag != "" {
		m.registerWorkerSession(sessionID, w, ufrag)
	}
	return rewriteCandidateJSON(raw, m.publicHost, m.publicPort)
}

func (m *webRTCMux) rewriteUserICE(raw string) string {
	if raw == "" {
		return raw
	}
	return rewriteCandidateJSON(raw, m.workerHost, m.listenPort)
}

func (m *webRTCMux) registerWorkerSession(sessionID string, w *Worker, workerUfrag string) {
	if m == nil || w == nil || sessionID == "" || workerUfrag == "" || w.WebRTCPort == 0 {
		return
	}

	workerAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(m.workerHost, strconv.Itoa(w.WebRTCPort)))
	if err != nil {
		m.log.Warn().Err(err).Str("worker", w.Id().String()).Msg("WebRTC mux worker addr resolve failed")
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	route := m.routesBySession[sessionID]
	if route == nil {
		route = &webRTCMuxRoute{sessionID: sessionID, workerID: w.Id().String()}
		m.routesBySession[sessionID] = route
	}

	if route.workerUfrag != "" && route.workerUfrag != workerUfrag {
		delete(m.routesByUfrag, route.workerUfrag)
	}
	if route.workerAddr != nil && route.workerAddr.String() != workerAddr.String() {
		delete(m.routesByWorker, route.workerAddr.String())
	}
	if old := m.routesByWorker[workerAddr.String()]; old != nil && old != route {
		m.removeRouteLocked(old)
	}

	route.workerID = w.Id().String()
	route.workerUfrag = workerUfrag
	route.workerAddr = workerAddr
	route.updatedAt = time.Now()
	m.routesByUfrag[workerUfrag] = route
	m.routesByWorker[workerAddr.String()] = route
	m.log.Debug().
		Str("session", sessionID).
		Str("worker", w.Id().String()).
		Str("worker_addr", workerAddr.String()).
		Str("ufrag", workerUfrag).
		Msg("WebRTC mux registered worker route")
}

func (m *webRTCMux) unregisterSession(sessionID string) {
	if m == nil || sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if route := m.routesBySession[sessionID]; route != nil {
		m.removeRouteLocked(route)
	}
}

func (m *webRTCMux) unregisterWorker(w *Worker) {
	if m == nil || w == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, route := range m.routesBySession {
		if route.workerID == w.Id().String() {
			m.removeRouteLocked(route)
		}
	}
}

func (m *webRTCMux) removeRouteLocked(route *webRTCMuxRoute) {
	delete(m.routesBySession, route.sessionID)
	if route.workerUfrag != "" {
		delete(m.routesByUfrag, route.workerUfrag)
	}
	if route.workerAddr != nil {
		delete(m.routesByWorker, route.workerAddr.String())
	}
	if route.browserAddr != nil {
		delete(m.routesByBrowser, route.browserAddr.String())
	}
	for addr := range route.browserAddrs {
		delete(m.routesByBrowser, addr)
	}
}

func rewriteRTCSessionSDP(raw string, host string, port int) (string, string) {
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return raw, ""
	}

	sdp, ok := payload["sdp"].(string)
	if !ok || sdp == "" {
		return raw, ""
	}

	ufrag := extractSDPUfrag(sdp)
	payload["sdp"] = rewriteSDPCandidates(sdp, host, port)

	encoded, err := json.Marshal(payload)
	if err != nil {
		return raw, ufrag
	}
	return string(encoded), ufrag
}

func rewriteCandidateJSON(raw string, host string, port int) string {
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return raw
	}

	candidate, ok := payload["candidate"].(string)
	if !ok || candidate == "" {
		return raw
	}

	rewritten, ok := rewriteICECandidate(candidate, host, port)
	if !ok {
		return raw
	}

	payload["candidate"] = rewritten
	if host != "" {
		payload["address"] = host
	}
	if port > 0 {
		payload["port"] = port
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func candidateJSONUfrag(raw string) string {
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	if value, ok := payload["usernameFragment"].(string); ok {
		return value
	}
	if candidate, ok := payload["candidate"].(string); ok {
		return candidateAttribute(candidate, "ufrag")
	}
	return ""
}

func rewriteSDPCandidates(sdp string, host string, port int) string {
	lines := strings.SplitAfter(sdp, "\n")
	for i, line := range lines {
		body, suffix := trimLineEnding(line)
		if strings.HasPrefix(body, "a=candidate:") {
			if rewritten, ok := rewriteICECandidate(strings.TrimPrefix(body, "a="), host, port); ok {
				lines[i] = "a=" + rewritten + suffix
			}
			continue
		}
		if strings.HasPrefix(body, "candidate:") {
			if rewritten, ok := rewriteICECandidate(body, host, port); ok {
				lines[i] = rewritten + suffix
			}
		}
	}
	return strings.Join(lines, "")
}

func trimLineEnding(line string) (string, string) {
	if strings.HasSuffix(line, "\r\n") {
		return strings.TrimSuffix(line, "\r\n"), "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return strings.TrimSuffix(line, "\n"), "\n"
	}
	if strings.HasSuffix(line, "\r") {
		return strings.TrimSuffix(line, "\r"), "\r"
	}
	return line, ""
}

func rewriteICECandidate(candidate string, host string, port int) (string, bool) {
	fields := strings.Fields(candidate)
	if len(fields) < 6 || !strings.HasPrefix(fields[0], "candidate:") {
		return candidate, false
	}
	if !strings.EqualFold(fields[2], "udp") {
		return candidate, false
	}
	if host != "" {
		fields[4] = host
	}
	if port > 0 {
		fields[5] = strconv.Itoa(port)
	}

	out := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		switch field {
		case "typ":
			out = append(out, field)
			if i+1 < len(fields) {
				out = append(out, "host")
				i++
			}
		case "raddr", "rport":
			if i+1 < len(fields) {
				i++
			}
		default:
			out = append(out, field)
		}
	}
	return strings.Join(out, " "), true
}

func extractSDPUfrag(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=ice-ufrag:") {
			return strings.TrimPrefix(line, "a=ice-ufrag:")
		}
	}
	return ""
}

func candidateAttribute(candidate string, attr string) string {
	fields := strings.Fields(candidate)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == attr {
			return fields[i+1]
		}
	}
	return ""
}

func stunUsername(packet []byte) (string, bool) {
	if len(packet) < 20 || packet[0]&0xC0 != 0 {
		return "", false
	}
	if binary.BigEndian.Uint32(packet[4:8]) != stunMagicCookie {
		return "", false
	}

	msgLen := int(binary.BigEndian.Uint16(packet[2:4]))
	end := 20 + msgLen
	if msgLen < 0 || end > len(packet) {
		return "", false
	}

	for offset := 20; offset+4 <= end; {
		attrType := binary.BigEndian.Uint16(packet[offset : offset+2])
		attrLen := int(binary.BigEndian.Uint16(packet[offset+2 : offset+4]))
		offset += 4
		if offset+attrLen > end {
			return "", false
		}
		if attrType == stunAttrUsername {
			return string(packet[offset : offset+attrLen]), true
		}
		offset += (attrLen + 3) &^ 3
	}
	return "", false
}

func muxPacketKind(packet []byte) string {
	if len(packet) == 0 {
		return "empty"
	}
	if len(packet) >= 20 && packet[0]&0xC0 == 0 && binary.BigEndian.Uint32(packet[4:8]) == stunMagicCookie {
		return "stun"
	}
	if packet[0] >= 20 && packet[0] <= 63 {
		return "dtls"
	}
	if packet[0] >= 128 && packet[0] <= 191 {
		return "rtp-rtcp"
	}
	return fmt.Sprintf("0x%02x", packet[0])
}

func splitICEUsername(username string) (string, string) {
	first, second, ok := strings.Cut(username, ":")
	if !ok {
		return username, ""
	}
	return first, second
}
