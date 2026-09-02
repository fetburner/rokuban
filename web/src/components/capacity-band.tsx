import { TriangleAlert } from 'lucide-react'

import type { CapacityOverage } from '@/api/generated'
import { overageWindow, shortageLabel, shortageLabelCompact, shortageRangeMessage } from '@/lib/capacity'
import { hourTicks, spanToPx, type TimeAxis } from '@/lib/epg-grid'

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
 * CapacityBands はチューナーが不足している区間を同じ site の全チャンネルを
 * 縦断する帯として描く
 * （`ProgramGrid` の `siteOverlay` に渡す。M2-10, issue #24）。
 *
 * **番組ではなく区間に描く。** 番組を着色すると「この番組が負ける」という勝敗の
 * 主張になるが、決めるのは mirakc であり、Rokuban から見えない消費者がいるので
 * 予測できない（docs/frontend.md「容量超過は番組ではなく区間に描く」、
 * docs/data.md §6.5）。したがってここは番組・予約を一切参照しない。
 *
 * 座標は番組セルと同じ `spanToPx` を通す。これが「帯とセルが同じ時刻で同じ位置に
 * 来る」ことの保証で、自前で時刻 → px を書くとその保証が消える。
 *
 * 呼び出し側が渡す site と一致する区間だけを描く。ProgramGrid の siteOverlay は
 * 同じ site の連続した列領域に親要素を限定し、overflow で別 site の列への描画を
 * クリップする。
 *
 * **見えるラベルはここでは描かない。** 帯の内側（局の列の上）に出すと、帯の上端が
 * 番組セルの上端と重なったとき、セルの時刻文字と同じ px に描かれて両方読めなく
 * なる（issue #460）。見えるラベルは `CapacityBandLabels`（`ProgramGrid` の
 * `gutterOverlay`）が局の列と重ならない時間軸列に出す。読み上げ用の文は帯が短くて
 * 見えるラベルが出ないときも必要なので、帯自身に残す（sr-only の対はここに残る）。
 *
 * **`announce` は 1 site につき 1 回だけ true にして呼ぶ。** `orderServices` は
 * 種別を最外に持つため、GR + BS を両方持つ site は列領域が非隣接な複数の走に
 * 分かれ、`ProgramGrid` はその走ごとに `siteOverlay` を呼ぶ。走ごとに sr-only を
 * 出すと同じ超過区間の説明が走の本数ぶん重複して読み上げられる（レビュー指摘）。
 * 帯の見た目（着色・罫線）は走ごとに必要なので `announce` を分けて呼び出し側
 * （`ProgramGrid` の `isFirstRunForSite`）に判断させる --- 帯自体を 1 site 1 回に
 * 減らすと今度は別の走の列に色が付かなくなる。
 */
export function CapacityBands({
  axis,
  overages,
  site,
  showSite = false,
  announce = true,
}: {
  axis: TimeAxis
  overages: readonly CapacityOverage[]
  site: string
  /** 複数 site のとき、読み上げ文に site 名を含める（列ヘッダ・一覧行の `showSite` と同じ条件）。 */
  showSite?: boolean
  /** false なら sr-only の読み上げ文を出さない（同じ site の 2 本目以降の走）。 */
  announce?: boolean
}) {
  return (
    <>
      {overages.filter((overage) => overage.site === site).map((overage) => (
        <CapacityBand
          key={`${overage.site}-${overage.startAt}`}
          axis={axis}
          overage={overage}
          showSite={showSite}
          announce={announce}
        />
      ))}
    </>
  )
}

function CapacityBand({
  axis,
  overage,
  showSite,
  announce,
}: {
  axis: TimeAxis
  overage: CapacityOverage
  showSite: boolean
  announce: boolean
}) {
  const span = overageWindow(overage)
  // 番組セルと同じ写像を通す。これが「帯とセルが同じ時刻で同じ位置に来る」ことの
  // 保証（軸の外は切り落とされ、交差しなければ null が返る）
  const rect = spanToPx(axis, span.startMs, span.endMs)
  if (!rect) return null

  return (
    <div
      data-testid="capacity-band"
      data-site={overage.site}
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
      {announce && (
        <span className="sr-only">{shortageRangeMessage(overage, { showSite })}</span>
      )}
    </div>
  )
}

