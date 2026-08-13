package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/worker"
)

// recordingListFields は ListRecordings / ListTrashRecordings が共有する射影。
// sqlc はクエリごとに別 struct を生成するので、ここで共通化してマッピングする。
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
	// AvailableEncodedAssets は recordingsAvailableEncodedAssetsSelect
	// （jsonb_agg({profile, sizeBytes})）を Scan した生 JSON。プロファイル名の
	// 配列とサイズの配列を並行に持つ形にしなかった理由は recordings_query.go の
	// コメント参照。nil（trash など SELECT に含めなかった行）と `[]`（active な
	// encoded が無い行）は区別しない --- どちらも recordingFromListFields で
	// EncodedAssets を省略する結果になる。
	AvailableEncodedAssets json.RawMessage
	// EncodeProfiles は凍結された desired 一覧（recordings.encode_profiles）。
	// AvailableEncodedAssets（observed、active のみ）とは異なり、pending な
	// ジョブのプロファイルも含む。事後追加（issue #133）で増える唯一の経路。
	EncodeProfiles []string

	// HasOriginalAsset は kind='original' の media_assets 行が **state を問わず**
	// 存在するか（issue #212）。OriginalSizeBytes（state <> 'deleted' の行だけを
	// 見る）とはわざと述語が違う --- この差が「まだ取り込めていない」と
	// 「取り込んだ後に削除した」を分ける（issue #211）。
	HasOriginalAsset bool
	// HasIngestableRecord は **ingest ジョブが投入される（された）はずの**
	// mirakc record の観測がこの録画に紐付いているか。原本も進捗も無いときに
	// 「取り込み待ち」と「そもそも取り込みが来ない」を分ける。
	//
	// 単なる record_sync 行の存在ではなく `status = 'finished'` で絞る
	// （recordings_query.go の SQL コメント参照）。watcher が ingest を投入する
	// 条件と同じものを見ていないと、failed / canceled の録画が永久に pending を
	// 名乗る。
	HasIngestableRecord bool
	// IngestWrittenBytes / IngestExpectedBytes / IngestObservedAt は
	// recording_ingest_progress の 1 行（無ければすべて nil）。
	IngestWrittenBytes  *int64
	IngestExpectedBytes *int64
	IngestObservedAt    *time.Time
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
//  2. 進捗行があれば transferring。バイト数と観測時刻を添える。
//  3. **ingest ジョブが来るはずの** record 観測だけがあれば pending
//     （取り込み待ち / 再試行待ち）。
//  4. どれでもなければ unknown --- 取り込みが始まった観測が無い。record 自体が
//     観測されていないか、mirakc の record が finished でない（録画中・失敗・
//     中止。この録画に ingest ジョブは投入されない）。
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
	case r.IngestWrittenBytes != nil:
		written := *r.IngestWrittenBytes
		return IngestProgress{
			State:         Transferring,
			WrittenBytes:  &written,
			ExpectedBytes: r.IngestExpectedBytes,
			ObservedAt:    r.IngestObservedAt,
		}
	case r.HasIngestableRecord:
		return IngestProgress{State: Pending}
	default:
		return IngestProgress{State: Unknown}
	}
}

// encodedAssetRow は available_encoded_assets（jsonb_agg）1 要素の JSON 形。
// jsonb_build_object のキー（'profile' / 'sizeBytes'）と一致させる。
type encodedAssetRow struct {
	Profile   string `json:"profile"`
	SizeBytes int64  `json:"sizeBytes"`
}

