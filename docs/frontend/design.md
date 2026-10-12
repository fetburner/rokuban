> [frontend.md](../frontend.md) の一部。索引から辿る

# デザイン言語（副調整室の計器盤）

**この節は以後の UI PR のレビュー判定基準。** 新しい画面・コンポーネントを足すときは
ここを満たしているかで見る。

形（トークン名と実際の値）の権威は `web/src/index.css`。ここには判断（なぜ）だけを書く。
**合否は実ブラウザで測る**（`pnpm e2e:design`。下記「合否は画素で測る」）。

## テーゼは「副調整室の計器盤」

Rokuban が扱うのは放送波と有限のチューナーで、UI の仕事は
**いま何が電波に乗っていて、何が録れていて、何が足りないか**を一目で言うこと。
装飾で個性を出す画面ではない。基準にするのは副調整室（サブ）の計器盤 ---
地は徹底して無彩、色が点いていること自体が情報。

原点は 16px のファビコン（[branding.md](branding.md)）で成立している階調構成で、
それをそのままアプリの地に持ち上げている。

## 地は「イ」の 3 値

地の階調は**ファビコンの 3 値と同じ値**を使う。

| ファビコン | トークン |
|---|---|
| 輝線 = 白 | `--paper`（紙白） |
| 間隙 = 灰 | `--scanline`（走査線グレー） |
| 字 = 黒 | `--sumi`（墨） |

値そのものはここには書かない（`index.css` と `scripts/gen-favicon.mjs` に既に 2 度
書いてあり、3 つ目を作ると必ずどれかが古くなる）。

ライトは「白地に黒」（ファビコンそのもの）、ダークは「消灯した画面に輝線」
（役割を入れ替える。反転ではない）。3 値の間を埋める段（`--tone-*`）は、
3 値がちょうど 50 / 500 / 900 に一致する Tailwind zinc の ramp から採る ---
別の ramp を持ち込むと、3 値だけが浮いて中間段と噛み合わなくなる。

zinc はわずかに寒色（hue ≈ 286、chroma ≤ 0.016）で、これが「地は無彩、色は信号」を
成立させている: 地が寒色側に寄っているぶん、暖色の信号（タリーレッド・琥珀）だけが
色として立ち上がる。

**3 値の一致は `web/tests/design-tokens.test.ts` が両方のファイルを読んで検査する**。
同じ決定が 2 箇所（`index.css` と `scripts/gen-favicon.mjs`）に書いてあるので、
片方だけ動かすと黙って食い違う。

例外は 1 つだけ。**浮いた面（`--card` / `--popover`）はライトで純白**を使う ---
ポップオーバー（録画の絞り込み・チャンネル選択）はオーバーレイを持たないので、
地と同色だと輪郭が罫線と影しか無くなる。

## 色は信号のみ

**恒常的な装飾色を持たない。** チャンネル色・アバター色・ブランドアクセントは入れない。
色が点いている箇所は、そのとき何かを主張している箇所に限る。

サイドバーの現在地は無彩色の `foreground` を地に塗り、文字とアイコンに
`background` を使う反転で示す。アクセント色を持たない Rokuban では、この中立な塗りが
HIG の「サイドバーのアイコンはアプリのアクセント色を使う」に相当する。
色相を足さず、選択中の画面だけを明るさの反転で示す。

キーボードフォーカスは意味色を使わず、`--scanline` の 2px 実線リングで示す。
現在地の反転面にフォーカスが重なる場合は、2px のオフセットを置いてリングを
ページの地に出す。

| 信号 | トークン | 意味 | 形 |
|---|---|---|---|
| タリーレッド | `--tally` / `--tally-foreground` | いま電波に乗っている（録画中・現在時刻） | **塗りと線のみ**（点灯した札・playhead） |
| 琥珀 | `--warning` | 警告（チューナー不足・条件なしのルール・期間指定の副作用） | 文字 + 淡い地 |
| destructive | `--destructive` | 取り返しがつかない・壊れた（失敗・ドロップ・EPG から消失・削除操作） | 文字 + 淡い地 |
| チャプター編集 | `--chapter-cut` / `--chapter-cut-muted` / `--chapter-selection` | カット区間と選択中の境界 | 区間の面・選択位置（両テーマで固定） |
| ジャンル淡色 | （Tailwind の 50 / 950） | 番組の種別。読まずに帯の並びを掴むための補助 | セルの淡い地 |

補助導線は `text-muted-foreground` の文字とアイコンで押せることを伝える。画面遷移には末尾の
`ChevronRight`、同じ画面でダイアログを開く操作には先頭の `Plus` を置く。枠・背景・アクセント色を
常設せず、有効スイッチの状態表示と競わせない。

番組表のジャンル凡例は色と語の対応表であり、色だけに意味を持たせず再認を助ける。
放送終了セルの減光は状態色を増やさず、ジャンル淡色に muted の面を重ねる濃淡の変更とする。

