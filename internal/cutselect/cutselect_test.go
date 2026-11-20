package cutselect

import "testing"

// TestValidateCutSelection は「cut を選ぶなら cut でないプロファイルを 1 つ以上」
// の判定を固定する。4 経路（ルール / override / 事後追加 / ingest の凍結）が
// この 1 つを共有するので、ここが緩むと 4 経路すべてが同時に緩む。
func TestValidateCutSelection(t *testing.T) {
	cut := map[string]struct{}{"cut": {}}
	cases := []struct {
		name                string
		names               []string
		cut                 map[string]struct{}
		recordingHLSEnabled bool
		want                bool
	}{
		{"cut only is rejected without recording hls", []string{"cut"}, cut, false, true},
		{"cut only is accepted with recording hls", []string{"cut"}, cut, true, false},
		{"two cut profiles only are rejected without recording hls", []string{"cut", "cut2"}, map[string]struct{}{"cut": {}, "cut2": {}}, false, true},
		{"two cut profiles only are accepted with recording hls", []string{"cut", "cut2"}, map[string]struct{}{"cut": {}, "cut2": {}}, true, false},
		{"cut plus a normal profile is accepted", []string{"cut", "h264"}, cut, false, false},
		{"normal profile first is accepted", []string{"h264", "cut"}, cut, false, false},
		{"normal only is accepted", []string{"h264"}, cut, false, false},
		{"empty selection is accepted", nil, cut, false, false},
		// cut プロファイルが 1 つも定義されていない構成では何も主張しない。
		{"no cut profiles configured", []string{"h264"}, map[string]struct{}{}, false, false},
		{"nil cut set", []string{"cut"}, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCutSelection(tc.names, tc.cut, tc.recordingHLSEnabled)
			if (err != nil) != tc.want {
				t.Errorf("ValidateCutSelection(%v, recordingHLSEnabled=%t) error = %v, want error %v", tc.names, tc.recordingHLSEnabled, err, tc.want)
			}
		})
	}
}
