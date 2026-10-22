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
const (
	// RecordingStatusRecording は録画中を表すステータス。
	RecordingStatusRecording = "recording"
	// RecordingStatusFinished は録画終了を表すステータス。
	RecordingStatusFinished = "finished"
	// RecordingStatusCanceled は録画キャンセルを表すステータス。
	RecordingStatusCanceled = "canceled"
	// RecordingStatusFailed は録画失敗を表すステータス。
	RecordingStatusFailed = "failed"
)

// メディアアセット種別。
const (
	// AssetKindOriginal は原本メディアアセットの種別。
	AssetKindOriginal = "original"
	// AssetKindEncoded はエンコード済みメディアアセットの種別。
	AssetKindEncoded = "encoded"
	// AssetKindThumbnail はサムネイルメディアアセットの種別。
	AssetKindThumbnail = "thumbnail"
	// AssetKindSeekTiles はシークタイルメディアアセットの種別。
	AssetKindSeekTiles = "seek_tiles"
)
