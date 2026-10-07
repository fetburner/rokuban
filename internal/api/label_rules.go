package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

const defaultLabelRulePriority = 0

// ListLabelRules はシリーズ分類ルールを勝者順に返す。
func (h *Server) ListLabelRules(ctx context.Context, _ ListLabelRulesRequestObject) (ListLabelRulesResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListLabelRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing label rules: %w", err)
	}
	out := make([]LabelRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, labelRuleFromRow(row))
	}
	return ListLabelRules200JSONResponse(out), nil
}

// GetLabelRule はシリーズ分類ルールを 1 件返す。
func (h *Server) GetLabelRule(ctx context.Context, req GetLabelRuleRequestObject) (GetLabelRuleResponseObject, error) {
	row, err := sqlcgen.New(h.pool).GetLabelRule(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetLabelRule404JSONResponse{Error: "label rule not found"}, nil
		}
		return nil, fmt.Errorf("getting label rule %d: %w", req.Id, err)
	}
	return GetLabelRule200JSONResponse(labelRuleFromRow(row)), nil
}

// CreateLabelRule はシリーズ分類ルールを作成し、同じ tx で録画全件再評価を投入する。
func (h *Server) CreateLabelRule(ctx context.Context, req CreateLabelRuleRequestObject) (CreateLabelRuleResponseObject, error) {
	if req.Body == nil {
		return CreateLabelRule400JSONResponse{Error: "request body is required"}, nil
	}
	input, err := labelRuleInput(*req.Body)
	if err != nil {
		//nolint:nilerr // validation errors are the handler's typed 400 response.
		return CreateLabelRule400JSONResponse{Error: err.Error()}, nil
	}
	valueMessage, err := h.labelRuleValueMessage(ctx, input.value)
	if err != nil {
		return nil, err
	}
	if valueMessage != "" {
		return CreateLabelRule400JSONResponse{Error: valueMessage}, nil
	}
	row, err := h.insertLabelRule(ctx, input)
	if err != nil {
		return nil, err
	}
	return CreateLabelRule201JSONResponse(labelRuleFromRow(row)), nil
}

// UpdateLabelRule はシリーズ分類ルールを上書きし、同じ tx で録画全件再評価を投入する。
func (h *Server) UpdateLabelRule(ctx context.Context, req UpdateLabelRuleRequestObject) (UpdateLabelRuleResponseObject, error) {
	if req.Body == nil {
		return UpdateLabelRule400JSONResponse{Error: "request body is required"}, nil
	}
	input, err := labelRuleInput(*req.Body)
	if err != nil {
		//nolint:nilerr // validation errors are the handler's typed 400 response.
		return UpdateLabelRule400JSONResponse{Error: err.Error()}, nil
	}
	valueMessage, err := h.labelRuleValueMessage(ctx, input.value)
	if err != nil {
		return nil, err
	}
	if valueMessage != "" {
		return UpdateLabelRule400JSONResponse{Error: valueMessage}, nil
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning label rule update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	row, err := q.UpdateLabelRule(ctx, sqlcgen.UpdateLabelRuleParams{
		ID: req.Id, Value: input.value, Keyword: input.keyword, Priority: input.priority,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UpdateLabelRule404JSONResponse{Error: "label rule not found"}, nil
		}
		return nil, fmt.Errorf("updating label rule %d: %w", req.Id, err)
	}
	if err := insertLabelRuleReconcile(ctx, tx, h.river); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing label rule update: %w", err)
	}
	return UpdateLabelRule200JSONResponse(labelRuleFromRow(row)), nil
}

// DeleteLabelRule はシリーズ分類ルールを削除し、同じ tx で録画全件再評価を投入する。
func (h *Server) DeleteLabelRule(ctx context.Context, req DeleteLabelRuleRequestObject) (DeleteLabelRuleResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning label rule deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	deleted, err := q.DeleteLabelRule(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("deleting label rule %d: %w", req.Id, err)
	}
	if deleted == 0 {
		return DeleteLabelRule404JSONResponse{Error: "label rule not found"}, nil
	}
	if err := insertLabelRuleReconcile(ctx, tx, h.river); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing label rule deletion: %w", err)
	}
	return DeleteLabelRule204Response{}, nil
}

