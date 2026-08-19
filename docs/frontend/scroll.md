> [frontend.md](../frontend.md) の一部。索引から辿る

# 進行方向・遡行の読み込み

番組リスト（[programs.md](programs.md)「番組リスト」）の時間窓の継ぎ足しと、
先頭挿入時のスクロール位置の復元。窓の管理は `pages/programs.tsx`、仮想化と
復元は `components/program-list.tsx`、純関数は `lib/previous-day-window.ts` /
`lib/scroll-preservation.ts`。

**この領域は jsdom で原理的に検出できない壊れ方を繰り返した。** 「`pnpm test` が
通った」を根拠に 3 回リリースし、3 回とも実機で壊れていた（毎回別の壊れ方）。
この失敗が「実装より先に実ブラウザの判定手段（`web/e2e/`）を作る」という規律
（CLAUDE.md「テスト規律」/ `web/e2e/README.md`）の出所である。

## 現行仕様の要約

- **進行方向（先の時間）は 6 時間ぶんずつ（`windowHours`）の自動読み込み +
  ボタンの受け皿。遡行（前の時間）はボタンのみで、1 暦日（前日 0 時〜当日 0 時）
  単位で読む**（`lib/previous-day-window.ts` の `previousDayWindow`）
- 遡行の下限は「now を時で切り捨てた時刻」。下限に達したらボタンを出さない
- 先頭挿入時のスクロール位置は、**アンカーの `programId` から挿入後の添字を
  引き直して `alignRowTop` を呼ぶ**方式で復元する（`components/program-list.tsx`）。
  DOM 要素を追いかけない
- アンカーは「**sticky 要素（`PageHeader`）の下端より下に見えている先頭行**」を
  選び（`lib/scroll-preservation.ts` の `findAnchorProgramId` / `captureAnchor`）、
  復元先は y=0 ではなくキャプチャ時点の実位置（`topPx`）
- **`alignRowTop` は挿入後に固定 2 回呼ぶ**（間に
  `window.dispatchEvent(new Event('scroll'))` を挟む）
- 「前を読み込む」ボタンは**通常のフロー**に置く（sticky にしない）。ラベルに
  読み込む日付を出す
- **レイアウト・スクロール位置は jsdom で計測できない。** 計測できない環境
  （`domLayoutMeasurable()` が false）では仮想化・IntersectionObserver・
  `alignRowTop` を使わず、合否判定は `web/e2e/` の実ブラウザで行う
  （`web/e2e/README.md`）

## 窓の刻み方

- **進行方向（先の時間）は自動 + ボタンの受け皿。** リスト末尾の番兵を
  IntersectionObserver で見て `fetchNextPage()` を呼ぶ。失敗したら自動を無効化し
  ボタン + エラー表示に落とす（さもないと失敗したまま無限にリクエストを
  投げ続ける）。「さらに読み込む」ボタンは自動が効いている通常時は隠すが、
  消しはしない ---
  番兵が発火しない環境・キーボード操作・失敗後の再試行の受け皿として残る
- **計測できない環境（`lib/list-virtualization.ts` の `domLayoutMeasurable()`
  が false）では IntersectionObserver 自体を作らない。** jsdom は番兵が常時
  可視と判定されるおそれがあり、際限なく読み込みを走らせてしまう。この環境では
  ボタンだけを受け皿にする
- **遡行（前の時間窓）はボタンでのみ行う。上スワイプなどのジェスチャにはしない。**
  理由は 2 つ: (1) Android Chrome の pull-to-refresh がページ最上端での
  オーバースクロールを占有しており衝突する（[programs.md](programs.md)
  「グリッドで横スワイプによるナビゲーションを使わない」と同じ種類の衝突）、
  (2) 上方向の自動読み込みは先頭に差し込んだ直後も番兵が上端付近に残るため
  境界まで連鎖してしまう
