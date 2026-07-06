package coordinator

import "github.com/giongto35/cloud-game/v3/pkg/api"

func (w *Worker) HandleRegisterRoom(rq api.RegisterRoomRequest) {
	w.RoomId = string(rq)
	if w.ReservedRoomId == w.RoomId {
		w.ReservedRoomId = ""
	}
}

func (w *Worker) HandleCloseRoom(rq api.CloseRoomRequest) {
	if w.hub != nil {
		w.hub.recordNDSStreamBytes(rq.RoomID, rq.BytesStreamed)
	}
	if rq.RoomID == w.RoomId {
		w.RoomId = ""
		w.ReservedRoomId = ""
		w.FreeSlots()
	}
}

func (w *Worker) HandleIceCandidate(rq api.WebrtcSignalRequest, users HasUserRegistry) error {
	if usr := users.Find(rq.Id); usr != nil {
		ice := *rq.Ice
		if w.rtcMux != nil {
			ice = w.rtcMux.rewriteWorkerICE(rq.Id, w, ice)
		}
		usr.SendWebrtcIceCandidate(ice)
	} else {
		w.log.Warn().Str("id", rq.Id).Msg("unknown session")
	}
	return nil
}

func (w *Worker) HandleLibGameList(inf api.LibGameListInfo) error {
	w.SetLib(inf.List)
	return nil
}

func (w *Worker) HandlePrevSessionList(sess api.PrevSessionInfo) error {
	if len(sess.List) == 0 {
		return nil
	}

	m := make(map[string]struct{})
	for _, v := range sess.List {
		m[v] = struct{}{}
	}
	w.SetSessions(m)
	return nil
}

func (w *Worker) HandleNDSSaveUploaded(status api.NDSSaveStatus) error {
	if w.hub == nil {
		return nil
	}
	w.hub.recordNDSSaveStatus(status)
	return nil
}
