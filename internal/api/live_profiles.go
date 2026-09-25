package api

import "context"

// ListLiveProfiles は config.live.profiles に定義されたプロファイルを設定順で返す。
//
// `/live` の画質セレクタが選べる名前を出すための公開面（M4-21 / issue #869）。
// 機微情報（ffmpeg のパス・extra_args・品質指定）は載せず、表示用の height までにする
// （`ListEncodeProfiles` と同じ規律）。
//
// **順序が既定の根拠である。** `?profile=` を省略したときの既定はサーバー側の先頭で
// あり（`internal/streamer` の `LiveConfig.profile`）、この一覧の先頭がそれと一致する。
// フロントは並びを変えない。
//
// **`live.enabled` は見ない。** 返すのは `config.live.profiles` の写しそのもので、
// **無効なデプロイでも profiles が書かれていれば返る**（`config.compose.yml` は
// `enabled: false` と `profiles` を並べて出荷している）。有効かどうかは
// `GET /api/capabilities` の `live` の側の問いである --- ここで無効を空配列に
// 潰すと、同じ config の状態を 2 箇所で判定することになる。
//
// 注入が無い（テストの部分構成）ときは空配列を返す。nil スライスを JSON null に
// しないよう、常に non-nil を返す。
func (h *Server) ListLiveProfiles(_ context.Context, _ ListLiveProfilesRequestObject) (ListLiveProfilesResponseObject, error) {
	out := make([]LiveProfileSummary, len(h.liveProfiles))
	copy(out, h.liveProfiles)
	return ListLiveProfiles200JSONResponse(out), nil
}
