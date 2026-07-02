package media

import (
	"strings"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

var testLog = logger.New(false)

func TestRound(t *testing.T) {
	tests := []struct {
		x     int
		scale float64
		want  int
	}{
		{640, 1.0, 640},
		{640, 2.0, 1280},
		{640, 1.5, 960},
		{640, 0.5, 320},
		{481, 1.0, 482}, // odd rounds up to even
		{481, 2.0, 962},
		{0, 2.0, 0},
	}
	for _, tt := range tests {
		got := round(tt.x, tt.scale)
		if got != tt.want {
			t.Errorf("round(%d, %v) = %d, want %d", tt.x, tt.scale, got, tt.want)
		}
	}
}

func TestBuilder(t *testing.T) {
	var b builder
	b.write("abc ")
	b.writef("x=%d ", 1)
	b.writeIfSet("")
	b.writeIfSet("! foo")

	s := b.String()
	if !strings.Contains(s, "abc") {
		t.Errorf("missing 'abc' in %q", s)
	}
	if !strings.Contains(s, "x=1") {
		t.Errorf("missing 'x=1' in %q", s)
	}
	if !strings.Contains(s, "! foo") {
		t.Errorf("missing '! foo' in %q", s)
	}
	if strings.Contains(s, "!!") {
		t.Errorf("writeIfSet wrote empty string: %q", s)
	}
}

func TestNewGstreamerDefaults(t *testing.T) {
	g := NewGstreamer(config.Encoder{}, testLog)
	if g == nil {
		t.Fatal("NewGstreamer returned nil")
	}
	if g.bpp != 4 {
		t.Errorf("bpp = %d, want 4", g.bpp)
	}
	if g.vidFmt == 0 {
		t.Error("vidFmt should not be zero (BGRx)")
	}
}

func TestSetRot(t *testing.T) {
	g := NewGstreamer(config.Encoder{}, testLog)
	g.SetRot(90)
	if g.oldRot != 90 {
		t.Errorf("oldRot = %d, want 90", g.oldRot)
	}
	g.SetRot(180)
	if g.oldRot != 180 {
		t.Errorf("oldRot = %d, want 180", g.oldRot)
	}
}

func TestPixFmtMapping(t *testing.T) {
	if pixFmtToGst[pixFmtBGRx] != "BGRx" {
		t.Errorf("BGRx format mismatch: %q", pixFmtToGst[pixFmtBGRx])
	}
	if pixFmtToGst[pixFmtBGRA] != "BGRx" {
		t.Errorf("XRGB8888 format mismatch: %q", pixFmtToGst[pixFmtBGRA])
	}
	if pixFmtToGst[pixFmtRGB16] != "RGB16" {
		t.Errorf("RGB16 format mismatch: %q", pixFmtToGst[pixFmtRGB16])
	}
}

var (
	testVideoCodec = config.Encoder{
		Video: config.Video{Codec: "vp8"},
		List:  map[string]config.CodecSettings{"vp8": {}},
	}
	testAudioCodec = config.Encoder{
		Audio: config.Audio{Codec: "opus"},
		List:  map[string]config.CodecSettings{"opus": {}},
	}
	testFullCodec = config.Encoder{
		Video: config.Video{Codec: "vp8"},
		Audio: config.Audio{Codec: "opus"},
		List:  map[string]config.CodecSettings{"vp8": {}, "opus": {}},
	}
)

func TestBuildVideoPipeline(t *testing.T) {
	p, err := buildVideoPipeline(640, 480, 640, 480, pixFmtBGRx, "bilinear2", 0, testVideoCodec, testLog, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	p.destroy()
}

func TestBuildAudioPipeline(t *testing.T) {
	p, err := buildAudioPipeline(48000, testAudioCodec, testLog)
	if err != nil {
		t.Fatal(err)
	}
	p.destroy()
}

func TestInitAndDestroy(t *testing.T) {
	g := NewGstreamer(testFullCodec, testLog)
	g.VideoW, g.VideoH = 64, 64
	g.VideoScale = 1
	g.AudioSrcHz = 48000

	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	g.Destroy()
}

func TestProcessVideo(t *testing.T) {
	g := NewGstreamer(testFullCodec, testLog)
	g.VideoW, g.VideoH = 64, 64
	g.VideoScale = 1
	g.AudioSrcHz = 48000

	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	defer g.Destroy()

	// Build a 64×64 BGRx solid-blue frame.
	w, h := 64, 64
	stride := w * 4
	frame := make([]byte, stride*h)
	for i := range h {
		for j := range w {
			off := i*stride + j*4
			frame[off+0] = 255 // B
			frame[off+1] = 0   // G
			frame[off+2] = 0   // R
			frame[off+3] = 255 // x
		}
	}

	done := make(chan struct{})
	g.ProcessVideo(Video{
		Frame:    RawFrame{Data: frame, W: w, H: h, Stride: stride},
		Duration: 16 * time.Millisecond,
	}, func(data []byte, dur time.Duration) {
		if len(data) == 0 {
			t.Error("encoded video is empty")
		}
		if dur != 16*time.Millisecond {
			t.Errorf("duration = %v, want 16ms", dur)
		}
		close(done)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for encoded video")
	}
}

func TestProcessAudio(t *testing.T) {
	g := NewGstreamer(testFullCodec, testLog)
	g.VideoW, g.VideoH = 64, 64
	g.VideoScale = 1
	g.AudioSrcHz = 48000

	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	defer g.Destroy()

	// Feed 20ms of 48kHz S16LE stereo silence → 1920 samples = 3840 bytes.
	samples := 48000 * 2 * 20 / 1000
	audio := make([]byte, samples*2) // S16LE = 2 bytes per sample

	done := make(chan struct{})
	g.ProcessAudio(audio, func(data []byte, dur time.Duration) {
		if len(data) == 0 {
			t.Error("encoded audio is empty")
		}
		close(done)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for encoded audio")
	}
}

func TestProcessAudioEmpty(t *testing.T) {
	g := NewGstreamer(testFullCodec, testLog)
	g.VideoW, g.VideoH = 64, 64
	g.VideoScale = 1
	g.AudioSrcHz = 48000

	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	defer g.Destroy()

	g.ProcessAudio(nil, func([]byte, time.Duration) {
		t.Fatal("empty audio should not be encoded")
	})
}

func TestReinit(t *testing.T) {
	g := NewGstreamer(testFullCodec, testLog)
	g.VideoW, g.VideoH = 64, 64
	g.VideoScale = 1
	g.AudioSrcHz = 48000

	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	defer g.Destroy()

	// Change scale and reinit.
	g.VideoScale = 2
	if err := g.Reinit(); err != nil {
		t.Fatal(err)
	}

	// Push a frame through the new pipeline.
	w, h := 64, 64
	stride := w * 4
	frame := make([]byte, stride*h)

	done := make(chan struct{})
	g.ProcessVideo(Video{
		Frame:    RawFrame{Data: frame, W: w, H: h, Stride: stride},
		Duration: 16 * time.Millisecond,
	}, func(data []byte, dur time.Duration) {
		if len(data) == 0 {
			t.Error("encoded video after reinit is empty")
		}
		close(done)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for encoded video after reinit")
	}
}
