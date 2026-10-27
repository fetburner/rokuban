package pgstore

// recordingPlaybackVisibleSQL は録画 API と原本 VOD が共有する削除状態の述語。
// r は recordings の別名。原本の状態とは独立に temp の公開にも適用する。
const recordingPlaybackVisibleSQL = `r.deleted_at IS NULL AND r.purged_at IS NULL AND r.superseded_at IS NULL`

// recordingPlaybackFinishedSQL は原本 VOD と世代付き録画 HLS が共有する完了状態の述語。
// 録画 API の一覧は録画中も返すため recordingPlaybackVisibleSQL には含めない。
const recordingPlaybackFinishedSQL = `r.status = 'finished'`

// activePlaybackOriginalSQL は一覧と再生が共有する原本コピーの述語。a は media_assets の別名。
// storeName が NULL の構成では、置き場を限定しない。
const activePlaybackOriginalSQL = `a.kind = 'original' AND EXISTS (
    SELECT 1 FROM media_asset_copies copy
    WHERE copy.media_asset_id = a.id
      AND ($1::text IS NULL OR copy.store = $1)
      AND copy.state = 'active'
)`
