package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

// recordingListFields は録画一覧と詳細取得が共有する射影。
// 一覧・詳細のクエリ結果をここで共通化してマッピングする。
type recordingListFields struct {
	ID                int64
	Site              string
	RuleID            *int64
	Source            string
	ServiceName       string
	ChannelType       string
	Channel           string
	NetworkID         int32
	ServiceID         int32
	EventID           int32
	Title             string
	Description       *string
	ProgramStartAt    time.Time
	ProgramDurationMs int64
	Status            string
	StartedAt         *time.Time
	EndedAt           *time.Time
	QualityEvents     json.RawMessage
	DeletedAt         *time.Time
	CreatedAt         time.Time
	OriginalSizeBytes *int64
	DropPackets       int64
	DropDrops         int64
	DropErrors        int64
	DropScrambled     int64
	// HasMeasuredDropSummary は、TS 計測記録が保存済み原本サイズと一致するか。
	HasMeasuredDropSummary bool
	// KeepOriginal は recording_encode_policy に凍結された原本保持ポリシー。
	// policy 行が無い復旧録画は SQL 側で安全側の既定値 always に落とす。
	KeepOriginal string
	// AvailableEncodedAssets は recordingsAvailableEncodedAssetsSelect
	// （jsonb_agg({profile, sizeBytes})）を Scan した生 JSON。プロファイル名の
	// 配列とサイズの配列を並行に持つ形にしなかった理由は recordings_query.go の
	// コメント参照。nil（trash など SELECT に含めなかった行）と `[]`（active な
	// encoded が無い行）は区別しない --- どちらも recordingFromListFields で
	// EncodedAssets を省略する結果になる。
	AvailableEncodedAssets  json.RawMessage
	ResumePositionMs        *int64
	ResumePositionUpdatedAt *time.Time
	WatchedAt               *time.Time
	// EncodeProfiles は凍結された desired 一覧（recording_encode_policy.encode_profiles）。
	// AvailableEncodedAssets（observed、active のみ）とは異なり、pending な
	// ジョブのプロファイルも含む。事後追加（issue #133）で増える唯一の経路。
	EncodeProfiles []string
	// EncodeAttempts は recording_encode_attempts（衛星表）の行を jsonb_agg
	// した生 JSON（issue #316）。`[]`（試行中/失敗中のプロファイルが無い）と
	// nil を区別しない --- どちらも encodeJobStatusesFromFields で「完了して
	// いないプロファイルはすべて queued」という結果になる。
	EncodeAttempts json.RawMessage
	// ChaptersOwned は recording_chapter_ownership に行があるか（= ユーザーが
	// 確認済み。不変条件 10）。ChapterSpans はユーザー層の jsonb_agg
	// （label は null を保つ）。
	ChaptersOwned bool
	ChapterSpans  json.RawMessage
	// Series は実効シリーズ（recording_series.value）。分類ルールが当たればその
	// 値、当たらなければ自動キー。**導出値であって録画の属性ではない**ので、
	// recording_series ビューから読む（recordings_query.go の SELECT 参照）。
	Series *string
	// SeriesKey はタイトルから導出した自動キー（recordings.series_key）。
	// 分類ルールが当たっても変わらない補助情報で、URL や絞り込みには使わない。
	SeriesKey *string

	// HasOriginalAsset は kind='original' の media_assets 行が **state を問わず**
	// 存在するか（issue #212）。OriginalSizeBytes（state <> 'deleted' の行だけを
	// 見る）とはわざと述語が違う --- この差が「まだ取り込めていない」と
	// 「取り込んだ後に削除した」を分ける（issue #211）。
	HasOriginalAsset bool
	// HasIngestableRecord は **ingest ジョブが投入される（された）はずの**
	// mirakc record の観測がこの録画に紐付いているか。原本も進捗も無いときに
	// 「取り込み待ち」と「そもそも取り込みが来ない」を分ける。
	//
	// 単なる record_sync 行の存在ではなく `status IN ('recording', 'finished')`
	// で絞る（recordings_query.go の SQL コメント参照）。watcher が ingest を
	// 投入する条件と同じものを見ていないと、failed / canceled の録画が永久に
	// pending を名乗る。
	HasIngestableRecord bool
	// HasAbnormallyEndedRecord は mirakc の record が canceled / failed で終わった
	// 観測があるか（`record_sync.status`）。**HasIngestableRecord の否定ではない** ---
	// 未知の status では worker が追従を続けるので、その間の進捗行は生きた観測で
	// ある。この列は「二度と ingest されない record に進捗行が残っている」ときだけ
	// 真になり、その残骸を transferring として読ませないために使う。
	HasAbnormallyEndedRecord bool
	// IngestWrittenBytes / IngestExpectedBytes / IngestObservedAt は
	// recording_ingest_progress の 1 行（無ければすべて nil）。
	IngestWrittenBytes  *int64
	IngestExpectedBytes *int64
	IngestObservedAt    *time.Time
	CMDetect            bool
	CMDetected          bool
	CMRanges            json.RawMessage
	CMAttemptState      *string
	CMAttemptStage      *string
	CMAttemptError      *string
}

