import { useWindowVirtualizer } from '@tanstack/react-virtual'
import { useLayoutEffect, useMemo, useRef } from 'react'

import type { ProgramListItem, Service } from '@/api/generated'
import { ProgramRow } from '@/components/program-row'
import { dayKey, formatDate } from '@/lib/format'
import { domLayoutMeasurable } from '@/lib/list-virtualization'

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
 * ## 計測できない環境（jsdom）では全部描く
 *
 * `measureElement` は `getBoundingClientRect` で高さを読む。jsdom はレイアウト
 * エンジンを持たないため常に 0 を返し、動的計測をそのまま使うと可視範囲の計算が
 * 際限なく前進し続ける不具合を実機測定で確認した（詳細は
 * `web/src/lib/list-virtualization.ts` のコメント）。`domLayoutMeasurable()` で
 * 環境を検出し、計測できない環境では `measureElement` を一切使わず、仮想化その
 * ものをバイパスして全行を通常のフローで描く（`web/src/lib/epg-grid.ts` の
 * `visibleTimeWindow` / `visibleColumnRange` と同じ「未計測なら間引かない」）。
 */
export function ProgramList({
  programs,
  serviceById,
  actions,
}: {
  programs: ProgramListItem[]
  serviceById: Map<number, Service>
  actions: ReservationActions
}) {
  const listRef = useRef<HTMLUListElement>(null)

  // ページ全体がスクロールするので、リストの手前にある PageHeader ぶんの
  // オフセットを引く必要がある（TanStack Virtual の window スクロール向けの
  // 標準的な使い方: https://tanstack.com/virtual の Window Virtualizer 例）。
  const scrollMarginRef = useRef(0)
  useLayoutEffect(() => {
    scrollMarginRef.current = listRef.current?.offsetTop ?? 0
  }, [])

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

  const virtualizer = useWindowVirtualizer({
    count: programs.length,
    estimateSize: () => estimatedRowHeightPx,
    overscan: overscanRows,
    scrollMargin: scrollMarginRef.current,
  })

  // 計測できない環境では仮想化そのものをバイパスする。`measureElement` を
  // 呼ばない（呼ぶと全行が高さ 0 に潰れて可視範囲の計算が壊れる。上記コメント
  // 参照）。`domLayoutMeasurable()` は実行環境の性質なのでレンダーごとに
  // 変わらず、hooks の呼び出し順にも影響しない。
  const renderAll = !domLayoutMeasurable()

  const virtualItems = renderAll ? [] : virtualizer.getVirtualItems()
  const totalSizePx = renderAll ? 0 : virtualizer.getTotalSize()
  const scrollMargin = scrollMarginRef.current

  const paddingTopPx =
    virtualItems.length > 0 ? Math.max(0, virtualItems[0].start - scrollMargin) : 0
  const paddingBottomPx =
    virtualItems.length > 0
      ? Math.max(0, totalSizePx - (virtualItems[virtualItems.length - 1].end - scrollMargin))
      : 0

  const renderedIndices = renderAll
    ? programs.map((_, index) => index)
    : virtualItems.map((item) => item.index)

  return (
    <ul ref={listRef}>
      {paddingTopPx > 0 && <li aria-hidden style={{ height: paddingTopPx }} />}
      {renderedIndices.map((index) => {
        const program = programs[index]
        const reserved = actions.reservedProgramIds.has(program.programId)

        return (
          <li
            key={program.programId}
            data-index={index}
            ref={renderAll ? undefined : virtualizer.measureElement}
          >
            {/* 日付ヘッダの top は PageHeader が実測して書き出す高さ。
                ハードコードするとフィルタ行の増減や文字サイズでずれる */}
            {showDateHeader[index] && (
              <h2 className="sticky top-[var(--page-header-height,0px)] z-[5] border-y border-border bg-muted/80 px-4 py-1.5 text-xs font-medium text-muted-foreground backdrop-blur">
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
  )
}
