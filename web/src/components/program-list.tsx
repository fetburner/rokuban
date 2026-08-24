import { useWindowVirtualizer } from '@tanstack/react-virtual'
import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
} from 'react'

import type { ProgramListItem, ProgramOverridesInput, Service } from '@/api/generated'
import { Button } from '@/components/ui/button'
import { ProgramRow } from '@/components/program-row'
import { dayKey, formatDate } from '@/lib/format'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import { findProgramIndex, programKeyAt } from '@/lib/program-list-key'
import { programServiceKey } from '@/lib/programs-search'
import { captureAnchor, type AnchorSnapshot } from '@/lib/scroll-preservation'
import { firstIndexForDayOffset, visibleDayOffset } from '@/lib/visible-day'

/**
 * ReservationActions は番組からの予約 / 取消と、番組ごとの実行中状態。
 *
 * 実行そのもの（`useReservationActions`）は `pages/programs.tsx` 側に残っている
 * ---
 * リストとグリッドの両方がこの同じ経路を通る必要があるためで、`ProgramList`
 * 固有の関心ではない。型だけをこちらに置いて `programs.tsx` から import する形に
 * しているのは、`programs.tsx`（複数のタスクが並行して触りうる共有ファイル）の
 * 差分をこの切り出しで最小にするため —— `ProgramList` が要求する形を
 * `programs.tsx` からエクスポートさせると、`ProgramList` を切り出したこの変更が
 * 共有ファイル側にも export の追加という差分を生む。
 *
 * `reserve` の第 2 引数（`overrides`）は issue #132 で足した ---
 * `ProgramRow` の展開パネルで encodeProfiles / keepOriginal を既定から
 * 変えていれば、そのまま overrides の PATCH ボディとして渡ってくる。
 * 既定のままなら `undefined`（overrides の PATCH は呼ばない）。
 */
export type ReservationActions = {
  reserve: (program: ProgramListItem, overrides?: ProgramOverridesInput) => void
  cancel: (programId: number) => void
  isBusy: (programId: number) => boolean
  /** サーバーの値に楽観的な上書きを重ねた「予約済み」集合。 */
  reservedProgramIds: Set<number>
}

/**
 * ProgramListHandle は `pages/programs.tsx` が `ref` 経由で呼ぶ命令的 API。
 *
 * ## なぜ必要か：「既にジャンプ先になっている日」を再タップしたときの無反応
 *
 * `DayStrip` のタップは `dayOffset`（state）を書き換えるだけだと、**既に
 * その日が `dayOffset` になっている**ときに何も起きない ---
 * React は同じ値での `setState` を再レンダーの理由にしないため、クエリも
 * 動かずスクロールもしない。しかしユーザーはスクロールで「いま見ている日」
 * （`visibleDay`）がその日から離れていることがあり、その状態での再タップは
 * 「その日の先頭へ戻る」ことを期待している（実機で確認した不具合）。
 *
 * `scrollToDayOffset` はこの「読み込み済みの日へ戻る」を実装する。見つからない
 * （まだ読み込んでいない日）場合は何もしない --- その場合の対応（クエリの
 * 起点を付け替える）は `pages/programs.tsx` 側の責務。
 */
export type ProgramListHandle = {
  /**
   * 指定した `dayOffset`（暦日）に一致する最初の番組へスクロールする
   * （`virtualizer.scrollToIndex`。② の遡行アンカー復元と同じ機構）。
   * 該当する番組が読み込み済みの `programs` に無ければ何もしない。
   */
  scrollToDayOffset: (dayOffset: number) => void
}

/**
 * 行の高さの見積もり（px）。`ProgramRow` は `min-h-14`（56px）+ 上下パディングで
 * 未展開時はおよそこの高さになる。展開時や日付ヘッダの行は実測（`measureElement`）
 * で上書きされるので、ここでの精度は初期スクロール位置の見た目にしか効かない。
 */
const estimatedRowHeightPx = 72

/** 画面外にも描いておく行数。スクロール中に空白が見えないための余白。 */
const overscanRows = 8

