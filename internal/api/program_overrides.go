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
// および DeleteProgramOverrides が消す対象）。
//
// skip はここに含めない。overrides のキーではなく program_intents.action が
// 担うフィールドで、PATCH では扱わない（取消は
// PUT .../intent {action: skip}。docs/recording.md §4.2「overrides API の形」）。
//
// フィールド名 → db.ReservationOptions の構造体フィールドの対応は、この変数と
// resetOverridesField（適用側）の 2 箇所だけに集約してある。新しいフィールドを
// 足すときはこの 2 箇所を揃えて更新すればよい。
var overridesFields = []ProgramOverridesInputReset{
	Priority, ContentPath, FilenameTemplate, KeepOriginal, EncodeProfiles,
}

func isKnownOverridesField(f ProgramOverridesInputReset) bool {
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
func resetOverridesField(opts *db.ReservationOptions, f ProgramOverridesInputReset) {
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

// PatchProgramOverrides は override をフィールド単位で部分更新する
// (PATCH /api/sites/{site}/programs/{programId}/overrides)。値を書いたフィールドは
// override を設定し、`reset` に名前を挙げたフィールドは override を削除する
// （フィールド単位の「ルールに戻す」）。どちらにも現れないフィールドは変更しない
// （docs/recording.md §4.2「overrides API の形」）。
//
// program_overrides だけを書く。program_intents（action）には一切触れない —
// これが分離の核心で、ルール由来の予約に PATCH しても手動予約の意図（record/skip）
// を巻き込む経路が構造的に存在しない（docs/recording.md §4.2「overrides は
// program_intents とは別の表に置く」）。
//
// (site, programId) を自身のキーとして書く（issue #29）。導出行（reservations）の
// 有無には依存しない。
func (h *Server) PatchProgramOverrides(ctx context.Context, req PatchProgramOverridesRequestObject) (PatchProgramOverridesResponseObject, error) {
	if !h.knownSite(req.Site) {
		return PatchProgramOverrides400JSONResponse{Error: "unknown site"}, nil
	}
	if req.Body == nil {
		return PatchProgramOverrides400JSONResponse{Error: "request body is required"}, nil
	}
	setters, resetFields, err := parseProgramOverridesInput(*req.Body)
	if err != nil {
		return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil
	}
	// 未知プロファイル名は overrides でも拒否する（ルールと同じ規律。issue #64）。
	if req.Body.EncodeProfiles != nil {
		if err := h.validateEncodeProfiles(*req.Body.EncodeProfiles); err != nil {
			return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil
		}
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// program_snapshots の存在を確保しつつ、その UPSERT が同一行の更新ロックを
	// 保持する（自然な直列化。docs/recording.md §4.2「マージは Go 側で型付きに
	// 行う」— Rokuban は単一世帯用アプリで同時 PATCH は事実上起きないが、
	// 安く取れるので取る）。
	if err := ensureProgramSnapshot(ctx, q, req.Site, req.ProgramId); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PatchProgramOverrides400JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		return nil, fmt.Errorf("ensuring program snapshot: %w", err)
	}

	existing, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
	})
	var existingJSON json.RawMessage
	if err == nil {
		existingJSON = existing.Overrides
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("getting program overrides: %w", err)
	}

	merged, err := mergeOverridesPatch(existingJSON, setters, resetFields)
	if err != nil {
		return nil, err
	}

	// このパッチを適用した後の実効値（既存 override + このパッチ + ルールの base
	// をマージした結果）で keepOriginal/encodeProfiles の組み合わせを検証する
	// （issue #104）。リクエスト単体（例えば keepOriginal だけを送る、あるいは
	// encodeProfiles だけを reset する）では組み合わせが見えないため、リクエスト
	// 直後の値ではなく必ずマージ後の実効値を見る。base はルール由来予約が
	// あればそこから、予約がまだ無ければ nil（手動予約相当）として扱う。
	var baseJSON json.RawMessage
	if res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
	}); err == nil {
		baseJSON = res.Base
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("getting reservation base: %w", err)
	}

	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshalling merged overrides: %w", err)
	}
	eff, err := db.EffectiveOptions(baseJSON, mergedJSON, nil)
	if err != nil {
		return nil, fmt.Errorf("computing effective options for validation: %w", err)
	}
	if err := validateEffectiveKeepOriginal(eff); err != nil {
		return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil
	}

	if err := persistOverrides(ctx, q, req.Site, req.ProgramId, mergedJSON); err != nil {
		return nil, err
	}
	if err := h.insertRulerPassHint(ctx, tx, req.Site); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return PatchProgramOverrides204Response{}, nil
}

