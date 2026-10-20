package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

func recordingQueryRowsToAPI(rows []store.RecordingQueryRow, includeDeletedAt bool, profiles profileSets) ([]Recording, error) {
	result := make([]Recording, 0, len(rows))
	for _, row := range rows {
		recording, err := recordingFromListFields(row, includeDeletedAt, profiles)
		if err != nil {
			return nil, err
		}
		result = append(result, recording)
	}
	return result, nil
}

// utcTimePtr は timestamptz の scan 結果を UTC の Location に正規化する。
//
// RecordingQuery.List（pgx.QueryExecModeExec で text protocol）は timestamptz を
// セッションの TimeZone（Postgres の TimeZone GUC。ローカル環境で既定が UTC
// でないことがある）で Location 付きに decode するが、RecordingQuery.Get
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
func ingestProgressFromFields(r store.RecordingQueryRow) IngestProgress {
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
//     無いものは queued と名乗らせない（下記 2 点。docs/recording/ingest-followup.md
//     §5.6 が `pending` に課した規律と同じ）
//
// 「来る根拠」が無いので queued を出さない 2 パターン:
//
//  1. ごみ箱の録画（r.DeletedAt が非 nil）。EncodeReconcileWorker の
//     ListMissingEncodeProfiles は view encodable_recordings でごみ箱を除いて
//     おり、ごみ箱の録画に reconcile から encode ジョブは投入されない
//     （internal/db/queries/encode_reconcile.sql 参照）。EncodedAssets/プレイヤーを
//     trash で出さないのと揃え、**running/failed の行が既にあっても
//     （削除前に始まっていた試行）試行状態を丸ごと省略する** --- 削除後に
//     その試行が本当に終わるかは api ロールには分からない（不変条件 1: api は
//     worker に問い合わせない）。実装は先頭の DeletedAt ガード 1 つで、
//     TestListRecordingsEncodeStatus_TrashOmitsEncodeStatus（running な試行行
//     付き）と TestEncodeJobStatusesFromFields の「ごみ箱の録画は試行行が
//     あっても丸ごと省略」が固定している。
//  2. knownProfiles が non-nil（api ロールが config.encode.profiles を注入
//     している）で、そのプロファイルが現在の config に存在しない。設定から
//     消えたプロファイルは encode reconcile が投入対象から外している
//     恒久的に満たせない集合（`ListUnsatisfiableEncodeProfiles` が
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
// （encode_reconcile.sql の同じ述語）。試行行が既にあればそちらを優先する ---
// 過去の観測（running/failed）は確認の有無に関わらず事実である。
//
// 戻り値は desired の並び順を保つ（TestEncodeJobStatusesFromFields_PreservesDesiredOrder。
// 試行行の map を回して組み立てると順序が非決定になるので、EncodeProfiles を
// 回す実装であることをテストが押さえている）。
func encodeJobStatusesFromFields(r store.RecordingQueryRow, done []string, profiles profileSets) ([]EncodeJobStatus, error) {
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
func currentKeepRanges(r store.RecordingQueryRow) ([]chapters.Range, error) {
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
func recordingFromListFields(r store.RecordingQueryRow, includeDeletedAt bool, profiles profileSets) (Recording, error) {
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
	// （3d56f92 の理由。性能実測は無い）。一覧（RecordingQuery.List）・単体
	// （RecordingQuery.Get）のどちらも常に SQL でこの列を連結し、この関数を
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

func cmDetectionFromListFields(r store.RecordingQueryRow) (CMDetection, error) {
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
// 動的 WHERE ビルダ（pgstore の RecordingQuery.List）を使う。sqlc の静的クエリに
// しない理由は pgstore の実装コメントを参照（trgm 式 GIN が汎用プランで使われなく
// なることを避けるため）。
//
// api は site に束縛されない（不変条件 1）ため、全サイトの録画を返す
// （issue #184 M4-12）。各要素の Site で区別する。
func (h *Server) ListRecordings(ctx context.Context, req ListRecordingsRequestObject) (ListRecordingsResponseObject, error) {
	f, errMsg := recordingsFilterFromParams(req.Params)
	if errMsg != "" {
		return ListRecordings400JSONResponse{Error: errMsg}, nil
	}

	if req.Params.EncodeState != nil {
		rows, err := h.loadEncodeQueueRows(ctx)
		if err != nil {
			return nil, fmt.Errorf("loading encode queue: %w", err)
		}
		recordingIDs := encodeQueueRecordingIDs(rows, *req.Params.EncodeState == ListRecordingsParamsEncodeStateRunning)
		f.EncodeRecordingIDs = &recordingIDs
	}

	if h.recordingQuery == nil {
		return nil, fmt.Errorf("recording query is required")
	}
	rows, err := h.recordingQuery.List(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("listing recordings: %w", err)
	}
	result, err := recordingQueryRowsToAPI(rows, f.Trash, h.profileSets())
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
// （RecordingQuery.Get 参照）。
func (h *Server) GetRecording(ctx context.Context, req GetRecordingRequestObject) (GetRecordingResponseObject, error) {
	if h.recordingQuery == nil {
		return nil, fmt.Errorf("recording query is required")
	}
	fields, err := h.recordingQuery.Get(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return GetRecording404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("getting recording %d: %w", req.Id, err)
	}
	rec, err := recordingFromListFields(fields, true, h.profileSets())
	if err != nil {
		return nil, fmt.Errorf("getting recording %d: %w", req.Id, err)
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
	if h.epgReader == nil {
		return nil, fmt.Errorf("EPG reader is required")
	}
	rows, err := h.epgReader.ListUpcomingProgramsBySeries(ctx, req.Id)
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
	err := h.recordingTrash.SoftDelete(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeleteRecording404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("soft-deleting recording %d: %w", req.Id, err)
	}
	return DeleteRecording204Response{}, nil
}

// RestoreRecording はごみ箱から録画を復元する。
// 即時 purge 要求も取り下げ、同一イベントに生きた録画があれば 409 を返す。
// 書き込みと直列化の保証は store の契約が持つ。
func (h *Server) RestoreRecording(ctx context.Context, req RestoreRecordingRequestObject) (RestoreRecordingResponseObject, error) {
	err := h.recordingTrash.Restore(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RestoreRecording404JSONResponse{Error: "recording not in trash"}, nil
		}
		if errors.Is(err, store.ErrConflict) {
			return RestoreRecording409JSONResponse{
				Error: "active recording already exists for the same event",
			}, nil
		}
		return nil, fmt.Errorf("restoring recording %d: %w", req.Id, err)
	}
	return RestoreRecording204Response{}, nil
}

// PurgeRecording は即時物理削除の要求を記録する。
// recording_purge_requests に行を入れ、未 soft-delete なら deleted_at も立てる。
// ファイルは消さない（M3-8 の削除 reconcile が拾う）。
func (h *Server) PurgeRecording(ctx context.Context, req PurgeRecordingRequestObject) (PurgeRecordingResponseObject, error) {
	err := h.recordingTrash.RequestPurge(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
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
// 無い、のいずれか）なら 409 を返す。対象 reconcile が候補なしで終わっても
// HTTP は成功扱いになるため、ここで明示的に検査する。
//
// 方針の更新は store のトランザクションで確定し、その後に reconcile をヒント投入する。
// 投入に失敗しても定期 reconcile が desired から不足分を拾う。
func (h *Server) validateEncodeSelection(desired []string) error {
	if err := h.validateCutSelection(desired); err != nil {
		return encodeProfileSelectionError{err: err}
	}
	return nil
}

type encodeProfileSelectionError struct{ err error }

func (e encodeProfileSelectionError) Error() string { return e.err.Error() }
func (e encodeProfileSelectionError) Unwrap() error { return e.err }

func (h *Server) AddRecordingEncodeProfiles(ctx context.Context, req AddRecordingEncodeProfilesRequestObject) (AddRecordingEncodeProfilesResponseObject, error) {
	if req.Body == nil || len(req.Body.Profiles) == 0 {
		return AddRecordingEncodeProfiles400JSONResponse{Error: "profiles must not be empty"}, nil
	}
	// 名前は追加分だけ、cut の選択規則はマージ後（下、tx 内）に当てる。
	if err := h.validateEncodeProfileNames(req.Body.Profiles); err != nil {
		return AddRecordingEncodeProfiles400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // Invalid input is an HTTP 400 response, not a handler failure.
	}

	err := h.encodePolicy.AppendProfiles(ctx, req.Id, req.Body.Profiles, h.validateEncodeSelection)
	if errors.Is(err, store.ErrNotFound) {
		return AddRecordingEncodeProfiles404JSONResponse{Error: "recording not found"}, nil
	}
	if errors.Is(err, store.ErrOriginalUnavailable) {
		return AddRecordingEncodeProfiles409JSONResponse{
			Error: "original media asset not active (deleted, deleting, or not yet ingested); cannot add encode profiles",
		}, nil
	}
	var sel encodeProfileSelectionError
	if errors.As(err, &sel) {
		return AddRecordingEncodeProfiles400JSONResponse{Error: err.Error()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("appending encode profiles for recording %d: %w", req.Id, err)
	}
	h.enqueueEncodeReconcileHint(ctx, req.Id)
	return AddRecordingEncodeProfiles204Response{}, nil
}

// RemoveRecordingEncodedAsset はエンコード版を 1 本だけ外す（凍結の 3 つ目の例外。
// docs/storage/retention.md §6）。encode_profiles から profile を外し、外した要求の
// 行を入れる。ファイルは消さない。削除 reconcile が名前付き述語
// removed_encoded_assets で拾って unlink する。
//
// 録画行を先にロックしてから policy 行を FOR UPDATE で取る。encode 公開と同じ
// ロック順にし、外す tx と公開 tx の循環待ちを防ぐ。同じ録画の版を同時に外す
// 2 本の tx が 0 コピーを作らないよう、0 コピー検査は書いた後の状態で行う
// （不変条件 9「適用の瞬間」）。検査は削除 reconcile と同じ view に任せる。
func (h *Server) RemoveRecordingEncodedAsset(ctx context.Context, req RemoveRecordingEncodedAssetRequestObject) (RemoveRecordingEncodedAssetResponseObject, error) {
	err := h.encodePolicy.RemoveEncodedAsset(ctx, req.Id, req.Profile, h.validateEncodeSelection)
	if errors.Is(err, store.ErrNotFound) {
		return RemoveRecordingEncodedAsset404JSONResponse{Error: "recording not found"}, nil
	}
	if errors.Is(err, store.ErrEncodedAssetNotFound) {
		return RemoveRecordingEncodedAsset404JSONResponse{Error: "encoded version not found"}, nil
	}
	var sel encodeProfileSelectionError
	if errors.As(err, &sel) {
		return RemoveRecordingEncodedAsset400JSONResponse{Error: err.Error()}, nil
	}
	if errors.Is(err, store.ErrLastViewableCopy) {
		return RemoveRecordingEncodedAsset409JSONResponse{
			Error: "this is the last viewable copy (no active original and no other encoded version); move the recording to trash instead",
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("removing encoded asset %q for recording %d: %w", req.Profile, req.Id, err)
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
// keep 区間を衛星表に記録してから commit し、encode_reconcile をヒントとして投入する。
// 通常の reconcile は active な encoded を完了として扱うため、明示要求は DB 行で
// 区別し、ジョブが失われても定期パスが拾えるようにする。
//
// 原本が active でないなら 409（カット版は原本から作り直すしかない）。
func (h *Server) ReencodeRecordingProfile(ctx context.Context, req ReencodeRecordingProfileRequestObject) (ReencodeRecordingProfileResponseObject, error) {
	// cut 専用の作り直し。cut の選択規則（cut だけの選択の拒否）は選択の集合に
	// 対する規則でここには当てはまらないので、名前と「cut か」だけを見る。
	if err := h.validateEncodeProfileNames([]string{req.Profile}); err != nil {
		return ReencodeRecordingProfile400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // Invalid input is an HTTP 400 response, not a handler failure.
	}
	if h.cutProfiles != nil {
		if _, ok := h.cutProfiles[req.Profile]; !ok {
			return ReencodeRecordingProfile400JSONResponse{Error: fmt.Sprintf("profile %q is not a cut profile", req.Profile)}, nil
		}
	}

	err := h.encodePolicy.RequestCutRebuild(ctx, req.Id, req.Profile)
	if errors.Is(err, store.ErrNotFound) {
		return ReencodeRecordingProfile404JSONResponse{Error: "recording not found"}, nil
	}
	if errors.Is(err, store.ErrOriginalUnavailable) {
		return ReencodeRecordingProfile409JSONResponse{
			Error: "original media asset not active (deleted, deleting, or not yet ingested); cannot rebuild the cut version",
		}, nil
	}
	if errors.Is(err, store.ErrChaptersNotOwned) {
		return ReencodeRecordingProfile409JSONResponse{Error: "chapters not yet confirmed; nothing to encode"}, nil
	}
	if errors.Is(err, store.ErrKeepRangesEmpty) {
		return ReencodeRecordingProfile409JSONResponse{Error: "timeline has no keep ranges; nothing to encode"}, nil
	}
	if errors.Is(err, store.ErrProfileNotDesired) {
		return ReencodeRecordingProfile409JSONResponse{Error: fmt.Sprintf("profile %q is not in the desired encode profiles of this recording; nothing to encode", req.Profile)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recording cut rebuild request for %d profile %q: %w", req.Id, req.Profile, err)
	}
	h.enqueueEncodeReconcileHint(ctx, req.Id)
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
// cmDetect が切り替わるたびに policy の世代番号を進め、試行行の削除は CM reconcile に任せる。
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

	update := store.EncodePolicyUpdate{}
	if req.Body.KeepOriginal != nil {
		keepOriginal := string(*req.Body.KeepOriginal)
		update.KeepOriginal = &keepOriginal
	}
	update.CMDetect = req.Body.CmDetect
	err := h.encodePolicy.SetPolicy(ctx, req.Id, update)
	if errors.Is(err, store.ErrNotFound) {
		return SetRecordingEncodePolicy404JSONResponse{Error: "recording not found"}, nil
	}
	if errors.Is(err, store.ErrEncodeProfilesRequired) {
		return SetRecordingEncodePolicy409JSONResponse{Error: wantKeepOriginalUntilEncodedMessage}, nil
	}
	if errors.Is(err, store.ErrOriginalUnavailable) {
		return SetRecordingEncodePolicy409JSONResponse{Error: "active original media asset required to enable CM detection"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("setting recording %d encode policy: %w", req.Id, err)
	}
	return SetRecordingEncodePolicy204Response{}, nil
}

// enqueueEncodeReconcileHint は DB commit 後に対象録画の reconcile を best effort で
// 投入する。命令の真実は DB 行なので、失敗しても定期パスが拾い直す。
func (h *Server) enqueueEncodeReconcileHint(ctx context.Context, recordingID int64) {
	if h.enqueuer == nil {
		return
	}
	if _, err := h.enqueuer.Enqueue(ctx, jobs.EncodeReconcileArgs{RecordingID: recordingID}); err != nil {
		slog.ErrorContext(ctx, "api: failed to enqueue encode reconcile hint for rebuild request",
			"recording_id", recordingID, "err", err)
	}
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
func (h *Server) ListRecordingDropStats(ctx context.Context, req ListRecordingDropStatsRequestObject) (ListRecordingDropStatsResponseObject, error) {
	rows, err := h.mediaReader.ListRecordingDropStats(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	positionRows, err := h.mediaReader.ListRecordingDropPositions(ctx, req.Id)
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
