package main

import (
	"encoding/binary"
	"flag"
	"log"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	headerLen      = 24
	maxPacketSize  = headerLen + 60*1024
	defaultAddress = "127.0.0.1:55355"
	broadcastID    = 0xffff
)

var magic = [4]byte{'R', 'N', 'P', '1'}

type peerKey struct {
	room uint64
	id   uint16
}

type packet struct {
	room uint64
	src  uint16
	dst  uint16
	data []byte
}

type peer struct {
	addr *net.UDPAddr
	seen time.Time
}

type roomStats struct {
	rxPackets        uint64
	rxBytes          uint64
	controlPackets   uint64
	broadcastPackets uint64
	directPackets    uint64
	forwardedPackets uint64
	forwardedBytes   uint64
}

func main() {
	addr := flag.String("address", defaultAddress, "UDP listen address")
	flag.Parse()

	udpAddr, err := net.ResolveUDPAddr("udp", *addr)
	if err != nil {
		log.Fatalf("resolve %s: %v", *addr, err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	defer conn.Close()

	log.Printf("melonDS netpacket hub listening on %s", conn.LocalAddr())

	var (
		mu      sync.Mutex
		peers   = map[peerKey]peer{}
		stats   = map[uint64]*roomStats{}
		lastLog = map[uint64]time.Time{}
	)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-2 * time.Minute)
			mu.Lock()
			for key, peer := range peers {
				if peer.seen.Before(cutoff) {
					delete(peers, key)
				}
			}
			mu.Unlock()
		}
	}()

	buf := make([]byte, maxPacketSize)
	for {
		n, srcAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("read: %v", err)
			continue
		}

		p, ok := decode(buf[:n])
		if !ok || p.src == 0 || p.src == broadcastID {
			continue
		}

		now := time.Now()
		mu.Lock()
		key := peerKey{room: p.room, id: p.src}
		oldPeer, knownPeer := peers[key]
		peers[key] = peer{addr: srcAddr, seen: now}
		if !knownPeer || oldPeer.addr.String() != srcAddr.String() {
			log.Printf("peer room=%x id=%d addr=%s known=%v", p.room, p.src, srcAddr, knownPeer)
		}

		rs := stats[p.room]
		if rs == nil {
			rs = &roomStats{}
			stats[p.room] = rs
		}
		if len(p.data) == 0 {
			rs.controlPackets++
		} else {
			rs.rxPackets++
			rs.rxBytes += uint64(len(p.data))
			if p.dst == broadcastID {
				rs.broadcastPackets++
			} else {
				rs.directPackets++
			}
		}

		var targets []*net.UDPAddr
		if len(p.data) > 0 {
			for key, peer := range peers {
				if key.room != p.room || key.id == p.src {
					continue
				}
				if p.dst != broadcastID && p.dst != key.id {
					continue
				}
				targets = append(targets, peer.addr)
			}
			rs.forwardedPackets += uint64(len(targets))
			rs.forwardedBytes += uint64(len(targets) * len(p.data))
		}
		shouldLog := now.Sub(lastLog[p.room]) >= time.Second
		var snapshot roomStats
		var peerIDs []int
		if shouldLog {
			snapshot = *rs
			*rs = roomStats{}
			lastLog[p.room] = now
			for key := range peers {
				if key.room == p.room {
					peerIDs = append(peerIDs, int(key.id))
				}
			}
			sort.Ints(peerIDs)
		}
		mu.Unlock()

		for _, target := range targets {
			if _, err := conn.WriteToUDP(buf[:n], target); err != nil {
				log.Printf("forward room=%x src=%d dst=%d to=%s: %v", p.room, p.src, p.dst, target, err)
			}
		}
		if shouldLog {
			log.Printf(
				"stats room=%x peers=%v rx=%d rx_bytes=%d control=%d broadcast=%d direct=%d forwarded=%d forwarded_bytes=%d",
				p.room,
				peerIDs,
				snapshot.rxPackets,
				snapshot.rxBytes,
				snapshot.controlPackets,
				snapshot.broadcastPackets,
				snapshot.directPackets,
				snapshot.forwardedPackets,
				snapshot.forwardedBytes,
			)
		}
	}
}

func decode(raw []byte) (packet, bool) {
	if len(raw) < headerLen || string(raw[:4]) != string(magic[:]) {
		return packet{}, false
	}

	payloadLen := int(binary.LittleEndian.Uint32(raw[20:24]))
	if payloadLen < 0 || payloadLen > len(raw)-headerLen {
		return packet{}, false
	}

	return packet{
		room: binary.LittleEndian.Uint64(raw[4:12]),
		src:  binary.LittleEndian.Uint16(raw[12:14]),
		dst:  binary.LittleEndian.Uint16(raw[14:16]),
		data: raw[headerLen : headerLen+payloadLen],
	}, true
}
