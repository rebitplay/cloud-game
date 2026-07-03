package api

import "encoding/json"

type (
	CloseRoomRequest struct {
		RoomID        string `json:"room_id"`
		BytesStreamed int64  `json:"bytes_streamed,omitempty"`
	}
	ConnectionRequest[T Id] struct {
		Addr       string `json:"addr,omitempty"`
		Id         T      `json:"id,omitempty"`
		IsHTTPS    bool   `json:"is_https,omitempty"`
		NDSGroup   string `json:"nds_group,omitempty"`
		NDSPlayer  int    `json:"nds_player,omitempty"`
		PingURL    string `json:"ping_url,omitempty"`
		Port       string `json:"port,omitempty"`
		Tag        string `json:"tag,omitempty"`
		WebRTCPort int    `json:"webrtc_port,omitempty"`
		Zone       string `json:"zone,omitempty"`
	}
	GetWorkerListResponse struct {
		Servers []Server `json:"servers"`
	}
	RegisterRoomRequest string
)

func (r CloseRoomRequest) String() string {
	return r.RoomID
}

func (r *CloseRoomRequest) UnmarshalJSON(data []byte) error {
	var roomID string
	if err := json.Unmarshal(data, &roomID); err == nil {
		r.RoomID = roomID
		return nil
	}

	type closeRoomRequest CloseRoomRequest
	var req closeRoomRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}
	*r = CloseRoomRequest(req)
	return nil
}

const (
	DataQueryParam   = "data"
	RoomIdQueryParam = "room_id"
	ZoneQueryParam   = "zone"
	WorkerIdParam    = "wid"
)

// Server contains a list of server groups.
// Server is a separate machine that may contain
// multiple sub-processes.
type Server struct {
	Addr       string `json:"addr,omitempty"`
	Id         Id     `json:"id,omitempty"`
	IsBusy     bool   `json:"is_busy,omitempty"`
	InGroup    bool   `json:"in_group,omitempty"`
	Machine    string `json:"machine,omitempty"`
	NDSGroup   string `json:"nds_group,omitempty"`
	NDSPlayer  int    `json:"nds_player,omitempty"`
	PingURL    string `json:"ping_url"`
	Port       string `json:"port,omitempty"`
	Replicas   uint32 `json:"replicas,omitempty"`
	Room       string `json:"room,omitempty"`
	Tag        string `json:"tag,omitempty"`
	WebRTCPort int    `json:"webrtc_port,omitempty"`
	Zone       string `json:"zone,omitempty"`
}
