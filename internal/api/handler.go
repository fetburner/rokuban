package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

var version = "dev"

// Server は予約 API のハンドラ実装。oapi-codegen の StrictServerInterface を満たす。
type Server struct {
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]

	// sites はこのプロセスが応答してよい mirakc サイト名の集合（メンバーシップ判定用。
	// knownSite 参照）。api は不変条件 1（mirakc にもファイルシステムにも依存しない）
	// によりどの site にも束縛されないので、権威は「config.mirakc/mirakcs レジストリに
	// 存在するか」であり、1 プロセスがレジストリの全 site を処理できる
	// （issue #184 M4-12。旧実装は起動時の config.mirakc.site 1 つに固定していた）。
	sites map[string]struct{}

	// siteNames は sites の定義順一覧（GET /api/sites の応答、ルールが対象 site を
	// 指定しなかったときの既定の投入先。issue #184）。
	siteNames []string

	// encodeProfiles は config に定義されたエンコードプロファイル名の集合。
	// ルール保存時の encodeProfiles 存在検証に使う（issue #64）。
	// 空（未注入）なら名前検証をスキップする（テストの部分構成を許す）。
	encodeProfiles map[string]struct{}

	// encodeProfileNames は定義順の名前一覧（GET /api/encode-profiles 用。issue #68）。
	encodeProfileNames []string
}

// NewServer は Server を生成する。riverClient は insert-only の River クライアントで、
// nil なら ruler_pass ヒントの投入をスキップする（RouterConfig.RiverClient 参照）。
// sites が空なら db.DefaultSite の 1 要素とみなす（テストの部分構成を許す。既存の
// 「site が空なら db.DefaultSite」規約を集合に持ち上げただけで新しい規約ではない）。
// encodeProfileNames は config.encode.profiles の名前一覧（未知名の 400 判定用。
// nil/空なら検証をスキップする）。一覧 API にも同じ順序で載せる。
func NewServer(pool *pgxpool.Pool, riverClient *river.Client[pgx.Tx], sites []string, encodeProfileNames []string) *Server {
	siteNames := sites
	if len(siteNames) == 0 {
		siteNames = []string{db.DefaultSite}
	}
	siteSet := make(map[string]struct{}, len(siteNames))
	for _, s := range siteNames {
		siteSet[s] = struct{}{}
	}
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
	return &Server{
		pool: pool, river: riverClient,
		sites: siteSet, siteNames: siteNames,
		encodeProfiles: profiles, encodeProfileNames: names,
	}
}

// knownSite は site がこのプロセスの応答対象レジストリに存在するかを返す。
// パスパラメータ {site} の検証はすべてこれを通す（issue #184 M4-12）。
func (h *Server) knownSite(site string) bool {
	_, ok := h.sites[site]
	return ok
}

// Healthz はヘルスチェックエンドポイント。
func (h *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200JSONResponse{Status: "ok"}, nil
}

// GetVersion はサーバーバージョンを返す。
func (h *Server) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	return GetVersion200JSONResponse{Version: version}, nil
}

// ListReservations は予約一覧を返す。導出行の読み取り専用ビュー（issue #29）。
// 全サイトの予約を返す（issue #184 M4-12）。各要素の Site で区別する。
func (h *Server) ListReservations(ctx context.Context, _ ListReservationsRequestObject) (ListReservationsResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListReservationsFull(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]Reservation, 0, len(rows))
	for _, r := range rows {
		res, err := reservationFromRow(r.Reservation, r.ProgramSnapshot, r.Overrides, r.IntentAction, r.NeverRecorded)
		if err != nil {
			return nil, err
		}
		result = append(result, res)
	}
	return ListReservations200JSONResponse(result), nil
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
	res, err := reservationFromRow(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction, row.NeverRecorded)
	if err != nil {
		return nil, err
	}
	return GetReservation200JSONResponse(res), nil
}

// reservationState は Reservation.state を (rule_id, base, neverRecorded) から
// 導出する。
//
// state 列そのものは Phase 1（#27/#28/#30）で reservations から落とされた ---
// active/detached は (rule_id, base) から導出できる値として列を持たないことに
// した。orphaned は一度 orphaned_at（reconciler だけが書く不可逆な観測）を
// 経たが、issue #98 で orphaned_at 自体も廃止され、「この予約に
// status='failed' の recordings 行が存在するか」という EXISTS 判定
// （neverRecorded。呼び出し元がクエリの never_recorded 列から受け取る）に
// 置き換わった。API レスポンスは 1 バイトも変えないため、ここで毎回計算して
// 返す（CLAUDE.md 不変条件 9「導出値と不可逆な事実を同じ列に載せない」・
// 「導出は読むたびに評価する」）。
//
// #30 症状 1（ルールを削除した経路では detached にならなかった）は、旧実装が
// この式を SQL の CASE で「前パスの rule_id」を見て遷移として保存していたため
// に起きた。ここで毎パス評価し直すことで、ルールの削除（FK の ON DELETE
// SET NULL が rule_id を先に NULL にする経路）でも編集と同じく detached になる。
func reservationState(ruleID *int64, base json.RawMessage, neverRecorded bool) ReservationState {
	switch {
	case neverRecorded:
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
//
// neverRecorded は呼び出し元のクエリ（GetReservationFull / ListReservationsFull）
// が EXISTS で計算した「この予約に status='failed' の recordings 行があるか」
// （issue #98。reservationState のコメント参照）。
func reservationFromRow(r sqlcgen.Reservation, snap sqlcgen.ProgramSnapshot, overrides []byte, intentAction *string, neverRecorded bool) (Reservation, error) {
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
		State:     reservationState(r.RuleID, r.Base, neverRecorded),
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
