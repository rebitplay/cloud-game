package coordinator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/games"
)

type reservedNDSWorker struct {
	player int
	roomID string
	stream bool
	worker *Worker
}

const (
	defaultNDSJoinTimeoutSec = 600
	defaultNDSIdleTimeoutSec = 300
	defaultNDSMaxDurationSec = 14400
	maxNDSDurationSec        = 21600
)

var (
	ndsRoomIDPattern = regexp.MustCompile(`^[a-z0-9-]{4,64}$`)
	ndsSHA1Pattern   = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
)

type ndsWorkerGroup struct {
	id      string
	invalid bool
	workers map[int]*Worker
}

func (h *Hub) handleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Hub) handleNDSRooms() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rooms" {
			writeAPIError(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		var req api.NDSRoomCreateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_json", "invalid JSON request")
			return
		}

		resp, status, err := h.createNDSRoomV1(req)
		if err != nil {
			writeAPIErrorFromErr(w, err)
			return
		}
		writeAPIJSON(w, status, resp)
	}
}

func (h *Hub) handleNDSRoomByID() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/rooms/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			writeAPIError(w, http.StatusNotFound, "not_found", "room not found")
			return
		}
		roomID := parts[0]
		if len(parts) == 1 {
			switch r.Method {
			case http.MethodGet:
				h.handleNDSRoomGet(w, roomID)
			case http.MethodDelete:
				h.handleNDSRoomDelete(w, roomID)
			default:
				writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			}
			return
		}
		if len(parts) == 4 && parts[1] == "players" && parts[3] == "token" && r.Method == http.MethodPost {
			player, err := strconv.Atoi(parts[2])
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_player", "player must be an integer")
				return
			}
			h.handleNDSRoomPlayerToken(w, roomID, player)
			return
		}
		writeAPIError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (h *Hub) handleNDSCapacity() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		byPlayers := map[string]int{"2": 0, "3": 0, "4": 0}
		for _, group := range h.ndsWorkerGroups() {
			for players := 2; players <= 4; players++ {
				if group.canHost(players) {
					byPlayers[strconv.Itoa(players)]++
				}
			}
		}
		writeAPIJSON(w, http.StatusOK, api.NDSCapacityResponse{
			ByPlayers:  byPlayers,
			FreeRooms:  byPlayers["4"],
			TotalRooms: envInt("NDS_MAX_ROOM_COUNT", len(h.ndsWorkerGroups())),
		})
	}
}