// utcTimePtr は timestamptz の scan 結果を UTC の Location に正規化する。
//
// queryRecordings（一覧、pgx.QueryExecModeExec で text protocol）は timestamptz を
// セッションの TimeZone（Postgres の TimeZone GUC。ローカル環境で既定が UTC
// でないことがある）で Location 付きに decode するが、queryRecordingByID
// （単体、既定の prepared/binary protocol）は time.Unix 経由で decode し
// プロセスの time.Local を Location に使う。同じ instant でも Location が
// 違うと `encoding/json` の RFC3339 出力（オフセット部分）が一致しない
// （issue #366）。両経路が必ず通る recordingFromListFields でここに正規化する
// ことで、実行モード・セッション TimeZone・プロセス TZ のいずれにも
// 依存しない wire representation にする。
func utcTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ingestProgressFromFields は原本の取り込み状態を一覧行の素の事実から導出する
// （issue #212）。**列に焼いた値ではなく毎回の導出**（不変条件 9: 毎パス
// 作り直せる値は列にしない）。
//
// 優先順位が意味を持つ:
//
//  1. 原本 media_asset 行があれば committed。state='deleted' でも committed の
//     ままにする --- 「取り込めなかった」と「取り込んだ後に消した」を混同しない
//     ため（issue #211）。原本が**いま**あるかどうかは Recording.sizeBytes の
//     有無が答える。原本行を進捗行より先に見るのは、コミット済みの録画に
//     取り残された進捗行（別経路で原本が登録された場合など）が「取り込み中」を
//     名乗らないようにするため（真実は media_assets 側。不変条件 5）。
//  2. **record が canceled / failed で終わっている**（HasAbnormallyEndedRecord）
//     なら unknown。進捗行より先に見る --- worker は cancel / fail を観測したとき
//     進捗行を消してから終端するが、その DELETE は失敗してもログだけで続行するので、
//     行が残りうる。残骸を transferring と読むと、二度と取り込まれない録画が
//     恒久的に「取り込み中（停滞）」を名乗る
//  3. 進捗行があれば transferring。バイト数と観測時刻を添える。
//  4. **ingest ジョブが来るはずの** record 観測だけがあれば pending
//     （取り込み待ち / 再試行待ち）。録画中も watcher が ingest を投入するので
//     recording の観測もここに入る。進捗行がまだ無い録画開始直後の数秒は
//     pending になる。
//  5. どれでもなければ unknown --- 取り込みが始まった観測が無い。record 自体が
//     観測されていないか、mirakc の record が failed / canceled になった。
//     録画中に投入済みの ingest ジョブがあっても、status を failed / canceled と
//     観測したジョブは進捗行を消してから終端する（internal/worker/ingest.go の
//     Work。TestIngestWorker_CanceledOrFailedRecordCancelsJobWithoutRetry）ので、
//     この分岐へ落ちる。ジョブがまだ観測していない間は 2（transferring）が拾う。
//
// **pending は「これから来る」の断定なので、来る根拠が無いものを入れない。**
// record_sync 行の存在だけを根拠にすると、failed / canceled の録画（ingest が
// 一度も投入されず、record_sync 行は消えない）が永久に「取り込み待ち」を名乗る
// --- API に区別の材料が無いまま UI が未来を断定するという、issue #211 が
// 潰したのと同じ形の誤りになる。
//
// 「リトライ中」を pending と区別する値は返さない（openapi.yaml の
// IngestProgress.state の説明参照）。
func ingestProgressFromFields(r recordingListFields) IngestProgress {
	switch {
	case r.HasOriginalAsset:
		return IngestProgress{State: Committed}
	case r.HasAbnormallyEndedRecord:
		// 取り消し・失敗した record の進捗行は残骸である（上の 2 の説明）。
		// 原本行の判定より後・進捗行より前に置くのは、コミット済みの録画を
		// 取り消した場合に原本の存在を優先させるためである。
		return IngestProgress{State: Unknown}
	case r.IngestWrittenBytes != nil:
		written := *r.IngestWrittenBytes
		return IngestProgress{
			State:         Transferring,
			WrittenBytes:  &written,
			ExpectedBytes: r.IngestExpectedBytes,
			ObservedAt:    utcTimePtr(r.IngestObservedAt),
		}
	case r.HasIngestableRecord:
		return IngestProgress{State: Pending}
	default:
		return IngestProgress{State: Unknown}
	}
}

// profileSets は api が config から注入するプロファイル名の集合。
//
//   - known が nil なら「設定を知らない」（テストの部分構成）ので、
//     設定から消えたプロファイルの判定をスキップする。
//   - cut は `cut: true` のプロファイル名。`awaiting_review` の導出に使う。
//     nil でも空 map でも「cut のプロファイルは無い」で同じ結果になる。
//
// **known と cut を別々の引数で渡さない。** 常に一緒に持ち回る 2 つで、
// 片方だけ渡し忘れると呼び出し側が増えるたびに静かに判定が落ちる。
type profileSets struct {
	known map[string]struct{}
	cut   map[string]struct{}
}

// encodedAssetRow は available_encoded_assets（jsonb_agg）1 要素の JSON 形。
// jsonb_build_object のキー（'profile' / 'sizeBytes'）と一致させる。
type encodedAssetRow struct {
	Profile   string `json:"profile"`
	SizeBytes int64  `json:"sizeBytes"`
	// KeepRanges は media_asset_cuts.keep_ranges（null = cut でない版）。
	KeepRanges json.RawMessage `json:"keepRanges"`
}

// encodeAttemptRow は encode_attempts（jsonb_agg）1 要素の JSON 形。
// jsonb_build_object のキー（'profile' / 'state'）と一致させる。
type encodeAttemptRow struct {
	Profile string `json:"profile"`
	State   string `json:"state"`
}

