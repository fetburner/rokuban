import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ProgramListItem, Service } from '@/api/generated'
import { ProgramGrid } from '@/components/program-grid'
import { epgColumnWidthPx, spanToPx, type TimeAxis } from '@/lib/epg-grid'
import { formatTime } from '@/lib/format'

/** 軸はローカル時刻の 0 時基準（hourTicks がローカルの毎時 0 分を返すため）。 */
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

function service(serviceId: number, name: string): Service {
  return {
    networkId: 32736,
    serviceId,
    name,
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
  }
}

function program(
  programId: number,
  serviceId: number,
  startMinutes: number,
  durationMinutes: number,
  overrides: Partial<ProgramListItem> = {},
): ProgramListItem {
  return {
    programId,
    networkId: 32736,
    serviceId,
    eventId: programId,
    startAt: new Date(at(startMinutes)).toISOString(),
    endAt: new Date(at(startMinutes + durationMinutes)).toISOString(),
    durationMs: durationMinutes * 60_000,
    name: `番組 ${programId}`,
    description: '',
    genres: [3],
    isFree: true,
    ...overrides,
  }
}

function renderGrid({
  services = [service(1024, 'NHK総合')],
  programs = [] as ProgramListItem[],
  reservations = new Set<number>(),
  now = at(19 * 60),
  selectedProgramId = null as number | null,
  onSelect = vi.fn(),
  overlay,
}: {
  services?: Service[]
  programs?: ProgramListItem[]
  reservations?: Set<number>
  now?: number
  selectedProgramId?: number | null
  onSelect?: (program: ProgramListItem) => void
  overlay?: (axis: TimeAxis) => React.ReactNode
} = {}) {
  const view = render(
    <ProgramGrid
      services={services}
      programs={programs}
      axis={axis}
      reservationByProgramId={reservations}
      selectedProgramId={selectedProgramId}
      onSelect={onSelect}
      now={now}
      overlay={overlay}
    />,
  )
  return { ...view, onSelect }
}

/** cell は programId でセルを引く。 */
function cell(programId: number): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-program-id="${programId}"]`)
  if (!el) throw new Error(`cell ${programId} not found`)
  return el
}

function queryCell(programId: number): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[data-program-id="${programId}"]`)
}

