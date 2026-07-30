package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/worker"
)

var version = "dev"

// defaultSite は M1 の単一サイト構成でのサイト名。定義は db.DefaultSite（唯一の出所）。
const defaultSite = db.DefaultSite

// Server は予約 API のハンドラ実装。oapi-codegen の StrictServerInterface を満たす。
type Server struct {
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]

	// encodeProfiles は config に定義されたエンコードプロファイル名の集合。
	// ルール保存時の encodeProfiles 存在検証に使う（issue #64）。
	// 空（未注入）なら名前検証をスキップする（テストの部分構成を許す）。
	encodeProfiles map[string]struct{}

	// encodeProfileNames は定義順の名前一覧（GET /api/encode-profiles 用。issue #68）。
	encodeProfileNames []string
}

// NewServer は Server を生成する。riverClient は insert-only の River クライアントで、
// nil なら ruler_pass ヒントの投入をスキップする（RouterConfig.RiverClient 参照）。
// encodeProfileNames は config.encode.profiles の名前一覧（未知名の 400 判定用。
// nil/空なら検証をスキップする）。一覧 API にも同じ順序で載せる。
func NewServer(pool *pgxpool.Pool, riverClient *river.Client[pgx.Tx], encodeProfileNames []string) *Server {
	// nil スライス = 検証オフ（テストの部分構成）。空スライス = 定義ゼロで全名が未知。
	// 本番の server.go は ProfileNames()（常に non-nil）を渡すので検証が常に効く。
	var profiles map[string]struct{}
	var names []string
	if encodeProfileNames != nil {
		profiles = make(map[string]struct{}, len(encodeProfileNames))
		names = make([]string, 0, len(encodeProfileNames))
		for _, n := range encodeProfileNames {
			profiles[n] = struct{}{}
			names = append(names, n)
		}
	}
	return &Server{pool: pool, river: riverClient, encodeProfiles: profiles, encodeProfileNames: names}
}

// Healthz はヘルスチェックエンドポイント。
func (h *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200JSONResponse{Status: "ok"}, nil
}

// GetVersion はサーバーバージョンを返す。
func (h *Server) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	return GetVersion200JSONResponse{Version: version}, nil
}

// ListReservations は予約一覧を返す。
func (h *Server) ListReservations(ctx context.Context, _ ListReservationsRequestObject) (ListReservationsResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListReservationsBySite(ctx, defaultSite)
	if err != nil {
		return nil, err
	}

	result := make([]Reservation, 0, len(rows))
	for _, r := range rows {
		res, err := reservationFromRow(r.Reservation, r.ProgramSnapshot, r.Overrides, r.IntentAction)
		if err != nil {
			return nil, err
		}
		result = append(result, res)
	}
	return ListReservations200JSONResponse(result), nil
}