// encodeJobStatusesFromFields は完了していないエンコードプロファイルの試行
// 状態を一覧行の素の事実から導出する（issue #316）。**列に焼いた値ではなく
// 毎回の導出**（不変条件 9: recording_encode_attempts の生の行から
// state を再構成するだけで、queued かどうかまで含めた最終形は保存しない）。
//
// 対象は `encodeProfiles`（desired）のうち、完了済み（観測された encoded
// アセット。引数 done）にまだ現れていないプロファイルだけ。完了した
// プロファイルはここに出さない --- `encodedAssets` の存在が「完了」を
// 表すので、同じ情報を 2 つの配列で主張しない。
//
//   - `recording_encode_attempts` に行があれば、その `state`（running/failed）
//     をそのまま使う
//   - 行が無ければ `queued`（試行がまだ始まっていない） --- ただし「来る根拠」が
//     無いものは queued と名乗らせない（下記 2 点。docs/recording/ingest.md
//     §5.6 が `pending` に課した規律と同じ）
//
// 「来る根拠」が無いので queued を出さない 2 パターン:
//
//  1. ごみ箱の録画（r.DeletedAt が非 nil）。EncodeReconcileWorker の
//     EnqueueMissingEncodesForKnownProfiles / ListMissingEncodeProfiles は
//     deleted_at IS NULL で絞っており、ごみ箱の録画にジョブは二度と投入されない
//     （internal/worker/encode_reconcile.go 参照）。EncodedAssets/プレイヤーを
//     trash で出さないのと揃え、**running/failed の行が既にあっても
//     （削除前に始まっていた試行）試行状態を丸ごと省略する** --- 削除後に
//     その試行が本当に終わるかは api ロールには分からない（不変条件 1: api は
//     worker に問い合わせない）。実装は先頭の DeletedAt ガード 1 つで、
//     TestListRecordingsEncodeStatus_TrashOmitsEncodeStatus（running な試行行
//     付き）と TestEncodeJobStatusesFromFields の「ごみ箱の録画は試行行が
//     あっても丸ごと省略」が固定している。
//  2. knownProfiles が non-nil（api ロールが config.encode.profiles を注入
//     している）で、そのプロファイルが現在の config に存在しない。設定から
//     消えたプロファイルは EnqueueMissingEncodesForKnownProfiles が投入対象から
//     外している恒久的に満たせない集合（`ListUnsatisfiableEncodeProfiles` が
//     数えているのと同じ集合）なので、試行行が無いものは省略する。ただし
//     running/failed の行が既にあれば設定に残っていなくてもそのまま出す ---
//     過去の観測は「来る」という断定ではないので規律の対象外
//     （TestListRecordingsEncodeStatus_UnknownProfileOmittedWhenConfigured）。
//     knownProfiles が nil（テストの部分構成などで注入が無い）ときはこの判定を
//     スキップする（既存の「nil = 検証オフ」規約と揃える）。
//
// cutProfiles は `cut: true` のプロファイル名の集合（nil なら判定しない）。
// **cut プロファイルで所有の行が無いものは `awaiting_review`** にする ---
// `queued`（ジョブが来る）とは別の主張で、投入側が実際に候補から外している
// （internal/worker/encode.go の enqueueMissingEncodes と
// encode_reconcile.sql の同じ述語）。試行行が既にあればそちらを優先する ---
// 過去の観測（running/failed）は確認の有無に関わらず事実である。
//
// 戻り値は desired の並び順を保つ（TestEncodeJobStatusesFromFields_PreservesDesiredOrder。
// 試行行の map を回して組み立てると順序が非決定になるので、EncodeProfiles を
// 回す実装であることをテストが押さえている）。
func encodeJobStatusesFromFields(r recordingListFields, done []string, profiles profileSets) ([]EncodeJobStatus, error) {
	if len(r.EncodeProfiles) == 0 {
		return nil, nil
	}
	if r.DeletedAt != nil {
		return nil, nil
	}

	doneSet := make(map[string]struct{}, len(done))
	for _, p := range done {
		doneSet[p] = struct{}{}
	}

	attempts := make(map[string]string)
	if len(r.EncodeAttempts) > 0 {
		var rows []encodeAttemptRow
		if err := json.Unmarshal(r.EncodeAttempts, &rows); err != nil {
			return nil, fmt.Errorf("decoding encode_attempts for recording %d: %w", r.ID, err)
		}
		for _, row := range rows {
			attempts[row.Profile] = row.State
		}
	}

	var statuses []EncodeJobStatus
	for _, profile := range r.EncodeProfiles {
		if _, ok := doneSet[profile]; ok {
			continue
		}
		if s, ok := attempts[profile]; ok {
			// 未知の state は queued に倒さず省略する。queued は「これから来る」
			// という断定（上記 2 パターンの規律そのもの）なので、意味の分からない
			// 観測を一番強い主張に写すのが一番危ない。recording_encode_attempts の
			// CHECK 制約が running/failed に絞っているので現状は到達不能。
			switch s {
			case "running":
				statuses = append(statuses, EncodeJobStatus{Profile: profile, State: EncodeJobStatusStateRunning})
			case "failed":
				statuses = append(statuses, EncodeJobStatus{Profile: profile, State: EncodeJobStatusStateFailed})
			default:
				slog.Warn("recordings: unknown encode attempt state, omitting",
					"recording_id", r.ID, "profile", profile, "state", s)
			}
			continue
		}
		if profiles.known != nil {
			if _, known := profiles.known[profile]; !known {
				continue
			}
		}
		if _, isCut := profiles.cut[profile]; isCut && !r.ChaptersOwned {
			statuses = append(statuses, EncodeJobStatus{Profile: profile, State: EncodeJobStatusStateAwaitingReview})
			continue
		}
		statuses = append(statuses, EncodeJobStatus{Profile: profile, State: EncodeJobStatusStateQueued})
	}
	return statuses, nil
}

// currentKeepRanges は一覧行の素の事実から、現在のタイムラインの keep 区間を
// 導出する。**chapters.Derive 1 か所を通る**（worker のカット版 encode・
// GET chapters と同じ関数）。
//
// 所有済みの行だけが対象（呼び出し側が ChaptersOwned を見る）。所有の行が無ければ
// 有効なタイムラインは自動層だが、cut 版は所有を前提にしか作られないので、
// そのときの比較相手は存在しない。
func currentKeepRanges(r recordingListFields) ([]chapters.Range, error) {
	var spans []chapters.Span
	if err := json.Unmarshal(r.ChapterSpans, &spans); err != nil {
		return nil, err
	}
	timeline := chapters.Derive(true, spans, nil, r.ProgramDurationMs)
	return chapters.KeepRanges(timeline), nil
}

