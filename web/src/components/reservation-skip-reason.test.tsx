import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { Reservation } from '@/api/generated'
import { ReservationVerdictBadge } from '@/components/reservation-row-parts'
import { ReservationSkipReason } from '@/components/reservation-skip-reason'
import { renderInRouter } from '@/test/router'

/**
 * 同期レンダリングのみのコンポーネントなので、`ProgramOverlapWarning` のような
 * 「クエリの決着を待ってから不在を確認する」仕掛けは要らない（fetch をしない）。
 * ただし「何も描画しない」を確かめるテストが空虚に通らないよう、**同じ入力から
 * 描画される他の要素**を対照として一緒に確認する。
 */
function reservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    site: 'default',
    programId: 1150000115041234,
    source: 'rule',
    state: 'active',
    skip: false,
    title: 'テスト番組',
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: '2026-07-30T19:00:00+09:00',
    durationMs: 1800000,
    createdAt: '2026-07-28T00:00:00+09:00',
    updatedAt: '2026-07-28T00:00:00+09:00',
    series: null,
    ...overrides,
  }
}

describe('ReservationVerdictBadge', () => {
  it('録画予定は無印にする', () => {
    const { container } = render(<ReservationVerdictBadge reservation={reservation()} overages={[]} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('orphaned を skip より優先して destructive の結論を出す', () => {
    render(
      <ReservationVerdictBadge reservation={reservation({ state: 'orphaned', skip: true })} overages={[]} />,
    )
    expect(screen.getByText('録画されず')).toBeInTheDocument()
    // text-muted-foreground だと bg-muted との合成後コントラストがライトで
    // 4.5 を割る（issue #308）。jsdom は色を測れないので、退行防止としては
    // クラス名のリテラル比較まで（実測は e2e:design の担当）。
    expect(screen.getByText('録画されず').className).toContain('text-destructive')
  })

  it('根拠があれば録画しない理由を重複として出す', () => {
    render(
      <ReservationVerdictBadge
        reservation={reservation({ skip: true, dedupMatchRecordingId: 12, dedupSimilarity: 0.9 })}
        overages={[]}
      />,
    )
    expect(screen.getByText('録画しない（重複）')).toBeInTheDocument()
    expect(screen.getByText('録画しない（重複）').className).toContain('text-foreground')
  })

  it('根拠がない skip は除外として出す', () => {
    render(<ReservationVerdictBadge reservation={reservation({ skip: true })} overages={[]} />)
    expect(screen.getByText('録画しない（除外）')).toBeInTheDocument()
  })

  it('交差する不足区間は既存の番組表リンクバッジを使う', async () => {
    const startMs = Date.parse(reservation().startAt)
    renderInRouter(
      <ReservationVerdictBadge
        reservation={reservation()}
        overages={[{
          site: 'default',
          startAt: new Date(startMs).toISOString(),
          endAt: new Date(startMs + 30 * 60_000).toISOString(),
          shortfall: 1,
          jammedTypes: ['GR'],
        }]}
      />,
    )
    const link = await screen.findByRole('link', { name: /この時間帯はチューナーが不足しています/ })
    expect(link).toHaveAttribute('href', expect.stringContaining('/programs?'))
  })
})

describe('ReservationSkipReason', () => {
  // 「録画 #id」がリンクになった（issue #233 M6-5）ので `Link` を描くのに
  // ルーターが要る（他のテストは `<span>` だけなので不要。この 1 件だけ
  // `renderInRouter` を使う）。
  it('重複の根拠（録画 id と類似度）を出す。録画 id は録画単体ページへのリンク', async () => {
    renderInRouter(
      <ReservationSkipReason
        reservation={reservation({ skip: true, dedupMatchRecordingId: 12, dedupSimilarity: 0.875 })}
      />,
    )
    // 「録画 #12」というリンクが録画単体ページ（/recordings/12）を指す
    // （固有名詞はリンクにする、issue #233 の原則）。RouterProvider は初回
    // マッチが解決するまで何も描かないので findBy* で待つ。
    const link = await screen.findByRole('link', { name: '録画 #12' })
    expect(link).toHaveAttribute('href', '/recordings/12')
    expect(screen.getByText(/録画しません（重複:/)).toBeInTheDocument()
    expect(screen.getByText(/類似度 0\.88/)).toBeInTheDocument()
  })

  it('根拠が無い skip は除外として説明する', () => {
    render(<ReservationSkipReason reservation={reservation({ skip: true })} />)
    expect(screen.getByText('録画しません（除外）')).toBeInTheDocument()
    expect(screen.queryByText(/類似度/)).not.toBeInTheDocument()
  })

  it('skip でなければ何も描画しない', () => {
    const { container } = render(<ReservationSkipReason reservation={reservation()} />)
    expect(container).toBeEmptyDOMElement()
  })
})
