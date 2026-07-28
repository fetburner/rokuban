import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { Reservation } from '@/api/generated'
import {
  ReservationSkipBadge,
  ReservationSkipReason,
} from '@/components/reservation-skip-reason'

/**
 * 同期レンダリングのみのコンポーネントなので、`ProgramOverlapWarning` のような
 * 「クエリの決着を待ってから不在を確認する」仕掛けは要らない（fetch をしない）。
 * ただし「何も描画しない」を確かめるテストが空虚に通らないよう、**同じ入力から
 * 描画される他の要素**を対照として一緒に確認する。
 */
function reservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    id: 1,
    programId: 1150000115041234,
    source: 'rule',
    state: 'active',
    skip: false,
    title: 'テスト番組',
    startAt: '2026-07-30T19:00:00+09:00',
    durationMs: 1800000,
    createdAt: '2026-07-28T00:00:00+09:00',
    updatedAt: '2026-07-28T00:00:00+09:00',
    ...overrides,
  }
}

describe('ReservationSkipBadge', () => {
  it('skip でなければ何も描画しない', () => {
    const { container } = render(<ReservationSkipBadge reservation={reservation()} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('根拠があれば「重複」を出す', () => {
    render(
      <ReservationSkipBadge
        reservation={reservation({ skip: true, dedupMatchRecordingId: 12, dedupSimilarity: 0.9 })}
      />,
    )
    expect(screen.getByText('重複')).toBeInTheDocument()
    expect(screen.queryByText('除外')).not.toBeInTheDocument()
  })

  // 根拠の有無で 2 つの経路を区別する（skip が立つ経路は重複排除だけではない）。
  it('根拠が無い skip は「除外」を出す', () => {
    render(<ReservationSkipBadge reservation={reservation({ skip: true })} />)
    expect(screen.getByText('除外')).toBeInTheDocument()
    expect(screen.queryByText('重複')).not.toBeInTheDocument()
  })

  // dedup 列があっても skip が false なら出さない（ユーザーの record 意図が
  // 重複判定に勝っている状態。根拠は残るが録画される。EPGStation#473）。
  it('根拠があっても skip が false なら何も描画しない', () => {
    const { container } = render(
      <ReservationSkipBadge
        reservation={reservation({ skip: false, dedupMatchRecordingId: 12, dedupSimilarity: 0.9 })}
      />,
    )
    expect(container).toBeEmptyDOMElement()
  })
})

describe('ReservationSkipReason', () => {
  it('重複の根拠（録画 id と類似度）を出す', () => {
    render(
      <ReservationSkipReason
        reservation={reservation({ skip: true, dedupMatchRecordingId: 12, dedupSimilarity: 0.875 })}
      />,
    )
    expect(screen.getByText(/録画 #12/)).toBeInTheDocument()
    expect(screen.getByText(/類似度 0\.88/)).toBeInTheDocument()
  })

  it('根拠が無い skip は除外として説明する', () => {
    render(<ReservationSkipReason reservation={reservation({ skip: true })} />)
    expect(screen.getByText('録画しない（除外）')).toBeInTheDocument()
    expect(screen.queryByText(/類似度/)).not.toBeInTheDocument()
  })

  it('skip でなければ何も描画しない', () => {
    const { container } = render(<ReservationSkipReason reservation={reservation()} />)
    expect(container).toBeEmptyDOMElement()
  })
})