**タリーは塗りと線にしか使わない。文字色にはしない。** 11px〜14px の赤い文字は
ダークの地に対して AA（4.5）に届かない。塗りにすれば「紙白の文字 / タリーの塗り」の
比で測れて、両テーマとも 4.5 を超える。この規律のおかげで「点灯している」という
比喩と読みやすさが同じ方向を向く。

**リストの「放送中」には色を使わない**（太さだけ）。チャンネル数ぶんの行が同時に
放送中になるので、赤くすると信号ではなく地になる。「いま」を主張するのは
番組表グリッドの線と札（画面に 1 本 / 1 枚しか出ない）。

`--tally` にだけ `--tally-foreground`（塗りの上に載る文字）を持たせ、`--warning` には
対の前景色を作っていない。琥珀は塗りに使わないので書き手がいない ---
使う人がいないトークンは足さない（CLAUDE.md 不変条件 11）。琥珀を塗りに使いたく
なったら、そのコードと同じ PR で `--warning-foreground` を足す。

**タリーレッドと destructive は色相が近い**。**区別は色ではなく形が担う**。
タリーは「点灯」なので塗り（`bg-tally` + 紙白の文字）、destructive は「壊れた」なので
文字と淡い地（`text-destructive` + `bg-destructive/10`）。同じ赤でも、
塗られているかどうかで「いま動いている」と「壊れた」が見分けられる。

この排他性のために外した色が 1 つある。**日付ストリップの「日 = 赤 / 土 = 青」は使わない。**
カレンダーの慣習ではあるが、赤が画面の中で 2 つの意味を持つと、
タリーが「いま電波に乗っている」を主張できなくなる。週末は濃さで立てる
（墨 = 週末 / 走査線グレー = 平日）。土と日のどちらかは文字自身が言うので、
色で区別する必要はない。

**沈黙は保証ではない。** 警告を出さないことは「大丈夫」を意味しない
（[data.md](../data.md) §6.5）。「競合なし」「収まります」を緑で出すことはしない ---
判定の盲点はすべて「警告を見逃す」方向に偏っている。緑の信号色を持たないのはこのため。

### コントラストは毎回測る

**数値はここに転記せず、実ブラウザで毎回測る。** 文字の下限は 4.5、面と線は 3。
半透明の地は、文字が実際に重なる背景へ合成してから比を出す。

判定は要素の背景だけでなく、祖先にある最初の不透明な面まで重ねる。文字の要素だけを掴んで背景が透明のままなら、判定そのものを失敗させる。

Chromium の計算値から実画素を読めない色表現があるため、画素を採取して判定する。背景画像に直接置いた色も採取できる形にする。

下限を割る組み合わせは除外しない。失敗バッジはライトの destructive の明度を下げ、色相を動かさず、sRGB に収まる彩度まで下げている。録画中との区別は塗りと文字、文字と淡い地の形で保つ。

一覧行の hover は 4 画面で `bg-muted/40` と `text-muted-foreground` を組み合わせ、ライトとダークの両方で測る。録画一覧の選択行にも同じ面を使う。hover は Lighthouse の対象外だが、常時見える選択行は監査対象である。

ライトでは `bg-muted` の上に `text-muted-foreground` を置くと下限を割る。背景値は変えず、該当する小バッジの文字を `text-foreground` にする。foreground は無彩の地の値であり、「色は信号のみ」と矛盾しない。

- `bg-muted` 上の文字色の網羅は測定結果の出力が権威である。出力に無い組み合わせは未測定であり、合格とは見なさない。
- **未検証**: ルール読み込み中のバナー、エンコード設定行の hover、destructive ボタンの hover、ブレーカーバナーの理由文は測っていない。他の不透明度の実測から外挿しない。
- モバイル番組のチャンネルピッカーのトリガーは `bg-background` と `text-foreground` の対で、地の無彩 3 値として測る。古い「トリガーが読めない」という観測が指した要素は未同定である。リモコン番号タグは開いたポップオーバーの候補側にあり、閉じた状態では DOM に存在しない。

<details><summary>根拠</summary>

