> [web/e2e/README.md](../README.md) の一部。索引から辿る

### デザイン（`design.mjs`）

**色は jsdom では測れない。** Tailwind のクラスは解決されず、oklch も計算されない。
`pnpm test` が全部通っても、色については何の保証にもならない ---
[docs/frontend/design.md](../../../docs/frontend/design.md) の「合否は画素で測る」に
実行可能な形を与えるのがこれ。

```sh
# 1) SPA を配れるサーバーを 1 つ立てる（API は下記のとおり全部差し替えるので何でもよい）
pnpm build && pnpm preview --port 4173 --strictPort &

# 2) 撮る + 判定する
E2E_URL=http://localhost:4173 pnpm e2e:design
```

`go:embed` 経路（`rokuban server --roles api`）に `E2E_URL` を向けても動くはずだが
**未検証**。API は全部差し替わるのでサーバーは静的配信しかしないという理屈だけで、
実際に回してはいない。

**mirakc も実チューナーも DB も要らない。** `/api/**` は `page.route` でブラウザ側から
丸ごと差し替える（`live.mjs` が HLS でやっているのと同じ手）。時刻も
`page.clock.setFixedTime` で固定してあるので、ショットの差分は実装の差分だけになる。

**フィクスチャは契約で検証される。**「唯一の視覚オラクル」が欠損データのまま
撮れていても、契約（`openapi.yaml`）が動くたびに誰も気付かない、という壊れ方が
実際にあった。

ルールの `textMatches` が旧形 `{ field, kind }` のまま
`{ target, mode }` に追従しておらず、ルール一覧に「undefinedに…を含む」が
描かれたまま exit 0 していた。

判定本体は `validateFixturesOrExit`
（`e2e/lib.mjs`）である。これは `verifyBundleMatchesOrExit` と同じ**前提条件**チェックで、
スクリプト固有の OK/NG 判定ではないため、共有ハーネス側に置いてある。

フィクスチャを
orval 生成の zod スキーマ（`web/src/api/zod.ts` の `List*ResponseItem`）で
`parse` し、1 件でも不一致なら他の判定を一切せず exit 1 する。これは ⓪ の
`verifyBundleMatchesOrExit` と同じ「前提が崩れていたら打ち切る」扱いである。
`design.mjs` に加えて `badge-links.mjs` / `sse-refresh.mjs` /
`grid-reserved.mjs` / `reservations-mobile.mjs` も自分のフィクスチャで呼ぶ。
**契約を変えたら、フィクスチャを持つ各スクリプトも同じ PR で直す。**

`zod.ts` は `import.meta.env` 等 Vite 依存を持たない素の TypeScript なので、
Node の ESM スクリプトから `../src/api/zod.ts` を直接 import できる。
追加のローダー（tsx・vite-node）は要らない。Node は型注釈だけを消す型
ストリッピングを既定で持つ（`.node-version` の 24.20.0 で実際に import
できることを確認済み）。`zod.ts` は enum・namespace・parameter properties の
ような変換が要る構文を持たないため、これだけで通る。`package.json` の
`e2e:design` 等の各スクリプトもそのままで変更していない。

`design.mjs` は画面のテキストに欠損文字列が混ざっていないかも全ショットに
掛ける ---「安いので全画面に掛ける」。見るのは `undefined` / `NaN`
（単語境界 `\b`）と `[object`（前方一致）。**`null` は対象にしない** ---
番組名・ルール名に偶然「null」を含む文字列が来ると単語境界だけでは区別できず
偽陽性になりうるため。

出るもの:

- `e2e/screenshots/*.png`（追跡しない）。主要 8 画面（ホーム・シリーズ一覧・番組ハブを含む）×
  ライト / ダーク × デスクトップ / モバイル。加えて番組表グリッド・
  サーキットブレーカー発動中・モバイルの「その他」を開いた状態・読み込み中
  （Skeleton の走査線を撮るため録画一覧の応答を遅延させたもの）を撮る。

  空状態（EmptyState の文言。既定のショットでは折り返しの下に隠れて
  文字が写らないので、スクロールしてから撮る）とホームの全セクション空状態
  （`home-empty-*`）を足す。
  さらにシリーズ一覧の格子とリスト（`series-card-*` / `series-list-*`）を 360px と 2560px で撮る。
  番組ハブは 400px とデスクトップ幅で配置とメニューを撮る（`series-hub-400-*` / `series-hub-desktop-*`）。
  **人が見て判断するための成果物**で、機械が比較するものではない
