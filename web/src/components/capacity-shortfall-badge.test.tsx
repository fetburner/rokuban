import { screen, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { CapacityOverage } from '@/api/generated'
import { CapacityShortfallBadge } from '@/components/capacity-shortfall-badge'
import { renderInRouter } from '@/test/router'

/** 時刻はローカルの 0 時基準（他の capacity 系テストと同じ組み方）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

/** at は 0 時からの分数を epoch ms に直す。 */
function atMs(minutes: number): number {
  return dayStart.getTime() + minutes * 60_000
}

/** iso は 0 時からの分数を ISO 文字列に直す。 */
function iso(minutes: number): string {
  return new Date(atMs(minutes)).toISOString()
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
 * CapacityShortfallBadge は「その時間帯にチューナーが足りていない」ことを一覧の
 * 行に出すバッジ。issue #233 M6-5 で番組表（`/programs`。ホーム新設（M8-3）前は
 * `/` だった）への `Link` になった --- ここは
 * その導線（href の宛先・`at` パラメータ）と、リンク化後も読み上げの規律
 * （見える側 `aria-hidden`、読み上げ文は `sr-only`）が保たれていることを見る。
 * 出し分け（交差する/しない）自体は `lib/capacity.test.ts` と
 * `pages/reservations.test.tsx` が担当するので、ここでは重複させない。
 */
describe('CapacityShortfallBadge', () => {
  it('交差する区間があれば、番組表ルートへ view=grid と不足区間の開始時刻（at）を積んだリンクになる', async () => {
    renderInRouter(
      <CapacityShortfallBadge
        overages={[overage(19 * 60, 20 * 60)]}
        site="default"
        startMs={atMs(19 * 60)}
        endMs={atMs(20 * 60)}
      />,
    )

    const link = await screen.findByRole('link')
    // `view=grid` を明示することで、`lg` 以上では推論を挟まず初回レンダーから
    // グリッドで開く（issue #437。`pages/programs.tsx` の `showGrid` 参照）
    expect(link).toHaveAttribute('href', `/programs?view=grid&at=${atMs(19 * 60)}`)
  })

  it('交差する区間が無ければ何も描画しない（リンクも作らない）', async () => {
    const { router } = renderInRouter(
      <CapacityShortfallBadge
        overages={[overage(10 * 60, 11 * 60)]}
        site="default"
        startMs={atMs(19 * 60)}
        endMs={atMs(20 * 60)}
      />,
    )

    // RouterProvider の初回マッチが解決するのを待ってから「無い」ことを見る
    // （非同期の空虚な成功を避ける。CLAUDE.md テスト規律）。マッチが解決する前は
    // 何も描かれておらず、それを「バッジが無い」と誤読しないよう、ルーター自身の
    // 状態（`idle`）で解決を待つ。
    await waitFor(() => expect(router.state.status).toBe('idle'))
    expect(screen.queryByRole('link')).toBeNull()
  })

  it('見える側は aria-hidden、読み上げ文は sr-only のまま（リンク化後も維持）', async () => {
    renderInRouter(
      <CapacityShortfallBadge
        overages={[overage(19 * 60, 20 * 60)]}
        site="default"
        startMs={atMs(19 * 60)}
        endMs={atMs(20 * 60)}
      />,
    )

    // アクセシブルネームは sr-only の文（主語が「時間帯」であることが伝わる文）
    // になる。<a> の accessible name は aria-hidden な子を無視し、sr-only な
    // 子（視覚的に隠すだけで除外されない）は含めるので、リンク化のために
    // 読み上げ文を書き直す必要が無いことがここで確かめられる。
    const link = await screen.findByRole('link', {
      name: 'この時間帯はチューナーが不足しています（BS が 1 本不足）',
    })
    // 見える短いラベルは二重読みを避けるため aria-hidden のまま
    expect(link).toHaveTextContent('チューナー不足（BS が 1 本）')
  })

  it('className を渡すと既定のクラスに追加される（警告色は上書きされない）', async () => {
    renderInRouter(
      <CapacityShortfallBadge
        overages={[overage(19 * 60, 20 * 60)]}
        site="default"
        startMs={atMs(19 * 60)}
        endMs={atMs(20 * 60)}
        className="mr-4"
      />,
    )

    const link = await screen.findByRole('link')
    expect(link).toHaveClass('mr-4')
    expect(link).toHaveClass('bg-warning/10')
    expect(link).toHaveClass('text-warning')
  })
})