// CreateReservation は手動予約を作成する。
//
// ユーザー意図（program_intents）・上書き（program_overrides）と導出行
// （reservations）を同一トランザクションで書く。意図だけ書いて ruler のパスを
// 待つ形にはしない（作成が UI に即座に反映されないため）。
func (h *Server) CreateReservation(ctx context.Context, req CreateReservationRequestObject) (CreateReservationResponseObject, error) {
	overrides := db.ReservationOptions{}
	if req.Body.Priority != nil {
		overrides.Priority = req.Body.Priority
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return nil, err
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// 番組の事実（title / 開始時刻 / 尺 / チャンネル識別）はすべて EPG
	// プロジェクションから引いてスナップショットする（サーバー権威。クライアント
	// からは受け取らない。#27 の決定「値の出所を射影ただ 1 つに固定する」）。
	// mirakc の programId 内部構造への依存を reconciler から消す目的に加え、
	// クライアントが古い番組表を握ったまま予約しても GC の比較対象
	// （program_snapshots.start_at + duration_ms）がずれないようにする。
	// ここで見つからなければ予約自体を作れない。
	source, err := q.GetProgramSnapshotSource(ctx, sqlcgen.GetProgramSnapshotSourceParams{
		Site:      defaultSite,
		ProgramID: req.Body.ProgramId,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CreateReservation400JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		return nil, fmt.Errorf("getting program snapshot source: %w", err)
	}

	// 番組の事実のスナップショットは program_snapshots に抽出された（#27）。
	// reservations / program_intents / program_overrides はいずれも
	// (site, program_id) の FK でこの行を参照するため、この 3 つより先に
	// upsert する。
	snap := sqlcgen.ProgramSnapshot{
		Site:        defaultSite,
		ProgramID:   req.Body.ProgramId,
		Title:       source.Title,
		StartAt:     source.StartAt,
		DurationMs:  source.DurationMs,
		NetworkID:   &source.NetworkID,
		ServiceID:   &source.ServiceID,
		ChannelType: &source.ChannelType,
		Channel:     &source.Channel,
	}
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        snap.Site,
		ProgramID:   snap.ProgramID,
		Title:       snap.Title,
		StartAt:     snap.StartAt,
		DurationMs:  snap.DurationMs,
		NetworkID:   snap.NetworkID,
		ServiceID:   snap.ServiceID,
		ChannelType: snap.ChannelType,
		Channel:     snap.Channel,
	}); err != nil {
		return nil, fmt.Errorf("upserting program snapshot: %w", err)
	}

	// 意図が先。導出行は ruler が作り直せるが、意図は誰も再生成できない。
	// action（record/skip）のみを持つ program_intents には overrides は載らない
	// （M2-4 で program_overrides に分離。docs/recording.md §4.2）。
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site:      defaultSite,
		ProgramID: req.Body.ProgramId,
		Action:    db.IntentRecord,
	}); err != nil {
		return nil, fmt.Errorf("upserting program intent: %w", err)
	}

	// priority 指定があれば上書きとして program_overrides にも書く。空（{}）なら
	// 行を作らない（「空の上書き = 行が無い」。isEmptyOverridesJSON のコメント参照）。
	if !isEmptyOverridesJSON(overridesJSON) {
		if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
			Site:      defaultSite,
			ProgramID: req.Body.ProgramId,
			Overrides: overridesJSON,
		}); err != nil {
			return nil, fmt.Errorf("upserting program overrides: %w", err)
		}
	}

	row, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      defaultSite,
		ProgramID: req.Body.ProgramId,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return CreateReservation409JSONResponse{Error: "reservation already exists for this program"}, nil
		}
		return nil, err
	}
	if err := h.insertReconcilePassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// CreateReservation は直前に program_intents{record} を書いたばかりなので
	// （上の UpsertProgramIntent 呼び出し）、この予約は常に手動由来である。
	// row（CreateManualReservation の戻り値）は program_intents を JOIN していない
	// ため、ここでは静的に db.IntentRecord を渡す。snap も同様にこの関数の中で
	// 組み立てた値をそのまま使う（コミット直後に再度 SELECT する必要がない）。
	intentAction := db.IntentRecord
	res, err := reservationFromRow(row, snap, overridesJSON, &intentAction)
	if err != nil {
		return nil, err
	}
	return CreateReservation201JSONResponse(res), nil
}

// GetReservation は指定 ID の予約を返す。
func (h *Server) GetReservation(ctx context.Context, req GetReservationRequestObject) (GetReservationResponseObject, error) {
	q := sqlcgen.New(h.pool)
	row, err := q.GetReservationFull(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetReservation404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}
	res, err := reservationFromRow(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction)
	if err != nil {
		return nil, err
	}
	return GetReservation200JSONResponse(res), nil
}

// DeleteReservation は予約を取消す。
//
// 取消は**無条件に intent{skip} を書いて導出行を落とす**。行を消すだけでは
// 「消された行」と「最初から無かった行」が ruler から区別できず、次の全量パスが
// 復活させてしまう（docs/recording.md §4.4）。意図が別表に残るので、
// 再生成者がいるかで分岐する必要はない。
func (h *Server) DeleteReservation(ctx context.Context, req DeleteReservationRequestObject) (DeleteReservationResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	row, err := q.GetReservationFull(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteReservation404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}

	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:      row.Reservation.Site,
		ProgramID: row.Reservation.ProgramID,
	}); err != nil {
		return nil, fmt.Errorf("recording skip intent: %w", err)
	}
	if _, err := q.DeleteReservation(ctx, req.Id); err != nil {
		return nil, err
	}
	if err := h.insertReconcilePassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return DeleteReservation204Response{}, nil
}

// insertReconcilePassHint は予約の作成/取消と同一トランザクションで
// ReconcilePassArgs を InsertTx する（ヒント経路。docs/recording.md §3.2
// 「予約の作成 / 取消」）。dual-write を避けるため、予約書き込みが失敗すれば
// このジョブも一緒にロールバックされる。副産物として、予約変更が mirakc へ
// 反映されるまでの待ち時間が定期パスの間隔（既定 30 秒）から実質即座になる
// （issue #25 §1）。
//
// h.river が nil の場合は何もしない（insertRulerPassHint と同じ理由。
// RouterConfig.RiverClient のコメント参照）。ReconcilePassArgs.InsertOpts の
// UniqueOpts{ByArgs, ByState} により、同一サイトのヒントは定期実行に合流する。
func (h *Server) insertReconcilePassHint(ctx context.Context, tx pgx.Tx) error {
	if h.river == nil {
		return nil
	}
	if _, err := h.river.InsertTx(ctx, tx, worker.ReconcilePassArgs{Site: defaultSite}, nil); err != nil {
		return fmt.Errorf("inserting reconcile_pass hint: %w", err)
	}
	return nil
}

