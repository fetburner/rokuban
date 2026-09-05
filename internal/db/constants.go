package db

// DefaultSite は site が未指定のときに解決される既定のサイト名。
//
// site は設定ファイル（`mirakcs:` レジストリ、[docs/schema.md] §1-5）で定義する
// サイト名で、空文字列で参照された箇所（`--sites` 省略時の 0 サイト束縛や
// verifySite の空 site 引数）がこの値に解決される。
//
// mirakc を指すすべてのテーブルが site を持つので、既定値の定義は 1 箇所に置く
// （api と watcher が別々に "default" を書いていたのを統合した）。
const DefaultSite = "default"

// 予約状態のラベル。Phase 1 以降、`reservations.state` という列は存在しない。
// active/detached は (rule_id, base) から、orphaned は recordings の存在から
// 導出する。この定数はテスト・ログでの表記を揃えるためだけに残す。
const (
	ReservationStateActive   = "active"
	ReservationStateDetached = "detached"
	ReservationStateOrphaned = "orphaned"
)

// 録画ステータス。mirakc の RecordInfo.Status と recordings の CHECK 制約で使う。
const (
	RecordingStatusRecording = "recording"
	RecordingStatusFinished  = "finished"
	RecordingStatusCanceled  = "canceled"
	RecordingStatusFailed    = "failed"
)

// メディアアセット種別。
const (
	AssetKindOriginal  = "original"
	AssetKindEncoded   = "encoded"
	AssetKindThumbnail = "thumbnail"
)

// メディアアセット状態。
const (
	AssetStateActive   = "active"
	AssetStateDeleting = "deleting"
	AssetStateDeleted  = "deleted"
)