// recordingFromListFields は一覧行を API の Recording に写す。
// includeDeletedAt が true のときだけ deletedAt を載せる（ごみ箱一覧向け）。
// knownProfiles は encodeJobStatusesFromFields に渡す（doc コメント参照。
// nil なら「設定から消えたプロファイル」の判定をスキップする）。
func recordingFromListFields(r recordingListFields, includeDeletedAt bool, profiles profileSets) (Recording, error) {
	rec := Recording{
		Id:           r.ID,
		Site:         r.Site,
		RuleId:       r.RuleID,
		Source:       RecordingSource(r.Source),
		ServiceName:  r.ServiceName,
		ChannelType:  RecordingChannelType(r.ChannelType),
		Channel:      r.Channel,
		NetworkId:    int(r.NetworkID),
		ServiceId:    int(r.ServiceID),
		EventId:      int(r.EventID),
		Title:        r.Title,
		Description:  r.Description,
		StartAt:      r.ProgramStartAt.UTC(),
		DurationMs:   r.ProgramDurationMs,
		Status:       RecordingStatus(r.Status),
		KeepOriginal: RecordingKeepOriginal(r.KeepOriginal),
		StartedAt:    utcTimePtr(r.StartedAt),
		EndedAt:      utcTimePtr(r.EndedAt),
		SizeBytes:    r.OriginalSizeBytes,
		Series:       r.Series,
		SeriesKey:    r.SeriesKey,
		CreatedAt:    r.CreatedAt.UTC(),
	}
	cmDetection, err := cmDetectionFromListFields(r)
	if err != nil {
		return Recording{}, err
	}
	rec.CmDetection = cmDetection
	if includeDeletedAt {
		rec.DeletedAt = utcTimePtr(r.DeletedAt)
	}
	// COALESCE で統計の無い行は 0 に見えるため、HasMeasuredDropSummary を別の
	// 事実として判定する。これは原本 TS の計測記録が保存済みサイズと一致する
	// 場合だけ真になる。状態を問わず原本行から判定するので、計測後の tombstone
	// では従来どおり要約を保ち、サイズ変更後の古い計測結果は表示しない。
	if r.HasMeasuredDropSummary {
		rec.DropSummary = &DropSummary{
			Packets:   r.DropPackets,
			Drops:     r.DropDrops,
			Errors:    r.DropErrors,
			Scrambled: r.DropScrambled,
		}
	}
	// 原本の取り込み状態（issue #212）。**常に載せる**（省略しない）---
	// 省略を「取り込み済み」とも「不明」とも読める曖昧な状態にしないため。
	// 導出の根拠は ingestProgressFromFields の doc コメント参照。
	ingest := ingestProgressFromFields(r)
	rec.Ingest = &ingest
	// ごみ箱の録画（r.DeletedAt が非 nil）では available_encoded_assets を
	// 出さない --- ごみ箱ではプレイヤーを出さないので値を揃えても使われない
	// （3d56f92 の理由。性能実測は無い）。一覧（queryRecordings）・単体
	// （queryRecordingByID）のどちらも常に SQL でこの列を連結し、この関数を
	// 経由するので、判定はここ 1 か所だけで足りる。
	if r.DeletedAt != nil {
		r.AvailableEncodedAssets = nil
	}
	// 再生可能な encoded 派生物（observed）。空 `[]`/nil なら省略（omitempty）。
	var encodedProfileNames []string
	if len(r.AvailableEncodedAssets) > 0 {
		var rows []encodedAssetRow
		if err := json.Unmarshal(r.AvailableEncodedAssets, &rows); err != nil {
			return Recording{}, fmt.Errorf("decoding available_encoded_assets for recording %d: %w", r.ID, err)
		}
		if len(rows) > 0 {
			// 「編集前の内容です」の判定に使う現在の keep 区間。チャプターが
			// 1 つも無い録画（大半）では導出しない --- 所有の行が無ければ
			// cut 版は存在しえない（投入側が所有を要求する）。
			var currentKeep []chapters.Range
			cutsPresent := false
			for _, row := range rows {
				if len(row.KeepRanges) > 0 && string(row.KeepRanges) != "null" {
					cutsPresent = true
					break
				}
			}
			if cutsPresent && r.ChaptersOwned {
				var err error
				currentKeep, err = currentKeepRanges(r)
				if err != nil {
					return Recording{}, fmt.Errorf("deriving current chapter timeline for recording %d: %w", r.ID, err)
				}
			}
			assets := make([]EncodedAsset, len(rows))
			encodedProfileNames = make([]string, len(rows))
			for i, row := range rows {
				assets[i] = EncodedAsset{Profile: row.Profile, SizeBytes: &row.SizeBytes}
				encodedProfileNames[i] = row.Profile
				if len(row.KeepRanges) == 0 || string(row.KeepRanges) == "null" {
					continue
				}
				// 凍結した区間がある = cut 版。asset の再生位置変換に使う
				// keepRanges と、現在のタイムラインと一致するかの cutStale を返す。
				isCut := true
				assets[i].Cut = &isCut
				var frozenRanges []chapters.Range
				if err := json.Unmarshal(row.KeepRanges, &frozenRanges); err != nil {
					return Recording{}, fmt.Errorf("decoding frozen cut ranges for recording %d: %w", r.ID, err)
				}
				keepRanges := make([]KeepRange, len(frozenRanges))
				for j, keepRange := range frozenRanges {
					keepRanges[j] = KeepRange{StartMs: keepRange.StartMs, EndMs: keepRange.EndMs}
				}
				assets[i].KeepRanges = &keepRanges
				stale := r.ChaptersOwned && len(currentKeep) > 0 && !chapters.SameRanges(frozenRanges, currentKeep)
				assets[i].CutStale = &stale
			}
			rec.EncodedAssets = &assets
		}
	}
	// 凍結された desired 一覧。空なら省略（omitempty）。UI が「追加済み」を
	// 判定するのに使う（issue #133）。
	if len(r.EncodeProfiles) > 0 {
		profiles := slices.Clone(r.EncodeProfiles)
		rec.EncodeProfiles = &profiles
	}
	// 完了していないエンコードプロファイルの試行状態（issue #316）。空なら
	// 省略（プロファイル未設定・全プロファイル完了済みのどちらでも省略）。
	statuses, err := encodeJobStatusesFromFields(r, encodedProfileNames, profiles)
	if err != nil {
		return Recording{}, err
	}
	if len(statuses) > 0 {
		rec.EncodeStatus = &statuses
	}
	if len(r.QualityEvents) > 0 {
		var events []map[string]any
		if err := json.Unmarshal(r.QualityEvents, &events); err != nil {
			return Recording{}, fmt.Errorf("decoding quality_events for recording %d: %w", r.ID, err)
		}
		if len(events) > 0 {
			rec.QualityEvents = &events
		}
	}
	if r.ResumePositionMs != nil {
		position := *r.ResumePositionMs
		rec.ResumePositionMs = &position
	}
	rec.WatchedAt = utcTimePtr(r.WatchedAt)
	return rec, nil
}

func cmDetectionFromListFields(r recordingListFields) (CMDetection, error) {
	state := CMDetectionStateDisabled
	if r.CMDetect {
		state = CMDetectionStateDetecting
		if r.CMAttemptState != nil && *r.CMAttemptState == "failed" {
			state = CMDetectionStateFailed
		}
		if r.CMDetected {
			state = CMDetectionStateDetected
		}
	}
	detection := CMDetection{State: state}
	if r.CMAttemptStage != nil {
		stage := CMDetectionStage(*r.CMAttemptStage)
		detection.Stage = &stage
	}
	if r.CMAttemptError != nil {
		errorMessage := *r.CMAttemptError
		detection.Error = &errorMessage
	}
	if r.CMDetected {
		var ranges []CMRange
		if err := json.Unmarshal(r.CMRanges, &ranges); err != nil {
			return CMDetection{}, fmt.Errorf("decoding CM ranges for recording %d: %w", r.ID, err)
		}
		detection.Ranges = &ranges
	}
	return detection, nil
}

// ListRecordings は録画履歴を絞り込み + キーセットページングで返す（既定は
// program_start_at 降順。issue #136）。trash=true のときごみ箱
// （deleted_at IS NOT NULL）を返す。trash と各絞り込み条件は直交する。
//
// 動的 WHERE ビルダ（buildRecordingsQuery / queryRecordings、recordings_query.go）
// を使う。sqlc の静的クエリにしない理由はそちらのコメント参照（trgm 式 GIN が
// 汎用プランで使われなくなることを避けるため）。
//
// api は site に束縛されない（不変条件 1）ため、全サイトの録画を返す
// （issue #184 M4-12）。各要素の Site で区別する。
func (h *Server) ListRecordings(ctx context.Context, req ListRecordingsRequestObject) (ListRecordingsResponseObject, error) {
	f, errMsg := recordingsFilterFromParams(req.Params)
	if errMsg != "" {
		return ListRecordings400JSONResponse{Error: errMsg}, nil
	}

	if f.EncodeState != "" {
		snapshot, err := h.loadEncodeQueue(ctx)
		if err != nil {
			return nil, fmt.Errorf("loading encode queue: %w", err)
		}
		ids := snapshot.runningRecordingIDs
		if f.EncodeState == ListRecordingsParamsEncodeStateQueued {
			ids = snapshot.queuedRecordingIDs
		}
		f.EncodeRecordingIDs = &ids
	}

	result, err := queryRecordings(ctx, h.pool, f, h.profileSets())
	if err != nil {
		return nil, fmt.Errorf("listing recordings: %w", err)
	}
	return ListRecordings200JSONResponse(result), nil
}

