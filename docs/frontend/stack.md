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
| フォント | Geist Variable（英数字）+ Noto Sans JP Variable（和文）。どちらも `@fontsource-variable/*` で自前配布 |

## 決め手（技術選定の経緯）

### 1. SSE との組み合わせがバックエンドの思想と対称になる

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計（参照: [data.md](../data.md)）。フロントも同じ形にできる:

- SSE イベントは該当クエリの `invalidateQueries` に徹する
- 真実は常に REST から再取得
- SSE の取りこぼしはグループごとの定期 invalidate で回復する（運用状態 60 秒 / EPG 10 分。`lib/events.ts`）。`staleTime` は判定の期限であって再取得の周期タイマーではないので、この定期経路が対称性の「定期 reconcile」に当たる（[api.md](../api.md) §SSE）

プッシュデータを信頼して手元状態を書き換える設計（Socket.IO 時代の EPGStation）より壊れ方が大幅に単純になる。

### 2. OpenAPI ファーストとの接続

orval で `useRecordings()` のような型付きフックまで生成物にできる。CI に組み込み、契約の破壊的変更は生成物の差分として検知する（参照: [api.md](../api.md) の REST API）。

### 3. 番組表グリッドのエコシステム

番組表グリッドはこのアプリ最大の UI 課題（数十チャンネル x 24 時間の巨大な仮想スクロール面）。TanStack Virtual なり canvas なり、この規模の実装例とライブラリの厚みは React が頭一つ抜けている。

Vue 3 / Svelte 5 でも成立する領域であり、決定打はエコシステムの厚み。

### 4. フォントは英数字と和文で 2 書体を使い分ける（Geist は廃止しない）

`<html lang="ja">` のこの UI では本文のほぼ全部が和文だが、Geist は日本語グリフを
持たない。単独指定のままだと和文は「指定していないシステムフォールバック」で
描画される（ファビコンは既に Noto Sans JP Black で、書体は既に意匠の一部
[branding.md](branding.md)）。

**Geist を廃止せず、`--font-sans` の先頭に残したまま Noto Sans JP を足す。**

```
--font-sans: 'Geist Variable', 'Noto Sans JP Variable', 'Hiragino Kaku Gothic ProN',
  'Hiragino Sans', 'Yu Gothic Medium', 'Yu Gothic', 'Meiryo', sans-serif;
```

`font-family` のフォールバックは **要素単位ではなく文字単位**で解決される
（1 つのテキストノードの中でも、グリフを持つ最初のフォントが文字ごとに選ばれる）。
そのため「英数字は Geist、和文は Noto Sans JP」という使い分けをコンポーネント側の
条件分岐で書く必要が無い --- `--font-sans` を 1 回変えるだけで両方に効く。
和文システムフォント（ヒラギノ / 游ゴシック / メイリオ）は Noto Sans JP の
ダウンロード中（`font-display: swap`）とダウンロード失敗時の保険。

Geist を残す理由は数字。`font-variant-numeric: tabular-nums`
（[design.md](design.md)「数字は tabular-nums」）が実際に等幅を作るかどうかを
実ブラウザで実測すると:

- **Geist Variable は tabular-nums で実際に等幅になる**（OpenType の `tnum` を持つ。
  無指定だと `1` は `8` よりずっと狭い比例幅）
- **Noto Sans JP Variable の半角数字は既定でほぼ等幅**で、tabular-nums の有無で
  幅が変わらない（効いても効かなくても実害はない）

したがって時刻・尺・PID などの数字は Geist が担当する形が実測で裏付けられており、
Geist を廃止する積極的な理由もない。`tabular-nums` の当て方（`html` に 1 度だけ、
個別指定はしない）は [design.md](design.md)「数字は tabular-nums」の判断。

#### サイズへの影響（実測。受け入れる）

Noto Sans JP の追加で以下が増える。**サブセット化・unicode-range の間引き・
遅延ロードは検討していない（未検討）。**

- **dist**: 1.2M → 6.6M（+5.4M）。Noto Sans JP の 124 個の unicode-range woff2
  チャンクが 5.2MB を占める
- **バイナリ**（go:embed で dist を同梱）: 31.7MiB → 36.9MiB（+5.1MiB、+16%）
- **`index.css`**: 53.9KB → 152.2KB（gzip 10.4KB → 44.1KB）。124 個の
  `@font-face`（`unicode-range` 宣言込み）が同じ 1 枚の CSS に乗り、
  レンダーブロッキングで届く。この CSS 自体の増分がページ表示に与える影響は
  **未測定**
- **初回ロードで実際に落ちる woff2**: 画面に出ている字種の数だけ増える。
  タイトルの種類が少ないデモ用スタブ（8 種）では 8 個 / 約 199KB だが、
  実データに近い字種の広がり（語彙 70 種程度）を持たせて番組表グリッドまで
  開くと 37 個 / 約 700KB まで増える（`e2e/design.mjs` と同じスタブ機構で
  計測。数十チャンネル × 24 時間という実際の主戦場はこの多い側に近い）。
  「unicode-range 分割だから初期ロードは小さい」は断言しない --- 分割の効果は
  画面に出る字種の数に依存し、字種が多い画面では複数チャンクが同時に落ちる

このサイズ増は受け入れる。和文が全く描画されない状態（システムフォールバック
任せ）よりはサイズを払ってでも Noto Sans JP を自前配布する方が
issue #225 の目的（和文がこのアプリの意匠の一部であることに実装を追いつかせる）
に合うという判断で、最適化はまだしていない。バイナリが大きい理由を後で
問うたときにここへ戻れるようにする記録であり、将来サブセット化等を検討する
ときの起点はここになる。
