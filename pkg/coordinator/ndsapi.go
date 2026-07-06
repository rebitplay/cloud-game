package coordinator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/games"
	"github.com/giongto35/cloud-game/v3/pkg/monitoring"
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
	ndsClosedRetention       = 10 * time.Minute
)

var (
	ndsRoomIDPattern = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)
	ndsSHA1Pattern   = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
)

type ndsWorkerGroup struct {
	id      string
	invalid bool
	workers map[int]*Worker
}

type ndsDemoRoomCreateRequest struct {
	BaseURL   string                     `json:"base_url,omitempty"`
	Options   api.NDSRoomOptions         `json:"options,omitempty"`
	Players   int                        `json:"players"`
	RomName   string                     `json:"rom_name,omitempty"`
	RomSHA1   string                     `json:"rom_sha1,omitempty"`
	RomURL    string                     `json:"rom_url"`
	Room      string                     `json:"room,omitempty"`
	Saves     []api.NDSPlayerSaveRequest `json:"saves,omitempty"`
	SHA1      string                     `json:"sha1,omitempty"`
	VideoCode string                     `json:"video_codec,omitempty"`
}

type ndsTimeResponse struct {
	RFC3339Nano string `json:"rfc3339_nano"`
	UnixMs      int64  `json:"unix_ms"`
	UnixNs      int64  `json:"unix_ns"`
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

func (h *Hub) handleNDSTime() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		now := time.Now().UTC()
		writeAPIJSON(w, http.StatusOK, ndsTimeResponse{
			RFC3339Nano: now.Format(time.RFC3339Nano),
			UnixMs:      now.UnixMilli(),
			UnixNs:      now.UnixNano(),
		})
	}
}

func (h *Hub) handleNDSDemoRoomCreate() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if h.draining.Load() {
			writeAPIJSON(w, http.StatusServiceUnavailable, api.NDSAPIError{
				Code:          "service_draining",
				Error:         "service is draining active NDS rooms",
				RetryAfterSec: 60,
			})
			return
		}
		if !h.ndsCreates.allow(requestIP(r), 30, time.Minute, time.Now()) {
			writeAPIJSON(w, http.StatusTooManyRequests, api.NDSAPIError{
				Code:          "rate_limited",
				Error:         "room create rate limit exceeded",
				RetryAfterSec: 60,
			})
			return
		}

		var req ndsDemoRoomCreateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_json", "invalid JSON request")
			return
		}

		resp, status, err := h.createNDSDemoRoom(r, req)
		if err != nil {
			writeAPIErrorFromErr(w, err)
			return
		}
		writeAPIJSON(w, status, resp)
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
		if h.draining.Load() {
			writeAPIJSON(w, http.StatusServiceUnavailable, api.NDSAPIError{
				Code:          "service_draining",
				Error:         "service is draining active NDS rooms",
				RetryAfterSec: 60,
			})
			return
		}
		apiKey, _ := ndsAPIKeyFromAuthorization(r.Header.Get("Authorization"))
		if !h.ndsCreates.allow(apiKey, 30, time.Minute, time.Now()) {
			writeAPIJSON(w, http.StatusTooManyRequests, api.NDSAPIError{
				Code:          "rate_limited",
				Error:         "room create rate limit exceeded",
				RetryAfterSec: 60,
			})
			return
		}
		var req api.NDSRoomCreateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_json", "invalid JSON request")
			return
		}

		started := time.Now()
		resp, status, err := h.createNDSRoomV1(req)
		elapsed := time.Since(started)
		if err != nil {
			monitoring.ObserveNDSRoomCreate("error", elapsed)
			if room := h.ndsRooms.get(req.Room); room != nil {
				h.auditNDSRoomCreate(req.Players, room, http.StatusInternalServerError, elapsed)
			}
			writeAPIErrorFromErr(w, err)
			return
		}
		monitoring.ObserveNDSRoomCreate(strconv.Itoa(status), elapsed)
		if room := h.ndsRooms.get(resp.RoomID); room != nil {
			h.auditNDSRoomCreate(req.Players, room, status, elapsed)
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
		byPlayers := h.ndsCapacityByPlayers()
		writeAPIJSON(w, http.StatusOK, api.NDSCapacityResponse{
			ByPlayers:  byPlayers,
			FreeRooms:  byPlayers["4"],
			TotalRooms: envInt("NDS_MAX_ROOM_COUNT", len(h.ndsWorkerGroups())),
		})
	}
}