// reservationState は Reservation.state を (rule_id, base, orphaned_at) から
// 導出する。
//
// state 列そのものは Phase 1（#27/#28/#30）で reservations から落とされた ---
// orphaned は不可逆な観測として orphaned_at（reconciler だけが書く）に、
// active/detached は (rule_id, base) から導出できる値として列を持たないことに
// した。API レスポンスは 1 バイトも変えないため、ここで毎回計算して返す
// （CLAUDE.md 不変条件 9「導出値と不可逆な事実を同じ列に載せない」・「導出は
// 読むたびに評価する」）。
//
// #30 症状 1（ルールを削除した経路では detached にならなかった）は、旧実装が
// この式を SQL の CASE で「前パスの rule_id」を見て遷移として保存していたため
// に起きた。ここで毎パス評価し直すことで、ルールの削除（FK の ON DELETE
// SET NULL が rule_id を先に NULL にする経路）でも編集と同じく detached になる。
func reservationState(ruleID *int64, base json.RawMessage, orphanedAt *time.Time) ReservationState {
	switch {
	case orphanedAt != nil:
		return Orphaned
	case ruleID == nil && len(base) > 0:
		return Detached
	default:
		return Active
	}
}

// reservationFromRow は予約行・番組スナップショット・ユーザーの上書き
// （program_overrides）・ユーザー意図（program_intents.action）から API 表現を
// 組む。snapshot / overrides / intentAction は予約行ではなく別表にあるので、
// まとめて受け取る（#27 で番組の事実のスナップショットが program_snapshots に
// 抽出された）。
//
// source は reservations の列ではなく、intentAction の有無から都度導出する
// （issue #26）。reservations.source 列は「ユーザーが手動で予約したか」（不可逆な
// 事実）と「いまルールが base を供給しているか」（rule_id の有無で変わる導出状態）
// を 1 列に混ぜていたため、手動予約にルールが一度でもマッチすると 'rule' に
// 書き換わり二度と戻らないバグがあった。rule_id や base の有無ではなく
// program_intents だけを判定材料にするのはそのため --- そうしないと「手動予約 +
// ルールがマッチ中」が rule と表示され、同じバグに逆戻りする
// （docs/recording.md §4.4「manual 行にルールがマッチしても昇格は要らない」）。
//
// skip も列ではなく db.EffectiveOptions の結果（base + overrides + action）である。
// 壊れた jsonb でエラーを返すのは意図的: skip=false を返して黙って進むと
// 「mirakc に同期されないのに理由が UI から読めない」という一番説明しにくい状態に
// なる（docs/schema.md §3「jsonb の Unmarshal 失敗を握りつぶさない」）。
// dedupMatchRecordingId / dedupSimilarity はその skip の根拠（M2-6）で、
// ruler が毎パス作り直す導出列をそのまま出す。
func reservationFromRow(r sqlcgen.Reservation, snap sqlcgen.ProgramSnapshot, overrides []byte, intentAction *string) (Reservation, error) {
	source := ReservationSourceRule
	if intentAction != nil && *intentAction == db.IntentRecord {
		source = ReservationSourceManual
	}
	opts, err := db.EffectiveOptions(r.Base, overrides, intentAction)
	if err != nil {
		return Reservation{}, fmt.Errorf("resolving effective options for reservation %d: %w", r.ID, err)
	}
	res := Reservation{
		Id:        r.ID,
		Site:      r.Site,
		ProgramId: r.ProgramID,
		Source:    source,
		State:     reservationState(r.RuleID, r.Base, r.OrphanedAt),
		// site を返すのは容量超過の判定がサイトごとに独立しているため
		// （docs/data.md §6.5）。クライアントに単一サイト前提の定数を持たせると、
		// 多サイト化のときに「他サイトの不足を自分の不足として出す」形で静かに壊れる。
		Skip:       opts.IsSkipped(),
		Title:      snap.Title,
		StartAt:    snap.StartAt,
		DurationMs: snap.DurationMs,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
	if r.RuleID != nil {
		res.RuleId = r.RuleID
	}
	// 2 列は必ず揃って設定/解除される（reservations_dedup_evidence_check）ので、
	// 片方だけ出る形にはならない。
	if r.DedupMatchRecordingID != nil {
		res.DedupMatchRecordingId = r.DedupMatchRecordingID
	}
	if r.DedupSimilarity.Valid {
		similarity := r.DedupSimilarity.Float32
		res.DedupSimilarity = &similarity
	}
	if len(overrides) > 0 && string(overrides) != "{}" {
		var m map[string]interface{}
		if json.Unmarshal(overrides, &m) == nil && len(m) > 0 {
			res.Overrides = &m
		}
	}
	return res, nil
}