// GetRecording は録画を単体で取得する（issue #232 M6-4。一覧要素と同形）。
//
// ごみ箱の録画（deleted_at IS NOT NULL）も 200 で返す --- 一覧の trash=true が
// 既にメタデータを 200 で返しているため単体 GET だけ厳しくする理由が無い
// （メディア配信の 404 契約とは別の判断。openapi.yaml の getRecording
// description 参照）。purged_at が立った tombstone（issue #135）は 404
// （queryRecordingByID 参照）。
func (h *Server) GetRecording(ctx context.Context, req GetRecordingRequestObject) (GetRecordingResponseObject, error) {
	rec, ok, err := queryRecordingByID(ctx, h.pool, req.Id, h.profileSets())
	if err != nil {
		return nil, fmt.Errorf("getting recording %d: %w", req.Id, err)
	}
	if !ok {
		return GetRecording404JSONResponse{Error: "recording not found"}, nil
	}
	return GetRecording200JSONResponse(rec), nil
}

// ListRecordingUpcoming は番組ハブの「次回」を返す（起点の録画と同じ実効
// シリーズで、まだ始まっていない EPG の番組）。
//
// 形は `POST /api/programs/search` の結果と同じ（ProgramSearchMatch）で、site を
// 運び畳まない。**予約状態は結合しない** --- 予約は頻繁に変わり番組はほとんど
// 変わらないので、UI が `GET /api/reservations` を別に引いて突き合わせる
// （docs/api/rest.md「予約状態は番組と結合しない」）。
//
// 起点の録画が無い・実効シリーズが NULL なら空配列を 200 で返す。EPG は射影で、
// 番組が 1 件も無いこともあるので、区別する材料が無いものを 404 にしない
// （`GET /api/recordings` の `seriesOf` と同じ扱い。openapi.yaml の
// listRecordingUpcoming description）。
//
// 実効シリーズの比較は SQL 側のビュー 2 つ（recording_series /
// epg_program_series）が持つ。Go 側に正規化を複製しない
// （internal/db/queries/epg_series.sql）。
func (h *Server) ListRecordingUpcoming(ctx context.Context, req ListRecordingUpcomingRequestObject) (ListRecordingUpcomingResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListUpcomingProgramsBySeries(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("listing upcoming programs for recording %d: %w", req.Id, err)
	}
	matches := make([]ProgramSearchMatch, len(rows))
	for i, row := range rows {
		matches[i] = ProgramSearchMatch{
			Site:       row.Site,
			ProgramId:  row.ProgramID,
			NetworkId:  int(row.NetworkID),
			ServiceId:  int(row.ServiceID),
			StartAt:    row.StartAt,
			DurationMs: row.DurationMs,
			Name:       row.Name,
			IsFree:     row.IsFree,
		}
	}
	return ListRecordingUpcoming200JSONResponse(matches), nil
}

// DeleteRecording は録画を論理削除する（ごみ箱へ）。
// deleted_at を立てるだけでファイルには触れない。既に削除済みでも冪等に 204。
func (h *Server) DeleteRecording(ctx context.Context, req DeleteRecordingRequestObject) (DeleteRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).SoftDeleteRecording(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteRecording404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("soft-deleting recording %d: %w", req.Id, err)
	}
	return DeleteRecording204Response{}, nil
}

// RestoreRecording はごみ箱から録画を復元する。
// deleted_at を消し、即時 purge 要求の行を消すだけ（ファイル操作ゼロ）。
// 同一イベントに生きている録画があると 409。
//
// 2 表の更新を 1 文のデータ変更 CTE ではなく**トランザクション内の 2 文**で流す。
// CTE ではアーム全体が 1 つのスナップショットを共有するため、UPDATE アームが
// 行ロックで待たされている間に commit された即時要求の行が DELETE アームから
// 見えず、「復元は 204 なのに要求行だけ残る」が観測された
// （TestRestoreRecording_ConcurrentPurgeRequest_Withdrawn）。
//
// ただし窓を閉じているのは 2 文に割ったことではない。DELETE が 0 行だったとき
// ロックは何も残らないので（READ COMMITTED に述語ロックは無い）、実際に閉じて
// いるのは**要求行を入れる経路が先に対象の recordings 行をロックすること** ——
// MarkRecordingPurgeRequested の CTE の UPDATE アームがそれを兼ねている
// （TestPurgeRecording_SerializedBehindRestoreRowLock）。ロックしない INSERT
// 経路を足すと猶予バイパスが再発する。詳細は
// internal/db/queries/recordings_trash.sql のコメント。
func (h *Server) RestoreRecording(ctx context.Context, req RestoreRecordingRequestObject) (RestoreRecordingResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction to restore recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.RestoreRecording(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RestoreRecording404JSONResponse{Error: "recording not in trash"}, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return RestoreRecording409JSONResponse{
				Error: "active recording already exists for the same event",
			}, nil
		}
		return nil, fmt.Errorf("restoring recording %d: %w", req.Id, err)
	}
	// ここまで来たのは UPDATE が 1 行返したとき（= 実際にごみ箱から出したとき）
	// だけ。0 行なら上で 404 して return しているので、要求だけ黙って取り消す
	// ことはない。
	if err := q.WithdrawRecordingPurgeRequest(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("withdrawing purge request for recording %d: %w", req.Id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing restore of recording %d: %w", req.Id, err)
	}
	return RestoreRecording204Response{}, nil
}

// PurgeRecording は即時物理削除の要求を記録する。
// recording_purge_requests に行を入れ、未 soft-delete なら deleted_at も立てる。
// ファイルは消さない（M3-8 の削除 reconcile が拾う）。
func (h *Server) PurgeRecording(ctx context.Context, req PurgeRecordingRequestObject) (PurgeRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).MarkRecordingPurgeRequested(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PurgeRecording404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("marking recording %d for purge: %w", req.Id, err)
	}
	return PurgeRecording204Response{}, nil
}

