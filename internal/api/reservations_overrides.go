package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// overridesFields は override として扱う全フィールド名（reset で許容する値、
// および DELETE /api/reservations/{id}/overrides が消す対象）。
//
// skip はここに含めない。overrides のキーではなく program_intents.action が
// 担うフィールドで、PATCH では扱わない（取消は DELETE /api/reservations/{id}。
// docs/recording.md §4.2「overrides API の形」）。
//
// フィールド名 → db.ReservationOptions の構造体フィールドの対応は、この変数と
// resetOverridesField（適用側）の 2 箇所だけに集約してある。新しいフィールドを
// 足すときはこの 2 箇所を揃えて更新すればよい。
var overridesFields = []ReservationOverridesInputReset{
	Priority, ContentPath, FilenameTemplate, KeepOriginal, EncodeProfiles,
}

func isKnownOverridesField(f ReservationOverridesInputReset) bool {
	for _, known := range overridesFields {
		if f == known {
			return true
		}
	}
	return false
}

// resetOverridesField は opts の該当フィールドを nil に戻す（override の削除）。
// フィールド名 → 構造体フィールドの対応はこの switch に集約する
// （overridesFields のコメント参照）。
func resetOverridesField(opts *db.ReservationOptions, f ReservationOverridesInputReset) {
	switch f {
	case Priority:
		opts.Priority = nil
	case ContentPath:
		opts.ContentPath = nil
	case FilenameTemplate:
		opts.FilenameTemplate = nil
	case KeepOriginal:
		opts.KeepOriginal = nil
	case EncodeProfiles:
		opts.EncodeProfiles = nil
	}
}

// UpdateReservationOverrides は override をフィールド単位で部分更新する
// (PATCH /api/reservations/{id})。値を書いたフィールドは override を設定し、
// `reset` に名前を挙げたフィールドは override を削除する（フィールド単位の
// 「ルールに戻す」）。どちらにも現れないフィールドは変更しない
// （docs/recording.md §4.2「overrides API の形」）。
//
// program_overrides だけを書く。program_intents（action）には一切触れない —
// これが分離の核心で、ルール由来の予約に PATCH しても手動予約の意図（record/skip）
// を巻き込む経路が構造的に存在しない（docs/recording.md §4.2「overrides は
// program_intents とは別の表に置く」）。
func (h *Server) UpdateReservationOverrides(ctx context.Context, req UpdateReservationOverridesRequestObject) (UpdateReservationOverridesResponseObject, error) {
	if req.Body == nil {
		return UpdateReservationOverrides400JSONResponse{Error: "request body is required"}, nil
	}
	setters, resetFields, err := parseReservationOverridesInput(*req.Body)
	if err != nil {
		return UpdateReservationOverrides400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	row, err := lockAndGetReservation(ctx, q, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UpdateReservationOverrides404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}

	finalOverrides, err := applyOverridesPatch(ctx, q, row.Reservation, row.Overrides, setters, resetFields)
	if err != nil {
		return nil, err
	}
	if err := h.insertReconcilePassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return UpdateReservationOverrides200JSONResponse(reservationFromRow(row.Reservation, finalOverrides)), nil
}

// ResetReservationOverrides は予約単位の「ルールに戻す」
// (DELETE /api/reservations/{id}/overrides)。program_overrides の行を DELETE
// するだけで、program_intents（action）には一切触れない
// （docs/recording.md §4.2「overrides は program_intents とは別の表に置く」）。
func (h *Server) ResetReservationOverrides(ctx context.Context, req ResetReservationOverridesRequestObject) (ResetReservationOverridesResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	row, err := lockAndGetReservation(ctx, q, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResetReservationOverrides404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}

	if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
		Site:      row.Reservation.Site,
		ProgramID: row.Reservation.ProgramID,
	}); err != nil {
		return nil, fmt.Errorf("deleting program overrides: %w", err)
	}
	if err := h.insertReconcilePassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return ResetReservationOverrides200JSONResponse(reservationFromRow(row.Reservation, nil)), nil
}

// lockAndGetReservation は予約行を FOR UPDATE でロックしてから GetReservationFull
// で全体（base / program_intents.action / program_overrides.overrides）を読む。
// LEFT JOIN 付きのクエリに直接 FOR UPDATE を書くと「どのテーブルをロックするか」
// の指定（FOR UPDATE OF）が要って煩雑なため、単一テーブルの FOR UPDATE で
// 直列化してから改めて full row を読む 2 段構成にしてある
// （docs/recording.md §4.2「マージは Go 側で型付きに行う」。同時 PATCH は
// Rokuban が単一世帯用アプリで構造的に起きないので、1 行ロックで足りる）。
func lockAndGetReservation(ctx context.Context, q *sqlcgen.Queries, id int64) (sqlcgen.GetReservationFullRow, error) {
	if _, err := q.LockReservation(ctx, id); err != nil {
		return sqlcgen.GetReservationFullRow{}, err
	}
	return q.GetReservationFull(ctx, id)
}