// GetLabelRuleValueKey は入力中の値が棚のキーとして何になるかを返す。
// 正規化は SQL 関数 series_key の 1 箇所にあり、UI に複製させない。
func (h *Server) GetLabelRuleValueKey(ctx context.Context, req GetLabelRuleValueKeyRequestObject) (GetLabelRuleValueKeyResponseObject, error) {
	var key *string
	if err := h.pool.QueryRow(ctx, "SELECT public.series_key($1)", req.Params.Value).Scan(&key); err != nil {
		return nil, fmt.Errorf("normalizing label rule value %q: %w", req.Params.Value, err)
	}
	if key == nil {
		return GetLabelRuleValueKey200JSONResponse{}, nil
	}
	return GetLabelRuleValueKey200JSONResponse{ValueKey: *key}, nil
}

// ListRecordingShelves は生きている録画を実効シリーズごとに集計し、再生可能な
// 件数と未視聴の放送イベント数を同じ集計から返す。絞り込みは録画一覧と同じ
// 条件をグループ化の前に当てる（buildRecordingShelvesQuery）。
func (h *Server) ListRecordingShelves(ctx context.Context, req ListRecordingShelvesRequestObject) (ListRecordingShelvesResponseObject, error) {
	if req.Params.Key != nil && !req.Params.Key.Valid() {
		return ListRecordingShelves400JSONResponse{Error: fmt.Sprintf("invalid key %q (want series)", *req.Params.Key)}, nil
	}
	f, errMsg := recordingsFilterFromShelvesParams(req.Params)
	if errMsg != "" {
		return ListRecordingShelves400JSONResponse{Error: errMsg}, nil
	}
	sql, args := buildRecordingShelvesQuery(f)
	rows, err := h.pool.Query(ctx, sql, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("listing recording shelves: %w", err)
	}
	defer rows.Close()
	out := []RecordingShelf{}
	for rows.Next() {
		var shelf RecordingShelf
		var count, playable, unwatched int64
		if err := rows.Scan(&shelf.Value, &shelf.Title, &count, &playable, &unwatched,
			&shelf.LatestStartAt, &shelf.RepresentativeId); err != nil {
			return nil, fmt.Errorf("scanning recording shelf: %w", err)
		}
		shelf.Count, shelf.PlayableCount, shelf.UnwatchedCount = int(count), int(playable), int(unwatched)
		shelf.LatestStartAt = shelf.LatestStartAt.UTC()
		out = append(out, shelf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recording shelves: %w", err)
	}
	return ListRecordingShelves200JSONResponse(out), nil
}

// recordingsFilterFromShelvesParams は棚の絞り込みを、録画一覧と同じ検証
// （recordingsFilterFromParams）に通す。棚が受けるのは録画一覧のパラメータの
// 部分集合なので、生成型を詰め替えるだけにして検証を 2 箇所に書かない。
// 棚に絞り込みを足したら、ここにも写す（写し忘れは黙って無視される。今ある
// 条件は TestListRecordingShelves_FiltersMatchListRecordings が検査する）。
func recordingsFilterFromShelvesParams(p ListRecordingShelvesParams) (recordingsFilter, string) {
	lp := ListRecordingsParams{
		Q:       p.Q,
		QTarget: (*ListRecordingsParamsQTarget)(p.QTarget),
		Genre:   p.Genre,
		Site:    p.Site,
		Service: p.Service,
		Status:  (*ListRecordingsParamsStatus)(p.Status),
		Source:  (*ListRecordingsParamsSource)(p.Source),
		RuleId:  p.RuleId,
		From:    p.From,
		To:      p.To,
	}
	if p.ChannelType != nil {
		cts := make([]ListRecordingsParamsChannelType, len(*p.ChannelType))
		for i, ct := range *p.ChannelType {
			cts[i] = ListRecordingsParamsChannelType(ct)
		}
		lp.ChannelType = &cts
	}
	return recordingsFilterFromParams(lp)
}

// buildRecordingShelvesQuery は棚の集計を組む。棚 1 件 = 実効シリーズの値 1 つ。
// 母集団は生きている録画（`deleted_at IS NULL AND superseded_at IS NULL`）で、
// 録画中・取り込み待ち・失敗も含む。絞り込み（recordingsFilterWhere。録画一覧と
// 共有）はこの母集団の録画 1 件ずつに、グループ化の前に当てる。
//
// 代表は program_start_at の新しい順で先頭の 1 件。value は画面のシリーズ名に使い、
// title は代表録画の生タイトルを補助表示する。キーが過剰併合を隠さないよう、両方返す。
//
// 値が NULL の棚も返す。棚一覧の UI は NULL を表示対象から外すが、API では
// 欠落と「分類されていない」を区別できるように残す。
//
// 値が NULL の棚の行は `GROUP BY value` が 1 つのグループにまとめる（SQL の
// GROUP BY は NULL を等しいものとして扱う）。
//
// 未視聴件数もこの集計で返す。放送イベントは生きている再生可能な（かつ絞り込んだ）
// 録画から束ね、視聴済み印はごみ箱・supersede 済み・別 site を含む全録画から読む。
// **絞り込みを watched_events に当ててはならない。** 当てると、別 site の録画で
// 見た回が `site` で絞ったときに未視聴へ戻る
// （TestListRecordingShelves_FilterKeepsWatchedMarksFromOutsideTheFilter）。
//
// **sqlc の静的クエリ（`sqlc.narg` で全条件を受ける形）にしない。** 計測では速さに
// 差が無かった（下記の (f_static) / (q_static)。prepared statement が汎用プランに
// 切り替わりうる 7 回目以降の中央値で比べた）。決め手は共有である。静的な形では
// 録画一覧（buildRecordingsQuery）と WHERE の組み立てを共有できず、条件を直すと
// 片方だけ直る。`service` の行値 IN も静的な形に載らない。録画一覧と同じく、条件が
// あるときだけ節を足し、QueryExecModeExec で毎回計画させる（queryRecordings のコメント）。
//
// **この形はプランの形に依存する。** 旧母集団（再生できる録画だけ。73,000 行がすべて
// 再生可能）での過去の実測（別の環境、sqlc / pgx の prepared statement 経由）:
//
//   - その形: 141 ms
//   - 代表と件数を別々の CTE に割る: 231 ms（playable をもう 1 度走査する）
//   - playable（recordings × playable_assets × recording_series の CTE）を
//     MATERIALIZED にしない: 617 ms
//
// 617 ms の仕組みは、MATERIALIZED を外すと部分一意索引 recordings_unique_active_event
// が選ばれ、その行数見積もりが 1 になって下流が全部 1 行の計画になり、代表を求める
// ソートが外側の行数ぶん繰り返されること、だった。
//
// 下の live は旧 playable に当たる（recordings を走査する CTE）が、MATERIALIZED にしない。
// 現スキーマ・合成 seed（下記）では 617 ms は再現せず、live を MATERIALIZED にした形は
// 本番形の 1.04〜1.13 倍遅い（3 回）。617 ms の再現条件は未検証なので、再発したら
// EXPLAIN で計画を調べる。絞り込みの WHERE は live の中に置く（集計の前に母集団を削る）。
//
// playable_assets は参照が 1 回なので MATERIALIZED にしない。MATERIALIZED にした形は
// 本番形の 1.02〜1.07 倍遅く（3 回）、結果は一致した。
//
// 実効シリーズは recording_series ビューが唯一の定義で、ここでも JOIN で読む
// （COALESCE(lr.value_key, r.series_key) を書き下すと定義が 2 箇所になる）。
// ビュー経由は書き下しより約 8% 遅かった（旧母集団の形、合成データ 73,000 行・141 棚・
// 分類ルール 50 本で約 223 ms 対 約 206 ms）。
//
// `internal/api/shelves_bench_test.go` は `ROKUBAN_BENCH_DATABASE_URL` がなければ
// スキップし、専用 DB で各形を交互に回して計測する。
// seed は生きている録画 71,000 行を含む全 73,000 行、141 棚、分類ルール 50 本で、
// 放送イベントを複数拠点の録画で作り、視聴済み印と再生状態を混ぜる。
// 各形を 12 ラウンド交互に回し、7 回目以降の中央値を 3 回測った（Apple M3 Max・
// PostgreSQL 16.2）。同じ回の旧母集団・実効シリーズ書き下し形（(o_inline)、229.2〜236.6 ms）
// との比で読む。予算は 141 ms の環境で決めた比 200/141 ≈ 1.42 倍である。
//
//   - 絞り込みなし: 309.5〜318.6 ms（1.31〜1.39 倍）
//   - アニメ × 17 日間（47 棚）: 58.1〜59.0 ms（0.25 倍）。静的な形は 56.3〜59.4 ms
//   - キーワード（1 棚）: 17.8〜19.7 ms（0.08 倍）。静的な形は 16.7〜18.6 ms
//
// 実データでの絶対値は未測定である。本番の playable_count は旧形の recording_count と
// 全棚で一致し、絞り込みありの形は Go で別に集計した期待値と一致する（ハーネスが検査する）。
func buildRecordingShelvesQuery(f recordingsFilter) (string, []any) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var where strings.Builder
	and := func(clause string) {
		where.WriteString("\n      AND ")
		where.WriteString(clause)
	}
	recordingsFilterWhere(f, and, arg)

	return `
WITH playable_assets AS (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
watched_events AS MATERIALIZED (
    -- 印を束ねる側は live / playable / 絞り込みを当てない。ごみ箱・supersede 済みの印も読む。
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_watched w ON w.recording_id = r.id
),
live AS (
    SELECT r.id,
           r.title,
           r.program_start_at,
           r.network_id,
           r.service_id,
           rs.value,
           pa.recording_id AS playable_recording_id,
           we.network_id AS watched_network_id
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    LEFT JOIN watched_events we
      ON we.network_id = r.network_id
     AND we.service_id = r.service_id
     AND we.program_start_at = r.program_start_at
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL` + where.String() + `
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) AS recording_count,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       (count(DISTINCT (l.network_id, l.service_id, l.program_start_at))
           FILTER (WHERE l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL))::bigint AS unwatched_count,
       max(l.program_start_at)::timestamptz AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST`, args
}

