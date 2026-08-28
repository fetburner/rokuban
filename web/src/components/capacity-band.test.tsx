import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, ProgramListItem, Service } from '@/api/generated'
import { CapacityBands } from '@/components/capacity-band'
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

  it('不足本数と詰まった種別を出す', () => {
    renderGrid([overage(20 * 60, 21 * 60, { shortfall: 2, jammedTypes: ['GR', 'BS'] })], [])

    expect(screen.getByText('チューナー不足（GR・BS が 2 本）')).toBeInTheDocument()
    // 読み上げ用には時刻付きの文（帯が短くて見えるラベルが出ないこともある）
    expect(
      screen.getByText('20:00〜21:00 はチューナーが不足しています（GR・BS が 2 本不足）'),
    ).toBeInTheDocument()
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

    const label = screen.getByText('チューナー不足（BS が 1 本）')
    expect(label).toHaveClass('text-warning')
    expect(label.className).not.toMatch(/amber|yellow|orange/)
  })
})
