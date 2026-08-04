// Package catalog は災害復旧用のコアメタデータ JSON の export / rescue を担う
// （docs/storage.md §8、issue #71 M3-9）。
//
// 保護対象はルール・録画履歴・media_assets・ドロップ統計・tombstone・
// 手動オーバーライド（と意図の FK 先 program_snapshots）のみ。EPG 射影と
// ジョブキューは再構築可能なので含めない。pg_dump に依存しない。
package catalog

import (
	"encoding/json"
	"time"
)

// Version は catalog JSON のスキーマ版。破壊的変更で上げる。
const Version = 1

// Subdir は media_dir 配下の catalog ディレクトリ名。
const Subdir = "catalog"

// DefaultKeep は世代保持の既定値（最新 N 件）。
const DefaultKeep = 7

// FilenamePrefix は catalog ファイル名の接頭辞。
const FilenamePrefix = "catalog-"

// Document は export / rescue で共有する catalog JSON の形。
type Document struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`
	// Site は export 時に絞り込んだサイト。空 / omit なら全サイト。
	Site *string `json:"site,omitempty"`

	Rules                   []Rule                  `json:"rules"`
	Recordings              []Recording             `json:"recordings"`
	RecordingEncodePolicies []RecordingEncodePolicy `json:"recordingEncodePolicies"`
	MediaAssets             []MediaAsset            `json:"mediaAssets"`
	DropStats               []DropStat              `json:"dropStats"`
	ProgramSnapshots        []ProgramSnapshot       `json:"programSnapshots"`
	ProgramIntents          []ProgramIntent         `json:"programIntents"`
	ProgramOverrides        []ProgramOverride       `json:"programOverrides"`
}

// Rule は rules 本体と子テーブルをまとめた 1 ルール分。
type Rule struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	Enabled         bool       `json:"enabled"`
	Priority        int32      `json:"priority"`
	IsFree          *bool      `json:"isFree,omitempty"`
	DurationMinMs   *int64     `json:"durationMinMs,omitempty"`
	DurationMaxMs   *int64     `json:"durationMaxMs,omitempty"`
	PeriodStartAt   *time.Time `json:"periodStartAt,omitempty"`
	PeriodEndAt     *time.Time `json:"periodEndAt,omitempty"`
	DedupeEnabled   bool       `json:"dedupeEnabled"`
	DedupeThreshold *float32   `json:"dedupeThreshold,omitempty"`
	// DedupeWindowSeconds は dedupe_window を秒に直した値。NULL は時間窓なし。
	DedupeWindowSeconds *int64          `json:"dedupeWindowSeconds,omitempty"`
	KeepOriginal        string          `json:"keepOriginal"`
	EncodeProfiles      []string        `json:"encodeProfiles"`
	FilenameTemplate    string          `json:"filenameTemplate"`
	Metadata            json.RawMessage `json:"metadata"`
	CreatedAt           time.Time       `json:"createdAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`

	TextMatches  []RuleTextMatch `json:"textMatches,omitempty"`
	Services     []RuleService   `json:"services,omitempty"`
	ChannelTypes []string        `json:"channelTypes,omitempty"`
	Genres       []int16         `json:"genres,omitempty"`
	Times        []RuleTime      `json:"times,omitempty"`
	Sites        []string        `json:"sites,omitempty"`
}

// RuleTextMatch は rule_text_matches の 1 行。
type RuleTextMatch struct {
	Seq           int32  `json:"seq"`
	Target        string `json:"target"`
	Mode          string `json:"mode"`
	Value         string `json:"value"`
	CaseSensitive bool   `json:"caseSensitive"`
	Negate        bool   `json:"negate"`
}

// RuleService は rule_services の 1 行。
type RuleService struct {
	NetworkID int32 `json:"networkId"`
	ServiceID int32 `json:"serviceId"`
}

// RuleTime は rule_times の 1 行。
type RuleTime struct {
	Seq      int32 `json:"seq"`
	Weekdays int32 `json:"weekdays"`
	StartSec int32 `json:"startSec"`
	EndSec   int32 `json:"endSec"`
}

