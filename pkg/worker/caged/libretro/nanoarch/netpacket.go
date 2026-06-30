package nanoarch

/*
#include <stdint.h>
#include <stdlib.h>
#include "libretro.h"

void bridge_netpacket_start(struct retro_netpacket_callback *cb, uint16_t client_id);
void bridge_netpacket_receive(struct retro_netpacket_callback *cb, const void *buf, size_t len, uint16_t client_id);
void bridge_netpacket_stop(struct retro_netpacket_callback *cb);
*/
import "C"

import (
	"encoding/binary"
	"hash/fnv"
	"net"
	stdos "os"
	"strconv"
	"time"
	"unsafe"
)

const (
	netpacketBroadcast  uint16 = 0xffff
	netpacketHeaderLen         = 24
	netpacketMaxPayload        = 60 * 1024
)

var netpacketMagic = [4]byte{'R', 'N', 'P', '1'}

type netpacketPacket struct {
	src  uint16
	data []byte
}

type netpacketState struct {
	cb       *C.struct_retro_netpacket_callback
	clientID uint16
	room     uint64
	hub      *net.UDPAddr
	conn     *net.UDPConn
	incoming chan netpacketPacket
	started  bool
}

func (s *netpacketState) reset() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	*s = netpacketState{incoming: make(chan netpacketPacket, 1024)}
}

func (n *Nanoarch) setNetpacketCallback(data unsafe.Pointer) {
	if data == nil {
		return
	}
	n.netpacket.cb = new(C.struct_retro_netpacket_callback)
	*n.netpacket.cb = *(*C.struct_retro_netpacket_callback)(data)
	if n.netpacket.incoming == nil {
		n.netpacket.incoming = make(chan netpacketPacket, 1024)
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
	if err != nil || id == 0 || id == uint64(netpacketBroadcast) {
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

	room := firstEnv("MELONDS_NETPLAY_ROOM", "REBIT_MELONDS_NETPLAY_ROOM")
	if room == "" {
		room = "default"
	}

	n.netpacket.clientID = uint16(id)
	n.netpacket.room = hashRoom(room)
	n.netpacket.hub = hubAddr
	n.netpacket.conn = conn
	if n.netpacket.incoming == nil {
		n.netpacket.incoming = make(chan netpacketPacket, 1024)
	}

	go n.readNetpacketLoop()

	C.bridge_netpacket_start(n.netpacket.cb, C.uint16_t(n.netpacket.clientID))
	n.netpacket.started = true
	n.log.Info().
		Uint16("client_id", n.netpacket.clientID).
		Str("room", room).
		Str("hub", hubAddr.String()).
		Msg("melonDS netpacket bridge started")

	for range 3 {
		n.sendNetpacketToHub(0, nil, n.netpacket.clientID)
		time.Sleep(10 * time.Millisecond)
	}
}

func (n *Nanoarch) stopNetpacket() {
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
		room, src, dst, payload, ok := decodeNetpacket(buf[:nr])
		if !ok || room != n.netpacket.room || src == n.netpacket.clientID {
			continue
		}
		if dst != n.netpacket.clientID && dst != netpacketBroadcast {
			continue
		}
		if len(payload) == 0 {
			continue
		}
		data := append([]byte(nil), payload...)
		select {
		case n.netpacket.incoming <- netpacketPacket{src: src, data: data}:
		default:
			n.log.Warn().Msg("dropping melonDS netpacket: receive queue full")
		}
	}
}

func (n *Nanoarch) sendNetpacketToHub(flags int, payload []byte, dst uint16) {
	if n.netpacket.conn == nil || n.netpacket.hub == nil {
		return
	}
	if len(payload) > netpacketMaxPayload {
		n.log.Warn().Int("bytes", len(payload)).Msg("dropping oversized melonDS netpacket")
		return
	}
	packet := encodeNetpacket(n.netpacket.room, n.netpacket.clientID, dst, uint32(flags), payload)
	if _, err := n.netpacket.conn.WriteToUDP(packet, n.netpacket.hub); err != nil {
		n.log.Error().Err(err).Msg("melonDS netpacket send failed")
	}
}

func (n *Nanoarch) pollNetpacketReceive() {
	for {
		select {
		case packet := <-n.netpacket.incoming:
			if len(packet.data) == 0 || n.netpacket.cb == nil {
				continue
			}
			ptr := C.CBytes(packet.data)
			C.bridge_netpacket_receive(n.netpacket.cb, ptr, C.size_t(len(packet.data)), C.uint16_t(packet.src))
			C.free(ptr)
		default:
			return
		}
	}
}

//export coreNetpacketSend
func coreNetpacketSend(flags C.int, buf unsafe.Pointer, length C.size_t, clientID C.uint16_t) {
	if buf == nil || length == 0 {
		return
	}
	if length > netpacketMaxPayload {
		Nan0.log.Warn().Uint64("bytes", uint64(length)).Msg("dropping oversized melonDS netpacket")
		return
	}
	Nan0.sendNetpacketToHub(int(flags), C.GoBytes(buf, C.int(length)), uint16(clientID))
}

//export coreNetpacketPollReceive
func coreNetpacketPollReceive() {
	Nan0.pollNetpacketReceive()
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

func decodeNetpacket(packet []byte) (room uint64, src uint16, dst uint16, payload []byte, ok bool) {
	if len(packet) < netpacketHeaderLen || string(packet[:4]) != string(netpacketMagic[:]) {
		return 0, 0, 0, nil, false
	}
	room = binary.LittleEndian.Uint64(packet[4:12])
	src = binary.LittleEndian.Uint16(packet[12:14])
	dst = binary.LittleEndian.Uint16(packet[14:16])
	payloadLen := int(binary.LittleEndian.Uint32(packet[20:24]))
	if payloadLen < 0 || payloadLen > len(packet)-netpacketHeaderLen {
		return 0, 0, 0, nil, false
	}
	return room, src, dst, packet[netpacketHeaderLen : netpacketHeaderLen+payloadLen], true
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
