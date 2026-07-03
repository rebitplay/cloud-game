package api

import (
	"encoding/json"
	"testing"
)

func TestCloseRoomRequestUnmarshalStringAndObject(t *testing.T) {
	var legacy CloseRoomRequest
	if err := json.Unmarshal([]byte(`"room-p1"`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.RoomID != "room-p1" || legacy.BytesStreamed != 0 {
		t.Fatalf("legacy = %#v", legacy)
	}

	var structured CloseRoomRequest
	if err := json.Unmarshal([]byte(`{"room_id":"room-p2","bytes_streamed":1234}`), &structured); err != nil {
		t.Fatal(err)
	}
	if structured.RoomID != "room-p2" || structured.BytesStreamed != 1234 {
		t.Fatalf("structured = %#v", structured)
	}
}