type labelRuleInputValues struct {
	key      string
	value    string
	keyword  string
	priority int32
}

func labelRuleInput(in LabelRuleInput) (labelRuleInputValues, error) {
	key := string(LabelRuleInputKeySeries)
	if in.Key != nil {
		if !in.Key.Valid() {
			return labelRuleInputValues{}, fmt.Errorf("invalid key %q (want series)", *in.Key)
		}
		key = string(*in.Key)
	}
	// DB の CHECK (btrim(keyword) <> '') と同じ判定。btrim は ASCII の空白（U+0020）
	// だけを落とすので、strings.TrimSpace ではなく Trim(" ") で揃える。
	if strings.Trim(in.Keyword, " ") == "" {
		return labelRuleInputValues{}, fmt.Errorf("keyword must not be empty")
	}
	priority := int32(defaultLabelRulePriority)
	if in.Priority != nil {
		priority = *in.Priority
	}
	return labelRuleInputValues{key: key, value: in.Value, keyword: in.Keyword, priority: priority}, nil
}

// labelRuleValueMessage は値が棚のキーとして意味を持つかを検査し、400 の本文を返す
// （空文字なら妥当）。
//
// **DB の CHECK (value_key IS NOT NULL) と同じ判定を先に走らせる。** CHECK は
// 制約違反（500）になるので、利用者の入力ミスを 400 として返すにはここで
// 見る必要がある。CHECK はそのまま残す（表現不可能にしておく方が強い。不変条件 10）。
func (h *Server) labelRuleValueMessage(ctx context.Context, value string) (string, error) {
	var key *string
	if err := h.pool.QueryRow(ctx, "SELECT public.series_key($1)", value).Scan(&key); err != nil {
		return "", fmt.Errorf("normalizing label rule value %q: %w", value, err)
	}
	if key == nil {
		return "value must contain a series key (it normalizes to an empty shelf key)", nil
	}
	return "", nil
}

