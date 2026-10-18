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
// skip は program_intents.action が担うため含めない。
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
// override を設定し、`reset` に名前を挙げたフィールドは override を削除する。
func (h *Server) PatchProgramOverrides(ctx context.Context, req PatchProgramOverridesRequestObject) (PatchProgramOverridesResponseObject, error) {
	if !h.knownSite(req.Site) {
		return PatchProgramOverrides400JSONResponse{Error: "unknown site"}, nil
	}
	if req.Body == nil {
		return PatchProgramOverrides400JSONResponse{Error: "request body is required"}, nil
	}
	patch, err := parseProgramOverridesInput(*req.Body)
	if err != nil {
		return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // Parse failures are returned as the API's typed 400 response.
	}
	if req.Body.EncodeProfiles != nil {
		if err := h.validateEncodeProfiles(*req.Body.EncodeProfiles); err != nil {
			return PatchProgramOverrides400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // Validation failures are returned as the API's typed 400 response.
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

// DeleteProgramOverrides は「ルールに戻す」操作で、意図には触れない。行がなくても冪等に 204。
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

// parseProgramOverridesInput は PATCH ボディを検証し、store の型付きパッチに変換する。
// 同じフィールドを値と reset の両方に指定した場合や未知の reset は 400 にする。
func parseProgramOverridesInput(in ProgramOverridesInput) (store.OverridesPatch, error) {
	var patch store.OverridesPatch
	setFields := make(map[ProgramOverridesInputReset]struct{}, len(overridesFields))
	if in.Priority != nil {
		if *in.Priority < 0 {
			return store.OverridesPatch{}, fmt.Errorf("invalid priority %d (must be >= 0)", *in.Priority)
		}
		patch.Priority = in.Priority
		setFields[Priority] = struct{}{}
	}
	if in.ContentPath != nil {
		if *in.ContentPath == "" {
			return store.OverridesPatch{}, fmt.Errorf(`contentPath must not be empty (use reset: ["contentPath"] to clear it)`)
		}
		patch.ContentPath = in.ContentPath
		setFields[ContentPath] = struct{}{}
	}
	if in.FilenameTemplate != nil {
		if err := contentpath.Validate(*in.FilenameTemplate); err != nil {
			return store.OverridesPatch{}, fmt.Errorf("invalid filenameTemplate %q (Go text/template; see docs/recording.md §3.2): %w",
				*in.FilenameTemplate, err)
		}
		patch.FilenameTemplate = in.FilenameTemplate
		setFields[FilenameTemplate] = struct{}{}
	}
	if in.KeepOriginal != nil {
		value := string(*in.KeepOriginal)
		if value != reservation.KeepOriginalAlways && value != reservation.KeepOriginalUntilEncoded {
			return store.OverridesPatch{}, fmt.Errorf("invalid keepOriginal %q (must be %q or %q)",
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
				return store.OverridesPatch{}, fmt.Errorf("unknown reset field %q", field)
			}
			if _, ok := setFields[field]; ok {
				return store.OverridesPatch{}, fmt.Errorf("field %q specified both as a value and in reset", field)
			}
			if _, duplicate := seen[field]; duplicate {
				continue
			}
			seen[field] = struct{}{}
			patch.Reset = append(patch.Reset, store.OverrideField(field))
		}
	}
	return patch, nil
}
