import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, ProgramListItem, Service } from '@/api/generated'
import { CapacityBandLabels, CapacityBands } from '@/components/capacity-band'
import { ProgramGrid } from '@/components/program-grid'
import { epgColumnWidthPx, type TimeAxis } from '@/lib/epg-grid'

/** 軸はローカル時刻の 0 時基準（program-grid.test.tsx と同じ組み方）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

const axis: TimeAxis = {
  startMs: dayStart.getTime(),
  endMs: dayStart.getTime() + 24 * 3_600_000,
  pxPerHour: 120,
}

/** at は軸の開始からの分数を epoch ms に直す。 */
function at(minutes: number): number {
  return axis.startMs + minutes * 60_000
}

/** iso は軸の開始からの分数を ISO 文字列に直す。 */
function iso(minutes: number): string {
  return new Date(at(minutes)).toISOString()
}

const service: Service = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

function program(programId: number, startMinutes: number, durationMinutes: number): ProgramListItem {
  return {
    programId,
    networkId: 32736,
    serviceId: service.serviceId,
    eventId: programId,
    startAt: iso(startMinutes),
    endAt: iso(startMinutes + durationMinutes),
    durationMs: durationMinutes * 60_000,
    name: `番組 ${programId}`,
    description: '',
    genres: [3],
    isFree: true,
  }
}

function overage(
  startMinutes: number,
  endMinutes: number,
  options: Partial<CapacityOverage> = {},
): CapacityOverage {
  return {
    site: 'default',
    startAt: iso(startMinutes),
    endAt: iso(endMinutes),
    shortfall: 1,
    jammedTypes: ['BS'],
    ...options,
  }
}

/**
 * renderGrid は帯を実際のグリッドの上に置く。
 *
 * `CapacityBands` を単体で描くと「番組セルと同じ座標に来る」ことを確かめられない
 * （それが `spanToPx` を共有している唯一の観測可能な帰結なので、セルと並べて描く）。
 */
function renderGrid(overages: CapacityOverage[], programs: ProgramListItem[]) {
  return render(
    <ProgramGrid
      services={[service]}
      programs={programs}
      axis={axis}
      reservationByProgramId={new Set()}
      selectedProgramId={null}
      onSelect={vi.fn()}
      now={at(19 * 60)}
      overlay={(gridAxis) => <CapacityBands axis={gridAxis} overages={overages} />}
      // 見えるラベルは時間軸列（gutterOverlay）に出る（issue #460）。
      // 単体テストでも実際に使う配線と同じ組み方にする。
      gutterOverlay={(gridAxis) => <CapacityBandLabels axis={gridAxis} overages={overages} />}
    />,
  )
}

function band(startMinutes: number): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[data-start-at="${iso(startMinutes)}"]`)
}

function cell(programId: number): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-program-id="${programId}"]`)
  if (!el) throw new Error(`cell ${programId} not found`)
  return el
}

describe('CapacityBands', () => {
  it('帯は同じ時刻の番組セルと同じ位置・高さに来る', () => {
    // 20:00-21:00 の不足と、同じ 20:00-21:00 の番組
    renderGrid([overage(20 * 60, 21 * 60)], [program(1, 20 * 60, 60)])

    const rendered = band(20 * 60)
    expect(rendered).not.toBeNull()
    // 自前で時刻 → px を計算すると（オフセットの取り違え・分/時間の混同で）ここがずれる
    expect(rendered?.style.top).toBe(cell(1).style.top)
    expect(rendered?.style.height).toBe(cell(1).style.height)
    expect(rendered?.style.top).toBe('2400px')
    expect(rendered?.style.height).toBe('120px')
  })

  it('区間が全チャンネルを縦断する（番組ではなく区間に描く）', () => {
    const services = [service, { ...service, serviceId: 1032, name: 'NHKEテレ' }]
    render(
      <ProgramGrid
        services={services}
        programs={[program(1, 20 * 60, 60)]}
        axis={axis}
        reservationByProgramId={new Set()}
        selectedProgramId={null}
        onSelect={vi.fn()}
        now={at(19 * 60)}
        overlay={(gridAxis) => (
          <CapacityBands axis={gridAxis} overages={[overage(20 * 60, 21 * 60)]} />
        )}
      />,
    )

    // 帯の層は列の総幅を張る。列ごとに描くと「この番組が負ける」の主張になる
    expect(band(20 * 60)?.parentElement?.parentElement?.style.width).toBe(
      `${services.length * epgColumnWidthPx}px`,
    )
  })

  it('不足本数と詰まった種別を出す（読み上げ用の全文。見た目は時間軸列側の短い形）', () => {
    renderGrid([overage(20 * 60, 21 * 60, { shortfall: 2, jammedTypes: ['GR', 'BS'] })], [])

    // 読み上げ用には時刻付きの全文（帯が短くて見えるラベルが出ないこともある）
    expect(
      screen.getByText('20:00〜21:00 はチューナーが不足しています（GR・BS が 2 本不足）'),
    ).toBeInTheDocument()
    // 見た目（時間軸列）は shortageLabelCompact の短い形。種別 2 つは列挙せず本数だけ
    expect(screen.getByText('-2')).toBeInTheDocument()
  })

  it('区間が結合済みでも複数あればすべて描く', () => {
    renderGrid([overage(10 * 60, 11 * 60), overage(20 * 60, 21 * 60)], [])

    expect(screen.getAllByTestId('capacity-band')).toHaveLength(2)
    expect(band(10 * 60)?.style.top).toBe('1200px')
    expect(band(20 * 60)?.style.top).toBe('2400px')
  })

  it('軸をまたぐ区間は軸の中だけを描く', () => {
    // 前日 23:00 から当日 1:00
    renderGrid([overage(-60, 60)], [])

    expect(band(-60)?.style.top).toBe('0px')
    expect(band(-60)?.style.height).toBe('120px')
  })

  it('軸と交差しない区間は描かない', () => {
    renderGrid([overage(25 * 60, 26 * 60)], [])

    // 軸の外を描くと、24 時間の外に帯が伸びて画面に出ないまま高さを主張する
    expect(screen.queryByTestId('capacity-band')).not.toBeInTheDocument()
  })

  it('区間が無ければ何も描かない（沈黙を肯定にしない）', () => {
    renderGrid([], [program(1, 20 * 60, 60)])

    // 番組は出ているので「まだ描かれていない」ではない
    expect(cell(1)).toBeInTheDocument()
    expect(screen.queryByTestId('capacity-band')).not.toBeInTheDocument()
    // 「収まります」「競合なし」に相当する肯定的な表示は出さない
    expect(screen.queryByText(/チューナー/)).not.toBeInTheDocument()
  })
})

