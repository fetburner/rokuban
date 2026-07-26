package mirakc

import "testing"

func TestSplitProgramID(t *testing.T) {
	tests := []struct {
		name          string
		programID     int64
		wantNetworkID int
		wantServiceID int
		wantEventID   int
	}{
		{"実測値", 327360102415397, 32736, 1024, 15397},
		{"contentpath_test.go の値", 100000500011234, 10000, 5000, 11234},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			networkID, serviceID, eventID := SplitProgramID(tt.programID)
			if networkID != tt.wantNetworkID || serviceID != tt.wantServiceID || eventID != tt.wantEventID {
				t.Errorf("SplitProgramID(%d) = (%d, %d, %d), want (%d, %d, %d)",
					tt.programID, networkID, serviceID, eventID,
					tt.wantNetworkID, tt.wantServiceID, tt.wantEventID)
			}
		})
	}
}

func TestComposeProgramID_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		networkID int
		serviceID int
		eventID   int
	}{
		{"最小値", 0, 0, 0},
		{"最大値(16bit境界)", 65535, 65535, 65535},
		{"実測値", 32736, 1024, 15397},
		{"contentpath_test.go の値", 10000, 5000, 11234},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			programID := ComposeProgramID(tt.networkID, tt.serviceID, tt.eventID)
			gotNetworkID, gotServiceID, gotEventID := SplitProgramID(programID)
			if gotNetworkID != tt.networkID || gotServiceID != tt.serviceID || gotEventID != tt.eventID {
				t.Errorf("SplitProgramID(ComposeProgramID(%d, %d, %d)) = (%d, %d, %d), want original values",
					tt.networkID, tt.serviceID, tt.eventID, gotNetworkID, gotServiceID, gotEventID)
			}
		})
	}
}

func TestComposeProgramID(t *testing.T) {
	got := ComposeProgramID(32736, 1024, 15397)
	want := int64(327360102415397)
	if got != want {
		t.Errorf("ComposeProgramID(32736, 1024, 15397) = %d, want %d", got, want)
	}
}

func TestServiceID(t *testing.T) {
	tests := []struct {
		name      string
		networkID int
		serviceID int
		want      int64
	}{
		{"最小値", 0, 0, 0},
		{"最大値(16bit境界)", 65535, 65535, 6553565535},
		{"実測値", 32736, 1024, 3273601024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ServiceID(tt.networkID, tt.serviceID)
			if got != tt.want {
				t.Errorf("ServiceID(%d, %d) = %d, want %d", tt.networkID, tt.serviceID, got, tt.want)
			}
		})
	}
}

// TestSplitProgramID_RealEPGStationData は実機の EPGStation（v2.10.0）の
// GET /api/reserves のレスポンスから取った値で分解を検算する。
//
// programId から導いた service id が、同じレスポンスの channelId と一致することを
// 見る。合成規則（NID*10^10 + SID*10^5 + EID / NID*10^5 + SID）が実際の
// Mirakurun 互換 ID と合っていることの、実データによる裏付け。
func TestSplitProgramID_RealEPGStationData(t *testing.T) {
	const (
		programID = int64(319205324851361)
		channelID = int64(3192053248)
	)
	nid, sid, eid := SplitProgramID(programID)
	if got := ServiceID(nid, sid); got != channelID {
		t.Errorf("ServiceID(%d, %d) = %d, want %d (実機の channelId)", nid, sid, got, channelID)
	}
	if got := ComposeProgramID(nid, sid, eid); got != programID {
		t.Errorf("ComposeProgramID(%d, %d, %d) = %d, want %d", nid, sid, eid, got, programID)
	}
}