// Recording は recordings の 1 行（tombstone 含む）。
// recordings.reservation_id は元々「導出物なので export しない」として
// 常に NULL で rescue していたが、issue #158 で列自体を落としたので、
// この構造体にはそもそも存在しない。
type Recording struct {
	ID                int64           `json:"id"`
	RuleID            *int64          `json:"ruleId,omitempty"`
	Source            string          `json:"source"`
	Site              string          `json:"site"`
	NetworkID         int32           `json:"networkId"`
	ServiceID         int32           `json:"serviceId"`
	EventID           int32           `json:"eventId"`
	ServiceName       string          `json:"serviceName"`
	ChannelType       string          `json:"channelType"`
	Channel           string          `json:"channel"`
	Title             string          `json:"title"`
	Description       *string         `json:"description,omitempty"`
	Extended          json.RawMessage `json:"extended,omitempty"`
	Genres            json.RawMessage `json:"genres,omitempty"`
	IsFree            bool            `json:"isFree"`
	ProgramStartAt    time.Time       `json:"programStartAt"`
	ProgramDurationMs int64           `json:"programDurationMs"`
	Status            string          `json:"status"`
	StartedAt         *time.Time      `json:"startedAt,omitempty"`
	EndedAt           *time.Time      `json:"endedAt,omitempty"`
	QualityEvents     json.RawMessage `json:"qualityEvents"`
	DeletedAt         *time.Time      `json:"deletedAt,omitempty"`
	PurgeAfter        *time.Time      `json:"purgeAfter,omitempty"`
	// KeepOriginalLegacy / EncodeProfilesLegacy: issue #159 より前は
	// recordings.keep_original / recordings.encode_profiles だった旧列。
	// 現在は RecordingEncodePolicy（recording_encode_policy 衛星表）に切り出した
	// ため、新しい export はこの 2 フィールドを書かない（常に nil で omit
	// される）。#159 より前に export された古いダンプは "keepOriginal" /
	// "encodeProfiles" キーを常に持つので、rescue 側はこれが non-nil であることを
	// 「旧ダンプである」判定に使い、migration 00030 backfill と同じ基準（原本
	// media_asset の有無。列の値そのものは使わない）でこのダンプ内の
	// doc.MediaAssets から recording_encode_policy 行を復元する
	// （internal/catalog/rescue.go 参照）。落とすと旧ダンプの rescue で凍結済み
	// ポリシーが黙って失われる（削除エンジンが対象外になり、事後追加は
	// 既定値 'always' で上書きされる）。
	KeepOriginalLegacy   *string  `json:"keepOriginal,omitempty"`
	EncodeProfilesLegacy []string `json:"encodeProfiles,omitempty"`
	// SupersededAt は「この行が active-event の枠を明け渡した」不可逆な事実
	// （issue #129 症状 2）。落とすと rescue 側で superseded 行と生きている行が
	// どちらも live に戻り、recordings_unique_active_event に衝突して復旧が
	// 落ちる。古い世代のカタログには存在しないので omitempty（nil = live）。
	SupersededAt *time.Time `json:"supersededAt,omitempty"`
	// PurgedAt は「完全削除が完了した」不可逆な事実（issue #135）。落とすと
	// rescue 後にごみ箱ビュー（purged_at IS NULL を要求）が purge 済みの
	// tombstone を再び蘇らせてしまう。古い世代のカタログには存在しないので
	// omitempty（nil = 未 purge）。
	PurgedAt  *time.Time `json:"purgedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// RecordingEncodePolicy は recording_encode_policy の 1 行（issue #159。凍結済み
// 「この録画の望ましい最終状態」）。
//
// **行の有無そのものが意味を持つ**（不変条件 10）。この録画の RecordingID が
// Document.RecordingEncodePolicies に載っていなければ「未凍結」であり、rescue は
// それを既定値の行で埋めない（Recording と違い、載っていない録画には何も
// upsert しない。internal/catalog/rescue.go 参照）。
type RecordingEncodePolicy struct {
	RecordingID    int64     `json:"recordingId"`
	KeepOriginal   string    `json:"keepOriginal"`
	EncodeProfiles []string  `json:"encodeProfiles"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// MediaAsset は media_assets の 1 行。
type MediaAsset struct {
	ID          int64      `json:"id"`
	RecordingID int64      `json:"recordingId"`
	Kind        string     `json:"kind"`
	Profile     *string    `json:"profile,omitempty"`
	RelPath     string     `json:"relPath"`
	SizeBytes   int64      `json:"sizeBytes"`
	State       string     `json:"state"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// DropStat は drop_stats の 1 行。
type DropStat struct {
	MediaAssetID int64   `json:"mediaAssetId"`
	Pid          int32   `json:"pid"`
	Packets      int64   `json:"packets"`
	Drops        int64   `json:"drops"`
	Errors       int64   `json:"errors"`
	Scrambled    int64   `json:"scrambled"`
	PidType      *string `json:"pidType,omitempty"`
}

// ProgramSnapshot は program_snapshots の 1 行（意図・上書きの FK 先）。
//
// EventID / ServiceName は issue #98 で追加された列（00025）。
// reconciler.recordNeverScheduled が recordings の never-scheduled 行を作る
// ときの識別（network_id, service_id, event_id）と表示名に使うため、
// 他のチャンネル識別列と同様に catalog の往復で失ってはならない。
// 古い世代の catalog には存在しないので omitempty（nil = 未対応 or 移行前）。
//
// **チャンネル・イベント識別 6 列はポインタのまま残す。** DB 側の
// program_snapshots は issue #101（00026）でこの 6 列を NOT NULL 化したが、
// catalog document は DB より寿命が長い（ディスク上のバックアップファイルは
// マイグレーションを追いかけない）。00026 より前に export された古い
// catalog ダンプを rescue する経路では nil がありうるため、この Document 型
// 自体は緩いままにする（applyDocument が nil を安全側でスキップする。
// internal/catalog/rescue.go 参照）。
type ProgramSnapshot struct {
	Site        string    `json:"site"`
	ProgramID   int64     `json:"programId"`
	Title       string    `json:"title"`
	StartAt     time.Time `json:"startAt"`
	DurationMs  int64     `json:"durationMs"`
	NetworkID   *int32    `json:"networkId,omitempty"`
	ServiceID   *int32    `json:"serviceId,omitempty"`
	ChannelType *string   `json:"channelType,omitempty"`
	Channel     *string   `json:"channel,omitempty"`
	EventID     *int32    `json:"eventId,omitempty"`
	ServiceName *string   `json:"serviceName,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// ProgramIntent は program_intents の 1 行。
type ProgramIntent struct {
	Site      string    `json:"site"`
	ProgramID int64     `json:"programId"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ProgramOverride は program_overrides の 1 行。
type ProgramOverride struct {
	Site      string          `json:"site"`
	ProgramID int64           `json:"programId"`
	Overrides json.RawMessage `json:"overrides"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}
