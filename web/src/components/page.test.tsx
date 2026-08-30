import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { EmptyState, ErrorState, ListSkeleton, Skeleton } from '@/components/page'

/**
 * 走査線の適用箇所を固定するテスト（不変条件 8）。
 *
 * `docs/frontend/design.md`「走査線は 3 箇所限定」の 3 箇所のうち、ここでは
 * EmptyState（空状態）と Skeleton / ListSkeleton（読み込み中）の 2 つを見る
 * （ON AIR は pages/live.test.tsx）。**通るだけでは何も保証しない**ので、
 * `scanlines` を外す・`text-foreground` を `text-muted-foreground` に戻す
 * という 2 通りの変異でこのテストが実際に落ちることを確認した
 * （報告参照。ここではアサーションの形だけを残す）。
 */
describe('EmptyState', () => {
  it('走査線ユーティリティ（scanlines）を持つ', () => {
    render(<EmptyState>空です</EmptyState>)
    const el = screen.getByText('空です')
    expect(el.className.split(' ')).toContain('scanlines')
  })

  it('文字色は text-foreground（text-muted-foreground ではない）', () => {
    // `.scanlines` の間隙は `--scanline` トークンに近く、`text-muted-foreground`
    // （= `--scanline` 相当）のままだと文字が間隙と衝突して読めなくなる
    // （index.css の `.scanlines` コメント参照）。`text-foreground` が正しい
    render(<EmptyState>空です</EmptyState>)
    const el = screen.getByText('空です')
    const classes = el.className.split(' ')
    expect(classes).toContain('text-foreground')
    expect(classes).not.toContain('text-muted-foreground')
  })

  it('<div> で包む（<p> にしない。issue #137 の hydration 警告の既往）', () => {
    render(<EmptyState>空です</EmptyState>)
    const el = screen.getByText('空です')
    expect(el.tagName).toBe('DIV')
  })
})

describe('ErrorState', () => {
  it('走査線を持たない（3 箇所限定の対象外）', () => {
    render(<ErrorState>失敗しました</ErrorState>)
    const el = screen.getByText('失敗しました')
    expect(el.className.split(' ')).not.toContain('scanlines')
  })
})

describe('Skeleton', () => {
  it('走査線ユーティリティ（scanlines）を持つ', () => {
    const { container } = render(<Skeleton className="h-4" />)
    const el = container.firstElementChild
    expect(el).not.toBeNull()
    expect(el!.className.split(' ')).toContain('scanlines')
  })

  it('渡した className（高さ・角丸の指定）を保つ', () => {
    const { container } = render(<Skeleton className="h-14" />)
    const el = container.firstElementChild!
    expect(el.className.split(' ')).toContain('h-14')
  })
})

describe('ListSkeleton', () => {
  it('内側の Skeleton がすべて走査線ユーティリティを持つ', () => {
    const { container } = render(<ListSkeleton rows={3} />)
    const skeletons = container.querySelectorAll('.scanlines')
    expect(skeletons).toHaveLength(3)
  })

  /**
   * WCAG 4.1.3（ステータスメッセージ）: 読み込み中という状態変化を、
   * フォーカスを移さずに支援技術へ伝える。`role="status"` を外すと落ちる
   * ことを確認済み（報告参照）。
   */
  it('role="status" と sr-only の「読み込み中」を 1 つだけ持つ', () => {
    render(<ListSkeleton rows={3} />)
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent('読み込み中')
    // 各行の Skeleton には重ねない（重ねると行数ぶん読み上げてしまう）
    expect(screen.getAllByRole('status')).toHaveLength(1)
  })

  it('内側の Skeleton は装飾のみ（aria-hidden）', () => {
    const { container } = render(<ListSkeleton rows={2} />)
    const skeletons = container.querySelectorAll('.scanlines')
    for (const el of skeletons) {
      expect(el).toHaveAttribute('aria-hidden', 'true')
    }
  })
})
