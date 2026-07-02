package nanoarch

/*
#include <stdint.h>
#include <stdlib.h>
#include "libretro.h"

void bridge_netpacket_start(struct retro_netpacket_callback *cb, uint16_t client_id);
void bridge_netpacket_receive(struct retro_netpacket_callback *cb, const void *buf, size_t len, uint16_t client_id);
void bridge_netpacket_poll(struct retro_netpacket_callback *cb);
bool bridge_netpacket_connected(struct retro_netpacket_callback *cb, uint16_t client_id);
void bridge_netpacket_disconnected(struct retro_netpacket_callback *cb, uint16_t client_id);
void bridge_netpacket_stop(struct retro_netpacket_callback *cb);
*/
import "C"

import (
	"encoding/binary"
	"hash/fnv"
	"net"
	stdos "os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	netpacketBroadcast      uint16 = 0xffff
	netpacketHeaderLen             = 24
	netpacketMaxPayload            = 60 * 1024
	netpacketQueueSize             = 8192
	netpacketEventQueueSize        = 1024
	netpacketSocketBuffer          = 4 * 1024 * 1024
	netpacketFlagPresence   uint32 = 1 << 31
)

var netpacketMagic = [4]byte{'R', 'N', 'P', '1'}

type netpacketPacket struct {
	src  uint16
	data []byte
}

type netpacketEvent struct {
	clientID uint16
}

type netpacketState struct {
	cb       *C.struct_retro_netpacket_callback
	clientID uint16
	room     uint64
	roomName string
	hub      *net.UDPAddr
	conn     *net.UDPConn
	incoming chan netpacketPacket
	events   chan netpacketEvent
	peerMu   sync.Mutex
	peers    map[uint16]struct{}
	started  bool
	stats    netpacketStats
	lastLog  atomic.Int64
}

type netpacketStats struct {
	txPackets      atomic.Uint64
	txBytes        atomic.Uint64
	txControl      atomic.Uint64
	rxPackets      atomic.Uint64
	rxBytes        atomic.Uint64
	delivered      atomic.Uint64
	deliveredBytes atomic.Uint64
	droppedQueue   atomic.Uint64
	ignoredRoom    atomic.Uint64
	ignoredDst     atomic.Uint64
	ignoredSelf    atomic.Uint64
	ignoredControl atomic.Uint64
}

func (s *netpacketState) reset() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	*s = netpacketState{
		incoming: make(chan netpacketPacket, netpacketQueueSize),
		events:   make(chan netpacketEvent, netpacketEventQueueSize),
		peers:    make(map[uint16]struct{}),
	}
}

func (n *Nanoarch) setNetpacketCallback(data unsafe.Pointer) {
	if data == nil {
		return
	}
	n.netpacket.cb = new(C.struct_retro_netpacket_callback)
	*n.netpacket.cb = *(*C.struct_retro_netpacket_callback)(data)
	if n.netpacket.incoming == nil {
		n.netpacket.incoming = make(chan netpacketPacket, netpacketQueueSize)
	}
	if n.netpacket.events == nil {
		n.netpacket.events = make(chan netpacketEvent, netpacketEventQueueSize)
	}
	if n.netpacket.peers == nil {
		n.netpacket.peers = make(map[uint16]struct{})
	}
	n.log.Info().Msg("netpacket interface registered by core")
}

