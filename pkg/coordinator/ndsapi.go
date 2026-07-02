package coordinator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/games"
)

type reservedNDSWorker struct {
	player int
	roomID string
	stream bool
	worker *Worker
}

const ndsReservationTTL = 10 * time.Minute

type ndsWorkerGroup struct {
	id      string
	invalid bool
	workers map[int]*Worker
}

func (h *Hub) handleNDSRoomCreate() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setAPIHeaders(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req api.NDSRoomCreateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON request")
			return
		}

		resp, err := h.createNDSRoom(r, req)
		if err != nil {
			status := http.StatusInternalServerError
			if errorsIsBadRequest(err) {
				status = http.StatusBadRequest
			} else if errorsIsConflict(err) {
				status = http.StatusConflict
			}
			writeAPIError(w, status, err.Error())
			return
		}
		writeAPIJSON(w, http.StatusCreated, resp)
	}
}

func (h *Hub) createNDSRoom(r *http.Request, req api.NDSRoomCreateRequest) (api.NDSRoomCreateResponse, error) {
	if req.Players < 2 || req.Players > 4 {
		return api.NDSRoomCreateResponse{}, badAPIRequest("players must be 2, 3, or 4")
	}
	if strings.TrimSpace(req.RomURL) == "" {
		return api.NDSRoomCreateResponse{}, badAPIRequest("rom_url is required")
	}

	fileName := games.NDSFileName(req.RomURL, req.RomName, "game.nds")
	gameName := games.GameNameFromFile(fileName)
	roomBase := cleanRoomBase(req.Room)
	if roomBase == "" {
		roomBase = "nds-" + com.NewUid().String()
	}

	reserved, groupID, err := h.reserveNDSGroup(req.Players, roomBase, gameName)
	if err != nil && isNDSCapacityConflict(err) {
		if spawnErr := h.spawnNDSGroup(req.Players); spawnErr != nil {
			return api.NDSRoomCreateResponse{}, spawnErr
		}
		reserved, groupID, err = h.reserveNDSGroup(req.Players, roomBase, gameName)
	}
	if err != nil {
		return api.NDSRoomCreateResponse{}, err
	}
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			releaseNDSReservations(reserved)
		}
	}()

	installReq := api.NDSRomInstallRequest{URL: req.RomURL, FileName: fileName}
	romPath := "nds/" + fileName
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		resp, err := slot.worker.InstallNDSRom(installReq)
		if err != nil || resp == nil || resp.Game == "" {
			return api.NDSRoomCreateResponse{}, fmt.Errorf("worker %s could not install ROM", slot.worker.Id().String())
		}
		gameName = resp.Game
		if resp.Path != "" {
			romPath = resp.Path
		}
	}

	saveURLs, err := playerSaveURLs(req.Saves, req.Players)
	if err != nil {
		return api.NDSRoomCreateResponse{}, err
	}
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		saveURL := saveURLs[slot.player]
		if saveURL == "" {
			continue
		}
		resp, err := slot.worker.PrepareNDSSession(api.NDSSessionPrepareRequest{
			RoomID:  slot.roomID,
			SaveURL: saveURL,
		})
		if err != nil || resp == nil || *resp != api.OK {
			return api.NDSRoomCreateResponse{}, fmt.Errorf("worker %s could not prepare save for player %d", slot.worker.Id().String(), slot.player)
		}
	}

	baseURL := publicBaseURL(r, req.BaseURL)
	players := make([]api.NDSPlayerStream, 0, len(reserved))
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		players = append(players, api.NDSPlayerStream{
			Group:  groupID,
			Player: slot.player,
			RoomID: slot.roomID,
			URL:    streamURL(baseURL, slot.roomID, slot.worker.Zone),
			Zone:   slot.worker.Zone,
		})
	}

	releaseOnFailure = false
	h.scheduleNDSReservationExpiry(reserved)
	return api.NDSRoomCreateResponse{
		Group:   groupID,
		Game:    gameName,
		Players: players,
		Room:    roomBase,
		Rom:     romPath,
	}, nil
}

func (h *Hub) reserveNDSGroup(players int, roomBase string, gameName string) ([]reservedNDSWorker, string, error) {
	if h.ndsRoomBaseInUse(roomBase) {
		return nil, "", apiConflict(fmt.Sprintf("NDS room %q is already active or reserved", roomBase))
	}

	groups := h.ndsWorkerGroups()
	for _, group := range groups {
		if group.invalid || !group.canHost(players) {
			continue
		}

		reserved := make([]reservedNDSWorker, 0, len(group.workers))
		ok := true
		for _, player := range group.playerNumbers() {
			worker := group.workers[player]
			roomID := ndsPlayerRoomID(roomBase, player, gameName)
			if !worker.ReserveRoom(roomID) {
				ok = false
				break
			}
			reserved = append(reserved, reservedNDSWorker{
				player: player,
				roomID: roomID,
				stream: player <= players,
				worker: worker,
			})
		}
		if ok {
			return reserved, group.id, nil
		}
		releaseNDSReservations(reserved)
	}

	return nil, "", apiConflict(fmt.Sprintf("not enough free NDS groups: need one free group with players 1-%d", players))
}

