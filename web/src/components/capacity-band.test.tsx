import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { CapacityOverage } from '@/api/generated'
import { CapacityBandLabels, CapacityBands } from '@/components/capacity-band'
import { ProgramGrid } from '@/components/program-grid'
import type { SiteProgram, SiteService } from '@/lib/all-sites-services'
import { type TimeAxis } from '@/lib/epg-grid'

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

const service: SiteService = {
  id: 3273601024,
  site: 'default',
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

function program(programId: number, startMinutes: number, durationMinutes: number): SiteProgram {
  return {
    site: 'default',
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
function renderGrid(overages: CapacityOverage[], programs: SiteProgram[]) {
  return render(
    <ProgramGrid
      services={[service]}
      programs={programs}
      axis={axis}
      reservationByProgramId={new Set()}
      selectedProgramId={null}
      onSelect={vi.fn()}
      now={at(19 * 60)}
      siteOverlay={(gridAxis, site) => (
        <CapacityBands axis={gridAxis} overages={overages} site={site} />
      )}
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

/**
 * `orderServices` は種別を最外に持つため、GR + BS を両方持つ site は
 * `siteColumnRanges` 上で非隣接な複数の走に分かれる（issue #460 再レビュー）。
 * `ProgramGrid` は走ごとに `siteOverlay` を呼ぶので、`CapacityBands` 側で
 * 重複を止めないと同じ超過区間の sr-only が走の本数ぶん重複する。
 */
describe('CapacityBands の announce（同じ site の 2 本目以降の走で読み上げを重複させない）', () => {
  const gr = { ...service, channelType: 'GR' as const, serviceId: 1024, remoteControlKeyId: 1 }
  const bs = { ...service, channelType: 'BS' as const, serviceId: 1032, remoteControlKeyId: 0 }

  it('siteOverlay を走ごとに announce=isFirstRunForSite で呼び、sr-only は 1 回だけ出す', () => {
    // 同じ site（default）の GR 列と BS 列の間に他 site の列を挟み、
    // siteColumnRanges 上で 2 本の非隣接な走にする。
    const other = { ...service, site: 'other', channelType: 'GR' as const, serviceId: 999 }
    render(
      <ProgramGrid
        services={[gr, other, bs]}
        programs={[]}
        axis={axis}
        reservationByProgramId={new Set()}
        selectedProgramId={null}
        onSelect={vi.fn()}
        now={at(19 * 60)}
        siteOverlay={(gridAxis, site, isFirstRunForSite) => (
          <CapacityBands
            axis={gridAxis}
            overages={[overage(20 * 60, 21 * 60)]}
            site={site}
            announce={isFirstRunForSite}
          />
        )}
      />,
    )

    // 帯自体（見た目）は走ごとに 2 本描かれる（GR 列・BS 列の両方に色を付ける必要がある）。
    expect(screen.getAllByTestId('capacity-band').filter((el) => el.dataset.site === 'default')).toHaveLength(2)
    // だが読み上げ文（sr-only）は 1 回だけ。
    expect(
      screen.getAllByText('20:00〜21:00 はチューナーが不足しています（BS が 1 本不足）'),
    ).toHaveLength(1)
  })
})

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

  it('区間が同じ site の全チャンネルを縦断する（番組ではなく区間に描く）', () => {
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
        siteOverlay={(gridAxis, site) => (
          <CapacityBands axis={gridAxis} overages={[overage(20 * 60, 21 * 60)]} site={site} />
        )}
      />,
    )

    // 単一 site なので帯の層は全列の総幅を張る。
    expect(band(20 * 60)?.parentElement?.style.width).toBe('352px')
  })

  it('帯は同じ site の列領域だけに描き、別 site の列へ広がらない', () => {
    const other = { ...service, site: 'other' }
    render(
      <ProgramGrid
        services={[service, other]}
        programs={[]}
        axis={axis}
        reservationByProgramId={new Set()}
        selectedProgramId={null}
        onSelect={vi.fn()}
        now={at(19 * 60)}
        siteOverlay={(gridAxis, site) => (
          <CapacityBands
            axis={gridAxis}
            overages={[overage(20 * 60, 21 * 60, { site: 'other' })]}
            site={site}
          />
        )}
      />,
    )

    const rendered = screen.getByTestId('capacity-band')
    expect(rendered).toHaveAttribute('data-site', 'other')
    expect(rendered.closest('[data-testid="program-grid-site-overlay"]')).toHaveAttribute(
      'data-site',
      'other',
    )
    expect(screen.getAllByTestId('program-grid-site-overlay')).toHaveLength(2)
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
        siteOverlay={(gridAxis, site) => (
          <CapacityBands axis={gridAxis} overages={[overage(20 * 60, 21 * 60)]} site={site} />
        )}
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

    // 色は外側の箱に置き、アイコン・文字とも currentColor で引き継ぐ
    const label = screen.getByTestId('capacity-band-label')
    expect(label).toHaveClass('text-warning')
    expect(label.className).not.toMatch(/amber|yellow|orange/)
  })
})

/**
 * 時間軸列のラベル配置。jsdom はレイアウトを計算しないので、実際に読めるかは
 * web/e2e/design.mjs が測る（scrollWidth <= clientWidth / 目盛りを隠さない）。
 * ここで固定するのは「どの overage にどの top を割り当てるか」という純粋な
 * ロジックだけ --- avoidTickRow の分岐。
 *
 * 同一 site 内の不足区間は重ならないが、全 site の和集合では別 site の区間が
 * 同時刻に来る。後者だけは同じ gutter 内で積み、重なりを避ける。
 */
describe('CapacityBandLabels の配置', () => {
  it('帯の上端にアンカーし、高さは自分の内容ぶんだけ（帯の全高を塗らない）', () => {
    // 2 時間 45 分の帯（330px）でもラベルに高さを明示的に指定しない
    // （旧実装は帯の高さ＝ rect.heightPx をそのまま style.height に渡していた）。
    // 21:00 は目盛りの行なので avoidTickRow の対象外にするため 20:15 始まりにする
    renderGrid([overage(20 * 60 + 15, 23 * 60)], [])

    const band = document.querySelector<HTMLElement>(`[data-start-at="${iso(20 * 60 + 15)}"]`)
    expect(band?.style.height).toBe('330px')
    const label = screen.getByTestId('capacity-band-label')
    // 高さは CSS（line-height）任せで、inline style には持たせない
    expect(label.style.height).toBe('')
    expect(screen.getByText('BS-1')).toBeInTheDocument()
  })

  it('隣接する（重ならない）2 本の帯があるとき、両方のラベルが別の位置に出る', () => {
    // 20:00-20:30（BS）と 20:30-21:00（GR）--- 隣接するが重ならない
    // （サーバーが実際に返しうる形。issue #460 レビュー should 1）
    renderGrid(
      [
        overage(20 * 60, 20 * 60 + 30, { jammedTypes: ['BS'] }),
        overage(20 * 60 + 30, 21 * 60, { jammedTypes: ['GR'] }),
      ],
      [],
    )

    const labels = screen.getAllByTestId('capacity-band-label')
    expect(labels).toHaveLength(2)
    expect(labels[0]?.style.top).not.toBe(labels[1]?.style.top)
    expect(screen.getByText('BS-1')).toBeInTheDocument()
    expect(screen.getByText('GR-1')).toBeInTheDocument()
  })

  it('別 site の同時刻のラベルは gutter 内で積み、同じ位置に重ねない', () => {
    renderGrid(
      [
        overage(20 * 60 + 15, 21 * 60, { site: 'tokyo', shortfall: 1 }),
        overage(20 * 60 + 15, 21 * 60, { site: 'takamatsu', shortfall: 2 }),
      ],
      [],
    )

    const labels = screen.getAllByTestId('capacity-band-label')
    expect(labels).toHaveLength(2)
    expect(labels[0]).toHaveAttribute('data-site', 'tokyo')
    expect(labels[1]).toHaveAttribute('data-site', 'takamatsu')
    expect(labels[0]?.style.top).not.toBe(labels[1]?.style.top)
  })

  it('ちょうど正時に始まる区間は、目盛りの行を避けて下にずれる', () => {
    // 軸は 0 時基準・120px/h なので 22:00 の目盛りは top 2640px
    renderGrid([overage(22 * 60, 23 * 60)], [])

    const label = screen.getByTestId('capacity-band-label')
    // 目盛りの行（20px 分の目安）を避けて下端まで押し下げる
    expect(label.style.top).toBe('2660px')
  })

  it('正時からずれた区間は目盛りを避けない（誤って常にずらさない）', () => {
    // 22:15 は目盛りから離れているので avoidTickRow は何もしない
    renderGrid([overage(22 * 60 + 15, 23 * 60)], [])

    const label = screen.getByTestId('capacity-band-label')
    expect(label.style.top).toBe('2670px')
  })

  it('正時起点で 9〜18 分の帯は、目盛り回避で押し下げた先が直後の帯と重なるので見えるラベルを描かない', () => {
    // 22:00-22:10（10 分、高さ 20px）が正時に始まり、直後に 22:10-23:00
    // （高さ 100px）が続く --- サーバーが実際に返しうる形（issue #460
    // 再レビュー実測: [03:00, 03:10) の CS と [03:10, 04:00) の GR）。
    // 22:00 の帯は avoidTickRow が top 2660 まで押し下げるが、帯自身の下端も
    // top 2660 なのでラベルが帯の外（= 直後の帯の領域）へはみ出す --- 直す前の
    // 実装はここで描いてしまい、直後の帯のラベル（押し下げられず top 2660）と
    // 完全に重なっていた。
    renderGrid(
      [
        overage(22 * 60, 22 * 60 + 10, { jammedTypes: ['CS'], shortfall: 1 }),
        overage(22 * 60 + 10, 23 * 60, { jammedTypes: ['GR'], shortfall: 1 }),
      ],
      [],
    )

    // 正時起点の短い帯（CS）は見えるラベルを持たない
    expect(screen.queryByText('CS-1')).not.toBeInTheDocument()
    // 直後の帯（GR）のラベルは通常どおり自分の帯の上端に出る
    const labels = screen.getAllByTestId('capacity-band-label')
    expect(labels).toHaveLength(1)
    expect(screen.getByText('GR-1')).toBeInTheDocument()
    expect(labels[0]?.style.top).toBe('2660px')
  })

  it('アイコンを見た目の手がかりとして持つ（色だけに頼らない。issue #460 レビュー should 2）', () => {
    renderGrid([overage(20 * 60 + 15, 21 * 60)], [])

    const label = screen.getByTestId('capacity-band-label')
    expect(label.querySelector('svg')).not.toBeNull()
    // 短縮ラベルの文字自体は別の要素（省略記号の対象を分けるため）
    expect(screen.getByTestId('capacity-band-label-text')).toHaveTextContent('BS-1')
  })

  it('種別が 2 つ以上詰まって本数だけの短縮形（例: 「-2」）になっても、title に全文を持つ', () => {
    // jammedTypes が 2 つ以上あると shortageLabelCompact は種別を列挙せず
    // 「-2」のような本数だけの形になり、種別も単位も読めない（issue #460
    // 再レビュー nit 1）。native title でマウス操作者に全文を補う。
    renderGrid([overage(20 * 60 + 15, 21 * 60, { shortfall: 2, jammedTypes: ['GR', 'BS'] })], [])

    expect(screen.getByText('-2')).toBeInTheDocument()
    const label = screen.getByTestId('capacity-band-label')
    expect(label).toHaveAttribute('title', 'チューナー不足（GR・BS が 2 本）')
  })
})
