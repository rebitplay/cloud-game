package libretro

import "testing"

func TestNetpacketRoomFromSessionID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "mkds lan player room",
			in:   "mkds-demo-launch-p3___Mario-Kart-DS-USA",
			want: "mkds-demo-launch",
		},
		{
			name: "keeps regular deep link",
			in:   "room-123___Mario-Kart-DS-USA",
			want: "room-123",
		},
		{
			name: "uses last player suffix",
			in:   "mkds-pro-room-p4___Mario-Kart-DS-USA",
			want: "mkds-pro-room",
		},
		{
			name: "keeps non player suffix",
			in:   "room-px___Mario-Kart-DS-USA",
			want: "room-px",
		},
		{
			name: "keeps plain room",
			in:   "mkds-demo",
			want: "mkds-demo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := netpacketRoomFromSessionID(tt.in); got != tt.want {
				t.Fatalf("netpacketRoomFromSessionID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