/**
 * ProgramList は番組リスト（時間順フラット + sticky 日付ヘッダ）。
 *
 * `pages/programs.tsx` から切り出した専用ファイル。理由は 2 つ:
 * 1. `programs.tsx` は複数タスクが並行して触りうる共有ファイル（CLAUDE.md
 *    「並行作業」）なので、この切り出し自体の差分をそこで最小にしたい
 * 2. 仮想化の実装（TanStack Virtual との結線）をグリッド（`program-grid.tsx`）
 *    から独立させ、それぞれの仮想化方式の違いを 1 ファイルずつに閉じ込める
 *
 * ## なぜ TanStack Virtual を使うか（グリッドは自前実装なのに、ここはライブラリ）
 *
 * グリッド（`program-grid.tsx`）が自前実装を選んだ理由は「縦軸が連続量で、
 * 番組セルは目盛りをまたぐ区間なので、行の並びを前提とする仮想化ライブラリの
 * 模型に乗らない」ため（docs/frontend.md）。リストはその逆で、**行の並びそのもの**
 * なので TanStack Virtual の想定する形に素直に乗る。自前実装を選ぶ理由が無い。
 *
 * ## なぜ `useWindowVirtualizer` か
 *
 * このリストは内側にスクロール容器を持たず、**ページ全体がスクロールする**
 * （グリッドは `h-full overflow-auto` の内側スクロール容器を持つ。両者は
 * スクロールモデルが違う）。`useVirtualizer` は自前のスクロール容器を要求するので、
 * ページ全体のスクロール位置と `window.innerHeight` を見る `useWindowVirtualizer`
 * を使う。
 *
 * ## sticky な日付ヘッダをどう保ったか
 *
 * 仮想化の一般的な実装（各行を `position: absolute` + `transform: translateY(...)`
 * で配置する）にすると、行の中に置いた `position: sticky` な日付ヘッダが効かなく
 * なる（sticky は通常のフローに乗った要素にしか効かない）。
 *
 * ここでは行を **通常のフローのまま** にして、可視範囲より前後の分だけ上下に
 * 高さぶんのスペーサー（`paddingTopPx` / `paddingBottomPx`）を置く方式にした。
 * 通常のフローなので既存の sticky マークアップをそのまま維持でき、変更量も
 * 小さい（絶対配置 + transform 方式は、スクロール位置に応じて layout を
 * 作り直す分マークアップの変更が大きい）。
 *
 * ## 行の高さは可変（動的計測）
 *
 * `ProgramRow` は展開すると詳細を出す（段階的開示）ので固定高さにできない。
 * 各 `<li>` に `ref={virtualizer.measureElement}` を渡し、実測させる。
 *
 * 実測値（`itemSizeCache`）は既定では添字（`(index) => index`）で引かれるため、
 * 遡行（前の時間窓の読み込み）でリスト先頭に行を差し込むと既存の全行の添字が
 * ずれ、記録済みの実測値が別の番組のものとして使われてしまう。`getItemKey` に
 * `programKeyAt`（programId ベース）を渡して防いでいる（下記コード参照。
 * docs/frontend.md「番組リスト」も参照）。
 *

 * ## 計測できない環境（jsdom）では全部描く
 *
 * `measureElement` は `getBoundingClientRect` で高さを読む。jsdom はレイアウト
 * エンジンを持たないため常に 0 を返し、動的計測をそのまま使うと可視範囲の計算が
 * 際限なく前進し続ける不具合を実機測定で確認した（詳細は
 * `web/src/lib/list-virtualization.ts` のコメント）。`domLayoutMeasurable()` で
 * 環境を検出し、計測できない環境では `measureElement` を一切使わず、仮想化その
 * ものをバイパスして全行を通常のフローで描く（`web/src/lib/epg-grid.ts` の
 * `visibleTimeWindow` / `visibleColumnRange` と同じ「未計測なら間引かない」）。
 *
 * ## 「いま見ている日」の通知
 *
 * `onVisibleDayChange` は `DayStrip` のハイライト用に、可視範囲の先頭の番組が
 * 属する日を通知する。`virtualizer.range`（スクロール位置と `estimateSize` から
 * 計算され、`measureElement` の実測とは独立）の `startIndex` を使うので、
 * `renderAll` 分岐の影響を受けない。導出そのものは `lib/visible-day.ts` の
 * 純関数（`programs` と先頭インデックスと `now` から dayOffset を返す）に
 * 切り出してあり、スクロールへの実際の追従（jsdom はレイアウトを計算しないため
 * 検証できない）とは別にテストする。
 *
 * ## 「前を読み込む」ボタンと遡行のアンカー復元（3 回目の修正で `programs.tsx` から移設）
 *
 * ボタンをここに置くのは、復元に必要な情報（仮想化の添字・計測値・
 * `virtualizer` そのもの）を持っているのが `ProgramList` だからである。
 * `hasPreviousPage` / `isFetchingPreviousPage` / `onLoadPrevious` は
 * `pages/programs.tsx` の `useInfiniteQuery` から来る値をそのまま渡すだけの props。
 *
 * 手順:
 * 1. ボタンを押した瞬間、`captureAnchor()`（`lib/scroll-preservation.ts`）で
 *    「sticky 要素の下端より下に見えている行」の { programId, topPx }（その時点の
 *    画面上の位置）を読み、ref に控える。この時点ではまだ何も挿入されていない
 *    ので、実際にレイアウトされている DOM を安全に読める
 * 2. `onLoadPrevious()` を呼ぶ（`fetchPreviousPage()`。先頭に新しい窓が積まれる）
 * 3. `programs` が変わったら（挿入完了）、控えた programId から
 *    `findProgramIndex`（`lib/program-list-key.ts`）で**挿入後の添字**を引き直し、
 *    `alignRowTop(newIndex, topPx)`（下記）でその行を「元々見えていた画面上の
 *    位置」に戻す
 *
 * 以前は手順 3 を「同じ programId の行を DOM から再度探して top を測り直す」
 * 方式（`locateAnchorTop` + `window.scrollBy`）でやっていたが、これは**仮想化と
 * 構造的に両立しなかった**。挿入直後の時点で、アンカーだった行はオーバースキャン
 * （8 行）の外へ弾き出されて DOM から消えている（差し込む量は 6 時間ぶん・約 79
 * 番組・約 5700px で、オーバースキャンの576pxを大きく超える）。消えた要素は
 * 見つからないので `null` が返り、`window.scrollBy` は一度も呼ばれず、
 * 「スクロール位置が変わらないまま可視範囲だけ再計算され、同じ位置に別の番組が
 * 来る」形で壊れていた（実機で確認済み。詳細は `lib/scroll-preservation.ts` の
 * コメント）。`virtualizer.scrollToIndex` は仮想化ライブラリ自身が持つ座標系
 * （見積もり→実測の遷移も含めて）を使うので、対象の行が現在 DOM に存在するか
 * どうかに依存しない。
 *
 * ## `alignRowTop`: `scrollToIndex` を「y=0 以外」に揃えるための薄いラッパー
 * （4 回目の修正で追加）
 *
 * `virtualizer.scrollToIndex(index, { align: 'start' })` は常に対象行の上端を
 * viewport の **y=0** に揃える。sticky な PageHeader の裏に隠れないよう、
 * あるいはキャプチャ時点で実際に見えていた位置（`topPx`。押した瞬間の
 * スクロール量次第で sticky の下端ぴったりとは限らない）に戻すには、
 * y=0 以外の基準に揃える必要がある。
 *
 * TanStack Virtual はこれを `scrollPaddingStart` オプション（既定 0）で表現する
 * ---
 * `align: 'start'` は実際には「y=`scrollPaddingStart`」に揃える。`alignRowTop`
 * は呼び出しのたびにこれを一時的に上書きしてから `scrollToIndex` を呼ぶ薄い
 * ラッパーで、`scrollPaddingStartRef`（`virtualizer` に渡すオプションの一部。
 * ref なので値を変えても再レンダー不要）と `virtualizer.setOptions()`
 * （React の再レンダーを経由せず `virtualizer.options` を直接書き換える）の
 * 両方を同時に更新する。
 *
 * **なぜ両方を更新する必要があるか**: 最初 `window.scrollTo` で y=0 から
 * `topPx` だけずらす方式を試したが、実機で確認すると **次のフレームで静かに
 * y=0 側へ戻ってしまった**。`scrollToIndex` は「対象行の上端を揃え続ける」
 * ことを内部状態（`scrollState`）に保存し、実測値が出揃うまで数フレームかけて
 * `getOffsetForIndex(index, align)` を再評価・再スクロールする
 * （`reconcileScroll`。見積もり→実測の遷移を追従するための仕組みで、これ自体は
 * 有用 --- 外すと今度は逆に、挿入した約 70 行ぶんの見積もり誤差がそのまま
 * スクロール位置のズレとして残ってしまうことも実機で確認した）。この
 * 再評価は `this.options.scrollPaddingStart`（＝react-virtual アダプタが
 * 次の再レンダーのたびに `useWindowVirtualizer` の呼び出し引数で
 * 上書きし直す値）を見るので、`virtualizer.setOptions()` だけを直接呼んで
 * その場をしのいでも、次の再レンダー（`window.dispatchEvent` が起こす
 * 同期的な再描画を含む）で `scrollPaddingStartRef.current` の値に巻き戻されて
 * しまう。ref と `setOptions` の両方を同時に更新することで、初回のジャンプも
 * 後続フレームの再評価も一貫して同じ基準（y=`topPx`）を使い続けるようにした。
 *
 * `scrollToDayOffset`（下記）でこの ref を明示的に 0 へ戻しているのは、直前の
 * 遡行操作が残した非 0 の値に、無関係な別の操作が引きずられないようにするため。
 *
 * 計測できない環境（`renderAll`）では仮想化そのものをバイパスしているので
 * `alignRowTop` を呼ばない —— 呼んでも対応する意味がなく、実機でしか効果を
 * 確認できないことに変わりはない。
 *
 * ## フレーム跳ね: 補正は「描画前」だけでは足りない（4 回目の修正で追記）
 *
 * `useLayoutEffect` は DOM 変更のコミット後・ペイント前に走るので、ここで
 * スクロール位置を補正すれば「一度描画されてから跳ねる」ことは無いはずに見える。
 * しかし実機で計測すると、挿入直後の 1 フレームだけ大きく（実測 400px 超）跳ねて
 * いた。原因は `window.scrollTo()` が `window.scrollY` を同期的に更新する一方、
 * `virtualizer` が可視範囲の計算に使う内部スクロール位置（`getVirtualItems()` が
 * 使う座標）は、ブラウザの 'scroll' イベント（`window.scrollTo` に対して非同期に
 * 発火する。早くても次のフレーム）を受けてはじめて更新される点にある ---
 * つまり `useLayoutEffect` 自体はペイント前でも、その中で呼ぶ `window.scrollTo`
 * は「ブラウザの scrollY を進める」ことと「`virtualizer` に新しい scrollY を
 * 教える」ことの間に非同期の間隙を持つ。この間隙のせいで、次のフレームの描画は
 * 「新しい scrollY のまま、まだ古い（差し込み前の見積もりサイズに基づく）
 * paddingTop で描かれた」状態になり、これが 1 フレームだけの跳ねとして見える。
 *
 * 対処は、`window.scrollTo` の直後に `window.dispatchEvent(new Event('scroll'))`
 * で同期的に 'scroll' イベントを発火させること。ブラウザが自発的に発火する
 * 'scroll' イベントは非同期だが、`dispatchEvent` で自分から発火させれば
 * イベントリスナー（`virtualizer` が登録している）はその場で同期的に呼ばれる。
 * リスナーは発火時点の実際の `window.scrollY`（既に `window.scrollTo` で更新
 * 済み）を読むので、`virtualizer` の内部状態はペイント前（このレイアウト
 * エフェクトが完了する前）に正しい scrollY へ追いつく。react-virtual の
 * アダプタはこの更新を `flushSync` で即時コミットする（`useFlushSync`
 * オプション、既定 true）ので、ペイントに間に合う。
 *
 * ## `ProgramListHandle`（「既にジャンプ先の日」を再タップしたときの復帰）
 *
 * `ref` 経由で `scrollToDayOffset` を公開する。`pages/programs.tsx` の
 * `DayStrip` タップハンドラが、タップされた日が既に `dayOffset`（state）と
 * 一致するときにこれを呼ぶ（一致しない = 違う日へのジャンプなら、従来どおり
 * `dayOffset` を書き換えてクエリの起点を付け替える）。対象の番組の添字は
 * `lib/visible-day.ts` の `firstIndexForDayOffset`（`visibleDayOffset` と
 * 対になる向きの純関数）で引く。詳細は上記 `ProgramListHandle` の doc コメント参照。
 */
