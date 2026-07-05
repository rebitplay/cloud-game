package api

import (
	"bytes"
	"encoding/json"
	"time"
)

type (
	NDSRoomCreateRequest struct {
		Options     NDSRoomOptions        `json:"options,omitempty"`
		PlayerSlots []NDSPlayerSlotCreate `json:"player_slots"`
		Players     int                   `json:"players"`
		Room        string                `json:"room"`
		Rom         NDSRomCreate          `json:"rom"`
	}

	NDSRomCreate struct {
		Name string `json:"name"`
		SHA1 string `json:"sha1"`
		URL  string `json:"url"`
	}

	NDSRoomOptions struct {
		IdleTimeoutSec int    `json:"idle_timeout_sec,omitempty"`
		JoinTimeoutSec int    `json:"join_timeout_sec,omitempty"`
		MaxDurationSec int    `json:"max_duration_sec,omitempty"`
		VideoCodec     string `json:"video_codec,omitempty"`
	}

	NDSPlayerSlotCreate struct {
		Player        int    `json:"player"`
		Ref           string `json:"ref"`
		SaveURL       string `json:"save_url,omitempty"`
		SaveUploadURL string `json:"save_upload_url"`
	}

	NDSPlayerSaveRequest struct {
		Player  int    `json:"player"`
		SRMURL  string `json:"srm_url,omitempty"`
		SaveURL string `json:"save_url,omitempty"`
	}

	NDSRoomCreateResponse struct {
		Group   string            `json:"group,omitempty"`
		Game    string            `json:"game"`
		Players []NDSPlayerStream `json:"players"`
		Room    string            `json:"room"`
		Rom     string            `json:"rom"`
	}

	NDSPlayerStream struct {
		Group  string `json:"group,omitempty"`
		Player int    `json:"player"`
		RoomID string `json:"room_id"`
		URL    string `json:"url"`
		Zone   string `json:"zone,omitempty"`
	}

	NDSRoomV1Response struct {
		Endpoint     string              `json:"endpoint"`
		Game         string              `json:"game"`
		JoinDeadline time.Time           `json:"join_deadline"`
		Players      []NDSPlayerJoinInfo `json:"players"`
		RoomID       string              `json:"room_id"`
		State        string              `json:"state"`
	}

	NDSPlayerJoinInfo struct {
		IceServers   []IceServer `json:"ice_servers"`
		Player       int         `json:"player"`
		Ref          string      `json:"ref"`
		SignalingURL string      `json:"signaling_url"`
		Token        string      `json:"token"`
	}

	NDSRoomStateResponse struct {
		ClosedAt  *time.Time       `json:"closed_at,omitempty"`
		CreatedAt time.Time        `json:"created_at"`
		Endpoint  string           `json:"endpoint"`
		Game      string           `json:"game"`
		Players   []NDSPlayerState `json:"players"`
		RoomID    string           `json:"room_id"`
		StartedAt *time.Time       `json:"started_at,omitempty"`
		State     string           `json:"state"`
		UpdatedAt time.Time        `json:"updated_at"`
	}

	NDSPlayerState struct {
		Connected   bool       `json:"connected"`
		ConnectedAt *time.Time `json:"connected_at,omitempty"`
		LastSaveAt  *time.Time `json:"last_save_at,omitempty"`
		LastSeen    *time.Time `json:"last_seen,omitempty"`
		Player      int        `json:"player"`
		Ref         string     `json:"ref"`
		SaveStatus  string     `json:"save_status,omitempty"`
	}

	NDSCapacityResponse struct {
		ByPlayers  map[string]int `json:"by_players"`
		FreeRooms  int            `json:"free_rooms"`
		TotalRooms int            `json:"total_rooms"`
	}

	NDSAPIError struct {
		Code          string `json:"code"`
		Error         string `json:"error"`
		RetryAfterSec int    `json:"retry_after_sec,omitempty"`
	}

	NDSRomInstallRequest struct {
		FileName string `json:"file_name"`
		SHA1     string `json:"sha1,omitempty"`
		URL      string `json:"url"`
	}

	NDSRomInstallResponse struct {
		Game string `json:"game"`
		Path string `json:"path"`
	}

	NDSSessionPrepareRequest struct {
		Player        int    `json:"player,omitempty"`
		Ref           string `json:"ref,omitempty"`
		RoomID        string `json:"room_id"`
		SaveURL       string `json:"save_url,omitempty"`
		SaveUploadURL string `json:"save_upload_url,omitempty"`
	}

	NDSFlushSaveRequest struct {
		RoomID string `json:"room_id"`
	}

	NDSSaveStatus struct {
		FlushedAt time.Time `json:"flushed_at,omitempty"`
		Player    int       `json:"player,omitempty"`
		Ref       string    `json:"ref,omitempty"`
		RoomID    string    `json:"room_id"`
		SHA1      string    `json:"sha1,omitempty"`
		Size      int       `json:"size,omitempty"`
		Status    string    `json:"status"`
	}
)

func (o *NDSRoomOptions) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		*o = NDSRoomOptions{}
		return nil
	}

	type ndsRoomOptions NDSRoomOptions
	var parsed ndsRoomOptions
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return err
	}
	*o = NDSRoomOptions(parsed)
	return nil
}
