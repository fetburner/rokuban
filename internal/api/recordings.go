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
	// EncodeAttempts は recording_encode_attempts（衛星表）の行を jsonb_agg
	// した生 JSON（issue #316）。`[]`（試行中/失敗中のプロファイルが無い）と
	// nil を区別しない --- どちらも encodeJobStatusesFromFields で「完了して
	// いないプロファイルはすべて queued」という結果になる。
	EncodeAttempts json.RawMessage

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
			ObservedAt:    utcTimePtr(r.IngestObservedAt),
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
//     EnqueueMissingEncodesForKnownProfiles / ListRecordingsMissingEncodes は
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
// 戻り値は desired の並び順を保つ（TestEncodeJobStatusesFromFields_PreservesDesiredOrder。
// 試行行の map を回して組み立てると順序が非決定になるので、EncodeProfiles を
// 回す実装であることをテストが押さえている）。
func encodeJobStatusesFromFields(r recordingListFields, done []string, knownProfiles map[string]struct{}) ([]EncodeJobStatus, error) {
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
		if knownProfiles != nil {
			if _, known := knownProfiles[profile]; !known {
				continue
			}
		}
		statuses = append(statuses, EncodeJobStatus{Profile: profile, State: EncodeJobStatusStateQueued})
	}
	return statuses, nil
}

// recordingFromListFields は一覧行を API の Recording に写す。
// includeDeletedAt が true のときだけ deletedAt を載せる（ごみ箱一覧向け）。
// knownProfiles は encodeJobStatusesFromFields に渡す（doc コメント参照。
// nil なら「設定から消えたプロファイル」の判定をスキップする）。
func recordingFromListFields(r recordingListFields, includeDeletedAt bool, knownProfiles map[string]struct{}) (Recording, error) {
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
		StartAt:     r.ProgramStartAt.UTC(),
		DurationMs:  r.ProgramDurationMs,
		Status:      RecordingStatus(r.Status),
		StartedAt:   utcTimePtr(r.StartedAt),
		EndedAt:     utcTimePtr(r.EndedAt),
		SizeBytes:   r.OriginalSizeBytes,
		CreatedAt:   r.CreatedAt.UTC(),
	}
	if includeDeletedAt {
		rec.DeletedAt = utcTimePtr(r.DeletedAt)
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
	// ごみ箱の録画（r.DeletedAt が非 nil）では available_encoded_assets を
	// 出さない --- ごみ箱ではプレイヤーを出さないので値を揃えても使われない
	// （3d56f92 の理由。性能実測は無い）。一覧（queryRecordings）・単体
	// （queryRecordingByID）のどちらも常に SQL でこの列を連結し、この関数を
	// 経由するので、判定はここ 1 か所だけで足りる（以前は一覧が SQL 側で
	// 列自体を省き、単体 GET が呼び出し元で nil 化する 2 手段だった）。
	// ごみ箱の録画（r.DeletedAt が非 nil）では available_encoded_assets を
	// 出さない --- ごみ箱ではプレイヤーを出さないので値を揃えても使われない
	// （3d56f92 の理由。性能実測は無い）。一覧（queryRecordings）・単体
	// （queryRecordingByID）のどちらも常に SQL でこの列を連結し、この関数を
	// 経由するので、判定はここ 1 か所だけで足りる（以前は一覧が SQL 側で
	// 列自体を省き、単体 GET が呼び出し元で nil 化する 2 手段だった）。
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
			assets := make([]EncodedAsset, len(rows))
			encodedProfileNames = make([]string, len(rows))
			for i, row := range rows {
				assets[i] = EncodedAsset{Profile: row.Profile, SizeBytes: &row.SizeBytes}
				encodedProfileNames[i] = row.Profile
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
	statuses, err := encodeJobStatusesFromFields(r, encodedProfileNames, knownProfiles)
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

	result, err := queryRecordings(ctx, h.pool, f, h.encodeProfiles)
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
	rec, ok, err := queryRecordingByID(ctx, h.pool, req.Id, h.encodeProfiles)
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