- コントラストの実ブラウザ判定: `pnpm e2e:design`、`web/e2e/design.mjs`
- 背景の合成と画素採取: `getComputedStyle`、`canvas` の `fillStyle`、`getImageData`、`background-image`、カスタムプロパティ
- 除外リストを置かない判定: `knownGaps`
- 行 hover と選択行: `components/program-row.tsx`、`components/recording-row.tsx`、`pages/reservations.tsx`、`pages/home.tsx`
- `bg-muted` の文字色修正: `ReservationVerdictBadge`、`components/ui/chip.tsx`、`components/day-strip.tsx`、`text-foreground`
- ルール読み込み中の未測定面: `RuleSourceBanner`、`components/rule-form.tsx`、`bg-muted/40`
- RuleSourceBanner の色: 本体は `text-muted-foreground` を持たず、エラー枝は `text-destructive`
- エンコード設定の hover: `components/encode-settings-fields.tsx`、`hover:bg-muted/60`、`text-muted-foreground`
- destructive の未測定面: `Button variant="destructive"`、`circuit-breaker-banner.tsx`
- 録画詳細とチャンネル候補: `/recordings/$id`、`RecordingDetail`、`ChannelOption`、`keepMounted`
- トークンの権威と測定対象: `web/src/index.css`、`--warning`、`--destructive`、`--tally`
- 半透明の警告文字の組み合わせ: `text-warning`、`bg-warning/10`、`oklch(...)`
- 地だけを基準にした場合の過大差: 実際の半透明面との比より 0.5〜0.7 高い
- hover の測定値: `hover:bg-muted/40`、`hover:text-foreground`
- 未測定 hover 面: `hover:bg-muted/60`、`hover:bg-destructive/20`、`text-destructive/80`
- destructive の hover 面は基底の `/10` より濃い `/20`: `hover:bg-destructive/20`
- ブレーカーバナーの未測定文字: `circuit-breaker-banner.tsx`、`text-destructive/80`、`bg-destructive/10`
- foreground を適用した録画ラベル: 完了、取り込み、site
- foreground を適用した番組ラベル: sticky 日付、グリッド、チャンネル番号
- foreground を適用した他のラベル: `ReservationVerdictBadge`、ルールの「無効」、ライブの種別・番号
- foreground を適用した画面群: 録画一覧、予約一覧、ホーム、番組リスト
- foreground を適用した録画・番組要素: `RecordingRow`、`ProgramRow`、`ChannelPicker`
- foreground を適用した live / rule 要素: チャンネル種別タグ、リモコン番号タグ、無効ラベル
- chip / day-strip の hover 面: `components/ui/chip.tsx`、`components/day-strip.tsx`、`hover:bg-muted`
- 実測済みの録画詳細: `RecordingDetail` の「チャンネル」`<dt>`、ページ地との文字色
- 同じ色を使う説明文と品質イベント: `RecordingDetail`。destructive の淡い地: `bg-destructive/10`
- ポップオーバーの未マウント状態: `keepMounted` を使わない `ChannelOption`

</details>

### 操作標的の寸法は基準を先に決めて実測する

形を変える前に、実ブラウザで測る標的と下限を固定する。DOM 属性やクラス名から寸法を推測しない。

現在の基準は物理端末の pt / dp ではなく、ブラウザの CSS px である。

| ポインタ / 用途 | 最低寸法 | 根拠と適用 |
|---|---:|---|
| fine / coarse の全操作標的 | 24 × 24px | [WCAG 2.5.8 Minimum (AA)](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum)。実効 hit 寸法で判定し、24px 未満は spacing 例外で見逃さない |
| coarse で頻度の高い主操作 | 高さ 44px 以上 | 行の予約・取消・ライブ、日付セル、チャンネル候補、ライブのチャンネル一覧。日付セルの幅は均等配分を保つため 24px 以上にする |
| coarse のモバイル主ナビ | 高さ 56px | safe-area と合わせて 44px 以上を確保し、4 項目の一等地を維持する |

全ボタンを 44px にはしない。管理画面の fine 操作は 24〜36px 程度に保ち、行の主操作や親指で頻繁に触る面だけ 44px にする。予約行の右端列と操作の重なりは、予約 UI の規則に従う。

測定では、実際に見える標的だけを列挙し、visual 寸法と hit 寸法を分ける。隠れた要素は除外し、擬似要素が当たり判定を広げる場合は実効寸法へ加える。標的間隔も記録するが、全面リンクと行内操作など意図した重なりがあるため、全画面に一律の gap は課さない。

- fine のデスクトップと coarse の 360px モバイルで測る。
- 実寸は 24px の下限と、役割ごとに決めた 44px の契約で判定する。
- undo トーストの action は 32px、close は 28px として判定に含める。

44 × 44px は強い参考線だが、WCAG AAA、Apple、Material の指針は対象と単位が異なる。CSS px にそのまま置き換えず、用途とポインタを分けて測る。

<details><summary>根拠</summary>

