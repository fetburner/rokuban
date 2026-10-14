> [frontend.md](../frontend.md) の一部。索引から辿る

# アセット配信 --- go:embed / S3+CDN 両対応

同じビルド成果物を、バイナリへの同梱と S3 バケットへの同期の両方に使う。フロントのコードとビルドは両モードで完全に同一である。

<details><summary>根拠</summary>

- 同じ成果物: `dist/`

</details>

## 両立のための規約

両モードで同じ挙動にするために、SPA フォールバック・API パス・SSE の振り分け・キャッシュ・後方互換の 5 点を揃える。各規約の要点は次の表の通り。

| 規約 | 要点 |
|---|---|
| SPA フォールバック | 両モードで用意 |
| API パス | 常に相対パス `/api/*` |
| SSE の振り分け | notifier（常駐）へ |
| キャッシュ規約 | 内容ハッシュ付きだけ immutable |
| 後方互換 | 運用開始後は API を保つ |

### SPA フォールバックと API パス

SPA フォールバックは両モードで用意し、API は常に相対パスで叩く。S3 配信時は CDN のパスルーティングで API をバックエンド origin へ振り分けるので、CORS も実行時のコンフィグ注入も要らない。

SPA フォールバックは Go 側が catch-all、S3 側が CloudFront Function の rewrite などで用意する。API のパスは `/api/*` で、S3 配信時にその全体をバックエンド origin へ振り分ける。

### SSE の振り分け

SSE だけは、api（scale-to-zero）には向けず、常駐する notifier へルーティングする。CDN のパスルーティングで SSE のパスを先に notifier origin へ、残りの API を api origin へ振り分ける。

API のパスの中で例外になるのは `/api/events` だけで、`/api/*` より先に、最長一致的に振り分ける。タイムアウトとキャッシュ無効化の設定も要る。CDN を迂回して notifier に直接繋ぐ選択肢も残す。

### キャッシュ規約

内容ハッシュ付きのアセットだけに immutable を付け、**それ以外はすべて no-cache にする。** go:embed モードでも同じヘッダを付けて挙動を揃える。

付けるヘッダは `Cache-Control` である。

### 後方互換

UI と API のデプロイのタイミングはずれうるので、運用開始後は API の後方互換を保つ。運用開始前は保たない（[api/rest.md](../api/rest.md) §契約の保護）。破壊的変更は、OpenAPI 生成クライアントの差分として CI で検知する。

<details><summary>根拠</summary>

- ハッシュ付きアセットの置き場: `assets/*`
- immutable を付けるヘッダ: `Cache-Control: immutable`
- 先に notifier へ振り分ける SSE のパス: `/api/events`

</details>

## ハッシュを持たないファイルに no-cache を付け忘れない

ハッシュ付きアセット以外は、一律 no-cache にする。内容ハッシュを持たないのは、エントリの HTML と、静的ファイル置き場由来のファビコン類である。内容が変わってもパスが変わらないので、キャッシュ指定を落とすと差し替えが伝わらない。

埋め込んだ FS の更新時刻はゼロ値なので、標準の配信関数は `Last-Modified` も `ETag` も出せない。`Cache-Control` がなければ検証子もなく、ブラウザのヒューリスティックキャッシュに委ねることになる。「ファビコンを差し替えたのに古いものが出続ける」という形で効く。

<details><summary>根拠</summary>

- ハッシュを持たないファイル: `index.html`、`public/` 由来の `favicon.svg` / `favicon.ico` / `apple-touch-icon.png`
- 更新時刻がゼロ値で検証子が出せない: 埋め込み FS の `ModTime`、`http.ServeContent`
- ハッシュ付きアセットの置き場: `assets/*`

</details>

## 配信経路の整理

go:embed 配信では、ハッシュ付きアセットに immutable、それ以外に no-cache のヘッダーを正しく付ければ十分である。本気の配信最適化は S3+CDN 経路の仕事であり、ここに nginx キャッシュを挟むと配信経路が 3 つになるため行わない（参照: [api.md](../api.md) のメディア配信）。
