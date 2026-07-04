package media

import (
	"os"
	"strings"
	"time"
)

const (
	ndsLatencyWatermarkMagic   uint16 = 0x4e44
	ndsLatencyWatermarkOriginX        = 4
	ndsLatencyWatermarkOriginY        = 4
	ndsLatencyWatermarkCell           = 8
	ndsLatencyWatermarkCols           = 12
	ndsLatencyWatermarkRows           = 6
	ndsLatencyWatermarkBits           = ndsLatencyWatermarkCols * ndsLatencyWatermarkRows
	ndsLatencyWatermarkBytes          = ndsLatencyWatermarkBits / 8
)

func ndsLatencyWatermarkEnabled() bool {
	for _, key := range []string{"NDS_LATENCY_WATERMARK_ENABLED", "CLOUD_GAME_NDS_LATENCY_WATERMARK_ENABLED"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
		case "1", "t", "true", "y", "yes", "on":
			return true
		}
	}
	return false
}

func cloneWithNDSTimestampWatermark(data []byte, w, h, stride int, pixFmt uint32, now time.Time) []byte {
	cloned := make([]byte, len(data))
	copy(cloned, data)
	drawNDSTimestampWatermark(cloned, w, h, stride, pixFmt, now.UnixMilli())
	return cloned
}

func drawNDSTimestampWatermark(data []byte, w, h, stride int, pixFmt uint32, unixMs int64) bool {
	if stride <= 0 || w <= 0 || h <= 0 {
		return false
	}
	if ndsLatencyWatermarkOriginX+ndsLatencyWatermarkCols*ndsLatencyWatermarkCell > w ||
		ndsLatencyWatermarkOriginY+ndsLatencyWatermarkRows*ndsLatencyWatermarkCell > h {
		return false
	}

	bpp := 4
	if pixFmt == pixFmtRGB16 {
		bpp = 2
	}
	if stride < w*bpp || len(data) < stride*h {
		return false
	}

	payload := ndsLatencyWatermarkPayload(unixMs)
	for bit := 0; bit < ndsLatencyWatermarkBits; bit++ {
		row := bit / ndsLatencyWatermarkCols
		col := bit % ndsLatencyWatermarkCols
		on := ((payload[bit/8] >> uint(7-bit%8)) & 1) == 1
		drawNDSTimestampWatermarkCell(data, stride, pixFmt, col, row, on)
	}
	return true
}

func ndsLatencyWatermarkPayload(unixMs int64) [ndsLatencyWatermarkBytes]byte {
	var payload [ndsLatencyWatermarkBytes]byte
	payload[0] = byte(ndsLatencyWatermarkMagic >> 8)
	payload[1] = byte(ndsLatencyWatermarkMagic & 0xff)
	ts := uint64(unixMs) & ((uint64(1) << 48) - 1)
	for i := 0; i < 6; i++ {
		payload[2+i] = byte(ts >> uint(40-8*i))
	}
	var checksum byte
	for _, b := range payload[:len(payload)-1] {
		checksum ^= b
	}
	payload[len(payload)-1] = checksum
	return payload
}

func drawNDSTimestampWatermarkCell(data []byte, stride int, pixFmt uint32, col, row int, on bool) {
	x0 := ndsLatencyWatermarkOriginX + col*ndsLatencyWatermarkCell
	y0 := ndsLatencyWatermarkOriginY + row*ndsLatencyWatermarkCell
	for y := y0; y < y0+ndsLatencyWatermarkCell; y++ {
		for x := x0; x < x0+ndsLatencyWatermarkCell; x++ {
			switch pixFmt {
			case pixFmtRGB16:
				off := y*stride + x*2
				if on {
					data[off], data[off+1] = 0xff, 0xff
				} else {
					data[off], data[off+1] = 0x00, 0x00
				}
			default:
				off := y*stride + x*4
				if on {
					data[off], data[off+1], data[off+2], data[off+3] = 0xff, 0xff, 0xff, 0xff
				} else {
					data[off], data[off+1], data[off+2], data[off+3] = 0x00, 0x00, 0x00, 0xff
				}
			}
		}
	}
}