- **遡行の下限は「now を時で切り捨てた時刻」。** 放送済み番組の閲覧は
  スコープ外。サーバーの EPG 保持期間の設定には依存させない（クライアント側で
  now を不変条件として持つだけで足りる）。下限に達したらボタンを出さない
- **遡行は 1 暦日（前日 0 時〜当日 0 時）単位で読む。進行方向（先の時間・
  自動読み込み）は 6 時間ぶんずつ（`windowHours`）。** 遡行だけ暦日に揃える
  理由は日付ヘッダの帯 --- 帯は「直前の番組と暦日が変わったか」で決まり
  （`components/program-list.tsx` の `showDateHeader`。直前の番組を表す
  `lastDay` の初期値が空文字列なので、**リストの先頭行には必ず帯が付く**）、
  遡行で差し込む窓の境界が暦日の境界（0 時）でないと、それまで先頭だった行
  （同じ日の続きになる）が帯を失って高さが変わり、下の内容がずれる。境界を
  常に暦日にすれば、この帯の増減による位置ずれが構造的に起きない。進行方向は
  増分読み込みとして機能しているだけで日付ヘッダの帯とは無関係なので 6 時間の
  ままにしてある。`pages/programs.tsx` の `useInfiniteQuery` は pageParam /
  ページの形を `{ startMs, endMs }`（取得した半開区間そのもの）にしてあり、
  `step` のような抽象的なカーソルにしていない --- 進行方向は
  `windowHours` 幅、遡行は 1 暦日幅と、2 方向で窓の刻み方が異なるため、
  共通の「窓の個数」では表現できない。次に遡って読む窓は
  `previousDayWindow`（純関数。前日 0 時が下限より前になる場合は下限で
  打ち切り、それでも下限に達していれば `null` を返して呼び出し側がボタンを
  消す）が決める
- **「前を読み込む」ボタンのラベルに読み込む日付を出す
  （「前を読み込む（8/5(水)）」の形。`pages/programs.tsx` が `previousDayWindow`
  の結果を `lib/format.ts` の `formatDate` で整形し、`ProgramList` の
  `previousDateLabel` prop に渡す）。** 押す前に何が起きるか（どの日が
  増えるか）分かるようにするため。「前を読み込む」という語自体は残す ---
  実機検証で使うスクリプト群がボタンを正規表現 `/前を読み込む/` で探しており、
  ここを削ると見つけられなくなる

## スクロール位置の復元

先頭への挿入はスクロール位置がずれる（Safari はスクロールアンカリングを
実装していないので、ブラウザ任せにはできない）。補正は「アンカーの programId
から仮想化ライブラリ上の添字を引き直し、`alignRowTop` を呼ぶ」方式で行う
（`components/program-list.tsx`）。

- **「前を読み込む」ボタンは `ProgramList` 自身が持つ**（仮想化を持つ
  コンポーネントに復元へ必要な情報 --- 添字・計測値・`virtualizer` そのもの ---
  を全部揃えるため。`pages/programs.tsx` は `hasPreviousPage` /
  `isFetchingPreviousPage` / `previousDateLabel` / `onLoadPrevious` を props で
  渡すだけ）
- 手順: (1) ボタンを押した瞬間、`captureAnchor()`（`lib/scroll-preservation.ts`）で
  「**sticky 要素の下端より下に見えている**行」の `{ programId, topPx }`（その
  時点で画面上のどこに見えていたか）を読む（まだ何も挿入されていないので実際に
  レイアウトされている DOM を安全に読める）。(2) `onLoadPrevious()` を呼ぶ。
  (3) `programs` の更新を検知したら、控えた `programId` から `findProgramIndex`
  （`lib/program-list-key.ts`）で**挿入後の添字**を引き直し、
  `alignRowTop(newIndex, topPx)` を呼んでその行を「元々見えていた画面上の
  位置」に戻す
