package media

import (
	"testing"
	"time"
)

func TestNDSLatencyWatermarkEnabled(t *testing.T) {
	t.Setenv("NDS_LATENCY_WATERMARK_ENABLED", "")
	t.Setenv("CLOUD_GAME_NDS_LATENCY_WATERMARK_ENABLED", "")
	if ndsLatencyWatermarkEnabled() {
		t.Fatal("watermark should be disabled by default")
	}

	t.Setenv("CLOUD_GAME_NDS_LATENCY_WATERMARK_ENABLED", "yes")
	if !ndsLatencyWatermarkEnabled() {
		t.Fatal("watermark should accept true-like alias env")
	}
}

func TestNDSTimestampWatermarkPayload(t *testing.T) {
	const unixMs = int64(1_777_777_777_123)
	payload := ndsLatencyWatermarkPayload(unixMs)

	if got := uint16(payload[0])<<8 | uint16(payload[1]); got != ndsLatencyWatermarkMagic {
		t.Fatalf("magic = %#x, want %#x", got, ndsLatencyWatermarkMagic)
	}

	var decoded uint64
	for _, b := range payload[2:8] {
		decoded = (decoded << 8) | uint64(b)
	}
	if int64(decoded) != unixMs {
		t.Fatalf("timestamp = %d, want %d", decoded, unixMs)
	}

	var checksum byte
	for _, b := range payload[:len(payload)-1] {
		checksum ^= b
	}
	if payload[len(payload)-1] != checksum {
		t.Fatalf("checksum = %#x, want %#x", payload[len(payload)-1], checksum)
	}
}

func TestDrawNDSTimestampWatermarkBGRx(t *testing.T) {
	const unixMs = int64(1_777_777_777_123)
	w, h := 128, 64
	stride := w * 4
	frame := make([]byte, stride*h)

	if !drawNDSTimestampWatermark(frame, w, h, stride, pixFmtBGRx, unixMs) {
		t.Fatal("drawNDSTimestampWatermark returned false")
	}

	payload := ndsLatencyWatermarkPayload(unixMs)
	for bit := 0; bit < ndsLatencyWatermarkBits; bit++ {
		x := ndsLatencyWatermarkOriginX + (bit%ndsLatencyWatermarkCols)*ndsLatencyWatermarkCell + ndsLatencyWatermarkCell/2
		y := ndsLatencyWatermarkOriginY + (bit/ndsLatencyWatermarkCols)*ndsLatencyWatermarkCell + ndsLatencyWatermarkCell/2
		off := y*stride + x*4
		wantOn := ((payload[bit/8] >> uint(7-bit%8)) & 1) == 1
		gotOn := frame[off] > 128 && frame[off+1] > 128 && frame[off+2] > 128
		if gotOn != wantOn {
			t.Fatalf("bit %d = %v, want %v", bit, gotOn, wantOn)
		}
	}
}

func TestDrawNDSTimestampWatermarkRGB16(t *testing.T) {
	w, h := 128, 64
	stride := w * 2
	frame := make([]byte, stride*h)

	if !drawNDSTimestampWatermark(frame, w, h, stride, pixFmtRGB16, 1_777_777_777_123) {
		t.Fatal("drawNDSTimestampWatermark returned false")
	}

	off := ndsLatencyWatermarkOriginY*stride + (ndsLatencyWatermarkOriginX+ndsLatencyWatermarkCell)*2
	if frame[off] == 0 && frame[off+1] == 0 {
		t.Fatal("expected second RGB16 watermark cell to be white for magic bit 1")
	}
}

func TestCloneWithNDSTimestampWatermarkDoesNotMutateInput(t *testing.T) {
	w, h := 128, 64
	stride := w * 4
	frame := make([]byte, stride*h)
	for i := range frame {
		frame[i] = 0x44
	}

	cloned := cloneWithNDSTimestampWatermark(frame, w, h, stride, pixFmtBGRx, time.UnixMilli(1_777_777_777_123))
	if len(cloned) != len(frame) {
		t.Fatalf("clone len = %d, want %d", len(cloned), len(frame))
	}
	if &cloned[0] == &frame[0] {
		t.Fatal("clone should not share backing array")
	}
	for i, b := range frame {
		if b != 0x44 {
			t.Fatalf("input mutated at %d: %#x", i, b)
		}
	}
	if string(cloned) == string(frame) {
		t.Fatal("clone did not receive a watermark")
	}
}