// AddRecordingEncodeProfiles は凍結済み encode_profiles への「事後追加」（issue
// #133、凍結の例外。docs/storage.md §6「原本 TS の保持ポリシー」・
// docs/recording/reservation-model.md §4.5「録画開始後の編集」）。
//
// AppendRecordingEncodeProfiles で追加専用（union + dedup）に書き、全置換は
// しない --- 誤って他プロファイルの指定を消す事故を避けるため（issue #133
// 「決めること 3」）。recording_encode_policy（issue #159）に行が無い（未凍結）
// 録画（internal/inplace.Register 経由で作られた原本など、ingest の
// resolveAndSnapshotEncodePolicy を通らなかったもの）でも、原本が active なら
// このクエリが既定値 'always' で行を新規に作る --- 「原本が active なのに
// 事後追加ができない」という issue #133 が解いた問題の再発を避けるため。
//
// 原本が active でない（GetActiveOriginalMediaAsset が ErrNoRows --- 削除済み・
// state='deleting'（unlink 待ち）・そもそも ingest が未完了で original 行が
// 無い、のいずれか）なら 409 を返す。EnqueueMissingEncodes は単体だとこの
// ケースで黙って return するため（サイレント no-op）、ここで明示的に検査する。
//
// encode_profiles の更新と encode_enqueue_hint ジョブの投入は同一トランザクション
// で行う（insertEncodeEnqueueHint。rules.go の insertRulerPassHint と同じ
// パターン）。実際の encode ジョブ投入（EnqueueMissingEncodes）は
// EncodeEnqueueHintWorker が worker ロール側で行う（internal/worker/encode.go の
// EncodeEnqueueHintArgs の doc コメント参照 --- api → worker の結合パターンを
// ヒントジョブ経由に揃える判断の理由）。
func (h *Server) AddRecordingEncodeProfiles(ctx context.Context, req AddRecordingEncodeProfilesRequestObject) (AddRecordingEncodeProfilesResponseObject, error) {
	if req.Body == nil || len(req.Body.Profiles) == 0 {
		return AddRecordingEncodeProfiles400JSONResponse{Error: "profiles must not be empty"}, nil
	}
	// 名前は追加分だけ、cut の選択規則はマージ後（下、tx 内）に当てる。
	if err := h.validateEncodeProfileNames(req.Body.Profiles); err != nil {
		return AddRecordingEncodeProfiles400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.GetRecordingByID(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AddRecordingEncodeProfiles404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading recording %d: %w", req.Id, err)
	}

	// 原本が active でないなら 409（罠: EnqueueMissingEncodes 単体はここで黙って
	// no-op になるため、サイレントな失敗にしないよう api 層で先に検査する）。
	if _, err := q.GetActiveOriginalMediaAsset(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ここに落ちる原因は「削除済み」に限らない --- state = 'deleting'
			// （unlink 待ち。一覧の射影は state <> 'deleted' なので UI 側は
			// 「原本あり」と見てボタンを出しうる。issue #105 の経路で active に
			// 戻ることもある）、あるいはそもそも ingest が完了しておらず
			// original 行自体が無い、のいずれもここに来る。3 パターンを区別する
			// 追加クエリのコストに見合わないため区別はしないが、文言は
			// 「削除済みとは限らない」ことが伝わる形にする。
			return AddRecordingEncodeProfiles409JSONResponse{
				Error: "original media asset not active (deleted, deleting, or not yet ingested); cannot add encode profiles",
			}, nil
		}
		return nil, fmt.Errorf("loading original media asset for recording %d: %w", req.Id, err)
	}

	// 外す tx（RemoveRecordingEncodedAsset）が要求行を書いて commit する前に
	// Append の文が走ると、CTE の DELETE は未 commit の要求行を見られず、足し直した
	// profile の要求行が残る。先にロックを取れば、次の文は外す側の commit 後の
	// スナップショットで走る。行が無ければ外す側も外せないので競合しない。
	// cut の判定もロック後の desired で行うため、判定より前に取る。
	if _, err := q.LockRecordingEncodePolicy(ctx, req.Id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("locking encode policy for recording %d: %w", req.Id, err)
	}

	// cut の規則は「既存 ∪ 追加分」に当てる（[h264] に cut だけを足すのは
	// 結果が [h264, cut] なので正当）。原本 HLS が使える live.enabled 構成では
	// cut のみも正当。policy 行が無ければ追加分のみ。
	if h.cutProfiles != nil {
		merged := req.Body.Profiles
		if policy, err := q.GetRecordingEncodePolicy(ctx, req.Id); err == nil {
			merged = append(slices.Clone(policy.EncodeProfiles), merged...)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("loading encode policy for recording %d: %w", req.Id, err)
		}
		if err := h.validateCutSelection(merged); err != nil {
			return AddRecordingEncodeProfiles400JSONResponse{Error: err.Error()}, nil
		}
	}

	// recording_encode_policy（issue #159）に行が無い（未凍結）録画への事後
	// 追加は、AppendRecordingEncodeProfiles 自体が「原本が active = 凍結済みと
	// みなす」既定値 'always' で行を作る（internal/inplace.Register 経由の原本は
	// resolveAndSnapshotEncodePolicy を通らないため行が無いことがある）。
	if err := q.AppendRecordingEncodeProfiles(ctx, sqlcgen.AppendRecordingEncodeProfilesParams{
		ID:       req.Id,
		Profiles: req.Body.Profiles,
	}); err != nil {
		return nil, fmt.Errorf("appending encode profiles for recording %d: %w", req.Id, err)
	}
	if err := h.insertEncodeEnqueueHint(ctx, tx, req.Id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return AddRecordingEncodeProfiles204Response{}, nil
}

// RemoveRecordingEncodedAsset はエンコード版を 1 本だけ外す（凍結の 3 つ目の例外。
// docs/storage/retention.md §6）。encode_profiles から profile を外し、外した要求の
// 行を入れる。ファイルは消さない。削除 reconcile が名前付き述語
// removed_encoded_assets で拾って unlink する。
//
// 同じ録画の版を同時に外す 2 本の tx が 0 コピーを作らないよう、policy 行を
// FOR UPDATE で取ってから書き、0 コピー検査は書いた後の状態で行う（不変条件 9
// 「適用の瞬間」）。検査は削除 reconcile と同じ view に任せる。
func (h *Server) RemoveRecordingEncodedAsset(ctx context.Context, req RemoveRecordingEncodedAssetRequestObject) (RemoveRecordingEncodedAssetResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction to remove recording %d encoded %q: %w", req.Id, req.Profile, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	rec, err := q.GetRecordingByID(ctx, req.Id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && rec.PurgedAt != nil) {
		return RemoveRecordingEncodedAsset404JSONResponse{Error: "recording not found"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading recording %d: %w", req.Id, err)
	}
	profile := req.Profile
	assetID, err := q.GetActiveEncodedMediaAssetID(ctx, sqlcgen.GetActiveEncodedMediaAssetIDParams{
		RecordingID: req.Id,
		Profile:     &profile,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RemoveRecordingEncodedAsset404JSONResponse{Error: "encoded version not found"}, nil
		}
		return nil, fmt.Errorf("loading encoded asset %q for recording %d: %w", req.Profile, req.Id, err)
	}
	// active な encoded がある = 凍結済みとみなす。catalog 無しの rescue
	// （internal/inplace.Register）は encoded の media_assets を作るが policy 行を
	// 作らないので、行が無いことがある。直列化の点として先に作る。409 では tx ごと
	// ロールバックするので、意味を持たない行は残らない。
	if err := q.FreezeRecordingEncodePolicyIfMissing(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("freezing encode policy for recording %d: %w", req.Id, err)
	}
	policy, err := q.LockRecordingEncodePolicy(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("locking encode policy for recording %d: %w", req.Id, err)
	}
	// 外した後の desired にも事後追加と同じ cut の選択規則を当てる。書く前に判定する
	// ので、違反ならロールバックするだけで何も残らない。
	if h.cutProfiles != nil {
		remaining := slices.DeleteFunc(slices.Clone(policy.EncodeProfiles), func(p string) bool { return p == req.Profile })
		if err := h.validateCutSelection(remaining); err != nil {
			return RemoveRecordingEncodedAsset400JSONResponse{Error: err.Error()}, nil
		}
	}
	if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{
		RecordingID: req.Id,
		Profile:     req.Profile,
	}); err != nil {
		return nil, fmt.Errorf("removing encode profile %q from recording %d: %w", req.Profile, req.Id, err)
	}
	if err := q.InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{
		RecordingID: req.Id,
		Profile:     req.Profile,
	}); err != nil {
		return nil, fmt.Errorf("recording removal request for recording %d encoded %q: %w", req.Id, req.Profile, err)
	}
	removable, err := q.IsRemovedEncodedAsset(ctx, assetID)
	if err != nil {
		return nil, fmt.Errorf("checking remaining copies of recording %d: %w", req.Id, err)
	}
	if !removable {
		return RemoveRecordingEncodedAsset409JSONResponse{
			Error: "this is the last viewable copy (no active original and no other encoded version); move the recording to trash instead",
		}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing removal of recording %d encoded %q: %w", req.Id, req.Profile, err)
	}
	return RemoveRecordingEncodedAsset204Response{}, nil
}

