> [frontend.md](../frontend.md) の一部。索引から辿る

# 前提条件と採用スタック

フロントは、静的ファイル一式の成果物にできる構成だけを採る。その上で Vite + React + TypeScript を中心に、技術選定の決め手と書体の判断を残す。

## 前提条件

成果物は静的ファイル一式でなければならない。go:embed で単一バイナリに同梱するためである。SSR サーバーを要求するフレームワーク（Next.js 標準構成、Nuxt SSR、Remix）は自動的に除外される。自宅内か認証の内側で動く管理 UI なので、SSR の動機（SEO・初回表示）も元々ない。

## 採用スタック

採用は Vite + React + TypeScript を中心に、TanStack・orval・Tailwind + shadcn/ui を重ねた構成である。動画再生はネイティブの video 要素と hls.js を使い分け、フォントは英数字と和文の 2 書体を自前配布する。個別の採用技術は次の表の通り。

| カテゴリ | 採用技術 |
|---|---|
| ビルド + フレームワーク | Vite + React + TypeScript |
| 状態管理・ルーティング・仮想化 | TanStack Query / Router / Virtual |
| API クライアント生成 | orval |
| スタイリング + UI コンポーネント | Tailwind + shadcn/ui |
| 動画再生 | `<video>` + hls.js |
| フォント | Geist Variable + Noto Sans JP Variable |

動画再生は、VOD（MP4 progressive）にネイティブの `<video>` を使い、ライブに hls.js を使う。hls.js はライブ視聴画面でだけ動的 import し、バンドルは別チャンクに分ける。フォントはどちらも `@fontsource-variable/*` で自前配布する。orval は OpenAPI から型付きクライアントと TanStack Query のフックを生成する。

## 技術選定の決め手

決め手は 4 つある。SSE と相性が良くバックエンドの設計と対称になること、OpenAPI ファーストと繋がること、番組表グリッドのエコシステムが厚いこと、フォントを 2 書体に分けることである。

### 1. SSE との組み合わせがバックエンドの思想と対称になる

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計であり（参照: [data.md](../data.md)）、フロントも同じ形にできる。SSE イベントは該当クエリの再取得の合図に徹し、真実は常に REST から取り直す。取りこぼしはグループごとの定期的な再取得で回復する。プッシュされたデータを信頼して手元の状態を書き換える設計より、壊れ方が大幅に単純になる。

定期的な再取得は、グループの周期と対象を [api.md](../api.md) §SSE の表に任せる。ここに列挙すると、グループが増えたときに列挙だけが古くなるためである。`staleTime` は判定の期限であり、再取得の周期タイマーではない。そのため、この定期経路が対称性でいう「定期 reconcile」に当たる。

<details><summary>根拠</summary>

- 該当クエリの再取得: `invalidateQueries`
- 定期的な再取得の実装: `lib/events.ts`
- 期限であって周期ではない: `staleTime`

</details>

### 2. OpenAPI ファーストとの接続

orval で型付きフックまで生成物にできる。例えば録画一覧の取得フックである。CI に組み込み、契約の破壊的変更は生成物の差分として検知する（参照: [api.md](../api.md) の REST API）。

<details><summary>根拠</summary>

- 生成されるフックの例: `useListRecordings()`

</details>

### 3. 番組表グリッドのエコシステム

番組表グリッドはこのアプリ最大の UI 課題で、数十チャンネル x 24 時間の巨大な仮想スクロール面になる。TanStack Virtual でも canvas でも、この規模の実装例とライブラリの厚みは React が頭一つ抜けている。Vue 3 や Svelte 5 でも成立する領域なので、決定打はエコシステムの厚みである。

### 4. フォントは英数字と和文で 2 書体を使い分ける（Geist は廃止しない）

英数字は Geist、和文は Noto Sans JP の 2 書体を使い、Geist は廃止しない。Geist は日本語グリフを持たないので、単独指定のままでは和文が指定していないシステムフォールバックで描画される。数字の等幅は Geist が担う。この形は実ブラウザの実測で裏付けてある。

#### 書体の足し方

Geist を廃止せず、サンセリフ指定の先頭に残したまま Noto Sans JP を足す。フォールバックは文字単位で解決されるので、指定を 1 回変えるだけで英数字と和文の両方に効く。

