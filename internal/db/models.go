package db

import (
	"encoding/json"
	"fmt"
	"time"
)

// Reservation は予約（ruler の 1 パスの導出出力）。
//
// Phase 1（#27/#28/#30）で番組の事実のスナップショット（title / 開始時刻 / 尺 /
// チャンネル識別）は program_snapshots に抽出され、state（active/detached）は
// (rule_id, base) から導出する値になったため列としては撤去された。「番組終了後に
// schedule が観測されなかった」という不可逆な観測は一時 orphaned_at 列を経て
// （Phase 1）、issue #98 で recordings の試行行（status='failed' +
// quality_events に recording.never-scheduled）に移設され、orphaned_at 列
// 自体も落ちた（00025）。sqlcgen.Reservation が canonical な生成型で、この型は
// テストの可読性のためだけに残っている（CLAUDE.md 不変条件 12「表は行の寿命で
// 割る」: この行に残るのは ruler の 1 パスの出力だけになった）。
type Reservation struct {
	ID                    int64           `db:"id"`
	Site                  string          `db:"site"`
	ProgramID             int64           `db:"program_id"`
	RuleID                *int64          `db:"rule_id"`
	Base                  json.RawMessage `db:"base"`
	CreatedAt             time.Time       `db:"created_at"`
	UpdatedAt             time.Time       `db:"updated_at"`
	DedupMatchRecordingID *int64          `db:"dedup_match_recording_id"`
	DedupSimilarity       *float32        `db:"dedup_similarity"`
}

// ReservationOptions は reservations.base / overrides の jsonb 構造。
// jsonb 内は camelCase（Go/JSON 規約）。
// EncodeProfiles は *[]string: nil=未指定、&[]string{}=エンコードなし override。
//
// FilenameTemplate は rules.filename_template（ruler が base に載せる）または
// ユーザーの明示的な上書き（program_overrides.overrides）由来の Go text/template
// テンプレート文字列。reconciler が予約行のスナップショットだけから
// internal/contentpath で展開する（docs/recording.md §3.2）。ルール作成/更新時に
// internal/contentpath.Validate で構文・実行時エラーを検証し 400 で弾く
// （internal/api/rules.go の validateRuleInput）。ContentPath（フルパスの直接
// 指定）とは別物で、両方指定された場合は ContentPath が勝つ
// （reconciler.createSchedule 参照）。
type ReservationOptions struct {
	Skip             *bool     `json:"skip,omitempty"`
	Priority         *int      `json:"priority,omitempty"`
	ContentPath      *string   `json:"contentPath,omitempty"`
	FilenameTemplate *string   `json:"filenameTemplate,omitempty"`
	EncodeProfiles   *[]string `json:"encodeProfiles,omitempty"`
	KeepOriginal     *string   `json:"keepOriginal,omitempty"`
}

// IsSkipped は実効の skip 判定（`opts.Skip != nil && *opts.Skip`）に名前を付けたもの。
//
// この 1 行は EffectiveOptions の呼び出し元 5 箇所（internal/reconciler,
// internal/capacity, internal/api/reservations_overlaps.go, internal/api/handler.go,
// cmd/rokuban/shadowdiff.go）がそれぞれ書き下していた。式そのものは単純だが、
// 「skip か」という判断が名前を持たないまま散らばっていたことが issue #54 の
// 見逃し（クエリ名が絞り込み済みだと嘘をつき、shadow-diff がこの判定を書き忘れた）
// の土壌になった。
func (o ReservationOptions) IsSkipped() bool {
	return o.Skip != nil && *o.Skip
}