func (n *Nanoarch) startNetpacketFromEnv() {
	hub := firstEnv("MELONDS_NETPLAY_HUB", "REBIT_MELONDS_NETPLAY_HUB")
	if hub == "" || n.netpacket.cb == nil {
		return
	}

	idRaw := firstEnv("MELONDS_NETPLAY_CLIENT_ID", "REBIT_MELONDS_NETPLAY_CLIENT_ID")
	id, err := strconv.ParseUint(idRaw, 10, 16)
	if err != nil || id == uint64(netpacketBroadcast) {
		n.log.Error().Str("client_id", idRaw).Msg("invalid melonDS netplay client id")
		return
	}

	hubAddr, err := net.ResolveUDPAddr("udp", hub)
	if err != nil {
		n.log.Error().Err(err).Str("hub", hub).Msg("invalid melonDS netplay hub")
		return
	}

	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		n.log.Error().Err(err).Msg("melonDS netplay UDP listen failed")
		return
	}
	_ = conn.SetReadBuffer(netpacketSocketBuffer)
	_ = conn.SetWriteBuffer(netpacketSocketBuffer)

	room := n.netpacketRoomName
	if room == "" {
		room = firstEnv("MELONDS_NETPLAY_ROOM", "REBIT_MELONDS_NETPLAY_ROOM")
	}
	if room == "" {
		room = "default"
	}

	n.netpacket.clientID = uint16(id)
	n.netpacket.room = hashRoom(room)
	n.netpacket.roomName = room
	n.netpacket.hub = hubAddr
	n.netpacket.conn = conn
	if n.netpacket.incoming == nil {
		n.netpacket.incoming = make(chan netpacketPacket, netpacketQueueSize)
	}
	if n.netpacket.events == nil {
		n.netpacket.events = make(chan netpacketEvent, netpacketEventQueueSize)
	}
	if n.netpacket.peers == nil {
		n.netpacket.peers = make(map[uint16]struct{})
	}

	go n.readNetpacketLoop()

	n.netpacket.started = true
	C.bridge_netpacket_start(n.netpacket.cb, C.uint16_t(n.netpacket.clientID))
	n.log.Info().
		Uint16("client_id", n.netpacket.clientID).
		Str("room", room).
		Uint64("room_hash", n.netpacket.room).
		Str("hub", hubAddr.String()).
		Msg("melonDS netpacket bridge started")

	go n.sendNetpacketPresenceLoop()
}

func (n *Nanoarch) stopNetpacket() {
	n.logNetpacketStats(true)
	if n.netpacket.started && n.netpacket.cb != nil {
		C.bridge_netpacket_stop(n.netpacket.cb)
	}
	if n.netpacket.conn != nil {
		_ = n.netpacket.conn.Close()
	}
	n.netpacket.started = false
	n.netpacket.conn = nil
	n.netpacket.hub = nil
	n.netpacket.cb = nil
}

func (n *Nanoarch) readNetpacketLoop() {
	buf := make([]byte, netpacketHeaderLen+netpacketMaxPayload)
	for {
		nr, _, err := n.netpacket.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		room, src, dst, flags, payload, ok := decodeNetpacket(buf[:nr])
		if !ok {
			continue
		}
		if room != n.netpacket.room {
			n.netpacket.stats.ignoredRoom.Add(1)
			n.logNetpacketStats(false)
			continue
		}
		if src == n.netpacket.clientID {
			n.netpacket.stats.ignoredSelf.Add(1)
			n.logNetpacketStats(false)
			continue
		}
		if dst != n.netpacket.clientID && dst != netpacketBroadcast {
			n.netpacket.stats.ignoredDst.Add(1)
			n.logNetpacketStats(false)
			continue
		}
		if len(payload) == 0 {
			if flags&netpacketFlagPresence != 0 {
				n.queueNetpacketConnected(src)
			}
			n.netpacket.stats.ignoredControl.Add(1)
			n.logNetpacketStats(false)
			continue
		}
		n.queueNetpacketConnected(src)
		n.netpacket.stats.rxPackets.Add(1)
		n.netpacket.stats.rxBytes.Add(uint64(len(payload)))
		data := append([]byte(nil), payload...)
		n.queueNetpacket(netpacketPacket{src: src, data: data})
		n.logNetpacketStats(false)
	}
}