この UI は本文のほぼ全部が和文だが、Geist は日本語グリフを持たない。ファビコンは既に Noto Sans JP Black で、書体は既に意匠の一部になっている（[branding.md](branding.md)）。

並びの権威は CSS のサンセリフ指定にある。フォールバックの解決は文字単位で、要素単位ではない。1 つのテキストノードの中でも、グリフを持つ最初のフォントが文字ごとに選ばれる。そのため、英数字と和文の使い分けをコンポーネント側の条件分岐で書く必要は無く、指定を 1 回変えるだけで両方に効く。和文のシステムフォント（ヒラギノ / 游ゴシック / メイリオ）は、Noto Sans JP のダウンロード中とダウンロード失敗時の保険である。

<details><summary>根拠</summary>

- 日本語の画面であること: `<html lang="ja">`
- 並びの権威: `web/src/index.css` の `--font-sans`
- ダウンロード中の表示: `font-display: swap`

</details>

#### 数字の等幅は Geist が担う

Geist を残す理由は数字である。Geist Variable は tabular-nums で実際に等幅になるが、Noto Sans JP Variable の半角数字は既定でほぼ等幅で、指定の有無で幅が変わらない。

tabular-nums（[design.md](design.md)「数字は tabular-nums」）が実際に等幅を作るかを、実ブラウザで実測した。Geist Variable は無指定だと数字の 1 は 8 よりずっと狭い比例幅になる。Noto Sans JP Variable は効いても効かなくても実害はない。

したがって、時刻・尺・PID などの数字は Geist が担当する形が実測で裏付けられていて、Geist を廃止する積極的な理由もない。tabular-nums は `html` に 1 度だけ当て、個別指定はしない。これは [design.md](design.md)「数字は tabular-nums」の判断である。

<details><summary>根拠</summary>

- 比べた数字: `1` と `8`
- 等幅を作る指定: `font-variant-numeric: tabular-nums`（略称 `tabular-nums`）
- 解決が文字単位の指定: `font-family`
- Geist が持つ OpenType 機能: `tnum`

</details>

#### サイズへの影響（実測。受け入れる）

Noto Sans JP の追加でサイズが増えるが、このサイズ増は受け入れる。サブセット化・unicode-range の間引き・遅延ロードは検討していない（未検討）。unicode-range 分割の効果は画面に出る字種の数に依存し、字種が多い画面では複数チャンクが同時に落ちる。

和文が全く描画されない状態（システムフォールバック任せ）よりは、サイズを払ってでも Noto Sans JP を自前配布する方が目的に合う。目的は和文がこのアプリの意匠の一部であることに実装を追いつかせることで、最適化はまだしていない。バイナリが大きい理由を後で問うたときにここへ戻れるようにする記録であり、将来サブセット化などを検討するときの起点もここになる。

増分の実測は次の通り。

- 配信物全体は 1.2M から 6.6M に増えた（+5.4M）。Noto Sans JP の 124 個の unicode-range woff2 チャンクが 5.2MB を占める
- バイナリは go:embed で成果物を同梱しているので、31.7MiB から 36.9MiB に増えた（+5.1MiB、+16%）
- メイン CSS は 53.9KB から 152.2KB に増えた（gzip 10.4KB から 44.1KB）。124 個の `@font-face`（unicode-range 宣言込み）が同じ 1 枚の CSS に乗り、レンダーブロッキングで届く。この CSS 自体の増分がページ表示に与える影響は未測定である
- 初回ロードで実際に落ちる woff2 は、画面に出ている字種の数だけ増える。タイトルの種類が少ないデモ用スタブ（8 種）では 8 個 / 約 199KB に留まった。実データに近い字種の広がり（語彙 70 種程度）を持たせて番組表グリッドまで開くと、37 個 / 約 700KB まで増えた。数十チャンネル × 24 時間という実際の主戦場は、この多い側に近い

「unicode-range 分割だから初期ロードは小さい」は断言しない。分割の効果は画面に出る字種の数に依存し、字種が多い画面では複数チャンクが同時に落ちるためである。

<details><summary>根拠</summary>

- 配信物: `dist/`
- メイン CSS: `index.css`
- 分割の宣言: `unicode-range`
- 計測に使ったスタブ機構: `e2e/design.mjs`

</details>