export const ProgramList = forwardRef<
  ProgramListHandle,
  {
    programs: ProgramListItem[]
    serviceByKey: Map<string, Service>
    actions: ReservationActions
    /**
     * 可視範囲の先頭の番組が変わるたびに「いま見ている日」の dayOffset を通知する。
     * `DayStrip` のハイライトはここから来る値を表示するだけで、ジャンプ先
     * （`dayOffset` state）とは別物 —— スクロールで日をまたいでもジャンプ先は
     * 変わらない。導出そのものは `lib/visible-day.ts` の純関数に切り出してある。
     */
    onVisibleDayChange?: (dayOffset: number) => void
    /** テストから現在時刻を固定するための注入口。省略時は `Date.now()`。 */
    now?: number
    /** 遡行できる前の窓が残っているか。false ならボタンを出さない。 */
    hasPreviousPage: boolean
    /** 遡行の取得中か。ボタンを無効化し、ラベルを「読み込み中…」に変える。 */
    isFetchingPreviousPage: boolean
    /**
     * 次に「前を読み込む」で取得する日付（`lib/previous-day-window.ts` の
     * `previousDayWindow` から `pages/programs.tsx` が算出し、
     * `lib/format.ts` の `formatDate` で整形した文字列。例:「8/5(水)」）。
     * ボタンのラベルに「前を読み込む（8/5(水)）」の形で出す（押す前に何が
     * 起きるか分かるように）。「前を読み込む」という語自体は残す ---
     * ここを削ると、ボタンを正規表現 `/前を読み込む/` で探す既存の実機検証
     * スクリプトが見つけられなくなる。
     *
     * `hasPreviousPage` が true なのに `null` のときは日付を省いたラベルに
     * フォールバックする（本来は同じ入力から出るので同時に起きないはずだが、
     * フォールバックを用意して壊れ方を穏やかにする）。
     */
    previousDateLabel: string | null
    /** ボタンを押したときに呼ぶ（`fetchPreviousPage()` の実行は呼び出し側の責務）。 */
    onLoadPrevious: () => void
  }
