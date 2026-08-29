> [frontend.md](../frontend.md) の一部。索引から辿る

# 番組リストの時間窓の読み込み

番組リスト（[programs.md](programs.md)「番組リスト」）の時間窓の継ぎ足し。窓の管理は
`pages/programs.tsx`、仮想化は `components/program-list.tsx`、純関数は
`lib/program-list.ts`。

**この領域は jsdom で原理的に検出できない壊れ方を繰り返した経緯がある**
（[web/e2e/README.md](../../web/e2e/README.md) 冒頭）。

## 進行方向の読み込み

- 6 時間（`windowHours`）ぶんずつの自動読み込み + ボタンの受け皿。リスト末尾の
  番兵を IntersectionObserver で見て `fetchNextPage()` を呼ぶ。失敗したら自動を
  無効化しボタン + エラー表示に落とす（さもないと失敗したまま無限にリクエストを
  投げ続ける）。「さらに読み込む」ボタンは自動が効いている通常時は隠すが、消しは
  しない --- 番兵が発火しない環境・キーボード操作・失敗後の再試行の受け皿として残る
- **計測できない環境（`lib/list-virtualization.ts` の `domLayoutMeasurable()` が
  false）では IntersectionObserver 自体を作らない。** jsdom は番兵が常時可視と
  判定されるおそれがあり、際限なく読み込みを走らせてしまう。この環境ではボタン
  だけを受け皿にする
- `pages/programs.tsx` の `useInfiniteQuery` は pageParam / ページの形を
  `{ startMs, endMs }`（取得した半開区間そのもの）にしてあり、`step` のような
  抽象的なカーソルにしていない
- **レイアウト・スクロール位置は jsdom で計測できない。** 計測できない環境では
  仮想化・IntersectionObserver・`scrollToIndex` を使わず、合否判定は `web/e2e/`
  の実ブラウザで行う（`web/e2e/README.md`）

## 遡行（前の時間窓の先頭挿入）はしない

到達経路が「DayStrip で未来の日へジャンプ → 上端まで戻る → 押す」の一択で、
代替（DayStrip で前の日をタップ）が既にある。先頭挿入の位置復元は仮想化・sticky
ヘッダと構造的に衝突し、実機退行 3 回分の機構（`web/e2e/README.md`）を要した。

## ボトムタブの裏に隠れる行

`main` の `padding-bottom`（`--bottom-nav-height`）が防げるのは**ドキュメント
最下端まで実際にスクロールしたときの重なりだけ**で、それ以外のスクロール位置
（初回表示を含む）で行の境界がタブの上端とずれれば、その行はタブの裏に半分入る
（390×844 の実測で 29px。`web/e2e/programs-bottom-nav.mjs` の④が毎回この値を
表示する）。

**「初回表示だけ重なり量ぶん `window.scrollBy` で押し出す」は採らない。** リストの
先頭行は常に日付ヘッダ（`sticky`）の直下に隙間なく続くので、一様にスクロールすると
末尾で消した量とちょうど同じだけ先頭行が日付ヘッダの裏へ入る（実測で 29px → 29px）
--- 同じ欠陥を反対の端に動かすだけである。

未解決: 消すには `<main>` をタブの高さぶん削って内側スクロール容器にする、あるいは
タブを重ねずに専有領域として確保するなど、ページ全体スクロールという前提を変える
設計判断が要る。この文書はその判断の権威ではない。

## テストの範囲

**フレーム単位の跳ね・スクロール位置合わせの見た目自体は、jsdom では検証
できない。** `getBoundingClientRect` が常に 0 を返しレイアウトを計算しない
環境なので、`ProgramList` はこの環境では仮想化そのものをバイパスし
`scrollToIndex` を呼ばない。実機（Playwright を使った実ブラウザでの計測。
`web/e2e/`）で判定している。「いま見ている日」のスクロール追従も同様に
自動テストでは検証できない（純関数の導出ロジック自体はテスト済み）。グリッドの
「受け入れは実機で行う」（[programs.md](programs.md)）と同じ扱い。