func (n *Nanoarch) sendNetpacketPresenceLoop() {
	if n.netpacket.clientID == 0 {
		n.sendNetpacketToHub(netpacketFlagPresence, nil, netpacketBroadcast)
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range 120 {
		n.sendNetpacketToHub(netpacketFlagPresence, nil, 0)
		if !n.netpacket.started || n.netpacket.conn == nil {
			return
		}
		<-ticker.C
	}
}

func (n *Nanoarch) queueNetpacket(packet netpacketPacket) {
	select {
	case n.netpacket.incoming <- packet:
		return
	default:
	}

	select {
	case <-n.netpacket.incoming:
		n.netpacket.stats.droppedQueue.Add(1)
	default:
	}

	select {
	case n.netpacket.incoming <- packet:
	default:
		n.netpacket.stats.droppedQueue.Add(1)
		n.log.Warn().Msg("dropping melonDS netpacket: receive queue full")
	}
}

func (n *Nanoarch) queueNetpacketConnected(clientID uint16) {
	if n.netpacket.clientID != 0 || clientID == 0 || clientID == netpacketBroadcast {
		return
	}

	n.netpacket.peerMu.Lock()
	if _, ok := n.netpacket.peers[clientID]; ok {
		n.netpacket.peerMu.Unlock()
		return
	}
	n.netpacket.peers[clientID] = struct{}{}
	n.netpacket.peerMu.Unlock()

	select {
	case n.netpacket.events <- netpacketEvent{clientID: clientID}:
	default:
		n.log.Warn().Uint16("client_id", clientID).Msg("dropping melonDS netpacket connected event: event queue full")
	}
}

func (n *Nanoarch) sendNetpacketToHub(flags uint32, payload []byte, dst uint16) {
	if n.netpacket.conn == nil || n.netpacket.hub == nil {
		return
	}
	if len(payload) == 0 && flags&netpacketFlagPresence == 0 {
		return
	}
	if len(payload) > netpacketMaxPayload {
		n.log.Warn().Int("bytes", len(payload)).Msg("dropping oversized melonDS netpacket")
		return
	}
	packet := encodeNetpacket(n.netpacket.room, n.netpacket.clientID, dst, flags, payload)
	if _, err := n.netpacket.conn.WriteToUDP(packet, n.netpacket.hub); err != nil {
		n.log.Error().Err(err).Msg("melonDS netpacket send failed")
		return
	}
	if len(payload) == 0 {
		n.netpacket.stats.txControl.Add(1)
	} else {
		n.netpacket.stats.txPackets.Add(1)
		n.netpacket.stats.txBytes.Add(uint64(len(payload)))
	}
	n.logNetpacketStats(false)
}

func (n *Nanoarch) pollNetpacketReceive() {
	if !n.netpacket.started || n.netpacket.cb == nil {
		return
	}

	for {
		select {
		case packet := <-n.netpacket.incoming:
			if len(packet.data) == 0 {
				continue
			}
			ptr := C.CBytes(packet.data)
			C.bridge_netpacket_receive(n.netpacket.cb, ptr, C.size_t(len(packet.data)), C.uint16_t(packet.src))
			C.free(ptr)
			n.netpacket.stats.delivered.Add(1)
			n.netpacket.stats.deliveredBytes.Add(uint64(len(packet.data)))
		default:
			n.logNetpacketStats(false)
			return
		}
	}
}

func (n *Nanoarch) pollNetpacketEvents() {
	if !n.netpacket.started || n.netpacket.cb == nil || n.netpacket.clientID != 0 {
		return
	}

	for {
		select {
		case event := <-n.netpacket.events:
			if ok := C.bridge_netpacket_connected(n.netpacket.cb, C.uint16_t(event.clientID)); !ok {
				n.log.Warn().Uint16("client_id", event.clientID).Msg("melonDS netpacket peer rejected by core")
			}
		default:
			return
		}
	}
}

func (n *Nanoarch) pollNetpacket() {
	if !n.netpacket.started || n.netpacket.cb == nil {
		return
	}

	n.pollNetpacketEvents()
	C.bridge_netpacket_poll(n.netpacket.cb)
	n.pollNetpacketReceive()
}

//export coreNetpacketSend
func coreNetpacketSend(flags C.int, buf unsafe.Pointer, length C.size_t, clientID C.uint16_t) {
	if buf == nil || length == 0 {
		Nan0.sendNetpacketToHub(uint32(flags), nil, uint16(clientID))
		return
	}
	if length > netpacketMaxPayload {
		Nan0.log.Warn().Uint64("bytes", uint64(length)).Msg("dropping oversized melonDS netpacket")
		return
	}
	Nan0.sendNetpacketToHub(uint32(flags), C.GoBytes(buf, C.int(length)), uint16(clientID))
}

//export coreNetpacketPollReceive
func coreNetpacketPollReceive() {
	Nan0.pollNetpacketReceive()
}

func (n *Nanoarch) logNetpacketStats(force bool) {
	if n.netpacket.clientID == 0 {
		return
	}
	if !force {
		now := time.Now().Unix()
		last := n.netpacket.lastLog.Load()
		if now == last || !n.netpacket.lastLog.CompareAndSwap(last, now) {
			return
		}
	}

	txPackets := n.netpacket.stats.txPackets.Swap(0)
	txBytes := n.netpacket.stats.txBytes.Swap(0)
	txControl := n.netpacket.stats.txControl.Swap(0)
	rxPackets := n.netpacket.stats.rxPackets.Swap(0)
	rxBytes := n.netpacket.stats.rxBytes.Swap(0)
	delivered := n.netpacket.stats.delivered.Swap(0)
	deliveredBytes := n.netpacket.stats.deliveredBytes.Swap(0)
	droppedQueue := n.netpacket.stats.droppedQueue.Swap(0)
	ignoredRoom := n.netpacket.stats.ignoredRoom.Swap(0)
	ignoredDst := n.netpacket.stats.ignoredDst.Swap(0)
	ignoredSelf := n.netpacket.stats.ignoredSelf.Swap(0)
	ignoredControl := n.netpacket.stats.ignoredControl.Swap(0)

	if !force && txPackets+txControl+rxPackets+delivered+droppedQueue+ignoredRoom+ignoredDst+ignoredSelf+ignoredControl == 0 {
		return
	}

	n.log.Info().
		Uint16("client_id", n.netpacket.clientID).
		Str("room", n.netpacket.roomName).
		Uint64("room_hash", n.netpacket.room).
		Uint64("tx_packets", txPackets).
		Uint64("tx_bytes", txBytes).
		Uint64("tx_control", txControl).
		Uint64("rx_packets", rxPackets).
		Uint64("rx_bytes", rxBytes).
		Uint64("delivered_packets", delivered).
		Uint64("delivered_bytes", deliveredBytes).
		Uint64("dropped_queue", droppedQueue).
		Uint64("ignored_room", ignoredRoom).
		Uint64("ignored_dst", ignoredDst).
		Uint64("ignored_self", ignoredSelf).
		Uint64("ignored_control", ignoredControl).
		Msg("melonDS netpacket stats")
}

func encodeNetpacket(room uint64, src uint16, dst uint16, flags uint32, payload []byte) []byte {
	packet := make([]byte, netpacketHeaderLen+len(payload))
	copy(packet[:4], netpacketMagic[:])
	binary.LittleEndian.PutUint64(packet[4:12], room)
	binary.LittleEndian.PutUint16(packet[12:14], src)
	binary.LittleEndian.PutUint16(packet[14:16], dst)
	binary.LittleEndian.PutUint32(packet[16:20], flags)
	binary.LittleEndian.PutUint32(packet[20:24], uint32(len(payload)))
	copy(packet[netpacketHeaderLen:], payload)
	return packet
}

func decodeNetpacket(packet []byte) (room uint64, src uint16, dst uint16, flags uint32, payload []byte, ok bool) {
	if len(packet) < netpacketHeaderLen || string(packet[:4]) != string(netpacketMagic[:]) {
		return 0, 0, 0, 0, nil, false
	}
	room = binary.LittleEndian.Uint64(packet[4:12])
	src = binary.LittleEndian.Uint16(packet[12:14])
	dst = binary.LittleEndian.Uint16(packet[14:16])
	flags = binary.LittleEndian.Uint32(packet[16:20])
	payloadLen := int(binary.LittleEndian.Uint32(packet[20:24]))
	if payloadLen < 0 || payloadLen > len(packet)-netpacketHeaderLen {
		return 0, 0, 0, 0, nil, false
	}
	return room, src, dst, flags, packet[netpacketHeaderLen : netpacketHeaderLen+payloadLen], true
}

func hashRoom(room string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(room))
	return h.Sum64()
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := stdos.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}