>(function ProgramList(
  {
    programs,
    serviceByKey,
    actions,
    onVisibleDayChange,
    now,
    hasPreviousPage,
    isFetchingPreviousPage,
    previousDateLabel,
    onLoadPrevious,
  },
  ref,
) {
  const listRef = useRef<HTMLUListElement>(null)

  // ページ全体がスクロールするので、リストの手前にある PageHeader（+ 遡行ボタンが
  // 出ているときはそのぶん）のオフセットを引く必要がある（TanStack Virtual の
  // window スクロール向けの標準的な使い方: https://tanstack.com/virtual の
  // Window Virtualizer 例）。
  //
  // ## なぜ state ではなく ref か（issue #141 で state → ref に直した）
  //
  // 以前は state にしていた --- 「`useLayoutEffect` 内の `setState` はペイント前に
  // 同じコミットで反映されるので、ユーザーには古い値が一切見えない」という想定
  // だった。しかし実機で「3 回目の遡行だけ 97px のフレーム跳ねが出る」不具合が
  // 見つかり、原因がこの想定の破れだった ---
  //
  // 遡行が下限に達して「前を読み込む」ボタンが消える（`hasPreviousPage` が
  // false になる）のと、遡行のアンカー復元（下記 `useLayoutEffect`、`programs` の
  // 変更で走る）が**同じコミット**で起きると、後者の effect は前者の
  // `setScrollMargin` がまだ反映されていない、**この render の `setOptions`
  // で古い** `scrollMargin` を適用されたままの `virtualizer`（インスタンス
  // 自体は `useWindowVirtualizer` 内部の `useState` に保持されマウント後は
  // 変わらない。下記 `alignRowTop` の doc コメント参照）を使って `alignRowTop` →
  // `scrollToIndex` → `dispatchEvent('scroll')` を呼ぶ。この `dispatchEvent` は
  // `virtualizer` 内部の scroll リスナーを同期的に発火させ、`flushSync` で
  // 即座に再コミットする（`alignRowTop` の doc コメント参照）が、その再コミットは
  // **まだ処理されていない `setScrollMargin` の更新より先に** 走ってしまう ---
  // 結果、ボタン分の高さ（実測 52px）だけ `paddingTopPx` の計算がずれた 1 フレームが
  // 実際に描画される（診断用スクリプトで `hasButton` が false に変わった直後の
  // フレームだけ跳ねの値が変わることを確認済み。フレーム跳ねの実測: 通常 45px →
  // このケースだけ 97px。差の 52px は消えたボタン分の高さと一致する）。
  //
  // ref にして `virtualizer.setOptions()` で直接反映すれば（`scrollPaddingStartRef`
  // / `alignRowTop` と同じ形）、React の再レンダーを待たずに測定直後の同期的な
  // 1 手で `virtualizer` 内部の値まで揃うため、この「古い値を見るコミット」が
  // そもそも起きない。
  const scrollMarginRef = useRef(0)

  // 日付ヘッダは「直前の番組」との比較で決まる。仮想化で可視範囲だけを
  // 描いても判定がずれないよう、日付境界は表示中の部分集合ではなく
  // 全件を通しで見て先に決めておく。
  const showDateHeader = useMemo(() => {
    const flags: boolean[] = new Array<boolean>(programs.length)
    let lastDay = ''
    programs.forEach((program, index) => {
      const day = dayKey(program.startAt)
      flags[index] = day !== lastDay
      lastDay = day
    })
    return flags
  }, [programs])

  // getItemKey は programId で計測値（itemSizeCache）を引かせる。既定は
  // 添字（(index) => index）で、先頭に遡行の行を差し込むと既存の全行の添字が
  // N ずれ、記録済みの実測値が別の番組のものとして使われてしまう（遡行の
  // スクロール位置が飛ぶ不具合の原因だった）。`programKeyAt` は単体テストが
  // 直接呼べるよう名前付きの純関数に切り出してある。他のオプション
  // （`estimateSize` 等）と同じくレンダーごとに新しい関数を渡す ---
  // メモ化していないので「programs が変わったのに古い配列を閉じ込めた
  // 関数が残る」という古さの問題がそもそも起きない（常に最新の
  // `programs` を閉じている）。
  // scrollToIndex/scrollToDayOffset が「行の上端を viewport のどこに揃えるか」
  // （`align: 'start'` の基準点）を都度切り替えるための ref。詳細は下記
  // `alignRowTop` のコメント参照。ref なのは、値を変えても再レンダーで
  // `virtualizer` を作り直す必要がなく（`setOptions` で直接反映する）、
  // 状態として保持する意味が無いため。
  const scrollPaddingStartRef = useRef(0)

  const virtualizer = useWindowVirtualizer({
    count: programs.length,
    estimateSize: () => estimatedRowHeightPx,
    getItemKey: (index) => programKeyAt(programs, index),
    overscan: overscanRows,
    scrollMargin: scrollMarginRef.current,
    scrollPaddingStart: scrollPaddingStartRef.current,
  })

  // `scrollMarginRef` の実測・反映。`hasPreviousPage` が変わって「前を読み込む」
  // ボタンが現れる/消えると `<ul>` の `offsetTop` 自体が動くので、都度測り直す。
  //
  // このコミット内で完了させる必要がある --- 下記の遡行アンカー復元の
  // `useLayoutEffect` は、`hasPreviousPage` と `programs` が同じコミットで
  // 変わったとき（遡行が下限に達してボタンが消える瞬間）に自分自身も走り、
  // その内部で `virtualizer` の `scrollMargin` を読む。この effect を先に
  // （宣言順が早い = 同じコミットのレイアウトフェーズで先に実行される）
  // 完了させておくことで、遡行アンカー復元側が常に最新の値を見られるようにする
  // （上記 `scrollMarginRef` の doc コメント参照）。
  useLayoutEffect(() => {
    const measured = listRef.current?.offsetTop ?? 0
    if (measured === scrollMarginRef.current) return
    scrollMarginRef.current = measured
    virtualizer.setOptions({ ...virtualizer.options, scrollMargin: measured })
  }, [hasPreviousPage, virtualizer])

  /**
   * alignRowTop は、`index` の行を viewport の `paddingTopPx` の位置（上端から
   * その px だけ下）に揃えてスクロールする。
   *
   * `virtualizer.scrollToIndex(index, { align: 'start' })` は常に viewport の
   * y=0 に揃える（`scrollPaddingStart` オプション、既定 0）。sticky な
   * PageHeader の裏に隠れないよう y=0 より下（sticky の下端、あるいは
   * キャプチャ時点の実際の位置）に揃えたい場合は、この `scrollPaddingStart`
   * を一時的に上書きする必要がある。
   *
   * `virtualizer.setOptions()` は React の再レンダーを経由せずに
   * `virtualizer.options` を直接書き換える（`useWindowVirtualizer` の
   * アダプタは次の再レンダーで `scrollPaddingStartRef.current` を読み直して
   * 上書きするので、ref も同時に更新して整合させる）。こうしておかないと、
   * `scrollToIndex` 自身の初回ジャンプは正しくても、後続フレームで走る内部の
   * 再評価（見積もり→実測の遷移を追従する仕組み。`reconcileScroll`）が
   * 「常に y=0」に基づいて再計算し、揃えた位置を y=0 側へ引き戻してしまう
   * （実機で確認済み）。`scrollPaddingStart` は `reconcileScroll` が呼ぶのと
   * 同じ `getOffsetForIndex` から参照されるので、ここで揃えておけば
   * 後続フレームの再評価も同じ基準（y=`paddingTopPx`）を使い続ける。
   *
   * `useCallback` にしてあるのは、依存する効果（下記の遡行アンカー復元・
   * `ProgramListHandle.scrollToDayOffset`）の依存配列にこの関数自体を含めても
   * `virtualizer`（安定した参照。`useWindowVirtualizer` 内部で `useState` に
   * 保持され、マウント後は変わらない）が変わらない限り毎回作り直されないため。
   */
  const alignRowTop = useCallback(
    (index: number, paddingTopPx: number) => {
      scrollPaddingStartRef.current = paddingTopPx
      virtualizer.setOptions({ ...virtualizer.options, scrollPaddingStart: paddingTopPx })
      virtualizer.scrollToIndex(index, { align: 'start' })
    },
    [virtualizer],
  )

  // 計測できない環境では仮想化そのものをバイパスする。`measureElement` を
  // 呼ばない（呼ぶと全行が高さ 0 に潰れて可視範囲の計算が壊れる。上記コメント
  // 参照）。`domLayoutMeasurable()` は実行環境の性質なのでレンダーごとに
  // 変わらず、hooks の呼び出し順にも影響しない。
  const renderAll = !domLayoutMeasurable()

  const virtualItems = renderAll ? [] : virtualizer.getVirtualItems()
  const totalSizePx = renderAll ? 0 : virtualizer.getTotalSize()

  const paddingTopPx =
    virtualItems.length > 0 ? Math.max(0, virtualItems[0].start - scrollMarginRef.current) : 0
  const paddingBottomPx =
    virtualItems.length > 0
      ? Math.max(
          0,
          totalSizePx - (virtualItems[virtualItems.length - 1].end - scrollMarginRef.current),
        )
      : 0

  // 「いま見ている日」は可視範囲の先頭インデックスから導く（日付ヘッダへの
  // IntersectionObserver ではない ---
  // ヘッダは可視範囲外で unmount されるため観測が途切れる）。`virtualizer.range`
  // は `measureElement` の実測とは独立に計算される（スクロール位置と
  // `estimateSize` から出す）ので、`renderAll`（jsdom 等の計測できない環境）の
  // 分岐に関係なく安全に読める。
  const firstVisibleIndex = virtualizer.range?.startIndex ?? 0
  useEffect(() => {
    onVisibleDayChange?.(visibleDayOffset(programs, firstVisibleIndex, now ?? Date.now()))
    // programs 自体の参照が変わるたび（絞り込み変更・新しい窓の追加）にも
    // 再評価する。先頭インデックスが同じでも中身の日付が変わりうるため。
  }, [onVisibleDayChange, programs, firstVisibleIndex, now])

  const renderedIndices = renderAll
    ? programs.map((_, index) => index)
    : virtualItems.map((item) => item.index)

  // 遡行のアンカー復元。ボタンを押した時点の { programId, topPx } を控えておき
  // （DOM 挿入前なので安全に読める）、`programs` の更新（挿入完了）を検知したら
  // 仮想化ライブラリ上の新しい添字を引いて `scrollToIndex` する。上記コメント
  // 「『前を読み込む』ボタンと遡行のアンカー復元」参照。
  const pendingAnchorRef = useRef<AnchorSnapshot | null>(null)

  const handleLoadPrevious = () => {
    pendingAnchorRef.current = captureAnchor()
    onLoadPrevious()
  }

  useLayoutEffect(() => {
    const anchor = pendingAnchorRef.current
    if (anchor === null) return
    pendingAnchorRef.current = null
    // 計測できない環境では仮想化そのものをバイパスしているので、その座標系に
    // 乗る scrollToIndex を呼んでも意味がない（上記コメント参照）。
    if (renderAll) return
    const newIndex = findProgramIndex(programs, anchor.programId)
    if (newIndex === null) return

    // 対象の行を、キャプチャ時点で実際に見えていた画面上の位置
    // （`anchor.topPx`。sticky の下端に隠れることも、押した瞬間のスクロール量も
    // 反映済み。`lib/scroll-preservation.ts` の `captureAnchor` 参照）へ揃える。
    // `alignRowTop`（上記コメント参照）を使うことで、`virtualizer` 内部の
    // 「見積もり→実測の遷移を追従する」仕組み（`reconcileScroll`）も同じ基準
    // （y=`anchor.topPx`）で後続フレームの再評価を行い続ける ---
    // 単に `scrollToIndex` の後で `window.scrollTo` によるズレ補正を別途行う
    // 方式だと、この仕組みが「常に y=0」に基づいて再評価し、補正を打ち消して
    // y=0 側へ引き戻してしまうことを実機で確認した。
    alignRowTop(newIndex, anchor.topPx)

    // `virtualizer` が可視範囲の計算に使う内部スクロール位置は、ブラウザの
    // 'scroll' イベント（`window.scrollTo` に対して非同期に発火する）を受けて
    // はじめて更新される。このイベントが実際に届くのは早くても次のフレームなので、
    // ここまでを `window.scrollTo` だけで済ませると「行を差し込んだ直後の
    // 1 フレームだけ、新しい scrollY のまま古い描画（差し込み前の見積もり
    // サイズに基づく paddingTop）が見える」瞬間が残ってしまう ---
    // `useLayoutEffect` はペイント前に走るが、`window.scrollTo` 自体が内部的に
    // 非同期（'scroll' イベント経由）なので、「描画前に補正する」だけでは
    // 1 フレーム漏れる。ここで同期的に 'scroll' イベントを発火させることで、
    // ペイント前（このレイアウトエフェクトが完了する前）に補正後の描画を確定させる
    // （実機で 1 フレームだけ大きく跳ねるのを確認し、これで消えることも確認した）。
    window.dispatchEvent(new Event('scroll'))

    // 1 回目の `alignRowTop` は、挿入された約 70 行のうち大半がまだ一度も
    // 実測されていない（`estimateSize` の見積もりのまま）時点での計算なので、
    // 実際の描画位置とは数百 px ズレることがある。上の `dispatchEvent` が
    // 引き起こす再描画で、その付近の行が実測されキャッシュが更新されるので、
    // 同じ操作をもう一度行うと今度は更新済みのキャッシュを使って計算し直され、
    // 実際の位置によく一致する。ここで直さないと、次のアニメーションフレームで
    // ライブラリ内部の再評価（`reconcileScroll`）が同じ補正をして「1 フレームだけ
    // 大きく跳ねてから直る」という、直したかった症状がそのまま残ってしまう
    // （実機で確認: 1 回目だけだと 1 フレーム分の跳ねが残り、2 回目を足すと消えた。
    // 3 回目を試しても scrollY は変わらなかった --- 2 回目の時点で使った
    // キャッシュが、対象行の周辺として既に安定していたということ）。
    // 「安定するまで」ではなく固定 2 回（`requestAnimationFrame` 等を使わない、
    // 同期的な決め打ちの手順）にしてあるのは、jsdom で検証できない自前の
    // 追従ループを作らないため。
    alignRowTop(newIndex, anchor.topPx)
    window.dispatchEvent(new Event('scroll'))
    // ペンディング（pendingAnchorRef）が無ければ最初の行で早期リターンするだけ
    // なので、`renderAll` / `virtualizer` / `alignRowTop` が毎レンダー作り直されて
    // この effect が programs 以外の理由でも走ることになっても無害。
  }, [programs, renderAll, virtualizer, alignRowTop])

  // `ProgramListHandle`。上記 doc コメント「ProgramListHandle（『既にジャンプ先の
  // 日』を再タップしたときの復帰）」参照。findProgramIndex ベースの遡行アンカー
  // 復元と同じ形（純関数で添字を引いて scrollToIndex）だが、こちらは「押した
  // 瞬間に控えた programId」ではなく「呼ばれた瞬間の dayOffset」から都度引く
  // ---
  // ボタンと違って事前に控えておく必要が無い（対象の日は既に読み込み済みという
  // 前提なので、呼ばれた時点の `programs` にそのまま存在する）。
  useImperativeHandle(
    ref,
    () => ({
      scrollToDayOffset: (dayOffset) => {
        // 計測できない環境では仮想化そのものをバイパスしているので、その座標系に
        // 乗る scrollToIndex を呼んでも意味がない（上記コメント参照）。
        if (renderAll) return
        const index = firstIndexForDayOffset(programs, dayOffset, now ?? Date.now())
        if (index === null) return
        // y=0 に揃える（`paddingTopPx=0`）。遡行のアンカー復元（上記）が
        // `scrollPaddingStartRef` を非 0 のまま残すことがあるので、この
        // 呼び出しでは明示的に 0 を指定して上書きする ---
        // 直前の遡行操作の値に引きずられないようにするため。
        alignRowTop(index, 0)
      },
    }),
    [programs, renderAll, now, alignRowTop],
  )

  return (
    <>
      {hasPreviousPage && (
        // 通常のフローに置く（5 回目の修正で sticky から戻した。上記 doc
        // コメント「『前を読み込む』ボタンと遡行のアンカー復元」参照）。
        // リストを下へスクロールした状態ではボタンは画面外（上）へ流れて
        // 見えなくなるが、それでよい --- このボタンを使うのは「読み込み済みの
        // 先頭まで戻ってきて、その前日も見たくなった」ときだけで、画面外から
        // 押しに行く経路は実在しない（特定の日付を見たいなら日付ストリップで
        // 跳ぶため）。読み進めている間は見えず、上端まで戻ると現れ、押すと
        // 1 日ぶんがその下に積まれてボタン自身が画面外へ押し上げられて消える
        // --- この「消える」こと自体が「読み込まれた」という視覚的フィード
        // バックになる。
        <div className="px-4 pb-2 pt-4">
          <Button
            variant="outline"
            size="sm"
            className="w-full"
            disabled={isFetchingPreviousPage}
            onClick={handleLoadPrevious}
          >
            {isFetchingPreviousPage
              ? '読み込み中…'
              : previousDateLabel
                ? `前を読み込む（${previousDateLabel}）`
                : '前を読み込む'}
          </Button>
        </div>
      )}
      <ul ref={listRef}>
        {paddingTopPx > 0 && <li aria-hidden style={{ height: paddingTopPx }} />}
        {renderedIndices.map((index) => {
          const program = programs[index]
          const reserved = actions.reservedProgramIds.has(program.programId)

          return (
            <li
              key={program.programId}
              data-index={index}
              // 「画面上端に見えている行」を押下時点で控える（captureAnchor /
              // lib/scroll-preservation.ts）ための目印。挿入後にこの属性で
              // DOM から再取得することはもうしない（仮想化と両立しなかった。
              // 上記コメント「『前を読み込む』ボタンと遡行のアンカー復元」参照）。
              // 添字ではなく programId にするのは getItemKey と同じ理由 ---
              // 先頭への挿入で添字はずれるが programId は行の実体と結びついたまま変わらない。
              data-program-id={program.programId}
              ref={renderAll ? undefined : virtualizer.measureElement}
            >
              {/* 日付ヘッダの top は PageHeader が実測して書き出す高さ。
                  ハードコードするとフィルタ行の増減や文字サイズでずれる。
                  「前を読み込む」ボタンは通常のフローに戻したので（5 回目の
                  修正）、ここに足し込む分はもう無い */}
              {showDateHeader[index] && (
                /* 文字色は text-foreground（bg-muted/80 との合成後コントラスト対策。
                   docs/frontend/design.md「コントラストは毎回測る」）。muted は淡い
                   地としてだけ使い、字は通常の本文と同じ濃さにする。
                   data-testid は e2e/design.mjs がこの見出しを一意に引くための目印
                   --- `h2` の 1 番目で引くと将来 PageHeader 等に h2 が入ったときに
                   別の要素を測ったまま通る。 */
                <h2
                  data-testid="program-list-date-heading"
                  className="sticky top-[var(--page-header-height,0px)] z-[5] border-y border-border bg-muted/80 px-4 py-1.5 text-xs font-medium text-foreground backdrop-blur"
                >
                  {formatDate(program.startAt)}
                </h2>
              )}
              <ProgramRow
                program={program}
                serviceName={
                  serviceByKey.get(programServiceKey(program.networkId, program.serviceId))?.name
                }
                reserved={reserved}
                pending={actions.isBusy(program.programId)}
                onReserve={(overrides) => actions.reserve(program, overrides)}
                onCancel={() => actions.cancel(program.programId)}
              />
            </li>
          )
        })}
        {paddingBottomPx > 0 && <li aria-hidden style={{ height: paddingBottomPx }} />}
      </ul>
    </>
  )
})