func (h *Hub) createNDSRoomV1(req api.NDSRoomCreateRequest) (api.NDSRoomV1Response, int, error) {
	if err := validateNDSRoomCreate(req); err != nil {
		return api.NDSRoomV1Response{}, 0, err
	}

	now := time.Now().UTC()
	if existing := h.ndsRooms.live(req.Room); existing != nil {
		resp, err := existing.response(h.conf.Webrtc.IceServers, now)
		if err != nil {
			return api.NDSRoomV1Response{}, 0, apiInternal(err.Error())
		}
		return resp, http.StatusOK, nil
	}

	endpoint := strings.TrimRight(envString("NDS_PUBLIC_ENDPOINT", ""), "/")
	if endpoint == "" {
		return api.NDSRoomV1Response{}, 0, apiInternal("NDS_PUBLIC_ENDPOINT is not configured")
	}

	fileName := games.NDSFileName(req.Rom.URL, req.Rom.Name, "game.nds")
	gameName := games.GameNameFromFile(fileName)
	roomID := req.Room
	joinTimeout := req.Options.JoinTimeoutSec
	if joinTimeout <= 0 {
		joinTimeout = defaultNDSJoinTimeoutSec
	}
	idleTimeout := req.Options.IdleTimeoutSec
	if idleTimeout <= 0 {
		idleTimeout = defaultNDSIdleTimeoutSec
	}
	maxDuration := req.Options.MaxDurationSec
	if maxDuration <= 0 {
		maxDuration = defaultNDSMaxDurationSec
	}

	reserved, groupID, err := h.reserveNDSGroup(req.Players, roomID, gameName)
	if err != nil && isNDSCapacityConflict(err) {
		if spawnErr := h.spawnNDSGroup(req.Players); spawnErr != nil {
			return api.NDSRoomV1Response{}, 0, noCapacityAPIError("no free NDS worker groups")
		}
		reserved, groupID, err = h.reserveNDSGroup(req.Players, roomID, gameName)
	}
	if err != nil {
		if isNDSCapacityConflict(err) {
			return api.NDSRoomV1Response{}, 0, noCapacityAPIError("no free NDS worker groups")
		}
		return api.NDSRoomV1Response{}, 0, err
	}
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			releaseNDSReservations(reserved)
		}
	}()

	installReq := api.NDSRomInstallRequest{URL: req.Rom.URL, FileName: fileName, SHA1: strings.ToLower(req.Rom.SHA1)}
	romPath := "nds/" + fileName
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		resp, err := slot.worker.InstallNDSRom(installReq)
		if err != nil || resp == nil || resp.Game == "" {
			return api.NDSRoomV1Response{}, 0, apiInternal(fmt.Sprintf("worker %s could not install ROM", slot.worker.Id().String()))
		}
		gameName = resp.Game
		if resp.Path != "" {
			romPath = resp.Path
		}
	}

	slots := playerSlotsByNumber(req.PlayerSlots)
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		saveURL := slots[slot.player].SaveURL
		resp, err := slot.worker.PrepareNDSSession(api.NDSSessionPrepareRequest{
			Ref:           slots[slot.player].Ref,
			RoomID:        slot.roomID,
			SaveURL:       saveURL,
			SaveUploadURL: slots[slot.player].SaveUploadURL,
		})
		if err != nil || resp == nil || *resp != api.OK {
			return api.NDSRoomV1Response{}, 0, apiInternal(fmt.Sprintf("worker %s could not prepare save for player %d", slot.worker.Id().String(), slot.player))
		}
	}

	players := make(map[int]*ndsSeat, req.Players)
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		slotReq := slots[slot.player]
		players[slot.player] = &ndsSeat{
			player:        slot.player,
			ref:           slotReq.Ref,
			roomID:        slot.roomID,
			saveURL:       slotReq.SaveURL,
			saveUploadURL: slotReq.SaveUploadURL,
			worker:        slot.worker,
		}
	}

	session := &ndsRoomSession{
		createdAt:    now,
		endpoint:     endpoint,
		game:         gameName,
		groupID:      groupID,
		idleTimeout:  time.Duration(idleTimeout) * time.Second,
		joinDeadline: now.Add(time.Duration(joinTimeout) * time.Second),
		maxDuration:  time.Duration(maxDuration) * time.Second,
		players:      players,
		reserved:     reserved,
		roomID:       roomID,
		romPath:      romPath,
		state:        ndsRoomReady,
		updatedAt:    now,
	}
	h.ndsRooms.put(session)
	h.startNDSRoomLifecycle(session)
	h.emitNDSWebhook("room.ready", session, nil)

	releaseOnFailure = false
	resp, err := session.response(h.conf.Webrtc.IceServers, now)
	if err != nil {
		return api.NDSRoomV1Response{}, 0, apiInternal(err.Error())
	}
	return resp, http.StatusCreated, nil
}

func (h *Hub) handleNDSRoomGet(w http.ResponseWriter, roomID string) {
	room := h.ndsRooms.get(roomID)
	if room == nil || room.state == ndsRoomClosed {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, room.stateResponse())
}