// Effective は base に overrides をマージした結果を返す。
func (o *ReservationOptions) Effective(base *ReservationOptions) ReservationOptions {
	if base == nil {
		if o == nil {
			return ReservationOptions{}
		}
		return *o
	}
	eff := *base
	if o == nil {
		eff.EncodeProfiles = cloneStringSlicePtr(eff.EncodeProfiles)
		return eff
	}
	if o.Skip != nil {
		eff.Skip = o.Skip
	}
	if o.Priority != nil {
		eff.Priority = o.Priority
	}
	if o.ContentPath != nil {
		eff.ContentPath = o.ContentPath
	}
	if o.FilenameTemplate != nil {
		eff.FilenameTemplate = o.FilenameTemplate
	}
	if o.EncodeProfiles != nil {
		eff.EncodeProfiles = o.EncodeProfiles
	}
	eff.EncodeProfiles = cloneStringSlicePtr(eff.EncodeProfiles)
	if o.KeepOriginal != nil {
		eff.KeepOriginal = o.KeepOriginal
	}
	return eff
}

// EffectiveOptions は base（ruler の導出結果）と overrides（program_overrides の
// ユーザー上書き）と intentAction（program_intents.action）から実効オプションを
// 組む。予約行・program_overrides 行の 2 つの jsonb を扱う箇所はすべてここを
// 通し、Unmarshal の失敗を握りつぶさない。
//
// skip の解決は docs/recording.md §4.2 の式に従う:
//
//	effective.skip = (action = 'skip') OR (意図がなく base.skip)
//
// つまり**意図があれば action だけが skip を決める**。action = 'record' なら
// base.skip が true でも false を返す --- M2-6 の重複排除が base に skip を
// 立てても、ユーザーの「録れ」意図が勝つ（同 §4.2「dedup skip（重複排除）」）。
// 意図が無いときだけ base / overrides 由来の skip がそのまま効く。
//
// action は overrides とは別表（program_intents）にあるので base 側の skip を
// 上書きする形になり、jsonb マージに細工を仕込む必要がない。
func EffectiveOptions(base, overrides []byte, intentAction *string) (ReservationOptions, error) {
	var b *ReservationOptions
	if len(base) > 0 {
		var v ReservationOptions
		if err := json.Unmarshal(base, &v); err != nil {
			return ReservationOptions{}, fmt.Errorf("unmarshalling base: %w", err)
		}
		b = &v
	}

	var o *ReservationOptions
	if len(overrides) > 0 {
		var v ReservationOptions
		if err := json.Unmarshal(overrides, &v); err != nil {
			return ReservationOptions{}, fmt.Errorf("unmarshalling overrides: %w", err)
		}
		o = &v
	}

	eff := o.Effective(b)
	// 意図があれば action が skip を決め切る（record なら false で上書きする）。
	// ここを *intentAction == IntentSkip のときだけ true を書く形にすると、
	// action='record' かつ base.skip=true の予約が skip されたままになる。
	if intentAction != nil {
		skip := *intentAction == IntentSkip
		eff.Skip = &skip
	}
	return eff, nil
}

// 番組単位のユーザー意図の action。
const (
	// IntentRecord は「録れ」。手動予約、およびルール由来予約への上書き。
	IntentRecord = "record"
	// IntentSkip は「録るな」。どのルール経由でも一貫して除外される。
	IntentSkip = "skip"
)

func cloneStringSlicePtr(p *[]string) *[]string {
	if p == nil {
		return nil
	}
	c := make([]string, len(*p))
	copy(c, *p)
	return &c
}

// ScheduleSync は mirakc schedule の観測。
//
// reservation_id 列は issue #148 で落ちた --- 書き手（reconciler の
// observeSchedules）はいたが読み手が本番コードに 1 つも無かった（CLAUDE.md
// 不変条件 10「意味を持たない行を作らない」）。
type ScheduleSync struct {
	Site         string          `db:"site"`
	ProgramID    int64           `db:"program_id"`
	State        string          `db:"state"`
	Options      json.RawMessage `db:"options"`
	Tags         []string        `db:"tags"`
	FailedReason json.RawMessage `db:"failed_reason"`
	ObservedAt   time.Time       `db:"observed_at"`
}

