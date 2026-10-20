package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

const defaultLabelRulePriority = 0

// ListLabelRules はシリーズ分類ルールを勝者順に返す。
func (h *Server) ListLabelRules(ctx context.Context, _ ListLabelRulesRequestObject) (ListLabelRulesResponseObject, error) {
	rows, err := h.labelRules.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing label rules: %w", err)
	}
	out := make([]LabelRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, labelRuleFromStore(row))
	}
	return ListLabelRules200JSONResponse(out), nil
}

// GetLabelRule はシリーズ分類ルールを 1 件返す。
func (h *Server) GetLabelRule(ctx context.Context, req GetLabelRuleRequestObject) (GetLabelRuleResponseObject, error) {
	row, err := h.labelRules.Get(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return GetLabelRule404JSONResponse{Error: "label rule not found"}, nil
		}
		return nil, fmt.Errorf("getting label rule %d: %w", req.Id, err)
	}
	return GetLabelRule200JSONResponse(labelRuleFromStore(row)), nil
}

// CreateLabelRule はシリーズ分類ルールを作成し、commit 後に録画全件再評価を依頼する。
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
	row, err := h.labelRules.Create(ctx, input.key, input.value, input.keyword, input.priority)
	if err != nil {
		return nil, err
	}
	h.enqueueLabelRuleReconcile(ctx)
	return CreateLabelRule201JSONResponse(labelRuleFromStore(row)), nil
}

// UpdateLabelRule はシリーズ分類ルールを上書きし、commit 後に録画全件再評価を依頼する。
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
	row, err := h.labelRules.Update(ctx, req.Id, input.value, input.keyword, input.priority)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UpdateLabelRule404JSONResponse{Error: "label rule not found"}, nil
		}
		return nil, fmt.Errorf("updating label rule %d: %w", req.Id, err)
	}
	h.enqueueLabelRuleReconcile(ctx)
	return UpdateLabelRule200JSONResponse(labelRuleFromStore(row)), nil
}

// DeleteLabelRule はシリーズ分類ルールを削除し、commit 後に録画全件再評価を依頼する。
func (h *Server) DeleteLabelRule(ctx context.Context, req DeleteLabelRuleRequestObject) (DeleteLabelRuleResponseObject, error) {
	err := h.labelRules.Delete(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeleteLabelRule404JSONResponse{Error: "label rule not found"}, nil
		}
		return nil, fmt.Errorf("deleting label rule %d: %w", req.Id, err)
	}
	h.enqueueLabelRuleReconcile(ctx)
	return DeleteLabelRule204Response{}, nil
}

// GetLabelRuleValueKey は入力中の値が棚のキーとして何になるかを返す。
// 正規化は SQL 関数 series_key の 1 箇所にあり、UI に複製させない。
func (h *Server) GetLabelRuleValueKey(ctx context.Context, req GetLabelRuleValueKeyRequestObject) (GetLabelRuleValueKeyResponseObject, error) {
	key, err := h.labelRules.SeriesKey(ctx, req.Params.Value)
	if err != nil {
		return nil, fmt.Errorf("normalizing label rule value %q: %w", req.Params.Value, err)
	}
	if key == nil {
		return GetLabelRuleValueKey200JSONResponse{}, nil
	}
	return GetLabelRuleValueKey200JSONResponse{ValueKey: *key}, nil
}

// ListRecordingShelves は生きている録画を実効シリーズごとに集計し、再生可能な
// 件数と未視聴の放送イベント数を同じ集計から返す。絞り込みは録画一覧と同じ
// 条件をグループ化の前に当てる（RecordingQuery.Shelves）。
func (h *Server) ListRecordingShelves(ctx context.Context, req ListRecordingShelvesRequestObject) (ListRecordingShelvesResponseObject, error) {
	if req.Params.Key != nil && !req.Params.Key.Valid() {
		return ListRecordingShelves400JSONResponse{Error: fmt.Sprintf("invalid key %q (want series)", *req.Params.Key)}, nil
	}
	f, errMsg := recordingsFilterFromShelvesParams(req.Params)
	if errMsg != "" {
		return ListRecordingShelves400JSONResponse{Error: errMsg}, nil
	}
	if h.recordingQuery == nil {
		return nil, fmt.Errorf("recording query is required")
	}
	rows, err := h.recordingQuery.Shelves(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("listing recording shelves: %w", err)
	}
	out := make([]RecordingShelf, 0, len(rows))
	for _, row := range rows {
		out = append(out, RecordingShelf{
			Value:            row.Value,
			Title:            row.Title,
			Count:            int(row.Count),
			PlayableCount:    int(row.PlayableCount),
			UnwatchedCount:   int(row.UnwatchedCount),
			LatestStartAt:    row.LatestStartAt.UTC(),
			RepresentativeId: row.RepresentativeID,
		})
	}
	return ListRecordingShelves200JSONResponse(out), nil
}

// recordingsFilterFromShelvesParams は棚の絞り込みを、録画一覧と同じ検証
// （recordingsFilterFromParams）に通す。棚が受けるのは録画一覧のパラメータの
// 部分集合なので、生成型を詰め替えるだけにして検証を 2 箇所に書かない。
// 棚に絞り込みを足したら、ここにも写す（写し忘れは黙って無視される。今ある
// 条件は TestListRecordingShelves_FiltersMatchListRecordings が検査する）。
func recordingsFilterFromShelvesParams(p ListRecordingShelvesParams) (store.RecordingFilter, string) {
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
	key, err := h.labelRules.SeriesKey(ctx, value)
	if err != nil {
		return "", fmt.Errorf("normalizing label rule value %q: %w", value, err)
	}
	if key == nil {
		return "value must contain a series key (it normalizes to an empty shelf key)", nil
	}
	return "", nil
}

func (h *Server) enqueueLabelRuleReconcile(ctx context.Context) {
	if h.enqueuer == nil {
		return
	}
	if _, err := h.enqueuer.Enqueue(ctx, jobs.LabelRuleReconcileArgs{}); err != nil {
		slog.ErrorContext(ctx, "api: failed to enqueue label rule re-evaluation hint", "err", err)
	}
}

func labelRuleFromStore(row store.LabelRule) LabelRule {
	priority := row.Priority
	return LabelRule{
		Id: row.ID, Key: LabelRuleKey(row.Key), Value: row.Value, ValueKey: row.ValueKey, Keyword: row.Keyword,
		Priority: &priority, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}