- 合否（exit code）。以下をすべて実画素・実描画で判定する:
  - サイドバーの現在地の塗りと、そのリンクに出る 2px フォーカスリングが、ライト / ダークともページの地と 3:1 以上か。PNG の実画素を測り、現在地のリングは 2px のオフセットでページ地に出ることも見る
  - 共通 Button の focus-visible リングがライト / ダークともページ地と 3:1 以上か。PNG のリング画素そのものを測るため、`ring-ring/50` への退行も検出する
  - 状態色（塗りか文字か / 赤か琥珀か）・地の無彩性・**WCAG コントラスト**
    （文字は 4.5、面と線は 3 が下限）。`bg-muted` 系の淡い面に乗る文字は、
    塗り・`/80` の sticky 日付見出し・`/30` の録画詳細パネルに加えて
    **hover 中の面**（一覧の行の `hover:bg-muted/40`）まで測る --- Lighthouse は
    hover を測らないので、監査に出ない面はここでしか押さえられない
    （下限を割ったものは除外せず、通常の失敗として扱う）。選択モードで
    選んだ行も同じ `bg-muted/40` を敷くが、こちらは常時見えるので測る
  - **和文が実際に Noto Sans JP、英数字が実際に Geist で描画されているか**を
    見る。CDP `CSS.getPlatformFontsForNode` に番組リストの行
    （`li[data-program-id]`）を渡し、その実使用フォントを読む。
    `main` や `body` のようなブロック要素だけを子に持つノードを渡すと
    常に空配列が返るため使えない。`getComputedStyle().fontFamily` は
    指定文字列を返すだけで実描画の保証にはならない。
    あわせて**和文まじりの文字列でも tabular-nums が実際に等幅を
    作っているか**を DOM の実測幅で見る（`docs/frontend/stack.md`
    「フォントは英数字と和文で 2 書体を使い分ける」）
- シリーズ一覧（`/series`）の格子とリスト（①-B）。360px のモバイルと 2560px のデスクトップで実測する。
  格子が 2 列 / 4 列・リストが 1 列になること、サムネイルが 16:9（誤差 0.01）であること、横にはみ出さないこと。
  画像が無いときの代替表示が `bg-muted` の塗りで走査線でないこと、メタが 14px であること。
  サムネイルを 16:9 でなくすると（`h-16 w-28` に戻す）リストの 2 判定が落ちることを確認した。
- 色以外にも、jsdom では原理的に測れないキーボード到達性を 1 件持つ。
  録画一覧の行リンクを Enter で開いて詳細（`/recordings/$id`）へ遷移し、
  詳細で Tab 走査だけで `<video>` へ到達すること（視聴は詳細ページに寄せる）。
  `<video>` に `tabIndex` を明示すると（jsdom の focus spy
  では検出できない形で）Tab 走査から外れてしまう退行が M5-4（issue #227）
  で実際に一度起きたため、ここで実ブラウザから固定している
- 共通 `Button` のフォーカスリング（`:focus-visible` の `ring-3` = box-shadow、
  および `border-ring` = 1px 罫線の border-color）が遷移**しない**こと。
  `transition-all` は CSS が実際に発火するかどうかまでは jsdom はもちろん
  `getComputedStyle` の 1 回読みでも確認できないため、ボタン要素に張った
  `transitionstart` イベント（プロパティ名つき）を実ブラウザで観測する ---
  box-shadow / outline* / border-*-color のいずれかが遷移対象に上がったら
  NG（border-color だけを見落としたレビュー指摘が実際にあったため、
  ロングハンド込みで前方一致を掛けている）。

  同じ手で両方向を確認する。hover の背景色の遷移が従来どおり起きること。
  押下フィードバック（`active:...:translate-y-px`。Tailwind v4 では
  `translate` プロパティにコンパイルされる）が遷移し**続ける**こと。
- 接続断バナー（`components/connection-banner.tsx`、issue #456）の地が無彩か。
  `/api/events` は明示のスタブを持たず catch-all（200 json）に落ちるので、
  Content-Type 不一致で SSE が即座に失敗し、追加の配線なしで「切断中」を作れる。
  `disconnectedBannerDelayMs`（10 秒）分は実時間で待つ
- 測ったコントラストの表。**数値の権威はこの出力**で、docs には転記しない
- `④-A` の操作標的計測では、fine のデスクトップと coarse の 360px モバイルを使う。
  主要 6 画面の
  `button, a[href], [role="button"], [role="switch"], input, select, summary` を
  実際の描画矩形から列挙する。各標的の visual / hit 寸法、最小エッジ間隔、
  意図的な重なり件数を出力し、
  実効 hit 寸法が 24×24 CSS px 未満なら exit 1 にする。`::before` / `::after` による
  当たり判定の拡張と祖先の overflow によるクリップを含めるので、クラス名だけの
  推測にならない。

  未フォーカスの sr-only スキップリンクは既存の Tab 後計測で扱う。
  日付セル・チャンネル候補・行主操作・ライブチャンネルは別途 44px 高、モバイルナビは
  幅44px・高さ56pxを、#729 のトースト action は 32px、close は 28px を個別に
  固定する。全面リンク等の意図的な重なりがあるため、間隔に全画面一律の閾値は置かず、
  配置の意味は `docs/frontend/reservations.md` に従う
