package pgstore

// recordingPlaybackVisibleSQL は録画 API と原本 VOD が共有する削除状態の述語。
// r は recordings の別名。原本の状態とは独立に temp の公開にも適用する。
const recordingPlaybackVisibleSQL = `r.deleted_at IS NULL AND r.purged_at IS NULL AND r.superseded_at IS NULL`

// recordingPlaybackFinishedSQL は原本 VOD が使う完了状態の述語。世代付き録画 HLS は録画中も
// temp を配信するので使わず、原本の行と進捗行の有無で入力を決める。
// 録画 API の一覧は録画中も返すため recordingPlaybackVisibleSQL には含めない。
const recordingPlaybackFinishedSQL = `r.status = 'finished'`

// activePlaybackOriginalSQL は一覧と再生が共有する原本コピーの述語を返す。a は media_assets の別名。
// storeParam は置き場名を束縛したプレースホルダ（例 "$1"）で、呼び出し側が自分の引数の位置を渡す。
// プレースホルダが NULL なら置き場を限定しない。
func activePlaybackOriginalSQL(storeParam string) string {
	return "a.kind = 'original' AND EXISTS (SELECT 1 FROM active_media_asset_copies(a.id, " + storeParam + "::text))"
}
