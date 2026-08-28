package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
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
			Id:                 mirakc.ServiceID(int(s.NetworkID), int(s.ServiceID)),
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
			StartAt:     p.StartAt,
			EndAt:       p.EndAt,
			DurationMs:  p.DurationMs,
			Name:        p.Name,
			Description: p.Description,
			Genres:      genreLv1List(p.GenreLv1),
			IsFree:      p.IsFree,
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

	p := Program{
		ProgramId:   row.ProgramID,
		NetworkId:   int(row.NetworkID),
		ServiceId:   int(row.ServiceID),
		EventId:     int(row.EventID),
		StartAt:     row.StartAt,
		EndAt:       row.EndAt,
		DurationMs:  row.DurationMs,
		Name:        row.Name,
		Description: row.Description,
		Genres:      genreLv1List(row.GenreLv1),
		IsFree:      row.IsFree,
	}
	// jsonb はそのまま構造体に載せ替える。プロジェクション時点で mirakc の
	// ペイロードをそのまま入れているので、ここでの変換は unmarshal だけ。
	if err := unmarshalIfPresent(row.Extended, &p.Extended); err != nil {
		return nil, fmt.Errorf("decoding extended for program %d: %w", row.ProgramID, err)
	}
	if err := unmarshalIfPresent(row.Genres, &p.GenreDetails); err != nil {
		return nil, fmt.Errorf("decoding genres for program %d: %w", row.ProgramID, err)
	}
	if err := unmarshalIfPresent(row.Video, &p.Video); err != nil {
		return nil, fmt.Errorf("decoding video for program %d: %w", row.ProgramID, err)
	}
	if err := unmarshalIfPresent(row.Audios, &p.Audios); err != nil {
		return nil, fmt.Errorf("decoding audios for program %d: %w", row.ProgramID, err)
	}
	return GetProgram200JSONResponse(p), nil
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
