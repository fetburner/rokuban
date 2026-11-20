// Package cutselect は cut（チャプターで切ったカット版）を選ぶプロファイル集合の規則を置く。
// 録画 HLS の有無で、cut だけの選択が許されるかが変わる。
package cutselect

import "fmt"

// ValidateCutSelection は、録画 HLS が無効な構成で「cut のプロファイルを選ぶなら
// cut でないプロファイルを 1 つ以上含む」ことを検査する。録画 HLS が無いと
// 確認に再生が要り、再生に encode が要り、encode に確認が要る循環になるため
// （チャプターの確認は録画 HLS の再生でだけできる）。
// 録画 HLS が有効なら録画 HLS で確認できるので cut だけの選択も許す。
// ルール検証・override 検証・ingest の凍結・POST /api/recordings/{id}/encode-profiles
// の 4 経路がこの同じ判定を使う。
//
// cut が空の集合なら常に nil（cut プロファイルが 1 つも定義されていない構成では
// この規則は何も主張しない）。names が空でも nil。録画 HLS が有効なら常に nil。
func ValidateCutSelection(names []string, cut map[string]struct{}, recordingHLSEnabled bool) error {
	if recordingHLSEnabled || len(cut) == 0 {
		return nil
	}
	sawCut := false
	for _, name := range names {
		if _, ok := cut[name]; ok {
			sawCut = true
			continue
		}
		return nil
	}
	if !sawCut {
		return nil
	}
	return fmt.Errorf("encodeProfiles contains only cut profiles; a profile with cut unset is required so the recording can be reviewed before trimming")
}