/**
 * ラベル自身の高さ（px）。帯の高さには連動させない --- 帯の高さぶん引き伸ばすと
 * （旧実装）ラベルが不透明な箱として帯の全高を覆い、範囲内の時間軸の目盛りや
 * 現在時刻チップまで消してしまった（issue #460 レビュー）。ラベルは 1 行の
 * 短い形なので中身の高さは固定できる。CSS 側（`leading-4` = 16px）と値を
 * 合わせてある（変えたら両方直す）。
 */
const labelHeightPx = 16

/**
 * 目盛りを避けるために見る高さの目安（px）。実測（e2e/design.mjs）で目盛り
 * 1 行はおよそ 18.5px --- 端数を避けるためここでは少し余裕を持たせて丸める。
 * 「本当に避けられているか」はこの定数の精度ではなく e2e が実ブラウザで測る
 * （目盛りの rect とラベルの rect が交差しないこと）。
 */
const tickAvoidHeightPx = 20

/**
 * avoidTickRow はラベルの上端が毎時の目盛りの行と重ならない位置まで押し下げる。
 *
 * 不足区間の境界は :00 / :30 に落ちることが番組と同じくらい多く（サーバー側の
 * 判定も番組境界を単位にする）、ラベルは帯の上端にアンカーするので、区間が
 * ちょうど正時に始まると目盛り（「00:00」等）と同じ y に来る。目盛り列の地は
 * 透明なので、ここで不透明なラベルを重ねるとその目盛りが読めなくなる
 * （issue #460 レビュー実測: `coveredTicks: ["00:00"]`）。時間軸列には番組セル
 * が無いので、目盛りとの位置調整だけがここでの唯一の避け先になる。
 */
function avoidTickRow(axis: TimeAxis, topPx: number): number {
  for (const tick of hourTicks(axis)) {
    const tickBottomPx = tick.topPx + tickAvoidHeightPx
    if (topPx < tickBottomPx && topPx + labelHeightPx > tick.topPx) {
      return tickBottomPx
    }
  }
  return topPx
}

/**
 * CapacityBandLabels は帯の見えるラベルを時間軸列（左端の `21:00` 等が並ぶ列）に
 * 描く（`ProgramGrid` の `gutterOverlay` に渡す）。
 *
 * **帯は区間の主張なので、局の列ではなく時間軸列に属する。** 局の列（帯の内側）に
 * 出すと、帯の上端が番組セルの上端と重なったときセルの時刻文字と同じ px に描かれて
 * 両方読めなくなる。時間軸列は番組セルを一切置かないので、この列に出す限り番組の
 * 文字と衝突しない。座標は `CapacityBands` と同じ `spanToPx` を通すので、帯と縦位置
 * の基準は揃う（ラベル自身は下記のとおり自分の高さぶんだけ持つ）。
 *
 * **同一 site 内の不足区間は重ならないが、別 site の区間は重なりうる。**
 * 全 site のラベルは 1 本の時間軸列に置くため、先に置いたラベルと交差する場合は
 * 1 行ぶん下へ積む。積んだ先が自分の帯を越える場合は描かない。
 * **ただし `avoidTickRow` の押し下げは帯の高さを見ない。**
 * 正時に始まる短い帯（9〜18 分）を押し下げると、直後に隣接する帯（サーバーが
 * 正当に返せる形）のラベルと同じ位置に来て衝突が復活する --- `CapacityBandLabel`
 * は押し下げた先が自分の帯からはみ出すときはラベルを描かないことでこれを防ぐ
 * （issue #460 再レビュー）。
 *
 * **見えるラベル自体（`shortageLabelCompact`）には site 名を入れない。** 時間軸列は
 * 56px しか無く、種別が 2 つ詰まった時点で `shortageLabelCompact` はすでに本数
 * だけの形（「-2」）まで削っており、ここに site 名を前置する余地を実測できない
 * （溢れる側に倒すより、溢れない形を選ぶ）。gutter は局の列から離れているため
 * 複数 site では帯の帰属が読めなくなるが、マウス操作者には `title`
 * （`shortageLabel`）、支援技術には帯側の `sr-only`（`shortageRangeMessage`。
 * `CapacityBand` 参照）で site 名を補う --- どちらも幅制約を受けない。
 */
