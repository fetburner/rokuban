package api

import (
	"io/fs"
	"net/http"
	"strings"
)

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
