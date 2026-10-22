package store

// DefaultSite は site が未指定のときに解決される既定のサイト名。
//
// site は設定ファイル（`mirakcs:` レジストリ、[docs/schema.md] §1-5）で定義する
// サイト名で、空文字列で参照された箇所（`--sites` 省略時の 0 サイト束縛や
// verifySite の空 site 引数）がこの値に解決される。
//
// mirakc を指すすべてのテーブルが site を持つので、既定値の定義は 1 箇所に置く
// （api と watcher が別々に "default" を書いていたのを統合した）。
const DefaultSite = "default"

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