- **DOM 要素を追いかけず、ライブラリの座標系に乗る。** `scrollToIndex`
  （`alignRowTop` が内部で呼ぶ）は仮想化ライブラリ自身が持つ座標系
  （見積もり→実測の遷移も含めて）を使うので、対象の行が現在 DOM に存在するか
  どうかに依存しない。**挿入後の DOM から同じ行を探して測り直す形は仮想化と
  構造的に両立しない** --- 差し込む量（1 暦日ぶん）はオーバースキャンを大きく
  超えるので、挿入した瞬間アンカーだった行は DOM から消え、`querySelector` が
  `null` を返して補正が一度も呼ばれない（スクロール位置が変わらないまま可視範囲
  だけ再計算され、同じ位置に別の番組が来る）。同じ理由で
  **挿入前後の `scrollHeight` の差分を `scrollBy` に渡す形も採らない** ---
  差し込んだ行は挿入直後まだ見積もり高さでしかなく、かつ高さの差分はリスト全体を
  見るので、遡行中に進行方向の自動読み込みが同時に走ると過補正する
- **アンカーは添字ではなく `programId` で引き直す。** 仮想化の
  `getItemKey`（`components/program-list.tsx`。既定は `(index) => index`）も
  同じ理由で `programId` にしてある --- 先頭への挿入で既存の全行の添字がずれ、
  添字キーのままだと TanStack Virtual の実測キャッシュ（`itemSizeCache`）が
  別の番組の値を引き継いでしまい、総高さと各行のオフセットが狂う
- **アンカーは「sticky 要素の下端より下に見えている行」を選ぶ
  （`lib/scroll-preservation.ts` の `findAnchorProgramId`）。** 画面上部には
  sticky な `PageHeader` が居座っており、その裏に隠れている行は「見ている
  先頭行」ではない。判定は `top >= stickyBottomPx`（sticky の下端より下に
  上端が完全に出ている）。`stickyBottomPx` は `--page-header-height`
  （`components/page.tsx` が実測して書き出す CSS 変数）から実測する ---
  フィルタ行の増減で変わりうる値なので、`captureAnchor` が呼ばれるたびに
  読み直す。現在この帯は `PageHeader` だけがつくる
- **復元は「y=0 に揃える」のではなく、キャプチャ時点の実際の画面上の位置
  （`topPx`）に戻す。** `virtualizer.scrollToIndex(index, { align: 'start' })`
  は常に行の上端を viewport の y=0 に揃えるだけで、sticky の裏に隠れることも、
  押した瞬間にどれだけスクロールしていたか（sticky の下端ぴったりとは限らない）
  も考慮しないため
- **`scrollToIndex` を直接使わず `alignRowTop`（`components/
  program-list.tsx`）に包む。** `scrollToIndex` は「対象行の上端を viewport の
  y=0 に揃え続ける」ことを内部状態（`scrollState`）に保存し、実測値が出揃うまで
  数フレームかけて `getOffsetForIndex` を再評価・再スクロールする
  （TanStack Virtual の `reconcileScroll`。見積もり→実測の遷移を追従するための
  仕組みで、これ自体は有用 --- 外すと挿入した数十〜数百行ぶんの見積もり誤差が
  そのままスクロール位置のズレとして残ることも実機で確認した）。この再評価は
  常に「y=0 に揃える」ことを目指すため、「`topPx` の分だけ y=0 からずらす」
  という補正と競合し、次のフレームで補正を打ち消して y=0 側へ引き戻してしまう
  （実機で確認: 補正直後は正しい位置に見えるが、数フレーム後に静かに y=0 側へ
  戻っていた）。TanStack Virtual の `scrollPaddingStart` オプション（既定 0）は
  実際には `align: 'start'` の基準点そのもの（`y=scrollPaddingStart` に揃える）
  なので、`alignRowTop` はこれを `topPx` に一時的に上書きしてから
  `scrollToIndex` を呼ぶ薄いラッパーにした。`reconcileScroll` は同じ
  `getOffsetForIndex`（`scrollPaddingStart` を見る）を使って再評価するので、
  後続フレームの再評価も同じ基準（y=`topPx`）を使い続けるようになる。
  `virtualizer.setOptions()`（React の再レンダーを経由せず `virtualizer.
  options` を直接書き換える）と、`useWindowVirtualizer` に渡す
  `scrollPaddingStart` の元になる ref の両方を同時に更新する必要がある ---
  react-virtual のアダプタは**次の再レンダーのたびに** `useWindowVirtualizer`
  の呼び出し引数で `this.options` を上書きし直すため、ref を更新しないまま
  `setOptions()` だけ呼んでも次の再レンダーで巻き戻る。`scrollToDayOffset`
  （[programs.md](programs.md)「『既にジャンプ先になっている日』の再タップ」）
  はこの ref を明示的に 0 へ戻してから呼ぶ --- 直前の遡行操作が残した
  非 0 の値に、無関係な別の操作が引きずられないようにするため
