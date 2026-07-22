package api

import (
	"io/fs"
	"net/http"
	"strings"
)

// NewSPAHandler は Vite ビルド出力を配信する http.Handler を返す。
// - /assets/* のハッシュ付きファイルには immutable キャッシュヘッダーを付与
// - index.html は常に no-cache（デプロイ直後に最新を返すため）
// - 存在しないパスはすべて index.html へフォールバック（SPA クライアントルーティング対応）
func NewSPAHandler(distFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(distFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		// ファイルが実在すればそのまま配信（キャッシュヘッダー付き）
		if f, err := distFS.Open(path); err == nil {
			_ = f.Close()

			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
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
