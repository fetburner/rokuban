package api

import "context"

// ListEncodeProfiles は config.encode.profiles に定義されたプロファイル名を返す。
//
// UI がルール / overrides の encodeProfiles を選ぶための公開面（M3-6 / issue #68）。
// 機微情報（ffmpeg パス・extra_args 等）は載せない。
//
// 注入が無い（テストの部分構成）ときは空配列を返す。nil スライスを JSON null
// にしないよう、常に non-nil を返す。
func (h *Server) ListEncodeProfiles(_ context.Context, _ ListEncodeProfilesRequestObject) (ListEncodeProfilesResponseObject, error) {
	out := make([]EncodeProfileSummary, 0, len(h.encodeProfileNames))
	for _, name := range h.encodeProfileNames {
		out = append(out, EncodeProfileSummary{Name: name})
	}
	return ListEncodeProfiles200JSONResponse(out), nil
}