export function CapacityBandLabels({
  axis,
  overages,
  showSite = false,
}: {
  axis: TimeAxis
  overages: readonly CapacityOverage[]
  /** 複数 site のとき、title（native tooltip）に site 名を含める。 */
  showSite?: boolean
}) {
  const placements: { overage: CapacityOverage; topPx: number }[] = []
  for (const overage of overages) {
    const span = overageWindow(overage)
    const rect = spanToPx(axis, span.startMs, span.endMs)
    if (!rect || rect.heightPx < labelMinHeightPx) continue
    let topPx = avoidTickRow(axis, rect.topPx)
    for (;;) {
      const collision = placements.find(
        (placed) => topPx < placed.topPx + labelHeightPx && topPx + labelHeightPx > placed.topPx,
      )
      if (collision === undefined) break
      topPx = collision.topPx + labelHeightPx
    }
    if (topPx + labelHeightPx > rect.topPx + rect.heightPx) continue
    placements.push({ overage, topPx })
  }

  return (
    <>
      {placements.map(({ overage, topPx }) => (
        <CapacityBandLabel
          key={`${overage.site}-${overage.startAt}`}
          overage={overage}
          topPx={topPx}
          showSite={showSite}
        />
      ))}
    </>
  )
}

function CapacityBandLabel({
  overage,
  topPx,
  showSite,
}: {
  overage: CapacityOverage
  topPx: number
  showSite: boolean
}) {
  return (
    <div
      data-testid="capacity-band-label"
      data-site={overage.site}
      aria-hidden="true"
      // 短縮ラベル（「BS-1」「-2」等）だけでは種別も単位も読めない場合がある
      // （複数種別が詰まると `shortageLabelCompact` は本数だけの `-2` を返し、
      // 実測の余白では他の略記が入らない）。マウス操作者には native title
      // （ネイティブツールチップ）で全文を補う --- div は aria-hidden なので
      // 支援技術には影響しない。複数 site のときは site 名も title に含める
      // （見えるラベル自体は幅の余地が無いので入れない。上記コメント参照）。
      title={shortageLabel(overage, { showSite })}
      // bg-background で不透明にする --- 時間軸列には毎時の目盛り（21:00 等）
      // が別途描かれており、`avoidTickRow` で通常は避けるが、それでも隣接する
      // 場合の可読性のために地を割る（透過だと文字同士が混ざる）。
      className="absolute inset-x-0 flex items-center gap-1 bg-background px-1 text-warning"
      style={{ top: topPx }}
    >
      {/* アイコンは色だけに頼らない警告の手がかり。琥珀の短い英数字
          （「BS-1」「CS-3」等）は日本語 EPG の時間軸列に置かれると実在の
          チャンネル名（NHK BS1 等）と読み違えられうる（issue #460 レビュー）。 */}
      <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
      {/* 時間軸列の幅に収まらない分だけ省略記号で切る（truncate =
          overflow:hidden + text-overflow:ellipsis + whitespace:nowrap）。
          `min-w-0` が無いと flex item は自分の内容ぶんまでしか縮まず truncate
          が効かない。`shortageLabelCompact` がこの幅を狙った短い形を返すので、
          通常は切れずに収まる --- 全文は帯側の sr-only（CapacityBand）が持つ。 */}
      <span
        data-testid="capacity-band-label-text"
        className="min-w-0 truncate text-xs leading-4 font-medium"
      >
        {shortageLabelCompact(overage)}
      </span>
    </div>
  )
}