// validateEffectiveKeepOriginal は実効値（base + overrides をマージした結果）の
// keepOriginal/encodeProfiles の組み合わせを検証する。
//
// keepOriginal='until_encoded' なのに encodeProfiles が空（未指定 or 明示的な
// 空配列）だと、削除 reconcile の派生物完備判定（ListUntilEncodedOriginalsToDelete）
// が空配列で恒真になり、サムネイルが 1 枚あるだけで唯一のコピーである原本が
// 物理削除される（docs/storage.md §6「唯一のコピーを消すパスがない」への違反、
// issue #104）。rules（internal/api/rules.go の validateRuleInput）・
// スキーマ（00006_rules.sql の CHECK）と同じ規律を overrides にも揃える。
func validateEffectiveKeepOriginal(eff db.ReservationOptions) error {
	if eff.KeepOriginal == nil || *eff.KeepOriginal != db.KeepOriginalUntilEncoded {
		return nil
	}
	if eff.EncodeProfiles == nil || len(*eff.EncodeProfiles) == 0 {
		return fmt.Errorf(
			"effective keepOriginal is %q but effective encodeProfiles is empty "+
				"(after merging with existing overrides and the rule's base); "+
				"specify encodeProfiles or keep keepOriginal as %q",
			db.KeepOriginalUntilEncoded, db.KeepOriginalAlways)
	}
	return nil
}

// DeleteProgramOverrides は「ルールに戻す」
// (DELETE /api/sites/{site}/programs/{programId}/overrides)。program_overrides の
// 行を削除するだけで、program_intents（action）には一切触れない
// （docs/recording.md §4.2「overrides は program_intents とは別の表に置く」）。
// 行が無くても冪等に 204。
func (h *Server) DeleteProgramOverrides(ctx context.Context, req DeleteProgramOverridesRequestObject) (DeleteProgramOverridesResponseObject, error) {
	if !h.knownSite(req.Site) {
		return DeleteProgramOverrides400JSONResponse{Error: "unknown site"}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
	}); err != nil {
		return nil, fmt.Errorf("deleting program overrides: %w", err)
	}
	if err := h.insertRulerPassHint(ctx, tx, req.Site); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return DeleteProgramOverrides204Response{}, nil
}

// mergeOverridesPatch は既存の program_overrides.overrides（無ければゼロ値）に
// setters（値を指定したフィールド）を適用し、resetFields（reset 指定された
// フィールド）を nil に戻した結果を返す（副作用なし）。
//
// PatchProgramOverrides は書き込む前にこの結果を base とマージした実効値を
// 検証する必要がある（issue #104）ため、マージと永続化（persistOverrides）を
// 分離してある。
func mergeOverridesPatch(
	existing json.RawMessage,
	setters []func(*db.ReservationOptions),
	resetFields []ProgramOverridesInputReset,
) (db.ReservationOptions, error) {
	opts := db.ReservationOptions{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &opts); err != nil {
			return db.ReservationOptions{}, fmt.Errorf("unmarshalling existing overrides: %w", err)
		}
	}
	for _, set := range setters {
		set(&opts)
	}
	for _, f := range resetFields {
		resetOverridesField(&opts, f)
	}
	return opts, nil
}

// persistOverrides は mergeOverridesPatch の結果（marshal 済み）を書き戻す。
//
// 結果が空（{}）になったら program_overrides の行そのものを DELETE する
// （「空の上書き = 行が無い」。isEmptyOverridesJSON のコメント参照）。空でなければ
// upsert する。
func persistOverrides(
	ctx context.Context,
	q *sqlcgen.Queries,
	site string,
	programID int64,
	finalJSON json.RawMessage,
) error {
	if isEmptyOverridesJSON(finalJSON) {
		if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
			Site:      site,
			ProgramID: programID,
		}); err != nil {
			return fmt.Errorf("deleting emptied program overrides: %w", err)
		}
		return nil
	}

	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:      site,
		ProgramID: programID,
		Overrides: finalJSON,
	}); err != nil {
		return fmt.Errorf("upserting program overrides: %w", err)
	}
	return nil
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

// parseProgramOverridesInput は PATCH ボディを検証し、適用する setter 関数
// の列（値を指定したフィールド）と reset するフィールド名の列に変換する。
//
// 同じフィールドが値と reset の両方に現れたら 400（意図が不明なので推測しない）、
// reset に未知のフィールド名があっても 400（タイポを黙って無視しない）。
// filenameTemplate は internal/contentpath.Validate で検証する
// （internal/api/rules.go の validateRuleInput と同じ流儀）。keepOriginal は
// enum を検証する。
func parseProgramOverridesInput(in ProgramOverridesInput) ([]func(*db.ReservationOptions), []ProgramOverridesInputReset, error) {
	var setters []func(*db.ReservationOptions)
	setFields := make(map[ProgramOverridesInputReset]struct{}, len(overridesFields))

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
		// 空文字は「明示指定なし」に潰れて reconciler の差分対象から外れる
		// （explicitContentPath、internal/reconciler/reconciler.go）ため、保存は
		// 成功するが何も起きないケースになる。既存 schedule の有無は見ない —
		// api は射影の状態（schedule_sync）に依存しない（不変条件 1）。override を
		// 消すのは reset。
		if *in.ContentPath == "" {
			return nil, nil, fmt.Errorf(`contentPath must not be empty (use reset: ["contentPath"] to clear it)`)
		}
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

	var resetFields []ProgramOverridesInputReset
	if in.Reset != nil {
		seen := make(map[ProgramOverridesInputReset]struct{}, len(*in.Reset))
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
