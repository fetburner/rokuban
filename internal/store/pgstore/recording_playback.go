package pgstore

// recordingPlaybackVisibleSQL は録画 API と原本 VOD が共有する削除状態の述語。
// r は recordings の別名。原本の状態とは独立に temp の公開にも適用する。
const recordingPlaybackVisibleSQL = `r.deleted_at IS NULL AND r.purged_at IS NULL AND r.superseded_at IS NULL`

// activePlaybackOriginalSQL は両経路が共有する原本の述語。a は media_assets の別名。
const activePlaybackOriginalSQL = `a.kind = 'original' AND a.state = 'active'`

// recordingPlaybackFinishedSQL は原本 VOD と世代付き録画 HLS が共有する完了状態の述語。
// 録画 API の一覧は録画中も返すため recordingPlaybackVisibleSQL には含めない。
const recordingPlaybackFinishedSQL = `r.status = 'finished'`
