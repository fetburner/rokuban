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
// 件数と未視聴の放送イベント数を同じ集計から返す。
func (h *Server) ListRecordingShelves(ctx context.Context, req ListRecordingShelvesRequestObject) (ListRecordingShelvesResponseObject, error) {
	if req.Params.Key != nil && !req.Params.Key.Valid() {
		return ListRecordingShelves400JSONResponse{Error: fmt.Sprintf("invalid key %q (want series)", *req.Params.Key)}, nil
	}
	rows, err := sqlcgen.New(h.pool).ListRecordingShelves(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recording shelves: %w", err)
	}
	out := make([]RecordingShelf, 0, len(rows))
	for _, row := range rows {
		out = append(out, RecordingShelf{
			Value:            row.Value,
			Title:            row.Title,
			Count:            int(row.RecordingCount),
			PlayableCount:    int(row.PlayableCount),
			UnwatchedCount:   int(row.UnwatchedCount),
			LatestStartAt:    row.LatestStartAt.UTC(),
			RepresentativeId: row.RepresentativeID,
		})
	}
	return ListRecordingShelves200JSONResponse(out), nil
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
