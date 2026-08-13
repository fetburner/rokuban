import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { LiveInterruptionWarning } from '@/components/live-interruption-warning'

/**
 * 同期レンダリングのみのコンポーネントなので `ProgramOverlapWarning` のような
 * 「クエリの決着を待ってから不在を確認する」仕掛けは要らない（fetch をしない）。
 *
 * `reservation === null` のときに何も描画しないことは `pages/live.test.tsx` でも
 * 文言の regex（`/録画予約/`）で確認しているが、それは**語の不在**しか主張しない
 * ---「いまなら安全に視聴できます」のような、指定した語を含まない別の肯定文言に
 * 変異させても regex では検出できない（レビューでの指摘。実際にこの変異で
 * `pnpm test` が全件通ることを確認した上で、このテストを足した）。ここでは
 * **描画そのものの不在**（`container` が空であること）を主張することで、
 * どんな文言の変異であっても検出できる形にする。
 */
describe('LiveInterruptionWarning', () => {
  it('reservation が null なら何も描画しない（どんな文言の肯定的な表示も無い）', () => {
    const { container } = render(<LiveInterruptionWarning reservation={null} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('reservation があれば条件付きの警告文言を描画する', () => {
    render(<LiveInterruptionWarning reservation={{ startAt: '2026-07-25T19:00:00+09:00' }} />)

    expect(screen.getByText(/から録画予約があります/)).toBeInTheDocument()
    // 「不足すると中断されます」という条件付きの文言（issue #235 の「罠」）
    expect(screen.getByText(/チューナーが不足すると視聴は中断されます/)).toBeInTheDocument()
  })
})
