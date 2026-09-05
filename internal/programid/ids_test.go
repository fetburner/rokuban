package programid

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

// TestSplitServiceID は ServiceID の逆変換を両方向で固定する。
//
// **期待値はリテラル。** `ServiceID(n, s)` を呼んで比べると、合成側と分解側が
// 同じ式を共有しているので、両方を同時に壊しても緑のまま通る。
func TestSplitServiceID(t *testing.T) {
	tests := []struct {
		id                   int64
		wantNetwork, wantSvc int
	}{
		{3273601024, 32736, 1024}, // NHK総合（GR）
		{400101, 4, 101},          // BS 101
		{600101, 6, 101},          // 110度CS 101（BS と serviceId が衝突する実例）
		{100001, 1, 1},
		{6553565535, 65535, 65535}, // 上限（MaxServiceID）
	}
	for _, tt := range tests {
		gotNetwork, gotSvc := SplitServiceID(tt.id)
		if gotNetwork != tt.wantNetwork || gotSvc != tt.wantSvc {
			t.Errorf("SplitServiceID(%d) = (%d, %d), want (%d, %d)",
				tt.id, gotNetwork, gotSvc, tt.wantNetwork, tt.wantSvc)
		}
		if back := ServiceID(tt.wantNetwork, tt.wantSvc); back != tt.id {
			t.Errorf("ServiceID(%d, %d) = %d, want %d", tt.wantNetwork, tt.wantSvc, back, tt.id)
		}
	}
}

// TestMaxServiceID は上限が「networkId / serviceId とも 16bit の最大」であることを
// 固定する。ここを下げると実在しうるチャンネル（networkId = 65535）が 400 になる。
//
// **spec との一致はここでは見ない。** 同じ数字は openapi.yaml の `maximum` にも
// あり（そこから web/src/api/zod.ts が生成される）、両者のずれは
// internal/api の TestSpecServiceBoundsMatchGo が openapi.yaml を実際に読んで
// 検査する。このテストが見るのは Go 側の値そのもの。
func TestMaxServiceID(t *testing.T) {
	if MaxServiceID != 6553565535 {
		t.Errorf("MaxServiceID = %d, want 6553565535 (65535*100000+65535)", MaxServiceID)
	}
	if got := ServiceID(65535, 65535); got != MaxServiceID {
		t.Errorf("ServiceID(65535, 65535) = %d, want MaxServiceID %d", got, MaxServiceID)
	}
	n, s := SplitServiceID(MaxServiceID)
	if n != 65535 || s != 65535 {
		t.Errorf("SplitServiceID(MaxServiceID) = (%d, %d), want (65535, 65535)", n, s)
	}
}
