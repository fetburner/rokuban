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
 *
 * **見えるラベルはここでは描かない。** 帯の内側（局の列の上）に出すと、帯の上端が
 * 番組セルの上端と重なったとき、セルの時刻文字と同じ px に描かれて両方読めなく
 * なる（issue #460）。見えるラベルは `CapacityBandLabels`（`ProgramGrid` の
 * `gutterOverlay`）が局の列と重ならない時間軸列に出す。読み上げ用の文は帯が短くて
 * 見えるラベルが出ないときも必要なので、帯自身に残す（sr-only の対はここに残る）。
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
      // 罫線の濃さ（/80）は「境界を伝える」役割から決めた実測値 --- これ未満だと
      // ライトで、帯が重なりうる面のうち最も不利なものに対して 3:1 を割る
      // （旧実装の淡い琥珀の罫線は 1.51 で、ライトでは境界がほぼ見えていなかった）。
      // e2e/design.mjs が毎回、グリッドのセルの面まで含めて測る
      className="absolute inset-x-0 border-y border-warning/80 bg-warning/10"
      style={{ top: rect.topPx, height: rect.heightPx }}
    >
      {/* 読み上げには常に時刻付きの文を出す（帯が短いと見えるラベルが出ないため、
          見た目のラベルとは別に持つ。見た目のラベルは CapacityBandLabels 側） */}
      <span className="sr-only">{shortageRangeMessage(overage)}</span>
    </div>
  )
}

/**
 * CapacityBandLabels は帯の見えるラベルを時間軸列（左端の `21:00` 等が並ぶ列）に
 * 描く（`ProgramGrid` の `gutterOverlay` に渡す）。
 *
 * **帯は区間の主張なので、局の列ではなく時間軸列に属する。** 局の列（帯の内側）に
 * 出すと、帯の上端が番組セルの上端と重なったときセルの時刻文字と同じ px に描かれて
 * 両方読めなくなる（issue #460）。時間軸列は番組セルを一切置かないので、この列に
 * 出す限り番組の文字と衝突しない。座標は `CapacityBands` と同じ `spanToPx` を通す
 * ので、帯と縦位置は必ず揃う。
 */
export function CapacityBandLabels({
  axis,
  overages,
}: {
  axis: TimeAxis
  overages: readonly CapacityOverage[]
}) {
  return (
    <>
      {overages.map((overage) => (
        <CapacityBandLabel
          key={`${overage.site}-${overage.startAt}`}
          axis={axis}
          overage={overage}
        />
      ))}
    </>
  )
}

function CapacityBandLabel({ axis, overage }: { axis: TimeAxis; overage: CapacityOverage }) {
  const span = overageWindow(overage)
  const rect = spanToPx(axis, span.startMs, span.endMs)
  if (!rect || rect.heightPx < labelMinHeightPx) return null

  return (
    <div
      data-testid="capacity-band-label"
      aria-hidden="true"
      // 時間軸列の幅（56px）に収まらない分は省略記号で切る（truncate =
      // overflow:hidden + text-overflow:ellipsis + whitespace:nowrap）。
      // 折り返すと帯の高さが低いとき次の帯のラベルへはみ出す --- 読み上げ用の
      // 全文は帯側の sr-only（CapacityBand）が持つので、ここは見た目だけ削ってよい。
      // bg-background で不透明にする --- 時間軸列には毎時の目盛り（21:00 等）が
      // 別途描かれており、帯がちょうど時刻境界から始まると縦位置が一致しうる。
      // 透過のままだと文字同士が混ざるので、ラベル側を不透明にして上に乗せる
      // （「現在時刻の札」が目盛りに乗るときの扱いと同じ）。
      className="absolute inset-x-0 truncate bg-background px-1 text-[10px] font-medium text-warning"
      style={{ top: rect.topPx, height: rect.heightPx, lineHeight: `${rect.heightPx}px` }}
    >
      {shortageLabel(overage)}
    </div>
  )
}
