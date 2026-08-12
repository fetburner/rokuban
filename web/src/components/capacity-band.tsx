import { TriangleAlert } from 'lucide-react'

import type { CapacityOverage } from '@/api/generated'
import { overageWindow, shortageLabel, shortageRangeMessage } from '@/lib/capacity'
import { spanToPx, type TimeAxis } from '@/lib/epg-grid'

/**
 * 帯の中にラベルを出す下限の高さ。これを下回る帯は着色だけになる。
 *
 * 帯の高さに下限は入れない（区間の長さそのものなので、下限を入れると
 * `spanToPx` を通す意味が失われる。docs/frontend.md「セルの高さに下限を設けない」）。
 * 制限するのは中身のラベルだけで、短い帯は文字が入らないので描かない。
 * 画面幅・帯の高さに依存しない伝達手段は一覧のバッジ側で担保する（#21）。
 */
const labelMinHeightPx = 18

/**
 * CapacityBands はチューナーが不足している区間を全チャンネル縦断の帯として描く
 * （`ProgramGrid` の `overlay` に渡す。M2-10, issue #24）。
 *
 * **番組ではなく区間に描く。** 番組を着色すると「この番組が負ける」という勝敗の
 * 主張になるが、決めるのは mirakc であり、Rokuban から見えない消費者がいるので
 * 予測できない（docs/frontend.md「容量超過は番組ではなく区間に描く」、
 * docs/data.md §6.5）。したがってここは番組・予約を一切参照しない。
 *
 * 座標は番組セルと同じ `spanToPx` を通す。これが「帯とセルが同じ時刻で同じ位置に
 * 来る」ことの保証で、自前で時刻 → px を書くとその保証が消える。
 *
 * 渡された区間はそのまま全部描く。サイトで絞るかどうかは呼び出し側の判断
 * （多サイト時のグリッド表示は未解決 --- BS / CS は全国共通だが地上波はサイトごとに
 * 別物なので列を畳めず、帯もサイトごとに要る。docs/data.md §6.5）。
 */
export function CapacityBands({
  axis,
  overages,
}: {
  axis: TimeAxis
  overages: readonly CapacityOverage[]
}) {
  return (
    <>
      {overages.map((overage) => (
        <CapacityBand
          key={`${overage.site}-${overage.startAt}`}
          axis={axis}
          overage={overage}
        />
      ))}
    </>
  )
}

function CapacityBand({ axis, overage }: { axis: TimeAxis; overage: CapacityOverage }) {
  const span = overageWindow(overage)
  // 番組セルと同じ写像を通す。これが「帯とセルが同じ時刻で同じ位置に来る」ことの
  // 保証（軸の外は切り落とされ、交差しなければ null が返る）
  const rect = spanToPx(axis, span.startMs, span.endMs)
  if (!rect) return null

  return (
    <div
      data-testid="capacity-band"
      data-start-at={overage.startAt}
      // 淡い着色 + 上下の罫線。塗りを強くすると番組セルのタイトルが読めなくなり、
      // 番組表として使えなくなる（区間の境界は罫線が伝える）。色は警告の信号色
      // （琥珀）そのもので、濃さだけを下げる --- 帯のためだけの別の琥珀を作らない
      // （docs/frontend/design.md「トークン外の生の色値を書かない」）。
      // overflow は切らない --- `overflow: hidden` は自身をスクロール容器にするので、
      // 中のラベルの sticky が効かなくなる（はみ出しは labelMinHeightPx で抑える）
      // 罫線の濃さ（/80）は「境界を伝える」役割から決めた実測値 --- これ未満だと
      // ライトで、帯が重なりうる面のうち最も不利なものに対して 3:1 を割る
      // （旧実装の淡い琥珀の罫線は 1.51 で、ライトでは境界がほぼ見えていなかった）。
      // e2e/design.mjs が毎回、グリッドのセルの面まで含めて測る
      className="absolute inset-x-0 border-y border-warning/80 bg-warning/10"
      style={{ top: rect.topPx, height: rect.heightPx }}
    >
      {/* 読み上げには常に時刻付きの文を出す（帯が短いとラベルが出ないため、
          見た目のラベルとは別に持つ）。見えるラベルは aria-hidden にして二重読みを避ける */}
      <span className="sr-only">{shortageRangeMessage(overage)}</span>
      {rect.heightPx >= labelMinHeightPx && (
        <span
          aria-hidden="true"
          // sticky left-0: 帯は列の総幅を張るので、横スクロールしてもラベルが
          // 画面内に残るようにする（左端に置いたままだと右へパンした先で消える）
          className="sticky left-0 flex w-fit items-center gap-1 px-1.5 text-[10px] font-medium whitespace-nowrap text-warning"
        >
          <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
          {shortageLabel(overage)}
        </span>
      )}
    </div>
  )
}
