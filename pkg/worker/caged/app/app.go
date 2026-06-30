package app

import "time"

type App interface {
	AudioSampleRate() int
	AspectRatio() float32
	AspectEnabled() bool
	Flipped() bool
	Init() error
	ViewportSize() (int, int)
	Scale() (float64, string)
	Rotation() uint
	PixFormat() uint32
	Start()
	Close()

	SetAudioCb(func(Audio))
	SetVideoCb(func(Video))
	SetDataCb(func([]byte))
	Input(port int, device byte, data []byte)
	KbMouseSupport() bool
	PointerSupport() bool
}

type Audio struct {
	Data     []byte
	Duration time.Duration
}

type Video struct {
	Frame    RawFrame
	Duration time.Duration
}

type RawFrame struct {
	Data   []byte
	Stride int
	W, H   int
}