- **`alignRowTop` の呼び出し後は `window.dispatchEvent(new Event('scroll'))` で
  'scroll' イベントを同期発火させる。** `window.scrollTo()`（`alignRowTop`
  経由の `scrollToIndex` も内部では同じ）は `window.scrollY` を同期的に
  更新する一方、`virtualizer` が可視範囲の計算に使う内部スクロール位置
  （`getVirtualItems()` が使う座標）は、ブラウザの 'scroll' イベント
  （`window.scrollTo` に対して非同期に発火する。早くても次のフレーム）を
  受けてはじめて更新される --- `useLayoutEffect` の中で補正しても、この間隙が
  1 フレームの跳ねとして描画されてしまう（押下直後に実測 400px 超の跳ね）。ブラウザが
  自発的に発火する 'scroll' イベントは非同期だが、`dispatchEvent` で自分から
  発火させればイベントリスナー（`virtualizer` が登録している）はその場で
  同期的に呼ばれ、発火時点の実際の `window.scrollY`（既に更新済み）を読む。
  react-virtual のアダプタはこの更新を `flushSync` で即時コミットする
  （`useFlushSync` オプション、既定 true）ので、ペイントに間に合う
- **`alignRowTop` は挿入後に 2 回呼ぶ**（`window.dispatchEvent` を挟んで）。
  1 回目は挿入された数十〜数百行の大半がまだ一度も実測されていない
  （`estimateSize` の見積もりのまま）時点での計算なので実際の描画位置とは
  数百 px ズレることがあるが、1 回目の `dispatchEvent` が引き起こす再描画で
  その付近の行が実測されキャッシュが更新されるため、2 回目は更新済みの
  キャッシュで計算し直されて実際の位置によく一致する（実機で確認: 1 回目
  だけだと 1 フレーム分の跳ねが残り、2 回目を足すと消えた。3 回目を試しても
  scrollY は変わらなかった）。「安定するまで」ではなく固定 2 回（jsdom で
  検証できない自前の追従ループを作らない）にしてある
- **`scrollMargin`（`<ul>` の `offsetTop`）は state ではなく ref +
  `virtualizer.setOptions()` で持つ。** 遡行ボタンの有無で `<ul>` の
  `offsetTop` 自体が動くため、`hasPreviousPage` が変わるたびに測り直す必要が
  ある。ref + `virtualizer.setOptions()` なら測定直後の同期的な 1 手で
  `virtualizer` 内部の値まで揃うため、同じコミットで走る別の
  `useLayoutEffect` が古い値を見ることがない。**state で持つとこれが破れる** ---
  「`useLayoutEffect` 内の `setState` はペイント前に同じコミットで反映される」は
  実機で成り立たず、遡行ボタンが消える（`hasPreviousPage` が変わる）のと復元の
  `useLayoutEffect` が同じコミットで走ると、後者は先に実行される `setState` が
  まだ反映されていない古い `scrollMargin` を見て、ボタン高さ（52px）分ずれた
  1 フレームが実際に描画された
