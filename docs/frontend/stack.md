> [frontend.md](../frontend.md) の一部。索引から辿る

# 前提条件と採用スタック

## 前提条件

go:embed で単一バイナリに同梱するため、成果物は**静的ファイル一式**である必要がある。SSR サーバーを要求するフレームワーク（Next.js 標準構成、Nuxt SSR、Remix）は自動的に除外される。自宅内 or 認証の内側で動く管理 UI なので SSR の動機（SEO・初回表示）も元々ない。

## 採用スタック

| カテゴリ | 採用技術 |
|---|---|
| ビルド + フレームワーク | Vite + React + TypeScript |
| 状態管理・ルーティング・仮想化 | TanStack Query / Router / Virtual |
| API クライアント生成 | orval（OpenAPI → 型付きクライアント + TanStack Query フック） |
| スタイリング + UI コンポーネント | Tailwind + shadcn/ui |
| 動画再生 | ネイティブ `<video>`（VOD / MP4 progressive）+ hls.js（ライブ。ライブ視聴画面のみ動的 import。バンドルは別チャンク） |

## 決め手（技術選定の経緯）

### 1. SSE との組み合わせがバックエンドの思想と対称になる

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計（参照: [data.md](../data.md)）。フロントも同じ形にできる:

- SSE イベントは該当クエリの `invalidateQueries` に徹する
- 真実は常に REST から再取得
- SSE の取りこぼしは stale-time 経過後の再取得で自然回復

プッシュデータを信頼して手元状態を書き換える設計（Socket.IO 時代の EPGStation）より壊れ方が大幅に単純になる。

### 2. OpenAPI ファーストとの接続

orval で `useRecordings()` のような型付きフックまで生成物にできる。CI に組み込み、契約の破壊的変更は生成物の差分として検知する（参照: [api.md](../api.md) の REST API）。

### 3. 番組表グリッドのエコシステム

番組表グリッドはこのアプリ最大の UI 課題（数十チャンネル x 24 時間の巨大な仮想スクロール面）。TanStack Virtual なり canvas なり、この規模の実装例とライブラリの厚みは React が頭一つ抜けている。

Vue 3 / Svelte 5 でも成立する領域であり、決定打はエコシステムの厚み。
