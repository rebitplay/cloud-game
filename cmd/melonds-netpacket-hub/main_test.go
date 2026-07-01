package main

import "testing"

func TestValidPeerID(t *testing.T) {
	tests := []struct {
		name string
		id   uint16
		want bool
	}{
		{name: "host", id: 0, want: true},
		{name: "client one", id: 1, want: true},
		{name: "broadcast is not a source peer", id: broadcastID, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validPeerID(tt.id); got != tt.want {
				t.Fatalf("validPeerID(%d) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