// Recording は録画履歴（永続資産）。
//
// reservation_id 列は issue #158 で落ちた --- reservations.id は ruler の
// 導出削除・再実体化で変わる不安定な値で、この列を宛先にした結合は #29 / #53 /
// #98 / #99 / #149 / #152 と 6 回同じ形のバグを生んだ（CLAUDE.md 不変条件 9
// 「identity」）。残っていた読者（表示用コピー）も放送イベントキー経由に
// 置き換えたので列自体を落とした。
type Recording struct {
	ID                int64           `db:"id"`
	RuleID            *int64          `db:"rule_id"`
	Source            string          `db:"source"`
	Site              string          `db:"site"`
	NetworkID         int             `db:"network_id"`
	ServiceID         int             `db:"service_id"`
	EventID           int             `db:"event_id"`
	ServiceName       string          `db:"service_name"`
	ChannelType       string          `db:"channel_type"`
	Channel           string          `db:"channel"`
	Title             string          `db:"title"`
	Description       *string         `db:"description"`
	Extended          json.RawMessage `db:"extended"`
	Genres            json.RawMessage `db:"genres"`
	IsFree            bool            `db:"is_free"`
	ProgramStartAt    time.Time       `db:"program_start_at"`
	ProgramDurationMs int64           `db:"program_duration_ms"`
	Status            string          `db:"status"`
	StartedAt         *time.Time      `db:"started_at"`
	EndedAt           *time.Time      `db:"ended_at"`
	KeepOriginal      string          `db:"keep_original"`
	EncodeProfiles    []string        `db:"encode_profiles"`
	QualityEvents     json.RawMessage `db:"quality_events"`
	DeletedAt         *time.Time      `db:"deleted_at"`
	// 即時物理削除の要求（「猶予を待たず今すぐ消して」）はここには無い。
	// api ロールが書き、api ロールが取り消す状態なので recordings 本体
	// （試行の帰結の観測）ではなく recording_purge_requests 衛星表の
	// 行の存在で表す（不変条件 13。migration のコメント参照）。
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// RecordSync は mirakc record の観測。
type RecordSync struct {
	Site          string          `db:"site"`
	RecordID      string          `db:"record_id"`
	RecordingID   *int64          `db:"recording_id"`
	ProgramID     int64           `db:"program_id"`
	Status        string          `db:"status"`
	ContentPath   *string         `db:"content_path"`
	ContentLength *int64          `db:"content_length"`
	Tags          []string        `db:"tags"`
	FailedReason  json.RawMessage `db:"failed_reason"`
	ObservedAt    time.Time       `db:"observed_at"`
}

// MediaAsset はメディアアセット（永続資産）。
type MediaAsset struct {
	ID          int64      `db:"id"`
	RecordingID int64      `db:"recording_id"`
	Kind        string     `db:"kind"`
	Profile     *string    `db:"profile"`
	RelPath     string     `db:"rel_path"`
	SizeBytes   int64      `db:"size_bytes"`
	State       string     `db:"state"`
	DeletedAt   *time.Time `db:"deleted_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// DropStat は PID 別ドロップ統計。
type DropStat struct {
	MediaAssetID int64 `db:"media_asset_id"`
	PID          int   `db:"pid"`
	Packets      int64 `db:"packets"`
	Drops        int64 `db:"drops"`
	Errors       int64 `db:"errors"`
	Scrambled    int64 `db:"scrambled"`
}

// QualityEvent は recordings.quality_events の配列要素。
type QualityEvent struct {
	At     time.Time       `json:"at"`
	Event  string          `json:"event"`
	Reason json.RawMessage `json:"reason"`
}

// QualityEventNeverScheduled は旧世代 catalog の quality_events.event の値。
// 欠測は issue #318 で never_scheduled_events 表へ移設され、reconciler はもう
// この値を書かない。issue #318 より前に export されたカタログには欠測が
// quality_events マーカー付きの failed 行として残っているので、rescue
// （internal/catalog.hasNeverScheduledMarker）がこの値でそれを検出し、
// recordings に戻さずスキップするために引き続き使う。
const QualityEventNeverScheduled = "recording.never-scheduled"

// DefaultSite は設定が単一 mirakc のときのサイト名。
//
// site は本来設定ファイルで定義するサイト名（[docs/schema.md] §1-5）だが、M1 の設定は
// 単一 `mirakc:` なので全行が同一サイトになる。多拠点対応（`mirakcs:` リスト）の際に
// ここを参照している箇所が設定由来の値に切り替わる。
//
// mirakc を指すすべてのテーブルが site を持つので、既定値の定義は 1 箇所に置く
// （api と watcher が別々に "default" を書いていたのを統合した）。
const DefaultSite = "default"

// ソース（rule/manual）。reservations.source 列は issue #26 で削除されたため、
// 現在このラベルを持つのは recordings.source（録画時点の provenance。
// internal/watcher が program_intents の有無から都度導出して書く）と、
// api が予約行を返すときに program_intents から導出する Reservation.source
// （internal/api/handler.go の reservationFromRow）だけ。
const (
	SourceRule   = "rule"
	SourceManual = "manual"
)

// 予約状態のラベル。Phase 1（#28/#30）以降、`reservations.state` という列は
// 存在しない --- active/detached は (rule_id, base) から読むたびに導出する
// （CLAUDE.md 不変条件 9）。orphaned はさらに issue #98 で orphaned_at 列
// （Phase 1 で state から分離した不可逆な観測）自体が無くなり、「この予約に
// status='failed' の recordings 行が存在するか」の EXISTS 判定に置き換わった
// （internal/db/queries/reservations.sql の GetReservationFull / api.handler.go
// の reservationState）。この 3 つの文字列定数はテスト・ログでの表記を
// 揃えるためだけに残す（internal/api.ReservationState の値と同じ語彙）。
const (
	ReservationStateActive   = "active"
	ReservationStateDetached = "detached"
	ReservationStateOrphaned = "orphaned"
)

// 録画ステータス。mirakc の RecordInfo.Status（GET /api/recording/records の
// recording.status）をそのまま持つ 4 値で、recordings_status_check
// （00002_schema_v1.sql → 00021_recordings_status_canceled.sql で 4 値化）と一致する。
//
// **これは mirakc 固有の語彙だが、不変条件 7「mirakc 固有の概念を永続テーブルに
// 入れない」の違反ではない。** 既存 3 値（recording/finished/failed）は元から
// mirakc の語彙であり、canceled はその踏襲（新規の違反ではない）。不変条件 7 が
// 禁じているのは mirakc の内部 ID・タグ形式・スケジュール状態
// （RecordingScheduleState 等）のような、mirakc の実装詳細に紐づく構造を持ち込む
// ことで、録画結果の語彙（成功/失敗/取消）はドメインの外部仕様として妥当な粒度
// （issue #130 のコメント参照）。
//
// **status の権威は「mirakc が報告したレコードの状態」であって、「Rokuban から
// 見た録画の帰結」ではない。** #98（schedule が一度も作られなかった予約を
// 「録れなかった」行として recordings に残すことの検討）はこの列に
// Rokuban 自身の観測（never-scheduled）を書こうとしているが、それは今の
// status の権威とは別の事実である。#98 を実装する人はここを混ぜないこと
// （別列にするか、この列の意味を作り直すかを #98 側で決める。docs/schema/
// recordings.md の「status の権威」を参照）。
//
// **未知の値（mirakc が将来値を追加した場合）はここに追加しない。**
// internal/watcher.normalizeRecordingStatus が CHECK 違反による永久リトライを
// 避けるため 'failed' に丸め、生の値は record_sync.status（CHECK 無し）に
// そのまま残る。次に mirakc が値を足したときはそちらのログを見て本 const と
// CHECK を更新すること。
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

// オリジナル保持ポリシー。
const (
	KeepOriginalAlways       = "always"
	KeepOriginalUntilEncoded = "until_encoded"
)