// applyOverridesPatch は既存の program_overrides.overrides（無ければゼロ値）に
// setters（値を指定したフィールド）を適用し、resetFields（reset 指定された
// フィールド）を nil に戻したうえで書き戻す。
//
// 結果が空（{}）になったら program_overrides の行そのものを DELETE する
// （「空の上書き = 行が無い」。isEmptyOverridesJSON のコメント参照）。空でなければ
// upsert する。PATCH（フィールド単位の reset）と ResetReservationOverrides
// （予約単位の全消去、resetFields に全フィールドを渡す形で同じ経路を通せるが、
// ResetReservationOverrides は DELETE の方が直接的なのでそちらを使う）の
// どちらからも呼べる共通の適用ロジック。
func applyOverridesPatch(
	ctx context.Context,
	q *sqlcgen.Queries,
	r sqlcgen.Reservation,
	existing json.RawMessage,
	setters []func(*db.ReservationOptions),
	resetFields []ReservationOverridesInputReset,
) (json.RawMessage, error) {
	opts := db.ReservationOptions{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &opts); err != nil {
			return nil, fmt.Errorf("unmarshalling existing overrides: %w", err)
		}
	}
	for _, set := range setters {
		set(&opts)
	}
	for _, f := range resetFields {
		resetOverridesField(&opts, f)
	}

	finalJSON, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("marshalling overrides: %w", err)
	}

	if isEmptyOverridesJSON(finalJSON) {
		if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
			Site:      r.Site,
			ProgramID: r.ProgramID,
		}); err != nil {
			return nil, fmt.Errorf("deleting emptied program overrides: %w", err)
		}
		return nil, nil
	}

	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:              r.Site,
		ProgramID:         r.ProgramID,
		Overrides:         finalJSON,
		ProgramStartAt:    r.ProgramStartAt,
		ProgramDurationMs: r.ProgramDurationMs,
	}); err != nil {
		return nil, fmt.Errorf("upserting program overrides: %w", err)
	}
	return finalJSON, nil
}

// isEmptyOverridesJSON は jsonb の overrides が実質空（{}）かどうかを判定する。
// db.ReservationOptions の全フィールドが omitempty なので、json.Marshal の結果が
// {} であることと「型付き構造体に何も設定されていない」は同値になる
// （新しいフィールドを足しても自動的に正しく動く）。
func isEmptyOverridesJSON(raw json.RawMessage) bool {
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	return len(m) == 0
}

// parseReservationOverridesInput は PATCH ボディを検証し、適用する setter 関数
// の列（値を指定したフィールド）と reset するフィールド名の列に変換する。
//
// 同じフィールドが値と reset の両方に現れたら 400（意図が不明なので推測しない）、
// reset に未知のフィールド名があっても 400（タイポを黙って無視しない）。
// filenameTemplate は internal/contentpath.Validate で検証する
// （internal/api/rules.go の validateRuleInput と同じ流儀）。keepOriginal は
// enum を検証する。
func parseReservationOverridesInput(in ReservationOverridesInput) ([]func(*db.ReservationOptions), []ReservationOverridesInputReset, error) {
	var setters []func(*db.ReservationOptions)
	setFields := make(map[ReservationOverridesInputReset]struct{}, len(overridesFields))

	if in.Priority != nil {
		// mirakc の schedule に渡す調停優先度。負の優先度は意味を持たない。
		if *in.Priority < 0 {
			return nil, nil, fmt.Errorf("invalid priority %d (must be >= 0)", *in.Priority)
		}
		p := *in.Priority
		setters = append(setters, func(o *db.ReservationOptions) { o.Priority = &p })
		setFields[Priority] = struct{}{}
	}
	if in.ContentPath != nil {
		v := *in.ContentPath
		setters = append(setters, func(o *db.ReservationOptions) { o.ContentPath = &v })
		setFields[ContentPath] = struct{}{}
	}
	if in.FilenameTemplate != nil {
		if err := contentpath.Validate(*in.FilenameTemplate); err != nil {
			return nil, nil, fmt.Errorf("invalid filenameTemplate %q (Go text/template; see docs/recording.md §3.2): %w",
				*in.FilenameTemplate, err)
		}
		v := *in.FilenameTemplate
		setters = append(setters, func(o *db.ReservationOptions) { o.FilenameTemplate = &v })
		setFields[FilenameTemplate] = struct{}{}
	}
	if in.KeepOriginal != nil {
		v := string(*in.KeepOriginal)
		if v != db.KeepOriginalAlways && v != db.KeepOriginalUntilEncoded {
			return nil, nil, fmt.Errorf("invalid keepOriginal %q (must be %q or %q)",
				v, db.KeepOriginalAlways, db.KeepOriginalUntilEncoded)
		}
		setters = append(setters, func(o *db.ReservationOptions) { o.KeepOriginal = &v })
		setFields[KeepOriginal] = struct{}{}
	}
	if in.EncodeProfiles != nil {
		v := *in.EncodeProfiles
		setters = append(setters, func(o *db.ReservationOptions) { o.EncodeProfiles = &v })
		setFields[EncodeProfiles] = struct{}{}
	}

	var resetFields []ReservationOverridesInputReset
	if in.Reset != nil {
		seen := make(map[ReservationOverridesInputReset]struct{}, len(*in.Reset))
		for _, f := range *in.Reset {
			if !isKnownOverridesField(f) {
				return nil, nil, fmt.Errorf("unknown reset field %q", f)
			}
			if _, ok := setFields[f]; ok {
				return nil, nil, fmt.Errorf("field %q specified both as a value and in reset", f)
			}
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			resetFields = append(resetFields, f)
		}
	}
	return setters, resetFields, nil
}