func (h *Hub) handleNDSRoomDelete(w http.ResponseWriter, roomID string) {
	room := h.closeNDSRoom(roomID, ndsCloseHost)
	if room == nil {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	writeAPIJSON(w, http.StatusAccepted, room.stateResponse())
}

func (h *Hub) handleNDSRoomPlayerToken(w http.ResponseWriter, roomID string, player int) {
	room := h.ndsRooms.live(roomID)
	if room == nil {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	resp, ok, err := room.tokenResponse(player, h.conf.Webrtc.IceServers, time.Now().UTC())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "token_error", err.Error())
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "player_not_found", "player not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

func validateNDSRoomCreate(req api.NDSRoomCreateRequest) error {
	if !ndsRoomIDPattern.MatchString(req.Room) {
		return badAPIRequest("invalid_room", "room must match [a-z0-9-]{4,64}")
	}
	if req.Players < 2 || req.Players > 4 {
		return badAPIRequest("invalid_players", "players must be 2, 3, or 4")
	}
	if strings.TrimSpace(req.Rom.URL) == "" {
		return badAPIRequest("invalid_rom_url", "rom.url is required")
	}
	if strings.TrimSpace(req.Rom.Name) == "" {
		return badAPIRequest("invalid_rom_name", "rom.name is required")
	}
	if !ndsSHA1Pattern.MatchString(req.Rom.SHA1) {
		return badAPIRequest("invalid_rom_sha1", "rom.sha1 must be a 40-character hex string")
	}
	if len(req.PlayerSlots) != req.Players {
		return badAPIRequest("invalid_player_slots", "player_slots must contain exactly players entries")
	}
	seen := map[int]struct{}{}
	for _, slot := range req.PlayerSlots {
		if slot.Player < 1 || slot.Player > req.Players {
			return badAPIRequest("invalid_player", fmt.Sprintf("player must be between 1 and %d", req.Players))
		}
		if _, ok := seen[slot.Player]; ok {
			return badAPIRequest("duplicate_player", "player_slots contains duplicate player")
		}
		seen[slot.Player] = struct{}{}
		if strings.TrimSpace(slot.Ref) == "" {
			return badAPIRequest("invalid_ref", "player ref is required")
		}
		if strings.TrimSpace(slot.SaveUploadURL) == "" {
			return badAPIRequest("invalid_save_upload_url", "save_upload_url is required")
		}
	}
	if req.Options.VideoCodec != "" && req.Options.VideoCodec != "h264" && req.Options.VideoCodec != "vp8" {
		return badAPIRequest("invalid_video_codec", "video_codec must be h264 or vp8")
	}
	if req.Options.MaxDurationSec > maxNDSDurationSec {
		return badAPIRequest("invalid_max_duration", fmt.Sprintf("max_duration_sec must be <= %d", maxNDSDurationSec))
	}
	if req.Options.MaxDurationSec < 0 || req.Options.IdleTimeoutSec < 0 || req.Options.JoinTimeoutSec < 0 {
		return badAPIRequest("invalid_timeout", "timeout values must be positive")
	}
	return nil
}

func playerSlotsByNumber(slots []api.NDSPlayerSlotCreate) map[int]api.NDSPlayerSlotCreate {
	out := make(map[int]api.NDSPlayerSlotCreate, len(slots))
	for _, slot := range slots {
		slot.Ref = strings.TrimSpace(slot.Ref)
		slot.SaveURL = strings.TrimSpace(slot.SaveURL)
		slot.SaveUploadURL = strings.TrimSpace(slot.SaveUploadURL)
		out[slot.Player] = slot
	}
	return out
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

type apiError struct {
	code     string
	message  string
	retrySec int
	status   int
}

func (e apiError) Error() string { return e.message }

func badAPIRequest(code string, message string) apiError {
	return apiError{code: code, message: message, status: http.StatusBadRequest}
}

func apiInternal(message string) apiError {
	return apiError{code: "internal_error", message: message, status: http.StatusInternalServerError}
}

func noCapacityAPIError(message string) apiError {
	return apiError{code: "no_capacity", message: message, retrySec: 15, status: http.StatusServiceUnavailable}
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

func writeAPIJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code string, message string) {
	writeAPIJSON(w, status, api.NDSAPIError{Code: code, Error: message})
}

func writeAPIErrorFromErr(w http.ResponseWriter, err error) {
	if e, ok := err.(apiError); ok {
		writeAPIJSON(w, e.status, api.NDSAPIError{Code: e.code, Error: e.message, RetryAfterSec: e.retrySec})
		return
	}
	writeAPIJSON(w, http.StatusInternalServerError, api.NDSAPIError{Code: "internal_error", Error: err.Error()})
}
