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
- SSE の取りこぼしは stale-time 経過後の再取得で自然回復

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
Geist を廃止する積極的な理由もない。

数字の `tabular-nums` は各コンポーネントで個別に指定するのではなく、`html` に
1 度だけ当てて全域に効かせる（`web/src/index.css` の `@layer base`）。触るたびに
指定を思い出す必要をなくすため --- 個別のクラス指定はここへ統合し、消してある。
