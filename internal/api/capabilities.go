package api

import "context"

// GetCapabilities はこのデプロイで有効になっているオプション機能を返す
// （GET /api/capabilities、issue #209）。
//
// フロントは config を読めない（api は設定ファイルを配らない。不変条件 1）ため、
// 「無効なら導線ごと消したい」設定はここでしか観測できない。返すのは真偽値だけで、
// config のキー名・値は載せない（ListEncodeProfiles / ListSites と同じ規律）。
//
// **返すのは config の状態であって、その機能が今すぐ使えることではない。**
// live.enabled が true でも streamer ロールが動いていなければライブは見られない
// （そちらはプレイリスト取得の 404 として出る。docs/frontend/live.md）。
func (h *Server) GetCapabilities(_ context.Context, _ GetCapabilitiesRequestObject) (GetCapabilitiesResponseObject, error) {
	return GetCapabilities200JSONResponse(h.capabilities), nil
}