- モバイルの「その他」ポップオーバー（`components/app-shell.tsx` の `MoreMenu`）
  を開いた状態の判定。固定されたボトムバーの上に浮くオーバーレイなので、
  はみ出し・重なりは jsdom（`app-shell.test.tsx`）では原理的に測れない。
  ここで実測するのは 3 点: ボトムタブが常に 4 個か / 開いたポップオーバーが
  ビューポート内に収まるか / ポップオーバーがトリガーの上端より上に出るか
  （バーの下に隠れていないか）。開いた状態のショットも `more-menu-open-*.png`
  として出る
- `prefers-reduced-motion: reduce` で主要な動き（Skeleton の
  `animate-pulse`・ポップオーバーの
  `slide-in-from-*`/`zoom-in-95`・共通 `Button` の `translate` 遷移）が
  縮退し、既定（`no-preference`）では従来どおり動くこと。**両方向**を見る
  --- 縮退側だけの判定は、動きを恒久的に殺した実装も通してしまう。
  Playwright の `reducedMotion` コンテキストオプションで OS 設定を
  エミュレートし、実要素の `getComputedStyle().animationDuration` /
  `transitionDuration` を実測する（jsdom は matchMedia も CSS の適用も
  測れない）

判定の設計で外してはいけない点が 2 つある。

- **半透明の地は合成してから測る。** `text-warning` が乗るのは地ではなく
  `bg-warning/10` の上。地に対する比だけを見ると 0.5〜0.7 甘い数字が出る
- **下限を割るものは通常の失敗にする。** 既知の不足を理由に合否から外したり、
  閾値を静かに下げたりしない

ダークは Playwright context の `colorScheme` で OS 設定をエミュレートする。
`design.mjs` はクラスを直接付けず、アプリが初回描画前に `html.dark` へ同期する到達経路を判定する。

**`getComputedStyle()` の戻り値を正規表現で読んではいけない**。
トークンが oklch なので Chromium は計算値も `oklch(...)` のまま返し、
`rgb(...)` を期待した実装は全部の判定を「読めない」で素通りさせる。
`design.mjs` は 1px 塗って `getImageData` で実画素を採っている。

トークン外の生の色値（`bg-amber-700` / `bg-black` / `#rrggbb`）の検出は実ブラウザが
要らないので、そちらは別のコマンドにしてあり **CI の lint job で回る**。

```sh
pnpm check:colors
```

検査が見ていない書き方（動的なクラス名の合成・CSS の名前付き色・3 桁の 16 進・
`public/` の資産）は `scripts/check-colors.mjs` に書き出してあり、実行のたびに出力する。

### README のスクリーンショット（`readme-screenshots.mjs`）

**判定ではなく撮影だけをする。** ルートの `README.md` が載せるスクリーンショットを `docs/images/` に書き出す。
画面を変えて README の絵が古くなったら、これを回して差し替える。

公開リポジトリに載せるので、実放送の映像と番組情報は使わない。
局と番組は架空のもので、映像は Blender Foundation のオープンムービー（CC BY 3.0）の予告編から切り出す。
予告編は初回だけ取得して `E2E_README_MEDIA_DIR`（既定は OS の一時ディレクトリ）に置く。
素材を変えたら README のクレジットも直す。

フィクスチャは他の判定と同じく `pnpm e2e:fixtures` で契約と照合されるので、API の形が変わると CI で気付ける。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm screenshots:readme
```

### フォントスケール（`font-scale.mjs`）

`pnpm e2e:font-scale` は、移行した `text-xs` が実ブラウザで 12px かを測る。
390px は fine と coarse（isMobile + hasTouch）の両方で /search・/recordings・/series の入力欄が 16px 以上、1280px は
既定の 14px を保つことを確かめる。番組表グリッドとホームの運用時間軸では、
横幅が時刻を示すラベルが 11px 以上かを測る。
直値 11px を下げる変異と、モバイル入力を 16px 未満にする変異で失敗する。
物理 iPhone の Safari で入力欄にフォーカスしたときの拡大は未検証で、実機確認を別途行う。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:font-scale
```

### 個人化の持続（`personalization.mjs`）

個人設定の置き場所（[docs/frontend/design.md](../../../docs/frontend/design.md)
§個人化）が実ブラウザで効いていることを見る。**jsdom では原理的に測れないもの
だけ**を対象にする --- Vitest の「再マウント」はリロードではないので、
`localStorage` が本当にページの読み込みをまたいで効くかは測れない。