/**
 * `CapacityBands`（overlay）と `CapacityBandLabels`（gutterOverlay）は対で
 * configure する規律をここで固定する。`ProgramGrid` は 2 つの独立した prop
 * を持つので（局の列と時間軸列は別の DOM 部分木で、これ以上結合させると
 * 密結合が増えるだけという判定。issue #460 レビュー「対応不要」）、
 * `overlay` だけ渡す呼び出しは型では防げない --- 実際にレビューで見つかった
 * 「片方だけ渡すと見える警告が黙って消える」を退行させないためのテスト。
 */
describe('CapacityBands と CapacityBandLabels は対で configure する', () => {
  it('overlay だけ渡すと、帯の色と sr-only は出るが見える警告が一切出ない', () => {
    render(
      <ProgramGrid
        services={[service]}
        programs={[]}
        axis={axis}
        reservationByProgramId={new Set()}
        selectedProgramId={null}
        onSelect={vi.fn()}
        now={at(19 * 60)}
        overlay={(gridAxis) => <CapacityBands axis={gridAxis} overages={[overage(20 * 60, 21 * 60)]} />}
        // gutterOverlay を渡していない --- ここが対を崩した呼び出し
      />,
    )

    expect(band(20 * 60)).toBeInTheDocument()
    expect(screen.getByText(/はチューナーが不足しています/)).toBeInTheDocument()
    // 見える警告（時間軸列のラベル）は無い --- 呼び出し側が両方配線しないと
    // 沈黙して壊れる、という危険を明示するテスト
    expect(screen.queryByTestId('capacity-band-label')).not.toBeInTheDocument()
  })
})

/**
 * 帯の色。jsdom は色を計算しないのでクラス名を見る（実画素とコントラストの判定は
 * web/e2e/design.mjs。docs/frontend/design.md）。
 */
describe('CapacityBands の色', () => {
  it('警告の信号色（琥珀）を使い、Tailwind 標準パレットを直接使わない', () => {
    renderGrid([overage(20 * 60, 21 * 60)], [])

    const el = band(20 * 60)
    expect(el).not.toBeNull()
    // 塗りは淡く、境界は罫線が伝える（番組セルのタイトルを潰さないため）
    expect(el).toHaveClass('bg-warning/10')
    expect(el).toHaveClass('border-warning/80')
    expect(el!.className).not.toMatch(/amber|yellow|orange/)
  })

  it('ラベルも同じ琥珀トークンを使う（帯のためだけの色を作らない）', () => {
    renderGrid([overage(20 * 60, 21 * 60)], [])

    const label = screen.getByText('BS-1')
    expect(label).toHaveClass('text-warning')
    expect(label.className).not.toMatch(/amber|yellow|orange/)
  })
})

/**
 * 時間軸列のラベル配置。jsdom はレイアウトを計算しないので、実際に読めるかは
 * web/e2e/design.mjs が測る（scrollWidth <= clientWidth / 目盛りを隠さない）。
 * ここで固定するのは「どの overage にどの top を割り当てるか」という純粋な
 * ロジックだけ --- CapacityBandLabels の並べ替え・押し下げの分岐。
 */
describe('CapacityBandLabels の配置', () => {
  it('帯の上端ではなく自分の高さぶんだけを占める（帯の全高を塗らない）', () => {
    // 3 時間の帯でもラベルの高さは固定（帯の高さに引き伸ばさない）
    renderGrid([overage(20 * 60, 23 * 60)], [])

    const label = screen.getByText('BS-1')
    // 帯本体（3 時間 = 360px）より明らかに小さい固定高さ
    expect(label.style.height).not.toBe('360px')
  })

  it('同時刻に重なる 2 本の帯があるとき、両方のラベルが別の位置に出る', () => {
    // 20:00-21:00（BS）と 20:15-20:45（GR）--- 同じ時間帯に重なる
    renderGrid(
      [overage(20 * 60, 21 * 60, { jammedTypes: ['BS'] }), overage(20 * 60 + 15, 20 * 60 + 45, { jammedTypes: ['GR'] })],
      [],
    )

    const labels = screen.getAllByTestId('capacity-band-label')
    expect(labels).toHaveLength(2)
    // 両方読める --- 同じ top に重なって片方が不透明な地の下に隠れていない
    expect(labels[0]?.style.top).not.toBe(labels[1]?.style.top)
    expect(screen.getByText('BS-1')).toBeInTheDocument()
    expect(screen.getByText('GR-1')).toBeInTheDocument()
  })
})