func (h *Hub) ndsCapacityByPlayers() map[string]int {
	byPlayers := map[string]int{"2": 0, "3": 0, "4": 0}
	for _, group := range h.ndsWorkerGroups() {
		for players := 2; players <= 4; players++ {
			if group.canHost(players) {
				byPlayers[strconv.Itoa(players)]++
			}
		}
	}
	if h.ndsSpawner != nil && h.ndsSpawner.enabled {
		remaining := h.ndsSpawner.remainingSpawnCapacity()
		for players := 2; players <= 4; players++ {
			if h.ndsSpawner.playersPerGroup >= players {
				byPlayers[strconv.Itoa(players)] += remaining
			}
		}
	}
	return byPlayers
}

func (h *Hub) createNDSDemoRoom(r *http.Request, req ndsDemoRoomCreateRequest) (api.NDSRoomCreateResponse, int, error) {
	roomID := cleanNDSRoomID(req.Room)
	if roomID == "" {
		roomID = "nds-" + com.NewUid().String()
	}

	romURL := strings.TrimSpace(req.RomURL)
	fileName := games.NDSFileName(romURL, req.RomName, "game.nds")
	romSHA1 := strings.ToLower(strings.TrimSpace(req.RomSHA1))
	if romSHA1 == "" {
		romSHA1 = strings.ToLower(strings.TrimSpace(req.SHA1))
	}
	if romSHA1 == "" {
		var err error
		romSHA1, err = localBuiltinNDSROMSHA1(romURL, fileName)
		if err != nil {
			return api.NDSRoomCreateResponse{}, 0, badAPIRequest("invalid_rom_sha1", "rom_sha1 is required for non-builtin ROM URLs")
		}
	}

	options := req.Options
	if options.VideoCodec == "" {
		options.VideoCodec = strings.TrimSpace(req.VideoCode)
	}
	if options.VideoCodec == "" {
		options.VideoCodec = "vp8"
	}

	v1Req := api.NDSRoomCreateRequest{
		Options: options,
		Players: req.Players,
		Room:    roomID,
		Rom: api.NDSRomCreate{
			Name: fileName,
			SHA1: romSHA1,
			URL:  romURL,
		},
		PlayerSlots: makeDemoPlayerSlots(roomID, req.Players, req.Saves),
	}

	v1Resp, status, err := h.createNDSRoomV1(v1Req)
	if err != nil {
		return api.NDSRoomCreateResponse{}, 0, err
	}

	baseURL := publicBaseURL(r, req.BaseURL)
	room := h.ndsRooms.get(v1Resp.RoomID)
	seatInfo := map[int]api.NDSPlayerStream{}
	groupID := ""
	romPath := "nds/" + fileName
	if room != nil {
		room.mu.Lock()
		groupID = room.groupID
		if room.romPath != "" {
			romPath = room.romPath
		}
		for _, seat := range room.players {
			zone := ""
			if seat.worker != nil {
				zone = seat.worker.Zone
			}
			seatInfo[seat.player] = api.NDSPlayerStream{
				Group:  groupID,
				Player: seat.player,
				RoomID: seat.roomID,
				Zone:   zone,
			}
		}
		room.mu.Unlock()
	}

	players := make([]api.NDSPlayerStream, 0, len(v1Resp.Players))
	for _, player := range v1Resp.Players {
		stream := seatInfo[player.Player]
		if stream.Player == 0 {
			stream = api.NDSPlayerStream{Group: groupID, Player: player.Player, RoomID: v1Resp.RoomID}
		}
		stream.URL = ndsDemoStreamURL(baseURL, stream.RoomID, stream.Zone, player.Token, stream.Player, v1Resp.Game)
		players = append(players, stream)
	}

	return api.NDSRoomCreateResponse{
		Group:   groupID,
		Game:    v1Resp.Game,
		Players: players,
		Room:    v1Resp.RoomID,
		Rom:     romPath,
	}, status, nil
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
	displayGameName := games.GameNameFromFile(fileName)
	launchGameName := strings.ToLower(strings.TrimSpace(req.Rom.SHA1))
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

	reserved, groupID, err := h.reserveNDSGroup(req.Players, roomID, launchGameName)
	if err != nil && isNDSCapacityConflict(err) {
		if spawnErr := h.spawnNDSGroup(req.Players); spawnErr != nil {
			return api.NDSRoomV1Response{}, 0, noCapacityAPIError("no free NDS worker groups")
		}
		reserved, groupID, err = h.reserveNDSGroup(req.Players, roomID, launchGameName)
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
	slots := playerSlotsByNumber(req.PlayerSlots)
	failureSeats := makeNDSFailureSeats(reserved, slots)
	failProvisioning := func(reason string) {
		h.recordNDSProvisioningFailure(roomID, endpoint, displayGameName, groupID, failureSeats, reason, now)
	}

	installReq := api.NDSRomInstallRequest{URL: req.Rom.URL, FileName: fileName, SHA1: strings.ToLower(req.Rom.SHA1)}
	romPath := filepath.ToSlash(filepath.Join("nds", launchGameName+".nds"))
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		resp, err := slot.worker.InstallNDSRom(installReq)
		if err != nil || resp == nil || resp.Game == "" {
			failProvisioning("rom_install_failed")
			return api.NDSRoomV1Response{}, 0, apiInternal(fmt.Sprintf("worker %s could not install ROM", slot.worker.Id().String()))
		}
		if resp.Path != "" {
			romPath = resp.Path
		}
	}

	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		saveURL := slots[slot.player].SaveURL
		resp, err := slot.worker.PrepareNDSSession(api.NDSSessionPrepareRequest{
			Name:          slots[slot.player].Name,
			Player:        slot.player,
			Ref:           slots[slot.player].Ref,
			RoomID:        slot.roomID,
			SaveURL:       saveURL,
			SaveUploadURL: slots[slot.player].SaveUploadURL,
		})
		if err != nil || resp == nil || *resp != api.OK {
			failProvisioning("save_prepare_failed")
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
			name:          slotReq.Name,
			player:        slot.player,
			ref:           slotReq.Ref,
			roomID:        slot.roomID,
			saveURL:       slotReq.SaveURL,
			saveStatus:    "unchanged",
			saveUploadURL: slotReq.SaveUploadURL,
			worker:        slot.worker,
		}
	}

	session := &ndsRoomSession{
		createdAt:    now,
		endpoint:     endpoint,
		game:         displayGameName,
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

func makeNDSFailureSeats(reserved []reservedNDSWorker, slots map[int]api.NDSPlayerSlotCreate) map[int]*ndsSeat {
	players := make(map[int]*ndsSeat)
	for _, slot := range reserved {
		if !slot.stream {
			continue
		}
		req := slots[slot.player]
		players[slot.player] = &ndsSeat{
			name:          req.Name,
			player:        slot.player,
			ref:           req.Ref,
			roomID:        slot.roomID,
			saveURL:       req.SaveURL,
			saveStatus:    "unchanged",
			saveUploadURL: req.SaveUploadURL,
			worker:        slot.worker,
		}
	}
	return players
}

func (h *Hub) recordNDSProvisioningFailure(roomID string, endpoint string, game string, groupID string, players map[int]*ndsSeat, reason string, now time.Time) {
	room := &ndsRoomSession{
		createdAt: now,
		endpoint:  endpoint,
		game:      game,
		groupID:   groupID,
		players:   players,
		reason:    reason,
		roomID:    roomID,
		state:     ndsRoomFailed,
		updatedAt: now,
	}
	h.ndsRooms.put(room)
	h.emitNDSWebhook("room.failed", room, map[string]any{"reason": reason})
	h.writeNDSJournal()
}

func (h *Hub) handleNDSRoomGet(w http.ResponseWriter, roomID string) {
	room := h.ndsRooms.get(roomID)
	if room == nil || ndsRoomExpiredFromRetention(room, time.Now().UTC()) {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, room.stateResponse())
}

func ndsRoomExpiredFromRetention(room *ndsRoomSession, now time.Time) bool {
	if room == nil {
		return true
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	if room.state != ndsRoomClosed || room.closedAt == nil {
		return false
	}
	return now.Sub(*room.closedAt) >= ndsClosedRetention
}

func (h *Hub) handleNDSRoomDelete(w http.ResponseWriter, roomID string) {
	room := h.ndsRooms.get(roomID)
	if room == nil {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	room.mu.Lock()
	closed := room.state == ndsRoomClosed || room.state == ndsRoomFailed
	room.mu.Unlock()
	if closed {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	if _, started := h.beginNDSRoomClose(roomID, ndsCloseHost); started {
		go h.finishNDSRoomClose(room, ndsCloseHost)
	}
	writeAPIJSON(w, http.StatusAccepted, room.stateResponse())
}

func (h *Hub) handleNDSRoomPlayerToken(w http.ResponseWriter, roomID string, player int) {
	room := h.ndsRooms.live(roomID)
	if room == nil {
		monitoring.IncNDSTokenFailure("room_not_found")
		writeAPIError(w, http.StatusNotFound, "room_not_found", "room not found")
		return
	}
	resp, ok, err := room.tokenResponse(player, h.conf.Webrtc.IceServers, time.Now().UTC())
	if err != nil {
		monitoring.IncNDSTokenFailure("sign_error")
		writeAPIError(w, http.StatusInternalServerError, "token_error", err.Error())
		return
	}
	if !ok {
		monitoring.IncNDSTokenFailure("player_not_found")
		writeAPIError(w, http.StatusNotFound, "player_not_found", "player not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, resp)
}

func validateNDSRoomCreate(req api.NDSRoomCreateRequest) error {
	if !ndsRoomIDPattern.MatchString(req.Room) {
		return badAPIRequest("invalid_room", "room must match [a-z0-9-]{3,64}")
	}
	if req.Players < 2 || req.Players > 4 {
		return badAPIRequest("invalid_players", "players must be 2, 3, or 4")
	}
	if strings.TrimSpace(req.Rom.URL) == "" {
		return badAPIRequest("invalid_rom_url", "rom.url is required")
	}
	if err := validateNDSAPIRemoteURL(req.Rom.URL, "rom_url"); err != nil {
		return err
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
		if slot.SaveURL != "" {
			if err := validateNDSAPIRemoteURL(slot.SaveURL, "save_url"); err != nil {
				return err
			}
		}
		if slot.SaveUploadURL != "" {
			if err := validateNDSAPIRemoteURL(slot.SaveUploadURL, "save_upload_url"); err != nil {
				return err
			}
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

func validateNDSAPIRemoteURL(rawURL string, field string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return badAPIRequest("invalid_"+field, field+" is not a valid URL")
	}
	if field == "rom_url" && isBuiltinNDSAPIURL(u) {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return badAPIRequest("invalid_"+field, field+" must use http or https")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return badAPIRequest("invalid_"+field, field+" host is required")
	}
	allowPrivate := ndsAPIAllowPrivateRemoteURLs()
	if ip := net.ParseIP(host); ip != nil && !allowPrivate && !isPublicNDSAPIIP(ip) {
		return badAPIRequest("invalid_"+field, field+" host resolved to a non-public IP")
	}
	if allowlist := strings.TrimSpace(firstNonEmptyEnv("NDS_DOWNLOAD_ALLOWED_HOSTS")); allowlist != "" {
		if !ndsAPIAllowedRemoteHost(host, allowlist) {
			return badAPIRequest("invalid_"+field, field+" host is not allowed")
		}
		ips, err := ndsAPILookupIP(context.Background(), "ip", host)
		if err != nil {
			return badAPIRequest("invalid_"+field, field+" host could not be resolved")
		}
		if len(ips) == 0 {
			return badAPIRequest("invalid_"+field, field+" host resolved no IPs")
		}
		for _, ip := range ips {
			if !allowPrivate && !isPublicNDSAPIIP(ip) {
				return badAPIRequest("invalid_"+field, field+" host resolved to a non-public IP")
			}
		}
	}
	return nil
}

var ndsAPILookupIP = net.DefaultResolver.LookupIP

func ndsAPIAllowPrivateRemoteURLs() bool {
	return envBool("NDS_ALLOW_PRIVATE_REMOTE_URLS", false)
}

func ndsAPIAllowedRemoteHost(host string, allowlist string) bool {
	for _, pattern := range strings.Split(allowlist, ",") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

func isPublicNDSAPIIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return !ip.IsUnspecified() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast()
}

func makeDemoPlayerSlots(roomID string, players int, saves []api.NDSPlayerSaveRequest) []api.NDSPlayerSlotCreate {
	saveURLs := map[int]string{}
	for _, save := range saves {
		if save.Player < 1 || save.Player > players {
			continue
		}
		saveURL := strings.TrimSpace(save.SRMURL)
		if saveURL == "" {
			saveURL = strings.TrimSpace(save.SaveURL)
		}
		saveURLs[save.Player] = saveURL
	}

	slots := make([]api.NDSPlayerSlotCreate, 0, players)
	for player := 1; player <= players; player++ {
		slots = append(slots, api.NDSPlayerSlotCreate{
			Name:    "Player " + strconv.Itoa(player),
			Player:  player,
			Ref:     fmt.Sprintf("%s-p%d", roomID, player),
			SaveURL: saveURLs[player],
		})
	}
	return slots
}

func cleanNDSRoomID(room string) string {
	room = strings.ToLower(strings.TrimSpace(room))
	var b strings.Builder
	lastDash := false
	for _, r := range room {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-")
	}
	if len(out) < 3 {
		return ""
	}
	return out
}

func localBuiltinNDSROMSHA1(rawURL string, fileName string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !isBuiltinNDSAPIURL(u) {
		return "", fmt.Errorf("not a builtin ROM URL")
	}
	name := games.NDSFileName(rawURL, fileName, "game.nds")
	if u.Opaque != "" {
		if decoded, err := url.PathUnescape(filepath.Base(u.Opaque)); err == nil && decoded != "" {
			name = games.NDSFileName("", decoded, "game.nds")
		}
	} else if u.Path != "" {
		name = games.NDSFileName("", filepath.Base(u.Path), "game.nds")
	}
	var data []byte
	var readErr error
	for _, base := range []string{".", "../.."} {
		data, readErr = os.ReadFile(filepath.Join(base, "assets", "games", "nds", name))
		if readErr == nil {
			break
		}
	}
	if readErr != nil {
		return "", readErr
	}
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:]), nil
}

func isBuiltinNDSAPIURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "builtin" || u.Scheme == "local") && (u.Opaque != "" || u.Path != "")
}

func ndsDemoStreamURL(baseURL string, roomID string, zone string, token string, player int, game string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		u = &url.URL{Scheme: "http", Host: baseURL}
	}
	u.Path = "/"
	q := u.Query()
	q.Set("id", roomID)
	q.Set("player", strconv.Itoa(player))
	q.Set("client", "v9")
	q.Set("view", "stream")
	q.Set("token", token)
	if game != "" {
		q.Set("game", game)
	}
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

func playerSlotsByNumber(slots []api.NDSPlayerSlotCreate) map[int]api.NDSPlayerSlotCreate {
	out := make(map[int]api.NDSPlayerSlotCreate, len(slots))
	for _, slot := range slots {
		slot.Name = strings.TrimSpace(slot.Name)
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
