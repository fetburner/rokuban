# フロントエンド

## 前提条件

go:embed で単一バイナリに同梱するため、成果物は**静的ファイル一式**である必要がある。SSR サーバーを要求するフレームワーク（Next.js 標準構成、Nuxt SSR、Remix）は自動的に除外される。自宅内 or 認証の内側で動く管理 UI なので SSR の動機（SEO・初回表示）も元々ない。

## 採用スタック

| カテゴリ | 採用技術 |
|---|---|
| ビルド + フレームワーク | Vite + React + TypeScript |
| 状態管理・ルーティング・仮想化 | TanStack Query / Router / Virtual |
| API クライアント生成 | orval（OpenAPI → 型付きクライアント + TanStack Query フック） |
| スタイリング + UI コンポーネント | Tailwind + shadcn/ui |
| 動画再生 | hls.js |

## 決め手

### 1. SSE との組み合わせがバックエンドの思想と対称になる

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計（参照: [data.md](data.md)）。フロントも同じ形にできる:

- SSE イベントは該当クエリの `invalidateQueries` に徹する
- 真実は常に REST から再取得
- SSE の取りこぼしは stale-time 経過後の再取得で自然回復

プッシュデータを信頼して手元状態を書き換える設計（Socket.IO 時代の EPGStation）より壊れ方が大幅に単純になる。

### 2. OpenAPI ファーストとの接続

orval で `useRecordings()` のような型付きフックまで生成物にできる。CI に組み込み、契約の破壊的変更は生成物の差分として検知する（参照: [api.md](api.md) の REST API）。

### 3. 番組表グリッドのエコシステム

番組表グリッドはこのアプリ最大の UI 課題（数十チャンネル x 24 時間の巨大な仮想スクロール面）。TanStack Virtual なり canvas なり、この規模の実装例とライブラリの厚みは React が頭一つ抜けている。

Vue 3 / Svelte 5 でも成立する領域であり、決定打はエコシステムの厚み。

## リアルタイム通知 --- SSE + invalidateQueries パターン

mirakc と同じ「接続時に現在状態を再送し、以降差分」という設計に揃える。クライアントの再接続処理が対称的になる。

### アーキテクチャ上の利点

- api ロールを複数レプリカにしても、SSE は各レプリカが Postgres の NOTIFY を購読して配るだけなので **Redis アダプタのような追加基盤は不要**
- レベルトリガーにより、SSE 接続断中の変更も stale-time 経過後の REST 再取得で自然回復する

通信設計の詳細は [api.md](api.md) を参照。

## アセット配信 --- go:embed / S3+CDN 両対応

同一の `dist/` を (a) go:embed で同梱、(b) S3 バケットに同期、の両方に使う。フロントのコードとビルドは両モードで完全に同一。

### 両立のための規約

| 規約 | 詳細 |
|---|---|
| SPA フォールバック | 両モードで用意。Go 側は catch-all、S3 側は CloudFront Function rewrite 等 |
| API パス | 常に相対パス `/api/*` を叩く。S3 配信時は CDN のパスベースルーティングで `/api/*` と `/events` をバックエンド origin へ振り分ける（CORS 不要、実行時コンフィグ注入不要） |
| SSE の CDN 通過 | タイムアウト・キャッシュ無効化の設定が必要。CDN を迂回して直接バックエンドに繋ぐ選択肢も残す |
| キャッシュ規約 | ハッシュ付きアセットは `Cache-Control: immutable`、`index.html` は no-cache。go:embed モードでも同じヘッダを付けて挙動を揃える |
| 後方互換 | UI と API のデプロイタイミングはずれ得るため API は後方互換を保つ。破壊的変更は OpenAPI 生成クライアントの差分として CI で検知 |

### 配信経路の整理

go:embed 配信でハッシュ付きアセット immutable + index.html no-cache のヘッダーを正しく付ければ十分。本気の配信最適化は S3+CDN 経路の仕事であり、ここに nginx キャッシュを挟むと配信経路が 3 つになるため行わない（参照: [api.md](api.md) のメディア配信）。

## ライブ視聴 --- EPGStation 水準のシンプルな UI

リッチな視聴体験（ハードウェアトランスコード、低遅延、コメント連携）は KonomiTV が既に高水準で解決しており、そこに張り合わない。同じ mirakc を共有すれば KonomiTV との共存も可能（チューナーは優先度調停で録画が勝つ）。

Rokuban 自体のライブ視聴は「チャンネル一覧から選んでブラウザ再生、画質切り替え程度」で良い。

### 実装方式

- **mirakc `/api/services/{id}/stream` → ffmpeg → HLS** の薄いパイプ。セグメントはローカルスクラッチに書き、api が配信
- `streamer` ロールが担当。リアルタイムに mirakc からの帯域を張り続けるため、分散モードではエッジ寄りに配置する（エンコードジョブと違いレイテンシ・帯域制約がある）

### セッション管理

- **ライブ視聴セッションは意図的に in-memory**。落ちたらクライアント再接続で済む使い捨て状態であり、「すべての状態を Postgres に」の原則の明示的な例外（参照: [overview.md](overview.md) の crash-only 設計原則）
- 「クライアントがいなくなったら ffmpeg を止める」idle GC が必要。セグメント要求がアプリを通ることで last-access の更新がタダで手に入る（参照: [api.md](api.md) のライブ HLS 配信）
