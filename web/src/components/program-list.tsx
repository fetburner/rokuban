import { useWindowVirtualizer } from '@tanstack/react-virtual'
import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react'

import type { ProgramListItem, Service } from '@/api/generated'
import { Button } from '@/components/ui/button'
import { ProgramRow } from '@/components/program-row'
import { dayKey, formatDate } from '@/lib/format'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import { findProgramIndex, programKeyAt } from '@/lib/program-list-key'
import { captureAnchor } from '@/lib/scroll-preservation'
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
 */
export type ReservationActions = {
  reserve: (program: ProgramListItem) => void
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
 *    「画面上端に見えている行」の programId を読み、ref に控える。この時点では
 *    まだ何も挿入されていないので、実際にレイアウトされている DOM を安全に読める
 * 2. `onLoadPrevious()` を呼ぶ（`fetchPreviousPage()`。先頭に新しい窓が積まれる）
 * 3. `programs` が変わったら（挿入完了）、控えた programId から
 *    `findProgramIndex`（`lib/program-list-key.ts`）で**挿入後の添字**を引き直し、
 *    `virtualizer.scrollToIndex(newIndex, { align: 'start' })` を呼ぶ
 *
 * 以前は手順 3 を「同じ programId の行を DOM から再度探して top を測り直す」
 * 方式（`locateAnchorTop` + `window.scrollBy`）でやっていたが、これは**仮想化と
 * 構造的に両立しなかった**。挿入直後の時点で、アンカーだった行はオーバースキャン
 * （8 行）の外へ弾き出されて DOM から消えている（差し込む量は 6 時間ぶん・約 79
 * 番組・約 5700px で、オーバースキャンの576pxを大きく超える）。消えた要素は
 * 見つからないので `null` が返り、`window.scrollBy` は一度も呼ばれず、
 * 「スクロール位置が変わらないまま可視範囲だけ再計算され、同じ位置に別の番組が
 * 来る」形で壊れていた（実機で確認済み。詳細は `lib/scroll-preservation.ts` の
 * コメント）。`scrollToIndex` は仮想化ライブラリ自身が持つ座標系（見積もり→実測の
 * 遷移も含めて）を使うので、対象の行が現在 DOM に存在するかどうかに依存しない。
 *
 * `align: 'start'` なので、押した時点で行の途中を見ていた場合は最大 1 行ぶん
 * ずれることがあるが、許容する（以前は数時間ずれていた）。
 *
 * 計測できない環境（`renderAll`）では仮想化そのものをバイパスしているので
 * `scrollToIndex` を呼ばない —— 呼んでも対応する意味がなく、実機でしか効果を
 * 確認できないことに変わりはない。
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
    serviceById: Map<number, Service>
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
    /** ボタンを押したときに呼ぶ（`fetchPreviousPage()` の実行は呼び出し側の責務）。 */
    onLoadPrevious: () => void
  }
