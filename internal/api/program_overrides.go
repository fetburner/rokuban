package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// overridesFields は override として扱う全フィールド名（reset で許容する値）。
// OpenAPI の生成型なので API 側に残してある。reservation.OverrideFields と一致する
// （食い違えば TestOverridesFieldsMatchReservation が落ちる）。
//
// skip はここに含めない。overrides のキーではなく program_intents.action が担うフィールドで、
// PATCH では扱わない（取消は PUT .../intent {action: skip}。
// docs/recording.md §4.2「overrides API の形」）。
//
// フィールド名 → reservation.Options の構造体フィールドの対応は reservation.MergeOverrides が持つ。
// 新しいフィールドを足すときの手順はそちらのコメントにある。
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

// PatchProgramOverrides は override をフィールド単位で部分更新する
// (PATCH /api/sites/{site}/programs/{programId}/overrides)。値を書いたフィールドは
// override を設定し、`reset` に名前を挙げたフィールドは override を削除する
// （フィールド単位の「ルールに戻す」）。どちらにも現れないフィールドは変更しない
// （docs/recording.md §4.2「overrides API の形」）。
//
// program_overrides だけを書き、program_intents（action）には触れない。ルール由来の予約に
// PATCH しても手動予約の意図（record/skip）を巻き込む経路が構造的に存在しない
// （docs/recording.md §4.2「overrides は program_intents とは別の表に置く」）。
// (site, programId) を自身のキーとして書き（issue #29）、導出行（reservations）の有無には依存しない。
// マージと実効値の検証は store が、直列化した後の既存の上書きに対して行う。
func (h *Server) PatchProgramOverrides(ctx context.Context, req PatchProgramOverridesRequestObject) (PatchProgramOverridesResponseObject, error) {
	if !h.knownSite(req.Site) {
		return PatchProgramOverrides400JSONResponse{Error: "unknown site"}, nil
	}
	if req.Body == nil {
		return PatchProgramOverrides400JSONResponse{Error: "request body is required"}, nil
	}
	patch, err := parseProgramOverridesInput(*req.Body)
	if err != nil {
		return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // 解析の失敗は API の型付き 400 応答として返す。
	}
	// 未知プロファイル名は overrides でも拒否する（ルールと同じ規律。issue #64）。
	if req.Body.EncodeProfiles != nil {
		if err := h.validateEncodeProfiles(*req.Body.EncodeProfiles); err != nil {
			return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // 検証の失敗は API の型付き 400 応答として返す。
		}
	}
	if err := h.reservationStore.PatchOverrides(ctx, req.Site, req.ProgramId, patch); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PatchProgramOverrides400JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		var invalid *reservation.InvalidEffectiveOptionsError
		if errors.As(err, &invalid) {
			return PatchProgramOverrides400JSONResponse{Error: invalid.Error()}, nil
		}
		return nil, fmt.Errorf("patching program overrides: %w", err)
	}
	h.enqueueRulerPassHint(ctx, req.Site)
	return PatchProgramOverrides204Response{}, nil
}

// DeleteProgramOverrides は「ルールに戻す」
// (DELETE /api/sites/{site}/programs/{programId}/overrides)。program_overrides の行を消すだけで、
// program_intents（action）には触れない（docs/recording.md §4.2「overrides は program_intents とは
// 別の表に置く」）。行が無くても冪等に 204。
func (h *Server) DeleteProgramOverrides(ctx context.Context, req DeleteProgramOverridesRequestObject) (DeleteProgramOverridesResponseObject, error) {
	if !h.knownSite(req.Site) {
		return DeleteProgramOverrides400JSONResponse{Error: "unknown site"}, nil
	}
	if err := h.reservationStore.DeleteOverrides(ctx, req.Site, req.ProgramId); err != nil {
		return nil, fmt.Errorf("deleting program overrides: %w", err)
	}
	h.enqueueRulerPassHint(ctx, req.Site)
	return DeleteProgramOverrides204Response{}, nil
}

// parseProgramOverridesInput は PATCH ボディを検証し、型付きのパッチに変換する。
//
// 同じフィールドが値と reset の両方に現れたら 400（意図が不明なので推測しない）、reset に未知の
// フィールド名があっても 400（タイポを黙って無視しない）。filenameTemplate は
// internal/contentpath.Validate で検証する（internal/api/rules.go の validateRuleInput と同じ流儀）。
// keepOriginal は enum を検証する。
func parseProgramOverridesInput(in ProgramOverridesInput) (reservation.OverridesPatch, error) {
	var patch reservation.OverridesPatch
	setFields := make(map[ProgramOverridesInputReset]struct{}, len(overridesFields))
	if in.Priority != nil {
		// mirakc の schedule に渡す調停優先度。負の優先度は意味を持たない。
		if *in.Priority < 0 {
			return reservation.OverridesPatch{}, fmt.Errorf("invalid priority %d (must be >= 0)", *in.Priority)
		}
		patch.Priority = in.Priority
		setFields[Priority] = struct{}{}
	}
	if in.ContentPath != nil {
		// 空文字は「明示指定なし」に潰れて reconciler の差分対象から外れる
		// （schedulesync.ExplicitContentPath）ため、保存は成功するが何も起きないケースになる。
		// 既存 schedule の有無は見ない --- api は射影の状態（schedule_sync）に依存しない（不変条件 1）。
		// override を消すのは reset。
		if *in.ContentPath == "" {
			return reservation.OverridesPatch{}, fmt.Errorf(`contentPath must not be empty (use reset: ["contentPath"] to clear it)`)
		}
		patch.ContentPath = in.ContentPath
		setFields[ContentPath] = struct{}{}
	}
	if in.FilenameTemplate != nil {
		if err := contentpath.Validate(*in.FilenameTemplate); err != nil {
			return reservation.OverridesPatch{}, fmt.Errorf("invalid filenameTemplate %q (Go text/template; see docs/recording.md §3.2): %w",
				*in.FilenameTemplate, err)
		}
		patch.FilenameTemplate = in.FilenameTemplate
		setFields[FilenameTemplate] = struct{}{}
	}
	if in.KeepOriginal != nil {
		value := string(*in.KeepOriginal)
		if value != reservation.KeepOriginalAlways && value != reservation.KeepOriginalUntilEncoded {
			return reservation.OverridesPatch{}, fmt.Errorf("invalid keepOriginal %q (must be %q or %q)",
				value, reservation.KeepOriginalAlways, reservation.KeepOriginalUntilEncoded)
		}
		patch.KeepOriginal = &value
		setFields[KeepOriginal] = struct{}{}
	}
	if in.EncodeProfiles != nil {
		profiles := *in.EncodeProfiles
		patch.EncodeProfiles = &profiles
		setFields[EncodeProfiles] = struct{}{}
	}

	if in.Reset != nil {
		seen := make(map[ProgramOverridesInputReset]struct{}, len(*in.Reset))
		for _, field := range *in.Reset {
			if !isKnownOverridesField(field) {
				return reservation.OverridesPatch{}, fmt.Errorf("unknown reset field %q", field)
			}
			if _, ok := setFields[field]; ok {
				return reservation.OverridesPatch{}, fmt.Errorf("field %q specified both as a value and in reset", field)
			}
			if _, duplicate := seen[field]; duplicate {
				continue
			}
			seen[field] = struct{}{}
			patch.Reset = append(patch.Reset, reservation.OverrideField(field))
		}
	}
	return patch, nil
}