func (h *Server) insertLabelRule(ctx context.Context, in labelRuleInputValues) (sqlcgen.LabelRule, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return sqlcgen.LabelRule{}, fmt.Errorf("beginning label rule creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	row, err := q.CreateLabelRule(ctx, sqlcgen.CreateLabelRuleParams{
		Key: in.key, Value: in.value, Keyword: in.keyword, Priority: in.priority,
	})
	if err != nil {
		return sqlcgen.LabelRule{}, fmt.Errorf("creating label rule: %w", err)
	}
	if err := insertLabelRuleReconcile(ctx, tx, h.river); err != nil {
		return sqlcgen.LabelRule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlcgen.LabelRule{}, fmt.Errorf("committing label rule creation: %w", err)
	}
	return row, nil
}

func insertLabelRuleReconcile(ctx context.Context, tx pgx.Tx, riverClient *river.Client[pgx.Tx]) error {
	if riverClient == nil {
		return nil
	}
	if _, err := riverClient.InsertTx(ctx, tx, jobs.LabelRuleReconcileArgs{}, nil); err != nil {
		return fmt.Errorf("inserting label rule re-evaluation: %w", err)
	}
	return nil
}

func labelRuleFromRow(row sqlcgen.LabelRule) LabelRule {
	priority := row.Priority
	// value_key は CHECK (value_key IS NOT NULL) で非 null（生成列なので sqlc は
	// ポインタにする）。
	valueKey := ""
	if row.ValueKey != nil {
		valueKey = *row.ValueKey
	}
	return LabelRule{
		Id: row.ID, Key: LabelRuleKey(row.Key), Value: row.Value, ValueKey: valueKey, Keyword: row.Keyword,
		Priority: &priority, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}
