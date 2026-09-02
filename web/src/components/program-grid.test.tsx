import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ProgramListItem } from '@/api/generated'
import { ProgramGrid } from '@/components/program-grid'
import { programIdentity, type SiteProgram, type SiteService } from '@/lib/all-sites-services'
import { spanToPx, type TimeAxis } from '@/lib/epg-grid'
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

function service(serviceId: number, name: string): SiteService {
  return {
    id: 32736 * 100_000 + serviceId,
    site: 'default',
    networkId: 32736,
    serviceId,
    name,
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  }
}

function program(
  programId: number,
  serviceId: number,
  startMinutes: number,
  durationMinutes: number,
  overrides: Partial<ProgramListItem> = {},
): SiteProgram {
  return {
    site: 'default',
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
  programs = [] as SiteProgram[],
  reservations = new Set<string>(),
  now = at(19 * 60),
  selectedProgramId = null as string | null,
  onSelect = vi.fn(),
  overlay,
}: {
  services?: SiteService[]
  programs?: SiteProgram[]
  reservations?: Set<string>
  now?: number
  selectedProgramId?: string | null
  onSelect?: (program: SiteProgram) => void
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
  it('GR のリモコン番号タグは text-foreground（issue #308。text-muted-foreground だと bg-muted との合成後コントラストがライトで 4.5 を割る）', () => {
    renderGrid({ services: [service(1024, 'NHK総合')] })

    // jsdom は色を測れないので、退行防止としてはクラス名のリテラル比較まで
    // （実測は e2e:design の担当）。
    const badge = screen.getByText('1')
    expect(badge.className).toContain('text-foreground')
    expect(badge.className).not.toContain('text-muted-foreground')
  })

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
    expect(column(1032)?.style.left).toBe('176px')
  })

  it('軸をまたぐ番組は軸の中だけを描く', () => {
    renderGrid({
      // 前日 23:00 から当日 1:00
      programs: [program(1, 1024, -60, 120)],
    })

    expect(cell(1).style.top).toBe('0px')
    expect(cell(1).style.height).toBe('120px')
  })

  it('セルの高さに応じて概要とジャンルを段階的に表示する', () => {
    renderGrid({
      programs: [
        program(1, 1024, 17 * 60, 53, { description: '短いセルの概要' }),
        program(2, 1024, 17 * 60 + 53, 54, { description: '二行ぶんの概要' }),
        program(3, 1024, 17 * 60 + 107, 75, { description: '高いセルの概要' }),
      ],
    })

    // 120px/h なので 53 分 = 106px、54 分 = 108px、75 分 = 150px。
    // 閾値を 0 にすると短いセルにも概要が出て、この期待が落ちる。
    expect(cell(1)).not.toHaveTextContent('短いセルの概要')
    expect(cell(2)).toHaveTextContent('二行ぶんの概要')
    expect(cell(2)).not.toHaveTextContent('ドラマ')
    expect(cell(3)).toHaveTextContent('高いセルの概要')
    expect(cell(3)).toHaveTextContent('ドラマ')
  })

  it('放送終了セルだけを読み上げで区別し、選択可能なままにする', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 30), program(2, 1024, 19 * 60 + 30, 30)],
      now: at(19 * 60 + 45),
    })

    expect(cell(1)).toHaveAttribute('data-ended', 'true')
    expect(cell(1).getAttribute('aria-label')).toMatch(/放送終了$/)
    expect(cell(1)).not.toBeDisabled()
    expect(cell(2)).not.toHaveAttribute('data-ended')
    expect(cell(2).getAttribute('aria-label')).not.toContain('放送終了')
  })

  it('5 分の放送終了セルは読み上げだけで伝え、見た目を変えない', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 5), program(2, 1024, 19 * 60 + 5, 6)],
      now: at(20 * 60),
    })

    expect(cell(1).getAttribute('aria-label')).toMatch(/放送終了$/)
    expect(cell(1).className).not.toContain('before:bg-muted/30')
    expect(cell(2).className).toContain('before:bg-muted/30')
  })

  it('終了時刻と現在時刻が同じセルは放送終了にしない', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 30)],
      now: at(19 * 60 + 30),
    })

    expect(cell(1)).not.toHaveAttribute('data-ended')
    expect(cell(1).getAttribute('aria-label')).not.toContain('放送終了')
  })

  it('予約済みの番組にだけ予約のマークが出る', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 60), program(2, 1024, 20 * 60, 60)],
      reservations: new Set([programIdentity('default', 1)]),
    })

    expect(cell(1)).toHaveAttribute('data-reserved', 'true')
    expect(cell(1).getAttribute('aria-label')).toContain('予約済み')
    // 見える「予約」。aria-label だけだと読み上げ専用で、グリッドを眺めても
    // 分からない（issue #307）。
    expect(cell(1)).toHaveTextContent('予約')
    expect(cell(2)).not.toHaveAttribute('data-reserved')
    expect(cell(2).getAttribute('aria-label')).not.toContain('予約済み')
    expect(cell(2)).not.toHaveTextContent('予約')
  })

  it('5 分の予約済み番組でも見える「予約」が残る', () => {
    renderGrid({
      programs: [program(1, 1024, 19 * 60, 5)],
      reservations: new Set([programIdentity('default', 1)]),
    })

    // セルの高さに下限は無い（5 分 = 10px）。overflow で本文が切れても
    // 印そのものは DOM に残す（実寸は e2e/grid-reserved.mjs が測る）。
    expect(cell(1).style.height).toBe('10px')
    expect(cell(1)).toHaveTextContent('予約')
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
      selectedProgramId: programIdentity('default', 2),
    })

    expect(cell(1)).toHaveAttribute('aria-pressed', 'false')
    expect(cell(2)).toHaveAttribute('aria-pressed', 'true')
  })

  it('現在時刻が軸の中にあれば全チャンネル縦断のインジケータを出す', () => {
    renderGrid({ programs: [program(1, 1024, 19 * 60, 60)], now: at(19 * 60 + 30) })

    const line = screen.getByTestId('program-grid-now-line')
    expect(line.style.top).toBe('2340px')
    // 帯と同じ層に置かれ、幅は全列ぶん（inset-0 の親が列の総幅を持つ）
    expect(line.parentElement?.parentElement?.style.width).toBe('176px')
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
    expect(band.parentElement?.parentElement?.style.width).toBe('352px')
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

  it('計測した列幅を配置と横の仮想化に使う', () => {
    const services = [
      service(1024, 'ch1'),
      service(1032, 'ch2'),
      service(1040, 'ch3'),
      service(1048, 'ch4'),
    ]
    renderGrid({
      services,
      programs: services.map((s, i) => program(i + 1, s.serviceId, 19 * 60, 60)),
    })

    // gutter 56px を除く 1000px を 4 列で割るので、実列幅は 250px。
    // left=440px でも overscan 1 列により先頭列が残る。仮想化だけ固定幅
    // 176px のままだと先頭列が落ちるため、配置と可視判定の不一致を検出できる。
    stubViewport(screen.getByTestId('program-grid'), { left: 440, width: 1056, height: 4000 })

    expect(column(1024)).not.toBeNull()
    expect(column(1032)?.style.left).toBe('250px')
    expect(column(1032)?.style.width).toBe('250px')
    expect(column(1032)?.parentElement?.style.width).toBe('1000px')
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
