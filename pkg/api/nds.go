package api

type (
	NDSRoomCreateRequest struct {
		BaseURL string                 `json:"base_url,omitempty"`
		Players int                    `json:"players"`
		Room    string                 `json:"room,omitempty"`
		RomName string                 `json:"rom_name,omitempty"`
		RomURL  string                 `json:"rom_url"`
		Saves   []NDSPlayerSaveRequest `json:"saves,omitempty"`
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

	NDSRomInstallRequest struct {
		FileName string `json:"file_name"`
		URL      string `json:"url"`
	}

	NDSRomInstallResponse struct {
		Game string `json:"game"`
		Path string `json:"path"`
	}

	NDSSessionPrepareRequest struct {
		RoomID  string `json:"room_id"`
		SaveURL string `json:"save_url,omitempty"`
	}
)