見るのは 5 点:

- ① 録画一覧をカード表示に切り替えてリロードしても、カード表示のまま
  （`aria-pressed=true`）で、URL には何も載っていない
- ② カード表示が本当に段組みになり（1 行目に 2 枚以上）、サムネイルの枠が
  行表示より広い。**列数もサムネイルの実寸も jsdom では 0 のまま**なので、
  グリッドのクラスが当たっていない退行はここでしか出ない
- ③ 検索を押すと条件が `?cond=` に載り、リロードで条件と結果の両方が戻る。
  リロード後に走る検索が **1 回だけ**であること（URL のハイドレーションと
  フォームの初期化で二重に叩く退行の検出）
- ④ 条件なしで `/search` を開くと、前回の条件はフォームに戻るが**検索は
  走らない**（未検索の案内が出たまま）
- ⑤ 録画詳細で実 `<video>.playbackRate` を 1.5 に変え、標準 controls と同じ
  `ratechange` 経路で保存されること、一覧を経由して別の録画の詳細へ移っても
  実 `<video>.playbackRate` が 1.5 のままなこと。**測っているのは
  「速度変更イベントが端末に保存され、録画をまたいで実 `<video>` に載る」こと**で、
  `savePlaybackRate` を no-op にする変異では保存待ちが失敗し、保存値の適用を外す変異では
  次の録画の `<video>.playbackRate` 判定が失敗する。

  **同一インスタンスのまま録画だけ差し替わる経路（`playbackRate` を当てる
  effect の依存漏れ）はここでは踏めない** --- 現在の UI では必ず一覧を経由し、
  `RecordingPlayer` が新規マウントして localStorage から読み直すため。その
  退行を捕まえているのは `src/components/recording-player.test.tsx` の
  `rerender` を使う jsdom テスト（実測で確認済み）

一覧の `<ul>` は行のテキストから `ancestor::ul[1]` で辿る。素の `ul li` は
サイドバーのナビゲーションにも当たり、「1 行目に 1 枚」という無関係な値を
測ってしまう（実際に踏んだ）。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:personalization
```

### チップの横あふれ（`chip-overflow.mjs`）

サービスチップに補助ラベル（同名のワンセグ / サブサービスを見分けるための
リモコン番号・物理チャンネル・serviceId。issue #306）を足すと、名前だけなら
収まっていたチップが親の幅を超える。`Chip` は `shrink-0` を持つので
**flex-basis が内容の最大幅を要求し、ページ全体が横に伸びる**。jsdom は
`scrollWidth` / `clientWidth` を計算しない（常に 0）ので、この壊れ方は
`pnpm test` が全部通っても検出できない。

見るのは 320px 幅（実機で最も狭い層）で 3 点:

- ① `/search` の条件フォームに長い局名 + 補助ラベルのチップを流し込んでも
  `document.documentElement` が横スクロールしない。**`Chip` から `max-w-full`
  を外すと落ちる**（実測: 有り 320 / 320、無し 462 / 320）
- ② 解き方が「チップの中で折り返す」であること（切り落としでも隠しでもない）。
  チップの箱がビューポートに収まり、箱の中で内容があふれておらず、実際に 2 行に
  なっている。`max-w-full` を外すと①と一緒に落ちる
- ③ `Chip` は共有プリミティブなので、録画一覧の絞り込みにある状態 5 件 / 種別 3 件の
  短いピルが丸ピルのまま 1 行であること。ジャンルはチェック付きメニューなので、
  320px でトリガーと通常候補 13 件が横にはみ出さないことを確認する。選択済みの
  予約ジャンルと拡張ジャンルは録画一覧の絞り込み E2E で確認する。ポップオーバーが
  ビューポートに収まることも合わせて測る。同じ③で録画一覧のサイトチップも見る
  ---レジストリに無い site を `?site=` に載せて開き、和集合で増えたチップが
  320px に収まることを確認する

**レジストリは 2 サイトにしてある（issue #531）。**`<ConditionFields>` の
サイトチップはレジストリと下書きの和集合が 2 つ以上のときしか描画しない。
そのため単一サイトかつ下書きが空のスタブでは、判定対象自体が存在しなかった。
片方を長い site 名にして①のサービスチップと同じ `Chip` を通し、
「サイト」のチップ列がちょうど 2 件描かれることと、①の横スクロール判定に
一緒に含めている。

**`break-words` は入れていない。** ①②を `break-words` 無しでも測ったが差が出な
かった（和文は文字間で折り返せる）。長い ASCII 1 語での挙動は未検証。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:chip-overflow
```
