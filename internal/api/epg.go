package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/programid"
)

// maxProgramWindow は GET /api/programs で受け付ける時間窓の最大幅。
// EPG プロジェクションのローリングウィンドウ（8 日）に対応し、
// 1 リクエストで全期間を引かせないための上限。
const maxProgramWindow = 7 * 24 * time.Hour

// ListServices は EPG プロジェクションのサービス一覧を返す。
func (h *Server) ListServices(ctx context.Context, req ListServicesRequestObject) (ListServicesResponseObject, error) {
	if !h.knownSite(req.Site) {
		return ListServices404JSONResponse{Error: "unknown site"}, nil
	}
	rows, err := sqlcgen.New(h.pool).ListEpgServices(ctx, req.Site)
	if err != nil {
		return nil, err
	}

	result := make([]Service, 0, len(rows))
	for _, s := range rows {
		result = append(result, Service{
			Id:                 programid.ServiceID(int(s.NetworkID), int(s.ServiceID)),
			NetworkId:          int(s.NetworkID),
			ServiceId:          int(s.ServiceID),
			Name:               s.Name,
			ChannelType:        ServiceChannelType(s.ChannelType),
			Channel:            s.Channel,
			RemoteControlKeyId: int(s.RemoteControlKeyID),
			HasLogoData:        s.HasLogoData,
			HasPrograms:        s.HasPrograms,
		})
	}
	return ListServices200JSONResponse(result), nil
}

// ListPrograms は時間窓に一部でも重なる番組を返す。
func (h *Server) ListPrograms(ctx context.Context, req ListProgramsRequestObject) (ListProgramsResponseObject, error) {
	if !h.knownSite(req.Site) {
		return ListPrograms404JSONResponse{Error: "unknown site"}, nil
	}
	if msg := windowError(req.Params.Start, req.Params.End); msg != "" {
		return ListPrograms400JSONResponse{Error: msg}, nil
	}
	// `?service=` は Service.id。DB は network_id / service_id を別々に持つので
	// 分解してから述語に渡す（splitServiceIDs のコメント参照）。
	var exactNetworkIDs, exactServiceIDs []int32
	if req.Params.Service != nil {
		var msg string
		exactNetworkIDs, exactServiceIDs, msg = splitServiceIDs(*req.Params.Service)
		if msg != "" {
			return ListPrograms400JSONResponse{Error: msg}, nil
		}
	}

	rows, err := sqlcgen.New(h.pool).ListEpgProgramsForList(ctx, sqlcgen.ListEpgProgramsForListParams{
		Site:            req.Site,
		WindowStart:     req.Params.Start,
		WindowEnd:       req.Params.End,
		ExactNetworkIds: exactNetworkIDs,
		ExactServiceIds: exactServiceIDs,
	})
	if err != nil {
		return nil, err
	}

	result := make([]ProgramListItem, 0, len(rows))
	for _, p := range rows {
		result = append(result, ProgramListItem{
			ProgramId:   p.ProgramID,
			NetworkId:   int(p.NetworkID),
			ServiceId:   int(p.ServiceID),
			EventId:     int(p.EventID),
			RecordingId: p.RecordingID,
			StartAt:     p.StartAt,
			EndAt:       p.EndAt,
			DurationMs:  p.DurationMs,
			Name:        p.Name,
			Description: p.Description,
			Genres:      genreLv1List(p.GenreLv1),
			IsFree:      p.IsFree,
			Intent:      programIntentAction(p.IntentAction),
		})
	}
	return ListPrograms200JSONResponse(result), nil
}

// GetProgram は 1 番組を UI 完全形（extended / video / audios 込み）で返す。
func (h *Server) GetProgram(ctx context.Context, req GetProgramRequestObject) (GetProgramResponseObject, error) {
	if !h.knownSite(req.Site) {
		return GetProgram404JSONResponse{Error: "unknown site"}, nil
	}
	row, err := sqlcgen.New(h.pool).GetEpgProgram(ctx, sqlcgen.GetEpgProgramParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetProgram404JSONResponse{Error: "program not found"}, nil
		}
		return nil, err
	}

	epg := row.EpgProgram
	p := Program{
		ProgramId:   epg.ProgramID,
		NetworkId:   int(epg.NetworkID),
		ServiceId:   int(epg.ServiceID),
		EventId:     int(epg.EventID),
		StartAt:     epg.StartAt,
		EndAt:       epg.EndAt,
		DurationMs:  epg.DurationMs,
		Name:        epg.Name,
		Description: epg.Description,
		Genres:      genreLv1List(epg.GenreLv1),
		IsFree:      epg.IsFree,
		Intent:      programIntentAction(row.IntentAction),
	}
	// jsonb はそのまま構造体に載せ替える。プロジェクション時点で mirakc の
	// ペイロードをそのまま入れているので、ここでの変換は unmarshal だけ。
	if err := unmarshalIfPresent(epg.Extended, &p.Extended); err != nil {
		return nil, fmt.Errorf("decoding extended for program %d: %w", epg.ProgramID, err)
	}
	if err := unmarshalIfPresent(epg.Genres, &p.GenreDetails); err != nil {
		return nil, fmt.Errorf("decoding genres for program %d: %w", epg.ProgramID, err)
	}
	if err := unmarshalIfPresent(epg.Video, &p.Video); err != nil {
		return nil, fmt.Errorf("decoding video for program %d: %w", epg.ProgramID, err)
	}
	if err := unmarshalIfPresent(epg.Audios, &p.Audios); err != nil {
		return nil, fmt.Errorf("decoding audios for program %d: %w", epg.ProgramID, err)
	}
	return GetProgram200JSONResponse(p), nil
}

// programIntentAction は SQL の nullable な action を OpenAPI の省略可能な
// intent へ写像する。program_intents.action は DB の CHECK 制約で record/skip
// に限定されているので、ここでは文字列を契約型へ変換するだけでよい。
func programIntentAction(action *string) *ProgramIntent {
	if action == nil {
		return nil
	}
	value := ProgramIntent(*action)
	return &value
}

// windowError は時間窓が不正なら理由を返す。妥当なら空文字を返す。
// 無言で切り詰めるのではなく、広すぎる窓は明示的に拒否する。
// error ではなくメッセージを返すのは、400 が Go のエラーではなく正常なレスポンスだから。
func windowError(start, end time.Time) string {
	if !end.After(start) {
		return "end must be after start"
	}
	if end.Sub(start) > maxProgramWindow {
		return fmt.Sprintf("time window must not exceed %d days", int(maxProgramWindow.Hours()/24))
	}
	return ""
}

func genreLv1List(lv1 []int16) []int {
	out := make([]int, len(lv1))
	for i, g := range lv1 {
		out[i] = int(g)
	}
	return out
}

// unmarshalIfPresent は jsonb が NULL でなければ out にデコードする。
func unmarshalIfPresent(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
