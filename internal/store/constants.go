package store

import "github.com/fetburner/rokuban/internal/sitename"

// DefaultSite は site が未指定のときに解決される既定のサイト名。
//
// 値の定義は internal/sitename にある。jobs が store に依存しないよう葉のパッケージに
// 置き、ここは既存の利用者（api・watcher など）を動かさないための別名として残す。
const DefaultSite = sitename.Default

// 録画ステータス。mirakc の RecordInfo.Status と recordings の CHECK 制約で使う。
// store の録画観測（RecordObservation.Status）が取る値でもある。
const (
	RecordingStatusRecording = "recording"
	RecordingStatusFinished  = "finished"
	RecordingStatusCanceled  = "canceled"
	RecordingStatusFailed    = "failed"
)

// メディアアセット種別。store の契約でメディアアセットの Kind が取る値（InPlaceAsset.Kind、MediaDeleteAsset.Kind など）。
const (
	AssetKindOriginal  = "original"
	AssetKindEncoded   = "encoded"
	AssetKindThumbnail = "thumbnail"
	AssetKindSeekTiles = "seek_tiles"
)
