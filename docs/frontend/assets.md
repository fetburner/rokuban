> [frontend.md](../frontend.md) の一部。索引から辿る

# アセット配信 --- go:embed / S3+CDN 両対応

同一の `dist/` を (a) go:embed で同梱、(b) S3 バケットに同期、の両方に使う。フロントのコードとビルドは両モードで完全に同一。

## 両立のための規約

| 規約 | 詳細 |
|---|---|
| SPA フォールバック | 両モードで用意。Go 側は catch-all、S3 側は CloudFront Function rewrite 等 |
| API パス | 常に相対パス `/api/*` を叩く。S3 配信時は CDN のパスベースルーティングで `/api/*` をバックエンド origin へ振り分ける（CORS 不要、実行時コンフィグ注入不要） |
| SSE の振り分け | `/api/events` だけは `/api/*` の中でも例外で、**api（scale-to-zero）ではなく notifier（常駐）へ**ルーティングする。CDN のパスルーティングで最長一致的に `/api/events` を先に notifier origin へ、残りの `/api/*` を api origin へ振り分ける。タイムアウト・キャッシュ無効化の設定も必要。CDN を迂回して notifier に直接繋ぐ選択肢も残す |
| キャッシュ規約 | ハッシュ付きアセット（`assets/*`）は `Cache-Control: immutable`、**それ以外はすべて no-cache**。go:embed モードでも同じヘッダを付けて挙動を揃える |
| 後方互換 | UI と API のデプロイタイミングはずれ得るため API は後方互換を保つ。破壊的変更は OpenAPI 生成クライアントの差分として CI で検知 |

## ハッシュを持たないファイルに no-cache を付け忘れない

`index.html` だけでなく `public/` 由来のファイル（`favicon.svg` / `favicon.ico` /
`apple-touch-icon.png`）もハッシュを持たない。内容が変わってもパスが変わらないので、
キャッシュ指定を落とすと差し替えが伝わらない。

埋め込んだ FS の `ModTime` はゼロ値なので `http.ServeContent` は `Last-Modified`
も `ETag` も出せない。`Cache-Control` がなければ検証子もないので、ブラウザの
ヒューリスティックキャッシュに委ねることになる。**ファビコンを差し替えたのに
古いものが出続ける**形で効く。

そのため `assets/*` 以外は一律 no-cache にする。

## 配信経路の整理

go:embed 配信でハッシュ付きアセット immutable + それ以外 no-cache のヘッダーを正しく付ければ十分。本気の配信最適化は S3+CDN 経路の仕事であり、ここに nginx キャッシュを挟むと配信経路が 3 つになるため行わない（参照: [api.md](../api.md) のメディア配信）。