// recordingFromListFields は一覧行を API の Recording に写す。
// includeDeletedAt が true のときだけ deletedAt を載せる（ごみ箱一覧向け）。
func recordingFromListFields(r recordingListFields, includeDeletedAt bool) (Recording, error) {
	rec := Recording{
		Id:          r.ID,
		Site:        r.Site,
		RuleId:      r.RuleID,
		Source:      RecordingSource(r.Source),
		ServiceName: r.ServiceName,
		ChannelType: RecordingChannelType(r.ChannelType),
		Channel:     r.Channel,
		NetworkId:   int(r.NetworkID),
		ServiceId:   int(r.ServiceID),
		EventId:     int(r.EventID),
		Title:       r.Title,
		Description: r.Description,
		StartAt:     r.ProgramStartAt,
		DurationMs:  r.ProgramDurationMs,
		Status:      RecordingStatus(r.Status),
		StartedAt:   r.StartedAt,
		EndedAt:     r.EndedAt,
		SizeBytes:   r.OriginalSizeBytes,
		CreatedAt:   r.CreatedAt,
	}
	if includeDeletedAt {
		rec.DeletedAt = r.DeletedAt
	}
	// ドロップ統計は ingest 済み（media_assets 行がある）録画にしか存在しない。
	// 未 ingest と「統計が全部 0」を区別できるよう、原本が無ければ省略する。
	if r.OriginalSizeBytes != nil {
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
	// 再生可能な encoded 派生物（observed）。空 `[]`/nil なら省略（omitempty）。
	//
	// `EncodedAssets`（profile + sizeBytes）と `EncodedProfiles`（profile
	// だけ、後方互換のため残す。openapi.yaml の `encodedProfiles` description・
	// docs/frontend/recordings.md 参照）は、必ずこの同じ `rows`（1 回の
	// jsonb_agg の結果）から同時に作る。名前だけの配列とサイズ付きの配列を
	// 別々の SQL 集約（2 本の array_agg/jsonb_agg）で作ると、片方だけ
	// ORDER BY を書き忘れたときに添字がずれる drift が起こり得るが、
	// 同じ `rows` から Go 側で両方を導出する形ではその種の drift は構造的に
	// 起こらない。
	if len(r.AvailableEncodedAssets) > 0 {
		var rows []encodedAssetRow
		if err := json.Unmarshal(r.AvailableEncodedAssets, &rows); err != nil {
			return Recording{}, fmt.Errorf("decoding available_encoded_assets for recording %d: %w", r.ID, err)
		}
		if len(rows) > 0 {
			assets := make([]EncodedAsset, len(rows))
			profiles := make([]string, len(rows))
			for i, row := range rows {
				assets[i] = EncodedAsset{Profile: row.Profile, SizeBytes: &row.SizeBytes}
				profiles[i] = row.Profile
			}
			rec.EncodedAssets = &assets
			rec.EncodedProfiles = &profiles
		}
	}
	// 凍結された desired 一覧。空なら省略（omitempty）。UI が「追加済み」を
	// 判定するのに使う（issue #133）。
	if len(r.EncodeProfiles) > 0 {
		profiles := slices.Clone(r.EncodeProfiles)
		rec.EncodeProfiles = &profiles
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
	return rec, nil
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

	result, err := queryRecordings(ctx, h.pool, f)
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
	rec, ok, err := queryRecordingByID(ctx, h.pool, req.Id)
	if err != nil {
		return nil, fmt.Errorf("getting recording %d: %w", req.Id, err)
	}
	if !ok {
		return GetRecording404JSONResponse{Error: "recording not found"}, nil
	}
	return GetRecording200JSONResponse(rec), nil
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
// deleted_at と purge_after を消すだけ（ファイル操作ゼロ）。
// 同一イベントに生きている録画があると 409。
func (h *Server) RestoreRecording(ctx context.Context, req RestoreRecordingRequestObject) (RestoreRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).RestoreRecording(ctx, req.Id)
	if err != nil {
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
	return RestoreRecording204Response{}, nil
}

// PurgeRecording は即時物理削除の要求印を立てる。
// purge_after = now() を書き、未 soft-delete なら deleted_at も立てる。
// ファイルは消さない（M3-8 の削除 reconcile が拾う）。
func (h *Server) PurgeRecording(ctx context.Context, req PurgeRecordingRequestObject) (PurgeRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).MarkRecordingPurgeAfter(ctx, req.Id)
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
// 原本が既に削除済み（GetActiveOriginalMediaAsset が ErrNoRows）なら 409 を返す。
// EnqueueMissingEncodes は単体だとこのケースで黙って return するため
// （サイレント no-op）、ここで明示的に検査する。
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
	if err := h.validateEncodeProfiles(req.Body.Profiles); err != nil {
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

	// 原本削除済みなら 409（罠: EnqueueMissingEncodes 単体はここで黙って no-op に
	// なるため、サイレントな失敗にしないよう api 層で先に検査する）。
	if _, err := q.GetActiveOriginalMediaAsset(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 「削除済み」に限らず deleting（unlink 待ち）も含む --- 一覧の射影は
			// state <> 'deleted' なので UI 側は deleting を「原本あり」と見て
			// ボタンを出しうる（issue #105 の経路で active に戻ることもある）。
			// その場合ここに落ちるので、文言は両方を含む形にする。
			return AddRecordingEncodeProfiles409JSONResponse{
				Error: "no encodable original media asset (deleted or being deleted); cannot add encode profiles",
			}, nil
		}
		return nil, fmt.Errorf("loading original media asset for recording %d: %w", req.Id, err)
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
	if _, err := h.river.InsertTx(ctx, tx, worker.EncodeEnqueueHintArgs{RecordingID: recordingID}, nil); err != nil {
		return fmt.Errorf("inserting encode_enqueue_hint: %w", err)
	}
	return nil
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
func (h *Server) ListRecordingDropStats(ctx context.Context, req ListRecordingDropStatsRequestObject) (ListRecordingDropStatsResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListRecordingDropStats(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	result := make([]DropStat, 0, len(rows))
	for _, d := range rows {
		stat := DropStat{
			Pid:       int(d.Pid),
			Packets:   d.Packets,
			Drops:     d.Drops,
			Errors:    d.Errors,
			Scrambled: d.Scrambled,
		}
		// 分類できなかった PID では pidType を省略する（M2-13, issue #24）。
		if d.PidType != nil && *d.PidType != "" {
			stat.PidType = d.PidType
		}
		result = append(result, stat)
	}
	return ListRecordingDropStats200JSONResponse(result), nil
}
