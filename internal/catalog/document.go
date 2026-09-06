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
//
// **列や JSON キーの引っ越し（同じ事実を別のキー・別の配列に移すこと）は
// 破壊的変更として数えない。** この版は ReadManifest / read が「自分より新しい
// 版は読めない」で**拒否する**ゲートに使われる（manifest.go）。上げると、
// 新しいバイナリが書いたダンプは古いバイナリで rescue できなくなる ——
// 災害復旧で古いバイナリしか手元にない状況を作りたくない。
//
// 引っ越しで版を上げないぶん、「新ダンプ × 旧バイナリ」では旧バイナリが知らない
// キーを黙って無視するので引っ越した事実は落ちる —— 即時削除の要求
// （recordingPurgeRequests）が落ちた場合、その録画はごみ箱に残って猶予超過で
// 消える（要求より遅く消える側に倒れる）。録画本体・アセット・tombstone は
// 残る。読めない方（版を上げる）よりこちらを選ぶ。
//
// **落ちたときに安全側へ倒れない事実を新しいキーへ移すなら、版を上げる。**
// この線引きは docs/storage/rescue.md §世代の完成判定 にある。
//
// **ただしゲートは片側であることに注意。** manifest.go が拒否するのは
// 「自分より新しい版」だけで、下限は無い —— 版を上げても**古いダンプは
// 引き続き読まれる**。「版を上げれば古い形式を拒否できる」は成り立たないので、
// 古い形式を読ませたくないなら下限の検査を足す必要がある。
const Version = 1

// Subdir は media_dir 配下の catalog ディレクトリ名。
const Subdir = "catalog"

// DefaultKeep は世代保持の既定値（最新 N 件の**完成世代**を残す。
// docs/storage.md §8「不完全世代の保持と掃除」）。
const DefaultKeep = 7

// FilenamePrefix は世代ディレクトリ名の接頭辞（`catalog-<UTC 時刻>`）。
const FilenamePrefix = "catalog-"

// Document は export / rescue で共有する catalog JSON の形。
type Document struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`

	Rules                   []Rule                  `json:"rules"`
	Recordings              []Recording             `json:"recordings"`
	RecordingEncodePolicies []RecordingEncodePolicy `json:"recordingEncodePolicies"`
	RecordingPurgeRequests  []RecordingPurgeRequest `json:"recordingPurgeRequests"`
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

// Recording は recordings の 1 行（tombstone 含む）。Source は録画作成時の
// snapshot をそのまま保持する。ストレージ再スキャンの rescue は予約や意図を
// 特定できないため "unattributed" を書き戻す。
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
	// 欠測は never_scheduled_events 表が持ち、recordings は観測された試行だけを
	// 持つ。欠測は過去番組の観測なので、放送 + 猶予で消える snapshots や復旧後の
	// 未来予約とは無関係であり、catalog の対象にしない。
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	// SupersededAt は「この行が active-event の枠を明け渡した」不可逆な事実
	// （issue #129 症状 2）。落とすと rescue 側で superseded 行と生きている行が
	// どちらも live に戻り、recordings_unique_active_event に衝突して復旧が
	// 落ちる。nil = live。
	SupersededAt *time.Time `json:"supersededAt,omitempty"`
	// PurgedAt は「完全削除が完了した」不可逆な事実（issue #135）。落とすと
	// rescue 後にごみ箱ビュー（purged_at IS NULL を要求）が purge 済みの
	// tombstone を再び蘇らせてしまう。nil = 未 purge。
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

// RecordingPurgeRequest は recording_purge_requests の 1 行（「ごみ箱の猶予を
// 待たず今すぐ完全削除してほしい」というユーザーの要求）。
//
// **行の有無そのものが意味を持つ**（不変条件 10）。この録画の RecordingID が
// Document.RecordingPurgeRequests に載っていなければ「要求は無い」であり、
// rescue はそれを既定値の行で埋めない。RequestedAt は判定には使わない
// （判定は行の存在だけ）。
type RecordingPurgeRequest struct {
	RecordingID int64     `json:"recordingId"`
	RequestedAt time.Time `json:"requestedAt"`
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
// 放送イベントの識別（network_id, service_id, event_id）と表示名は catalog の
// 往復で失ってはならない（reconciler が欠測を書くとき、および watcher が録画行を
// 作るときに snapshot から引く）。
//
// **識別 6 列は非ポインタだが、値が正しい保証はない。** ディスク上のバックアップは
// 手で編集されうるし、書き込みが途中で切れることもある。ただし rescue が弾くのは
// 「DB が実際に拒否する行」だけで、「放送を同定できない行」ではない —— CHECK の
// 掛かった channel_type が列挙外のときだけ applyProgramSnapshots が
// insertableSnapshot で落とす。他の 5 列は NOT NULL しか無いので、空でも 0 でも
// そのまま復元する（FK 先を失うと program_intents / program_overrides —— ユーザーが
// 明示した意図 —— も連動して落ちる。理由は insertableSnapshot の doc コメント。
// internal/catalog/rescue.go 参照）。
type ProgramSnapshot struct {
	Site        string    `json:"site"`
	ProgramID   int64     `json:"programId"`
	Title       string    `json:"title"`
	StartAt     time.Time `json:"startAt"`
	DurationMs  int64     `json:"durationMs"`
	NetworkID   int32     `json:"networkId"`
	ServiceID   int32     `json:"serviceId"`
	ChannelType string    `json:"channelType"`
	Channel     string    `json:"channel"`
	EventID     int32     `json:"eventId"`
	ServiceName string    `json:"serviceName"`
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