function column(serviceId: number): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[data-service-id="${serviceId}"]`)
}

/**
 * stubViewport は jsdom にレイアウトが無いぶんを埋める。
 *
 * jsdom は clientHeight / clientWidth を常に 0 として返し、setup.ts の
 * ResizeObserver スタブも何も通知しないため、これを入れないとグリッドは
 * 「未計測 = 全部描く」の分岐にしか入らない。仮想化が実際に間引くことを
 * 確かめるテストは、値を差し込んでから scroll を発火させる必要がある。
 *
 * scrollTop は setter 付きで定義する（コンポーネントは初期表示で「今」の位置へ
 * scrollTop を代入するので、値だけの defineProperty だと strict mode で例外になる）。
 */
function stubViewport(
  el: HTMLElement,
  { top = 0, left = 0, height = 600, width = 800 } = {},
): void {
  Object.defineProperty(el, 'scrollTop', { configurable: true, get: () => top, set: () => {} })
  Object.defineProperty(el, 'scrollLeft', { configurable: true, get: () => left, set: () => {} })
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => height })
  Object.defineProperty(el, 'clientWidth', { configurable: true, get: () => width })
  fireEvent.scroll(el)
}

describe('ProgramGrid', () => {
  it('番組の位置と高さが放送時刻に対応する', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 60), program(2, 1024, 20 * 60, 30)],
    })

    // 19:00 開始・60 分 → 縮尺 120px/h なので top 2280px・高さ 120px
    expect(cell(1).style.top).toBe('2280px')
    expect(cell(1).style.height).toBe('120px')
    // 20:00 開始・30 分
    expect(cell(2).style.top).toBe('2400px')
    expect(cell(2).style.height).toBe('60px')
  })

  it('重なっている番組は同じ縦位置に、別の列に置かれる', () => {
    renderGrid({
      services: [service(1024, 'NHK総合'), service(1032, 'NHKEテレ')],
      programs: [program(1, 1024, 19 * 60, 60), program(2, 1032, 19 * 60, 120)],
    })

    // 同時性は縦位置の一致として現れる（グリッドの存在理由）
    expect(cell(1).style.top).toBe(cell(2).style.top)
    expect(cell(1).style.height).not.toBe(cell(2).style.height)
    // 列は横位置で分かれる
    expect(column(1024)?.style.left).toBe('0px')
    expect(column(1032)?.style.left).toBe(`${epgColumnWidthPx}px`)
  })

  it('軸をまたぐ番組は軸の中だけを描く', () => {
    renderGrid({
      // 前日 23:00 から当日 1:00
      programs: [program(1, 1024, -60, 120)],
    })

    expect(cell(1).style.top).toBe('0px')
    expect(cell(1).style.height).toBe('120px')
  })

  it('予約済みの番組にだけ予約のマークが出る', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 60), program(2, 1024, 20 * 60, 60)],
      reservations: new Set([1]),
    })

    expect(cell(1)).toHaveAttribute('data-reserved', 'true')
    expect(cell(1).getAttribute('aria-label')).toContain('予約済み')
    expect(cell(2)).not.toHaveAttribute('data-reserved')
    expect(cell(2).getAttribute('aria-label')).not.toContain('予約済み')
  })

  it('ジャンルが aria-label に入る（色だけの情報にしない）', () => {
    const drama = program(1, 1024, 19 * 60, 60, { genres: [3] })
    const unknown = program(2, 1024, 20 * 60, 60, { genres: [] })
    renderGrid({ programs: [drama, unknown] })

    expect(cell(1).getAttribute('aria-label')).toContain('ドラマ')
    // 知らない / 無いジャンルは「その他」に丸めない（分類の失敗を分類済みに見せない）
    expect(cell(2).getAttribute('aria-label')).toBe(`${formatTime(unknown.startAt)} · 番組 2`)
  })

  it('セルを押すと番組が選択される', () => {
    const onSelect = vi.fn()
    renderGrid({ programs: [program(1, 1024, 19 * 60, 60)], onSelect })

    fireEvent.click(cell(1))
    expect(onSelect).toHaveBeenCalledTimes(1)
    expect(onSelect.mock.calls[0][0].programId).toBe(1)
  })

  it('選択中の番組は aria-pressed で示す', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 60), program(2, 1024, 20 * 60, 60)],
      selectedProgramId: 2,
    })

    expect(cell(1)).toHaveAttribute('aria-pressed', 'false')
    expect(cell(2)).toHaveAttribute('aria-pressed', 'true')
  })

  it('現在時刻が軸の中にあれば全チャンネル縦断のインジケータを出す', () => {
    renderGrid({ programs: [program(1, 1024, 19 * 60, 60)], now: at(19 * 60 + 30) })

    const line = screen.getByTestId('program-grid-now-line')
    expect(line.style.top).toBe('2340px')
    // 帯と同じ層に置かれ、幅は全列ぶん（inset-0 の親が列の総幅を持つ）
    expect(line.parentElement?.parentElement?.style.width).toBe(`${epgColumnWidthPx}px`)
    expect(screen.getByTestId('program-grid-now-label')).toBeInTheDocument()
  })

  it('現在時刻が軸の外ならインジケータを出さない', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 60)],
      now: axis.endMs + 3_600_000,
    })

    // 軸の外という判定を落とすと 24 時間の外に線が描かれる（見えないので気付かない）
    expect(screen.queryByTestId('program-grid-now-line')).not.toBeInTheDocument()
    expect(screen.queryByTestId('program-grid-now-label')).not.toBeInTheDocument()
  })

  it('全チャンネル縦断の帯を重ねられる（M2-10 の容量超過はここに乗る）', () => {
    const services = [service(1024, 'NHK総合'), service(1032, 'NHKEテレ')]
    renderGrid({
      services,
      programs: [program(1, 1024, 19 * 60, 60)],
      overlay: (gridAxis) => {
        const rect = spanToPx(gridAxis, at(20 * 60), at(21 * 60))
        if (!rect) return null
        return (
          <div
            data-testid="capacity-band"
            className="absolute inset-x-0"
            style={{ top: rect.topPx, height: rect.heightPx }}
          />
        )
      },
    })

    const band = screen.getByTestId('capacity-band')
    // 20:00-21:00 の帯は、同じ時刻の番組セルと同じ座標に来る
    expect(band.style.top).toBe('2400px')
    expect(band.style.height).toBe('120px')
    // 帯の層は列の総幅を張る（番組ではなく区間に描く = チャンネルを縦断する）
    expect(band.parentElement?.parentElement?.style.width).toBe(
      `${services.length * epgColumnWidthPx}px`,
    )
  })

  it('計測できていないうちは間引かない（空のグリッドを出さない）', () => {
    renderGrid({
      programs: [program(1, 1024, 2 * 60, 60), program(2, 1024, 19 * 60, 60)],
    })

    // jsdom は clientHeight が 0。ここで間引くと画面に何も出ない
    expect(queryCell(1)).not.toBeNull()
    expect(queryCell(2)).not.toBeNull()
  })

  it('可視範囲から外れた番組を DOM から落とす（縦の仮想化）', () => {
    renderGrid({
      programs: [
        program(1, 1024, 2 * 60, 60), // 02:00 — 遠く離れている
        program(2, 1024, 19 * 60, 60), // 19:00 — 可視
        program(3, 1024, 17 * 60, 60), // 17:00 — オーバースキャンの内側
      ],
    })

    // 19:00（= 2280px）を上端に、高さ 600px の窓を差し込む
    stubViewport(screen.getByTestId('program-grid'), { top: 2280, height: 600 })

    expect(queryCell(1)).toBeNull()
    expect(queryCell(2)).not.toBeNull()
    expect(queryCell(3)).not.toBeNull()
  })

  it('可視範囲から外れた列を DOM から落とす（横の仮想化）', () => {
    const services = [
      service(1024, 'ch1'),
      service(1032, 'ch2'),
      service(1040, 'ch3'),
      service(1048, 'ch4'),
      service(1056, 'ch5'),
    ]
    renderGrid({
      services,
      programs: services.map((s, i) => program(i + 1, s.serviceId, 19 * 60, 60)),
    })

    // 幅 200px（= 2 列弱）ぶんしか見えていない状態
    stubViewport(screen.getByTestId('program-grid'), { left: 0, width: 200, height: 4000 })

    expect(column(1024)).not.toBeNull()
    expect(column(1032)).not.toBeNull()
    expect(column(1056)).toBeNull()
    // ヘッダも同じ範囲で間引く（列だけ消えて名前が残ると対応がずれる）
    expect(screen.getByText('ch1')).toBeInTheDocument()
    expect(screen.queryByText('ch5')).not.toBeInTheDocument()
  })
})