// ReencodeRecordingProfile は cut 版を作り直す（`encodedAssets[].cutStale` が真の
// ときのユーザーの明示的な操作）。
//
// **自動では作り直さない。** チャプターを直した瞬間に再エンコードすると、
// ユーザーが確認していない区間が黙って本編から消える。api は「今のタイムラインは
// 凍結した区間と違う」ことを導出して見せるだけで、作り直しはユーザーの操作で行う。
//
// 投入は `encode_enqueue_hint` ジョブ経由（AddRecordingEncodeProfiles と同じ
// パターン。api → worker の結合をヒントジョブに揃える）。通常の投入経路は
// 「active な encoded がある」ことを理由に候補から外すので、ここは
// EncodeWorker 自身の冪等判定（凍結した区間が現在の keep と一致するか）が
// 効いて作り直しになる。
//
// 原本が active でないなら 409（カット版は原本から作り直すしかない）。
func (h *Server) ReencodeRecordingProfile(ctx context.Context, req ReencodeRecordingProfileRequestObject) (ReencodeRecordingProfileResponseObject, error) {
	// cut 専用の作り直し。cut の選択規則（cut だけの選択の拒否）は選択の集合に
	// 対する規則でここには当てはまらないので、名前と「cut か」だけを見る。
	if err := h.validateEncodeProfileNames([]string{req.Profile}); err != nil {
		return ReencodeRecordingProfile400JSONResponse{Error: err.Error()}, nil
	}
	if h.cutProfiles != nil {
		if _, ok := h.cutProfiles[req.Profile]; !ok {
			return ReencodeRecordingProfile400JSONResponse{Error: fmt.Sprintf("profile %q is not a cut profile", req.Profile)}, nil
		}
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.GetRecordingByID(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReencodeRecordingProfile404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading recording %d: %w", req.Id, err)
	}
	if _, err := q.GetActiveOriginalMediaAsset(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReencodeRecordingProfile409JSONResponse{
				Error: "original media asset not active (deleted, deleting, or not yet ingested); cannot rebuild the cut version",
			}, nil
		}
		return nil, fmt.Errorf("loading original media asset for recording %d: %w", req.Id, err)
	}
	// 投入側（worker）は未確認・keep 空の cut を投入しない。ここで 204 を返すと
	// ボタンが黙って効かず cutStale も残るので、同じ導出（currentKeepRanges =
	// cutStale と同じ chapters.Derive）で先に 409 にする。
	state, err := q.GetRecordingChapterState(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("loading chapter state for recording %d: %w", req.Id, err)
	}
	if !state.Owned {
		return ReencodeRecordingProfile409JSONResponse{Error: "chapters not yet confirmed; nothing to encode"}, nil
	}
	spans, err := q.GetRecordingChapterSpansJSON(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("loading chapter spans for recording %d: %w", req.Id, err)
	}
	keep, err := currentKeepRanges(recordingListFields{ChapterSpans: spans, ProgramDurationMs: state.ProgramDurationMs})
	if err != nil {
		return nil, fmt.Errorf("deriving current chapter timeline for recording %d: %w", req.Id, err)
	}
	if len(keep) == 0 {
		return ReencodeRecordingProfile409JSONResponse{Error: "timeline has no keep ranges; nothing to encode"}, nil
	}
	// 冪等判定を「古い」と読ませるための 1 手: 何もしないヒントジョブを積む。
	// EncodeWorker は active な encoded の凍結区間が現在の keep と一致すれば
	// スキップし、違えば作り直す。api は判定を持たない（不変条件 5: 真実は
	// 定期 reconcile が再取得する）。
	if err := h.insertEncodeEnqueueHint(ctx, tx, req.Id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ReencodeRecordingProfile204Response{}, nil
}

// wantKeepOriginalUntilEncodedMessage は encode_profiles が空/未凍結のときの
// 409 メッセージ。SetRecordingKeepOriginal の UPDATE 自身の WHERE がこの条件を
// 判定するため、ここでは rows==0 をこのメッセージに翻訳するだけ（下記 doc
// コメント参照）。
const wantKeepOriginalUntilEncodedMessage = "cannot set keepOriginal=until_encoded without desired encode profiles; add encode profiles first"

// SetRecordingEncodePolicy は録画後の原本保持・CM 検出方針を上書きする。
//
// 各フィールドは独立した任意指定。keepOriginal の変更は凍結済み
// recording_encode_policy のみを更新し、未凍結の録画では always が既定のため
// no-op、until_encoded は desired な encode_profiles が無ければ 409 になる。
// cmDetect=true は active な原本を適用時に再確認し、未凍結なら既定の保持方針と
// 空の encode_profiles で policy を作る。false は未凍結 policy を作らない。
// CM 検出ジョブはここでは投入せず、worker の定期 reconcile に任せる。
//
// 物理削除もヒントジョブの投入も行わない。原本保持の変更は削除 reconcile が、
// CM 検出の変更は CM 定期 reconcile が DB の現在値から検出する。
func (h *Server) SetRecordingEncodePolicy(ctx context.Context, req SetRecordingEncodePolicyRequestObject) (SetRecordingEncodePolicyResponseObject, error) {
	if req.Body == nil {
		return SetRecordingEncodePolicy400JSONResponse{Error: "at least one policy field is required"}, nil
	}
	if req.Body.KeepOriginal == nil && req.Body.CmDetect == nil {
		return SetRecordingEncodePolicy400JSONResponse{Error: "at least one of keepOriginal or cmDetect is required"}, nil
	}
	if req.Body.KeepOriginal != nil && !req.Body.KeepOriginal.Valid() {
		return SetRecordingEncodePolicy400JSONResponse{
			Error: fmt.Sprintf("invalid keepOriginal %q (want always or until_encoded)", *req.Body.KeepOriginal),
		}, nil
	}

	// 無効なデプロイでは誰もジョブを積まないので、until_encoded の原本が
	// 検出結果を待って永久に残る。受け付けない。
	if req.Body.CmDetect != nil && *req.Body.CmDetect && !h.capabilities.CmDetect {
		return SetRecordingEncodePolicy409JSONResponse{Error: cmDetectDisabledMessage}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction to set recording %d encode policy: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	rec, err := q.GetRecordingByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SetRecordingEncodePolicy404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading recording %d: %w", req.Id, err)
	}
	// GetRecordingByID は述語なし（ingest worker と共有するクエリなので緩めない）。
	// purged_at が立った tombstone は GET /api/recordings/{id}（queryRecordingByID、
	// purged_at IS NULL）と同じく 404 にする --- deleted_at（ごみ箱）は復元すれば
	// 効くので inert ではなく、ここでは見ない。
	if rec.PurgedAt != nil {
		return SetRecordingEncodePolicy404JSONResponse{Error: "recording not found"}, nil
	}

	if req.Body.KeepOriginal != nil {
		keepOriginal := string(*req.Body.KeepOriginal)
		rows, err := q.SetRecordingKeepOriginal(ctx, sqlcgen.SetRecordingKeepOriginalParams{
			RecordingID:  req.Id,
			KeepOriginal: keepOriginal,
		})
		if err != nil {
			return nil, fmt.Errorf("setting recording %d keep_original: %w", req.Id, err)
		}
		if rows == 0 && *req.Body.KeepOriginal == SetRecordingEncodePolicyInputKeepOriginalUntilEncoded {
			return SetRecordingEncodePolicy409JSONResponse{Error: wantKeepOriginalUntilEncodedMessage}, nil
		}
	}
	if req.Body.CmDetect != nil {
		if *req.Body.CmDetect {
			rows, err := q.SetRecordingCMDetection(ctx, req.Id)
			if err != nil {
				return nil, fmt.Errorf("enabling CM detection for recording %d: %w", req.Id, err)
			}
			if rows == 0 {
				return SetRecordingEncodePolicy409JSONResponse{Error: "active original media asset required to enable CM detection"}, nil
			}
		} else {
			if err := q.ClearRecordingCMDetection(ctx, req.Id); err != nil {
				return nil, fmt.Errorf("disabling CM detection for recording %d: %w", req.Id, err)
			}
			if err := q.DeleteCMDetectionAttempt(ctx, req.Id); err != nil {
				return nil, fmt.Errorf("clearing CM detection attempt for recording %d: %w", req.Id, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing recording %d encode policy: %w", req.Id, err)
	}
	return SetRecordingEncodePolicy204Response{}, nil
}

// insertEncodeEnqueueHint は AddRecordingEncodeProfiles と同一トランザクションで
// EncodeEnqueueHintArgs を InsertTx する（ヒント経路。rules.go の
// insertRulerPassHint と同じパターン）。dual-write を避けるため、
// encode_profiles の更新が失敗すればこのジョブも一緒にロールバックされる。
//
// h.river が nil の場合は何もしない（insertRulerPassHint と同じ理由。テストや、
// 将来 River を持たない api 構成を許容するため）。
func (h *Server) insertEncodeEnqueueHint(ctx context.Context, tx pgx.Tx, recordingID int64) error {
	if h.river == nil {
		return nil
	}
	if _, err := h.river.InsertTx(ctx, tx, jobs.EncodeEnqueueHintArgs{RecordingID: recordingID}, nil); err != nil {
		return fmt.Errorf("inserting encode_enqueue_hint: %w", err)
	}
	return nil
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
func (h *Server) ListRecordingDropStats(ctx context.Context, req ListRecordingDropStatsRequestObject) (ListRecordingDropStatsResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListRecordingDropStats(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	positionRows, err := q.ListRecordingDropPositions(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	positionsByPID := make(map[int32][]DropPosition, len(positionRows))
	for _, p := range positionRows {
		positionsByPID[p.Pid] = append(positionsByPID[p.Pid], DropPosition{
			ByteOffset: p.ByteOffset,
			ElapsedMs:  p.ElapsedMs,
		})
	}

	result := make([]DropStat, 0, len(rows))
	for _, d := range rows {
		positions := positionsByPID[d.Pid]
		if positions == nil {
			positions = make([]DropPosition, 0)
		}
		stat := DropStat{
			Pid:       int(d.Pid),
			Packets:   d.Packets,
			Drops:     d.Drops,
			Errors:    d.Errors,
			Scrambled: d.Scrambled,
			Positions: positions,
		}
		// 分類できなかった PID では pidType を省略する（M2-13, issue #24）。
		if d.PidType != nil && *d.PidType != "" {
			stat.PidType = d.PidType
		}
		result = append(result, stat)
	}
	return ListRecordingDropStats200JSONResponse(result), nil
}