- 寸法の基準を先に決める: [不変条件 11](../invariants.md)
- 要素の列挙と `getBoundingClientRect()` の測定: `web/e2e/design.mjs` の ④-A、Playwright
- 測定画面: 番組、検索、予約、録画、ルール、ライブ
- visual / hit 寸法、最小エッジ間隔、意図的な重なり件数を出力する
- 対象 selector: `button, a[href], [role="button"], [role="switch"], input, select, summary`
- 非表示要素と skip link の扱い: `display:none`、`visibility:hidden`、`overflow`、`sr-only`、Tab 後の既存判定
- coarse の日付セルは高さ 44px、幅 24px 以上を保つ
- モバイル主ナビは `min-h-14` と safe-area で高さ 56px にする
- 擬似要素と undo 操作: `::before`、`size="sm"`、`size="icon-sm"`
- 予約行の操作列: [reservations.md](reservations.md)
- 参考基準: [WCAG 2.5.5 Enhanced (AAA)](https://www.w3.org/WAI/WCAG22/Understanding/target-size-enhanced)、[Apple のアクセシビリティ指針](https://developer.apple.com/design/human-interface-guidelines/accessibility)、[Material のアクセシビリティ指針](https://m1.material.io/usability/accessibility.html)
- 参考値: Apple は 44pt、Material は 48dp。CSS px へそのまま置き換えない

</details>

## トークン外の生の色値を書かない

コンポーネントが書いてよいのは**意味を持つトークン名**だけ
（`bg-background` / `text-muted-foreground` / `text-warning` / `bg-tally`）。
`bg-amber-700` のような Tailwind 標準パレットの直指定と、`#rrggbb` / `rgb()` /
`oklch()` の生値は書かない。**色の定義は `web/src/index.css` の 1 箇所に集める。**

```sh
cd web && pnpm check:colors
```

CI の lint job でも回る。除外は理由込みで `web/scripts/check-colors.mjs` に並べてある
（トークンの定義 / ジャンル淡色 / 映像のレターボックス /
映像上の操作バーと再生プレビュー / ダイアログの幕）。
**「テーマの色ではないもの」だけが除外の対象**で、ライト・ダークで変わってはいけない
黒（映像が来ていない領域・背後を沈める幕）と、映像上で読みやすさを保つ操作バーがこれに当たる。
操作バーの色は黒い幕・白い塗り・CM のオレンジ・設定メニューの半透明の黒い小窓である。
スマホの設定シートは映像の外（画面の下）に出るので、地の色はトークン（`bg-card`）を使う。

検査が**見ていない**書き方も同じファイルに書いてある（動的なクラス名の合成・CSS の
名前付き色・3 桁の 16 進・`public/` の資産）。「`check:colors` が通った」を
「色をトークン外に書いていない」と読み替えない。

濃さを変えたいときも**別の色を作らず、同じトークンの不透明度を下げる**
（容量超過の帯は琥珀の塗りを薄くし、罫線を濃く残す）。帯のためだけの琥珀を作ると、
琥珀が 2 つになった時点で「琥珀 = 警告」が言えなくなる。**不透明度の実際の値は
コンポーネント側が持つ**（ここに書くと、下限を割って直したときに追随し損ねる）。

## 文字サイズは 4 段に揃える

`xs` はバッジ、`sm` はチャンネル・日時・尺などの副情報、`base` は本文と一覧の題名、
`lg` は見出しに使う。`xs` は 12px のままにする。macOS HIG はキャプションと
フットノートの最小サイズを 10pt としている。この役割には、Rokuban では 12px の
`xs` を選ぶ。13px に上げると `sm`（14px）との差が 1px しかなく、サイズ段階を
見分けにくくなる。小さい文字を別の直値で増やさず、
通常の画面では 4 段のうち最小の `xs` に揃えるためである。実際の寸法は `index.css` と
Tailwind のスケールを権威とし、この文書には転記しない。

直値 11px の例外は、横幅が時刻を表す番組表グリッドとホームの運用時間軸に限る。
該当するラベルは 11px を下限にし、その他の表示は `text-[Npx]` を書かず 4 段のトークンを使う。
`web/tests/font-tokens.test.ts` が、例外の `data-testid` 付き要素の外の直値と、
例外内の 11px 未満を落とす。幅が md
未満の入力欄は 16px にし、iPhone Safari のフォーカス時の拡大を避ける。実機で拡大しない
ことは未検証なので、確認は実機確認の受け入れに残す。

ページ本文は main の幅いっぱいにする。中央寄せや共通の幅上限は置かない --- 全幅の
sticky ヘッダと本文の左端を揃え、棚やカード一覧でも横幅を情報量として使う。
番組・予約・録画など行全体がボタンまたはリンクの一覧では、右端のシェブロンは操作先
ではなく行の目印なので、題名から離れても操作のしやすさは変わらない。番組リストと
検索結果は広い画面（1536px 以上）で番組説明の先頭 1 行を別列に置き、題名の列幅を
固定して説明の左端を行ごとに揃える。説明は折り返さず、仮想化行の高さを保つ。
放送局を横に並べる番組表グリッドも、横幅が情報量なので同じく全幅である。

## 数字は tabular-nums

時刻・尺・サイズ・ドロップ数・PID は縦に並ぶので、等幅数字にしないと桁が揃わず
「並べて比べる」という計器盤の唯一の仕事ができない。

**`html` に 1 度だけ当てて全域に効かせる**。**コンポーネント側で個別に指定しない**。
触るたびに指定を思い出す必要をなくすためである。`web/tests/font-tokens.test.ts` が
「コンポーネント側に `tabular-nums` の個別指定が残っていないこと」を検査する。
残っていれば全域適用に統合した決定と矛盾するので、そこで落ちる。フォントと
tabular-nums の実効性（どの書体が実際に等幅を作るか）は
[stack.md](stack.md)「フォントは英数字と和文で 2 書体を使い分ける」にある。

## 走査線は読み込み中・ON AIR の 2 箇所限定

走査線は Skeleton と ON AIR バッジだけに使う。ファビコンの 3 値は構造を流用し、本文で縞の上に文字を置くのは ON AIR だけにする。
空状態には縞を敷かない。Skeleton と同じ質感になると、データが無い状態と読み込み中を見分けにくくなるためである。周期は輝線 1px と間隙 1px の 2px にし、ファビコンと揃える。

「読み込み中」は共通の Skeleton / ListSkeleton に限る。ListSkeleton は 1 リージョンに 1 つの `role="status"` と読み上げ文を持ち、内側の Skeleton は装飾として隠す。すべての一覧の初回読み込みでこの形を使う。

各画面固有の `<p>読み込み中…</p>` は一覧の初回読み込みではないため、個々の status にする。対象はサイト取得、チャンネル選択、ルール条件、番組詳細、由来バナー、ドロップ統計、ライブプレイヤーである。

一覧の続き取得には `role="status"` を付けない。IntersectionObserver でスクロールのたびに mount / unmount すると、連続スクロール中に読み上げが繰り返されるためである。

ListSkeleton は固定高の行が縦に並ぶ一覧に使う。ポップオーバーの幅制約、チップの折り返し、単一行の詳細、列のある表にはこの形を使わない。

- ON AIR の文字は `text-tally-foreground` とし、縞の両方とのコントラストを測る。Skeleton は文字を持たない装飾である。
- 縞の片方にはタリーそのものを使い、もう片方は色相と彩度を保って明度だけ下げる。oklch の補間で色相がずれる方法は使わない。
- 色相・彩度を変えずに明度だけ動かすため、相対カラー構文 `oklch(from var(--tally) calc(l * ...) c h)` を使う。
- 縞の色は `background-color` とグラデーションから読めるよう別々のカスタムプロパティにする。背景画像の中に直接置いた色は測れない。

走査線自体に動きは付けない。他の状態色も静止しているためである。Skeleton の `animate-pulse` は走査線ではなく、読み込み中の明滅である。

<details><summary>根拠</summary>

- 色の値と縞の定義: `web/src/index.css`、`scripts/gen-favicon.mjs`、`--scan-gap`、`--scan-lit`
- ON AIR の文字色と縞の両方を測る判定: `web/e2e/design.mjs`、`text-tally-foreground`
- 用途を Skeleton と ON AIR に制限する検査: `web/tests/scanline-usage.test.ts`
- `<p>読み込み中…</p>` の画面固有 status: サイト、チャンネル、ルール、番組詳細、由来バナー、ドロップ統計、ライブ
- ListSkeleton の読み上げ: `role="status"`、sr-only、内側の `Skeleton` は `aria-hidden`
- 続き読み込みの例外: `IntersectionObserver`、mount / unmount、`role="status"`
- 使用箇所の手動点検: `grep -rln --include='*.ts' --include='*.tsx' --include='*.css' 'scanlines' web/src | grep -v '\.test\.'`
- 走査線の使用元: `components/page.tsx`、`pages/live.tsx`、`scanlines`、`tally-scanlines`
- 使用クラスの境界: `.scanlines`、`.tally-scanlines`、`scanlines`、`tally-scanlines`
- CSS Color 4 の相対カラー構文と oklch 補間: `color-mix(in oklch, ...)`

</details>

## アニメーションは `prefers-reduced-motion` で縮退する

動きは Skeleton の明滅、Popover / Dialog の出入り、Button の押下フィードバックにある。洗い出しは網羅ではない。

個別のクラスごとに縮退を書かず、一つの `prefers-reduced-motion` 規則へ集約する。時間をほぼ 0 に短縮し、animation 自体は消さない。

縮退時と通常時の両方向を実ブラウザで確認する。

<details><summary>根拠</summary>

- 動きのクラスとコンポーネント: `animate-pulse`、`Skeleton`、`slide-in-from-*`、`zoom-in-95`、`fade-in-0`、`Button`、`translate`
- 集約先と animation を消さない理由: `web/src/index.css`、`@media (prefers-reduced-motion: reduce)`、`animation-duration`、`transition-duration`
- 通常時と縮退時の実ブラウザ判定: `web/e2e/design.mjs`
- 動きの全般方針: `html`、`tabular-nums`、`animation: none` は使わない

</details>

## トーストのキーボード到達性

短命トーストの action には Alt+T でキーボードからアクセスできる。Alt+T でフォーカスを
移すことで、フォーカス中にタイマーを止める既存の一時停止もキーボードから利用できる。

## 確認ダイアログの初期フォーカス

HIG は Return だけで alert を閉じないよう、既定ボタンを避けるよう勧める。このため確認
ダイアログを開いた直後は、`role="alertdialog"` の本体にフォーカスを置く。キャンセルも確定も
既定ボタンにせず、本文を読んでから操作できる状態にする。Tab の最初の移動先はキャンセルとし、
Escape で閉じたら開く前のボタンへフォーカスを戻す。
Return / Tab / Escape の実ブラウザ判定は `web/e2e/issue-1233-dialog-focus.mjs` が行う。

## 入力エラーの文言は欄を持つコンポーネントだけが出す

入力欄のコンポーネントがエラー文言を表示するなら、親が受け取るのは保存可否の boolean だけにする。
判定関数が表示用の文字列も返す形では、親が同じ文字列を描画したとき画面に同じ文が 2 行出る。読み上げも重複しうる（未検証）。
名前欄のように欄が自前で文言を表示しない場合は、親が文言を表示する。
Vitest の `afterEach` は、テストが到達した状態の終了時点の DOM にある
`role="alert"` の文言重複を検出するが、途中の状態は見ない。検査が通っても
重複がない保証にはならず、欄を持つコンポーネントだけが文言を出す規則が本体である。

## 角は立てる

`--radius` は `0.25rem`。shadcn 既定の `0.625rem`（10px）は SaaS のカードの丸みで、
1 行 56px の密な一覧に対して過剰に柔らかい。計器のパネルとスイッチは角が立っている。
`--radius-*` の派生段（0.6〜2.6 倍）はこの値に追従する。

## 頻度 3 段

配置は実装順ではなく**触る頻度**で決める。

| 段 | 対象 | 置き場 |
|---|---|---|
| 一等地 | 番組・録画（シリーズを含む） | 最短の導線。モバイルは親指の届く位置 |
| 中間 | 予約・ライブ | 一等地の次 |
| 端 | 検索・ルール・破壊的操作 | 意図して探しに行く位置 |

切替の形は URL ではなく、ユーザーが選ぶ意味で決める。同じデータの見せ方を変えるなら
segmented control、データを絞る条件なら filter chips、別の画面内容へ移るなら tabs にする。

- **segmented control**: 録画 / シリーズ、番組のリスト / 番組表、予約のシリーズ / 時間順
- **filter chips**: 予約一覧の「すべて / 要確認」。日付 strip は既存の塗りつぶしセルを維持する
- **tabs**: 録画の「ライブラリ / ごみ箱」、ホームの「見る / 管理」。選択中は無彩の下線で示す

操作標的の大きさは頻度とは独立して決める。coarse pointer で表示中の操作要素は
44×44 CSS px 以上の hit 領域を確保し、fine pointer では既定 Button の高さを 32px に保つ。

単一世帯の運用なので、頻度の仮説は自分の行動で検証してよい。

**「端」に寄せる実装手段は overflow メニュー（`components/ui/dropdown-menu.tsx`）
である**。`@base-ui/react` の `Menu` を shadcn の作法でラップした標準部品で
ある。破壊的・稀な操作はこれに乗せ、開く/編集のような主操作とは同格に並べない。
新しい破壊的・稀な操作を行に足すときは、新しい overflow の作法を都度作らずこの
部品を再利用する。適用先は増減しうるので、ここには数え上げない --- 実際の
適用箇所は `dropdown-menu` の import 元を grep すれば分かる。

## 合否は画素で測る

**色は jsdom では測れない。** Tailwind のクラスは解決されないし、oklch も計算されない。
`pnpm test` が全部通っても、色については何の保証にもならない。判定手段は 2 つに分かれる。

| 手段 | 見るもの | CI |
|---|---|---|
| `pnpm e2e:design` | **実ブラウザの実画素**。主要画面 × ライト / ダーク × デスクトップ / モバイルのスクリーンショットと、状態色・地の無彩性・コントラストの機械判定 | 載せない |
| `pnpm check:colors` | トークン外の生の色値（実ブラウザ不要） | lint job |
| `pnpm test`（`tests/design-tokens.test.ts` ほか） | 実ブラウザ無しで文字列として言えることだけ ---「3 値がファビコンと一致」「地と字が 3 値を参照している」「信号色がライト・ダーク両方にある」「どのトークンのクラスが当たっているか」 | web-test job |

トーストが多行に折り返したときのメッセージと action / close の縦中央一致も、`web/e2e/design.mjs` の実ブラウザ判定で測る。

**変えた状態色は必ず `e2e:design` の判定に載せる。** 判定の外に 1 箇所でも置くと、
そこだけ既定値へ静かに戻っても全部緑のまま通る。いま載っているのは録画中バッジ・
失敗バッジ・チューナー不足バッジ・番組リストの時刻（色が付いていないこと）・
現在時刻の線と札・容量超過の帯・ルールの「条件なし」。

サイドバーの現在地、現在地に重なるリング、共通 Button のフォーカスリングは
ライト / ダークそれぞれで PNG の実画素を採り、ページ地とのコントラストを 3:1
以上とする。Button は半透明リングへ戻ると border-color の判定だけでは検出できないため、
描画済みリングの画素そのものを測る。
非現在地リンクと畳んだサイドバーのリングも同じく実画素で測る。`ring-ring/NN` と `outline-ring/NN` の不透明度修飾は `check:colors` が禁止する。

手順は [web/e2e/README.md](../../web/e2e/README.md) §デザイン。
**判定を足すときは、直す前の実装で実際に落ちることを確認する。**

**`e2e:design` は CI に無いので、手元で走らせて exit code を見るところまでやる**。
判定側が DOM の変化（バッジが `<span>` から `<a>` に変わる等）に追随できていないと、
locator が空を掴んで赤くなる。しかし誰も走らせなければ数マイルストーン気付かれない
（実際に起きた）。

`e2e/design.mjs` は API を `page.route` で丸ごと差し替えるので、mirakc も実チューナーも
DB も要らない。時刻も固定してあり、ショットの差分は実装の差分だけになる。

## ダークは OS 設定に追従する

ダークへの到達手段は `prefers-color-scheme` への自動追従とする。
明示的な切り替え UI は持たない（下記「端末ごとの個人設定は localStorage に置く」）。

初回フレームだけライトになることを避けるため、`index.html` の inline script が React の起動前に
`html.dark` を付ける。起動後は `main.tsx` が OS 設定の `change` を購読して同じクラスを同期する。
`.dark` クラスの経路は、実ブラウザ判定（`e2e:design`）で使うために残す。

## 端末ごとの個人設定は localStorage に置く

**端末ごとの見た目や操作の好みは `localStorage` に置く。**

Rokuban は利用者という概念を持たない（[api/deployment.md](../api/deployment.md) §認証）。
サーバーに置いても「世帯でひとつ」の値にしかならず、端末ごとの値にはならない。
スマホと PC でテーマや密度が違うのはむしろ正しい挙動なので、好みは端末ごとで足りる。

サーバーへ移す条件は 1 つだけ。**端末間で食い違うと事実が壊れる項目**（視聴済みの印など、
「どちらの端末が正しいか」を利用者が判定できない値）が出たときに、そのとき初めて表を作る。
それは個人設定ではなく世帯グローバルな事実なので、利用者ごとの表にはならない。
再生位置と視聴済みの印はこの条件に当たるため、`recording_playback_positions` と
`recording_watched` に移した
（[recordings.md](recordings.md) §視聴状態）。

| 対象 | キー | 備考 |
|---|---|---|
| テーマ | 持たない（OS 追従のみ） | 上記「ダークは OS 設定に追従する」 |
| サイドバーの畳み | `rokuban:sidebar:collapsed` | [shell.md](shell.md) |
| 再生速度 | `rokuban:playback-rate` | **端末ごとに 1 つ**（VOD と追っかけで共有） |
| チャプター編集中の再生速度 | `rokuban:chapter-edit-playback-rate` | 通常再生とは分けて端末ごとに 1 つ |
| 録画一覧の表示形式（リスト / カード） | `rokuban:recordings:view` | 出す情報は同じで並べ方だけが変わる |
| 番組表の表示形式（リスト / 番組表） | `rokuban:programs:view` | URL の `view` が優先。モバイルは常にリスト |
| 番組表の時間軸の縮尺 | `rokuban:programs:grid-scale` | 120 / 240 / 480 px/時。URL には持たない |
| 予約一覧のまとめ方（シリーズ / 時間順） | `rokuban:reservations:group` | 同じ情報の並べ替えだけを変える。既定はシリーズ |
| 最後の検索条件 | `rokuban:search:last` | URL が優先（下記） |
| ホームのモード | `rokuban:home:mode` | `watch` / `ops`。URL の `?mode=` が優先し、初回既定は `watch` |

**再生速度を録画ごとに持たない**のは、速度が「この録画をどう見るか」ではなく「自分が
どう見るか」の好みだから。録画を変えるたびに 1 倍へ戻ると、毎回選び直しになる。
チャプター編集中の速度も端末ごとの好みだが、CM 確認のために上げた値が通常視聴へ
残らないよう、`rokuban:playback-rate` とは別に保存する。

### メディア操作は標準 controls と重ねない

自前の操作 UI は、次のどれかを担う場合に置く。

- 再生中の `<video>` の状態だけでなく資源を切り替える（プロファイル、字幕、追っかけの
  offset、原本 TS の取得）
- アプリやサーバーの状態を動かす（ライブ視聴の開始など）
- 標準 controls にその操作が無いことを対象ブラウザで実測した

再生・一時停止、シーク、音量、再生速度、PiP は、標準 controls が提供するブラウザでは
そちらを使う。同じ値を自前 UI と標準 UI の両方から変更できると、別々の表示や状態に
なって利用者を迷わせる。設定の保存が Rokuban 側にあることは、自前の操作 UI を置く
理由にしない。たとえば再生速度は、標準 controls の `ratechange` を端末の
`localStorage` に保存し、新しい VOD / 追っかけの `<video>` に `defaultPlaybackRate` として
適用する。通常のライブ視聴は別扱いにし、未検証のネイティブ HLS 再生速度を適用しない。

2026-09-23 の Chromium 151 実測では、標準 controls の `⋮` メニューに Playback speed と
Picture in Picture があった。そのため、録画プレーヤーから再生速度セレクトと PiP ボタンを
外した。ブラウザごとに標準バーの機能は異なる。Rokuban 独自の操作を追加する前に、対象
ブラウザで標準 controls に機能が無いことを実測する。画面に置かないキー操作は、標準
controls に無い追加ショートカットだけを残し、入力欄や `<video>` にフォーカスがある間は
処理しない（[recordings.md](recordings.md)、[live.md](live.md)）。

### 検索条件は URL が正、localStorage は URL が空のときだけ

押した検索の条件は `/search?cond=<JSON>` にも載る。共有・ブックマーク・リロードで
同じ結果に戻るのは URL の仕事で、これは個人化ではなく**資源の同定**だから
（`/recordings` の絞り込みと同じ判断）。

localStorage の「前回の条件」は、**URL が条件を持たないときの初期値にしか使わない。**
順序は `?ruleId=`（ルール編集。条件の正本はルール）> `?cond=` > localStorage > 空。
復元するのはフォームだけで、**開いただけでは検索しない** —— 押していない操作が
起きたのと同じになる。

URL に載せる値は openapi 由来のスキーマ（`api/zod.ts`）でまるごと検証し、壊れた条件は
条件なしとして開く。localStorage 側も**読むたびに同じスキーマで検証する**（保存した
あとに openapi が変わる）。

### 設定ページは作らない

設定は「端」（上記「頻度 3 段」）だが、専用ページも作らない。**どの項目も、その画面を
見ているあいだにしか意味を持たない**ので、操作点そのものに置くほうが近い。
再生速度は VOD / 追っかけの操作バーの再生設定、表示形式は一覧のヘッダ、検索条件は検索フォーム
自身が持つ。

設定ページを作る条件は「**どの画面にも属さない項目**が出たとき」。それまでは、
1 画面 1 項目の設定を集めたページは、探しに行く手間を増やすだけになる。

### 持たないと決めたもの

- **番組表の局幅・フォントサイズの数値設定。** 列幅は画面幅への clamp で自動的に
  決まる（[programs.md](programs.md)）。時間軸の縮尺だけは、短い番組を正確に選ぶ
  判定基準として 3 段階を持つ（[programs.md](programs.md)）。
- **ボトムタブの項目カスタマイズ・並べ替え。** 頻度 3 段は設計の主張そのもので、
  並べ替えを渡すと主張が消える。単一世帯なので、頻度の仮説は自分の行動で検証できる
- **一覧の 1 ページ件数。** 一覧はページ番号を持たず、続きを継ぎ足す形（録画は
  スクロールでの追加取得、検索は「さらに表示」）なので、設定する対象が無い
- **行の高さ（密度）を表示形式とは別に持つこと。** リストとカードの 2 択で足りる。
  つまみを 2 つ渡すと、組み合わせの数だけ「誰も見ていない見た目」が増える

### localStorage の作法

- キーは `rokuban:` 接頭辞のあと関心事を先頭に置き、区別に要るぶんだけ `:` で
  スコープを足す（上の表がすべて）。例:
  - スコープ無し: `rokuban:playback-rate`
  - 画面:関心事: `rokuban:recordings:view`
- **読み書きは必ず try/catch で囲む。** private mode や設定でのストレージ無効化では
  参照そのものが例外を投げる
- **保存が無い状態が既定値。** `localStorage` を丸ごと消しても壊れる画面があってはならない。
  ストレージを触るテストは自分で後片付けする（`afterEach` で clear。
  `app-shell.test.tsx` だけは `beforeEach`。どちらでもよい）。
  そのうえで**既定値だけで成立するテストを書く** --- 片付けを忘れた隣のテストに
  汚されても落ちない状態にしておけば、順序に依存する壊れ方を持ち込まずに済む
- **初回フレームで見えてしまう値だけは `index.html` の inline script で React 起動前に当てる。**
  いまそこにあるのは `prefers-color-scheme` の判定だけ。地の色やレイアウトの密度を保存する
  ようになったらここに足すが、起動前に走るコードなので、初回フレームに映らない項目は足さない
