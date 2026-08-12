package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

// apiPathPrefix は SPA フォールバックに落としてはならないパスの接頭辞。
const apiPathPrefix = "/api/"

// apiPathExact は接頭辞に一致しない API 側のパス（`/api` そのもの）。
// SPA のクライアントルートに `/api` は無いので、末尾スラッシュの有無で
// 「404」と「HTML 200」に分かれるのを防ぐ。
const apiPathExact = "/api"

// isAPIPath は SPA フォールバックの対象外（= API 側）かを返す。
func isAPIPath(path string) bool {
	return path == apiPathExact || strings.HasPrefix(path, apiPathPrefix)
}

// spaOrAPINotFound は未マッチのリクエストを振り分けるハンドラを返す。
// `/api/` 配下は 404（JSON）、それ以外は SPA（index.html）へフォールバックする。
//
// **`/api/` を index.html に落とすと「無い」が「200 の HTML」になる。**
// 実害の出た経路（issue #209）: live.enabled が false のとき streamer はライブの
// ルートを一切登録しないので `GET /api/sites/{site}/services/{id}/live/playlist.m3u8`
// は未マッチになるが、SPA フォールバックが index.html を 200 で返していた。
// フロントの probeLivePlaylist は `response.ok` を見るので probe は通り、その後
// hls.js / <video> が HTML を m3u8 として解釈できずに再生エラーになる ---
// 「無効な機能」ではなく「壊れた再生」として見えていた。
//
// 同じ形は登録されないルートすべてに効く（api ロール単独の `/api/events` など。
// router.go の Mounter のコメントが「生えない（404 になる）」と書いていた挙動は、
// SPA を配る構成では実際には HTML 200 だった）。
func spaOrAPINotFound(spa http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			// 生成ハンドラのエラー応答（ErrorResponse）と同じ形にして、
			// フロントの apiErrorMessage がそのまま本文を読めるようにする。
			_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "not found"})
			return
		}
		spa.ServeHTTP(w, r)
	}
}

// NewSPAHandler は Vite ビルド出力を配信する http.Handler を返す。
// - /assets/* のハッシュ付きファイルには immutable キャッシュヘッダーを付与
// - それ以外は no-cache（デプロイ直後に最新を返すため）
// - 存在しないパスはすべて index.html へフォールバック（SPA クライアントルーティング対応）
//
// ハッシュを持たないのは index.html と public/ 由来のファイル（favicon.svg,
// favicon.ico, apple-touch-icon.png）で、いずれも内容が変わってもパスが変わらない。
// 埋め込んだ FS の ModTime はゼロ値なので http.ServeContent は Last-Modified も
// ETag も出せず、Cache-Control がないとブラウザのヒューリスティックキャッシュに
// 委ねることになる。差し替えたのに古いファビコンが出続ける形で効く。
func NewSPAHandler(distFS fs.FS) http.Handler {
	fileServer := http.FileServerFS(distFS)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		// ファイルが実在すればそのまま配信（キャッシュヘッダー付き）
		if f, err := distFS.Open(path); err == nil {
			_ = f.Close()

			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}

			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA フォールバック: index.html を no-cache で返す
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