>(function ProgramList(
  {
    programs,
    serviceById,
    actions,
    onVisibleDayChange,
    now,
    hasPreviousPage,
    isFetchingPreviousPage,
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
  // ref ではなく state にしてあるのは、`hasPreviousPage` が変わる（遡行ボタンが
  // 現れる/消える）と `<ul>` の `offsetTop` 自体が動くため。ref だとここで
  // 再計測しても仮想化オプションへの反映が次の（別の理由での）再レンダーまで
  // 遅れてしまう（ref の更新自体は再レンダーを起こさない）。state なら
  // `useLayoutEffect` 内の `setState` がペイント前に同じコミットで
  // 反映されるので、ユーザーには古い値が一切見えない。
  const [scrollMargin, setScrollMargin] = useState(0)
  useLayoutEffect(() => {
    setScrollMargin(listRef.current?.offsetTop ?? 0)
  }, [hasPreviousPage])

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
  const virtualizer = useWindowVirtualizer({
    count: programs.length,
    estimateSize: () => estimatedRowHeightPx,
    getItemKey: (index) => programKeyAt(programs, index),
    overscan: overscanRows,
    scrollMargin,
  })

  // 計測できない環境では仮想化そのものをバイパスする。`measureElement` を
  // 呼ばない（呼ぶと全行が高さ 0 に潰れて可視範囲の計算が壊れる。上記コメント
  // 参照）。`domLayoutMeasurable()` は実行環境の性質なのでレンダーごとに
  // 変わらず、hooks の呼び出し順にも影響しない。
  const renderAll = !domLayoutMeasurable()

  const virtualItems = renderAll ? [] : virtualizer.getVirtualItems()
  const totalSizePx = renderAll ? 0 : virtualizer.getTotalSize()

  const paddingTopPx =
    virtualItems.length > 0 ? Math.max(0, virtualItems[0].start - scrollMargin) : 0
  const paddingBottomPx =
    virtualItems.length > 0
      ? Math.max(0, totalSizePx - (virtualItems[virtualItems.length - 1].end - scrollMargin))
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

  // 遡行のアンカー復元。ボタンを押した時点の programId を控えておき（DOM 挿入前
  // なので安全に読める）、`programs` の更新（挿入完了）を検知したら仮想化ライブラリ
  // 上の新しい添字を引いて `scrollToIndex` する。上記コメント「『前を読み込む』
  // ボタンと遡行のアンカー復元」参照。
  const pendingAnchorProgramIdRef = useRef<number | null>(null)

  const handleLoadPrevious = () => {
    pendingAnchorProgramIdRef.current = captureAnchor()
    onLoadPrevious()
  }

  useLayoutEffect(() => {
    const programId = pendingAnchorProgramIdRef.current
    if (programId === null) return
    pendingAnchorProgramIdRef.current = null
    // 計測できない環境では仮想化そのものをバイパスしているので、その座標系に
    // 乗る scrollToIndex を呼んでも意味がない（上記コメント参照）。
    if (renderAll) return
    const newIndex = findProgramIndex(programs, programId)
    if (newIndex === null) return
    virtualizer.scrollToIndex(newIndex, { align: 'start' })
    // ペンディング（pendingAnchorProgramIdRef）が無ければ最初の行で早期リターン
    // するだけなので、`renderAll` / `virtualizer` が毎レンダー作り直されて
    // この effect が programs 以外の理由でも走ることになっても無害。
  }, [programs, renderAll, virtualizer])

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
        virtualizer.scrollToIndex(index, { align: 'start' })
      },
    }),
    [programs, renderAll, virtualizer, now],
  )

  // ボタン自身の高さを `--load-previous-height` に書き出す（`components/page.tsx`
  // の PageHeader と同じパターン）。日付ヘッダの sticky top はこれを
  // `--page-header-height` に足して使う（下記 JSX 参照）。ハードコードすると
  // ボタンのラベルが「読み込み中…」に変わったときの幅・折返しでずれる。
  const loadPreviousRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    const el = loadPreviousRef.current
    const parent = el?.parentElement
    if (!el || !parent) return

    const publish = () => {
      parent.style.setProperty('--load-previous-height', `${el.offsetHeight}px`)
    }
    publish()

    const observer = new ResizeObserver(publish)
    observer.observe(el)
    return () => {
      observer.disconnect()
      parent.style.removeProperty('--load-previous-height')
    }
  }, [hasPreviousPage])

  return (
    <>
      {hasPreviousPage && (
        // sticky にして PageHeader の直下に常時留める。理由はコメント
        // 「『前を読み込む』ボタンと遡行のアンカー復元」参照 --- 通常フローの
        // ままだとリストを下へスクロールした状態ではボタンが画面外に出て、
        // 押すには一旦画面外まで戻ってからクリックする必要がある。この
        // 「押すために戻る」動作そのものが（実ユーザーの手動スクロールであれ、
        // Playwright の actionability チェックによる自動スクロールであれ）
        // captureAnchor() が読む DOM を「実際に見ていた行」から「ボタンを
        // 押すために戻った先」へすり替えてしまい、以降の scrollToIndex は
        // その（間違った）行を正しく復元するだけなので気付けない。z-10 は
        // 同じ top に来る日付ヘッダ（z-[5]）より前面に出すため（ボタンが
        // 日付ヘッダを隠さないよう、日付ヘッダ側の top をこのボタンの高さぶん
        // 押し下げてある。下記 JSX の `--load-previous-height` 参照）。
        <div
          ref={loadPreviousRef}
          className="sticky top-[var(--page-header-height,0px)] z-10 bg-background px-4 pb-2 pt-4"
        >
          <Button
            variant="outline"
            size="sm"
            className="w-full"
            disabled={isFetchingPreviousPage}
            onClick={handleLoadPrevious}
          >
            {isFetchingPreviousPage ? '読み込み中…' : '前を読み込む'}
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
              {/* 日付ヘッダの top は PageHeader（+ 遡行ボタンが出ているときは
                  そのぶん）が実測して書き出す高さ。ハードコードするとフィルタ行の
                  増減や文字サイズ、ボタンのラベル変化でずれる。`--load-previous-height`
                  はボタンが無いときは未設定（既定 0px）なので、ボタンが無ければ
                  従来どおり `--page-header-height` だけになる */}
              {showDateHeader[index] && (
                <h2 className="sticky top-[calc(var(--page-header-height,0px)+var(--load-previous-height,0px))] z-[5] border-y border-border bg-muted/80 px-4 py-1.5 text-xs font-medium text-muted-foreground backdrop-blur">
                  {formatDate(program.startAt)}
                </h2>
              )}
              <ProgramRow
                program={program}
                serviceName={serviceById.get(program.serviceId)?.name}
                reserved={reserved}
                pending={actions.isBusy(program.programId)}
                onReserve={() => actions.reserve(program)}
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