func (h *Hub) ndsWorkerGroups() []ndsWorkerGroup {
	byID := make(map[string]*ndsWorkerGroup)
	for worker := range h.workers.Values() {
		if worker.NDSPlayer < 1 || worker.NDSPlayer > 4 {
			continue
		}
		groupID := worker.NDSGroup
		if groupID == "" {
			groupID = "default"
		}
		group := byID[groupID]
		if group == nil {
			group = &ndsWorkerGroup{
				id:      groupID,
				workers: make(map[int]*Worker),
			}
			byID[groupID] = group
		}
		if _, exists := group.workers[worker.NDSPlayer]; exists {
			group.invalid = true
			continue
		}
		group.workers[worker.NDSPlayer] = worker
	}

	groups := make([]ndsWorkerGroup, 0, len(byID))
	for _, group := range byID {
		groups = append(groups, *group)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].id < groups[j].id
	})
	return groups
}

func (g ndsWorkerGroup) canHost(players int) bool {
	if g.invalid {
		return false
	}
	for player := 1; player <= players; player++ {
		if g.workers[player] == nil {
			return false
		}
	}
	for _, worker := range g.workers {
		if !worker.HasSlot() {
			return false
		}
	}
	return true
}

func (g ndsWorkerGroup) playerNumbers() []int {
	players := make([]int, 0, len(g.workers))
	for player := range g.workers {
		if player >= 1 && player <= 4 {
			players = append(players, player)
		}
	}
	sort.Ints(players)
	return players
}

func (h *Hub) ndsRoomBaseInUse(roomBase string) bool {
	for worker := range h.workers.Values() {
		if ndsRoomBaseFromWorkerRoom(worker.RoomId) == roomBase ||
			ndsRoomBaseFromWorkerRoom(worker.ReservedRoomId) == roomBase {
			return true
		}
	}
	return false
}

func ndsPlayerRoomID(roomBase string, player int, gameName string) string {
	return fmt.Sprintf("%s-p%d___%s", roomBase, player, gameName)
}

func ndsRoomBaseFromWorkerRoom(roomID string) string {
	if roomID == "" {
		return ""
	}
	if base, _, ok := strings.Cut(roomID, "___"); ok {
		roomID = base
	}
	idx := strings.LastIndex(roomID, "-p")
	if idx < 0 || idx == 0 || idx+2 >= len(roomID) {
		return roomID
	}
	suffix := roomID[idx+2:]
	if strings.Contains(suffix, "-") {
		return roomID
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return roomID
		}
	}
	return roomID[:idx]
}

func releaseNDSReservations(reserved []reservedNDSWorker) {
	for _, slot := range reserved {
		slot.worker.ReleaseReservation(slot.roomID)
	}
}

func (h *Hub) scheduleNDSReservationExpiry(reserved []reservedNDSWorker) {
	for _, slot := range reserved {
		slot := slot
		time.AfterFunc(ndsReservationTTL, func() {
			if slot.worker.ReleaseReservation(slot.roomID) {
				h.log.Info().Str("room", slot.roomID).Msg("expired pending NDS room reservation")
			}
		})
	}
}

func playerSaveURLs(saves []api.NDSPlayerSaveRequest, players int) (map[int]string, error) {
	out := map[int]string{}
	for _, save := range saves {
		if save.Player < 1 || save.Player > players {
			return nil, badAPIRequest(fmt.Sprintf("save player must be between 1 and %d", players))
		}
		saveURL := strings.TrimSpace(save.SRMURL)
		if saveURL == "" {
			saveURL = strings.TrimSpace(save.SaveURL)
		}
		if saveURL != "" {
			out[save.Player] = saveURL
		}
	}
	return out, nil
}

func streamURL(baseURL string, roomID string, zone string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u = &url.URL{Scheme: "http", Host: baseURL}
	}
	u.Path = "/"
	q := u.Query()
	q.Set("id", roomID)
	q.Set("player", "1")
	q.Set("client", "v7")
	q.Set("view", "stream")
	if zone != "" {
		q.Set("zone", zone)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func publicBaseURL(r *http.Request, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return strings.TrimRight(override, "/")
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func cleanRoomBase(room string) string {
	room = games.SafeName(room, "")
	room = strings.ReplaceAll(room, " ", "-")
	for strings.Contains(room, "___") {
		room = strings.ReplaceAll(room, "___", "_")
	}
	return room
}

type badAPIRequest string

func (e badAPIRequest) Error() string { return string(e) }

func errorsIsBadRequest(err error) bool {
	_, ok := err.(badAPIRequest)
	return ok
}

type apiConflict string

func (e apiConflict) Error() string { return string(e) }

func errorsIsConflict(err error) bool {
	_, ok := err.(apiConflict)
	return ok
}

func isNDSCapacityConflict(err error) bool {
	return errorsIsConflict(err) && strings.HasPrefix(err.Error(), "not enough free NDS groups")
}

func setAPIHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func writeAPIJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeAPIJSON(w, status, map[string]string{"error": message})
}