- **「前を読み込む」ボタンは通常のフローに置く（sticky にしない。
  `components/program-list.tsx`）。** 読み進めている間はボタンは画面外（上）に
  あり見えない、上端まで戻ると現れる、押すと 1 日ぶんがその下に積まれて
  ボタン自身が画面外へ押し上げられて消える --- この「消える」こと自体が
  「読み込まれた」という視覚的フィードバックになる。画面外からこのボタンを
  押しに行く経路は実在しない --- 特定の日付を見たいならジャンプ先の指定は
  `DayStrip` が担うので、遡行ボタンの用途は「読み込み済みの先頭まで戻ってきて、
  その前日も見たくなった」の一択であり、戻ってくる操作（スクロール）は常に
  クリックより前に完了している。`web/e2e/checks.mjs` もボタンを押す前に
  明示的にリスト上端まで戻ってからクリックする（画面外のボタンを押しに行く
  経路は判定しない）

## ボトムタブの裏に隠れる行

`main` の `padding-bottom`（`--bottom-nav-height`。`components/app-shell.tsx`）は
**ドキュメント最下端まで実際にスクロールしたときにしか**ボトムタブとの重なりを
防げない --- ネイティブの最大スクロール位置は常にその予約領域の直前でコンテンツの
下端が揃うようクランプされるためである。ページ全体スクロール + `fixed` なタブと
いう今の組み合わせでは、それ以外のスクロール位置（初回表示・日付ジャンプ直後を
含む）でたまたま行の境界がタブの上端とずれた位置に来ると、時刻や「予約」ボタンを
含む行がタブの裏に半分だけ隠れた状態でユーザーの目に入る --- これは初回表示に
限った現象ではなく、`fixed` なタブが実際にレイアウト分の空間を確保していないこと
自体に起因する一般的な性質である。

**以前はここに「初回表示だけ、検出した重なり量ぶん `window.scrollBy` で押し出す」
補正があったが削除した。** ページ全体を一律にスクロールさせる方式では、リストの
先頭行は常にその日付ヘッダ（`sticky`）の直下に隙間なく（0px）続くため、末尾側の
重なりを消すのに必要な分だけスクロールすると、**その量とちょうど同じだけ先頭行が
日付ヘッダの裏へ食い込む**（実機計測で確認: 末尾の重なり 29px を消す補正が、
先頭行に同じ 29px の食い込みを新たに作った）。単一のスクロール位置で両端の重なりを
同時に消すことは、行の並びと日付ヘッダの間に恒常的な隙間が無い限り数学的に
できない --- 直した側と同じ欠陥を反対の端に動かしているだけなので、この方式は
不採用にした。

未解決: この重なり自体を無くすには、タブの高さぶんだけ `<main>` の実効高さを
削って中に収める（内側スクロール容器にする、あるいはタブを重ねずに専有領域として
確保する）ような、ページ全体スクロールという今の前提を変える設計判断が要る。
この文書はその判断の権威ではないので、ここでは決めない。

## テストの範囲

**フレーム単位の跳ね・スクロール位置合わせの見た目自体は、jsdom では検証
できない。** `getBoundingClientRect` が常に 0 を返しレイアウトを計算しない
環境なので、`ProgramList` はこの環境では仮想化そのものをバイパスし
`alignRowTop` を呼ばない。実機（Playwright を使った実ブラウザでの計測。
`web/e2e/`）で判定している。「いま見ている日」のスクロール追従も同様に
自動テストでは検証できない（純関数の導出ロジック自体はテスト済み）。グリッドの
「受け入れは実機で行う」（[programs.md](programs.md)）と同じ扱い。アンカー選択の
判定（`findAnchorProgramId`）自体は純関数として両方向（sticky の裏の行を
選ばないこと / 下に出ている行を選ぶこと）テスト済み。

