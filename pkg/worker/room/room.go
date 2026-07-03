package room

import (
	"iter"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/media"
)

type MediaPipe interface {
	// Destroy frees all allocated resources.
	Destroy()
	// Init initializes the pipe: allocates needed resources.
	Init() error
	// Reinit initializes video and audio pipes with the new settings.
	Reinit() error
	// ProcessAudio pushes 16bit PCM audio frames into the encoder.
	ProcessAudio([]byte, func([]byte, time.Duration))
	// ProcessVideo pushes a video frame into the encoder.
	ProcessVideo(media.Video, func([]byte, time.Duration))
}

type SessionManager[T Session] interface {
	Add(T) bool
	Empty() bool
	Find(string) T
	RemoveL(T) int
	// Reset used for proper cleanup of the resources if needed.
	Reset()
	Values() iter.Seq[T]
}

type Session interface {
	Disconnect()
	SendAudio([]byte, time.Duration)
	SendVideo([]byte, time.Duration)
	SendData([]byte)
}

type SessionKey string

func (s SessionKey) String() string { return string(s) }
func (s SessionKey) Id() string     { return s.String() }

type Room[T Session] struct {
	app   app.App
	id    string
	media MediaPipe
	users SessionManager[T]

	closed        bool
	HandleClose   func()
	streamedBytes atomic.Int64
}

func NewRoom[T Session](id string, app app.App, um SessionManager[T], media MediaPipe) *Room[T] {
	return &Room[T]{id: id, app: app, users: um, media: media}
}

func (r *Room[T]) InitMedia() {
	r.app.SetAudioCb(func(a app.Audio) {
		r.media.ProcessAudio(a.Data, r.sendAudio)
	})
	r.app.SetVideoCb(func(v app.Video) {
		r.media.ProcessVideo(media.Video{
			Frame:    media.RawFrame{Data: v.Frame.Data, W: v.Frame.W, H: v.Frame.H, Stride: v.Frame.Stride},
			Duration: v.Duration,
		}, r.sendVideo)
	})
}

func (r *Room[T]) sendAudio(data []byte, dur time.Duration) {
	var bytes int64
	for u := range r.users.Values() {
		u.SendAudio(data, dur)
		bytes += int64(len(data))
	}
	if bytes > 0 {
		r.streamedBytes.Add(bytes)
	}
}

func (r *Room[T]) sendVideo(data []byte, dur time.Duration) {
	var bytes int64
	for u := range r.users.Values() {
		u.SendVideo(data, dur)
		bytes += int64(len(data))
	}
	if bytes > 0 {
		r.streamedBytes.Add(bytes)
	}
}

func (r *Room[T]) App() app.App         { return r.app }
func (r *Room[T]) Id() string           { return r.id }
func (r *Room[T]) SetApp(app app.App)   { r.app = app }
func (r *Room[T]) SetMedia(m MediaPipe) { r.media = m }
func (r *Room[T]) StreamedBytes() int64 {
	if r == nil {
		return 0
	}
	return r.streamedBytes.Load()
}
func (r *Room[T]) StartApp()      { r.app.Start() }
func (r *Room[T]) AddUser(user T) { r.users.Add(user) }
func (r *Room[T]) RemoveUser(user T) int {
	return r.users.RemoveL(user)
}
func (r *Room[T]) Send(data []byte) {
	for u := range r.users.Values() {
		u.SendData(data)
	}
}

func (r *Room[T]) Close() {
	if r == nil || r.closed {
		return
	}
	r.closed = true

	if r.app != nil {
		r.app.Close()
	}
	if r.media != nil {
		r.media.Destroy()
	}
	if r.HandleClose != nil {
		r.HandleClose()
	}
}

// Router tracks and routes freshly connected users to an app room.
// Rooms and users has 1-to-n relationship.
type Router[T Session] struct {
	room  *Room[T]
	users SessionManager[T]
	mu    sync.Mutex
}

func (r *Router[T]) FindRoom(id string) *Room[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.room != nil && r.room.Id() == id {
		return r.room
	}
	return nil
}

func (r *Router[T]) Remove(user T) {
	if left := r.users.RemoveL(user); left == 0 {
		r.Close()
		r.SetRoom(nil) // !to remove
	}
}

func (r *Router[T]) AddUser(user T)           { r.users.Add(user) }
func (r *Router[T]) Close()                   { r.mu.Lock(); r.room.Close(); r.room = nil; r.mu.Unlock() }
func (r *Router[T]) FindUser(uid string) T    { return r.users.Find(uid) }
func (r *Router[T]) Room() *Room[T]           { r.mu.Lock(); defer r.mu.Unlock(); return r.room }
func (r *Router[T]) SetRoom(room *Room[T])    { r.mu.Lock(); r.room = room; r.mu.Unlock() }
func (r *Router[T]) HasRoom() bool            { r.mu.Lock(); defer r.mu.Unlock(); return r.room != nil }
func (r *Router[T]) Users() SessionManager[T] { return r.users }
func (r *Router[T]) Reset() {
	r.mu.Lock()
	if r.room != nil {
		r.room.Close()
		r.room = nil
	}
	for u := range r.users.Values() {
		u.Disconnect()
	}
	r.users.Reset()
	r.mu.Unlock()
}

type AppSession struct {
	Session
	uid SessionKey
}

func (p AppSession) Id() SessionKey { return p.uid }

type GameSession struct {
	AppSession
	RoomId string
	Index  int // track user Index (i.e. player 1,2,3,4 select)
}

func NewGameSession(id string, s Session) *GameSession {
	return &GameSession{AppSession: AppSession{uid: SessionKey(id), Session: s}}
}
