import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { EmptyState, ErrorState, ListSkeleton, PageHeader, Skeleton } from '@/components/page'

/**
 * 走査線の適用箇所を固定するテスト（不変条件 8）。
 *
 * EmptyState は読み込み中に見えない中立な地を保ち、Skeleton / ListSkeleton
 * だけが走査線を持つ（ON AIR は pages/live.test.tsx）。**通るだけでは何も
 * 保証しない**ので、EmptyState に `scanlines` を戻す・Skeleton から外す・
 * EmptyState の文字色を `text-muted-foreground` に戻す変異が落ちる形にする。
 */
describe('EmptyState', () => {
  it('中立な地を保ち、走査線と pulse を使わない', () => {
    render(<EmptyState>空です</EmptyState>)
    const el = screen.getByText('空です')
    const classes = el.className.split(' ')
    expect(classes).not.toContain('scanlines')
    expect(classes).not.toContain('animate-pulse')
  })

  it('文字色は text-foreground（text-muted-foreground ではない）', () => {
    // 空状態はページの地に載るため、本文の既定色を保つ。
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

// issue #467: 詳細 2 ページ（録画・予約）の「戻る」ボタンを乗せる leading
// スロット。省略すると 1 本目が、省いても他の子だけで通ってしまわないよう
// title と同時に描画されることも見る。
describe('PageHeader', () => {
  it('leading を渡すと title の隣に描画される', () => {
    render(<PageHeader title="録画の詳細" leading={<button>戻る</button>} />)
    expect(screen.getByRole('button', { name: '戻る' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '録画の詳細' })).toBeInTheDocument()
  })

  it('leading を渡さなければ何も描画しない', () => {
    render(<PageHeader title="録画の詳細" />)
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
  })
})

describe('ErrorState', () => {
  it('走査線を持たない（使用箇所は読み込み中と ON AIR に限定）', () => {
    render(<ErrorState>失敗しました</ErrorState>)
    const el = screen.getByText('失敗しました')
    expect(el.className.split(' ')).not.toContain('scanlines')
  })

  // WCAG 4.1.3（ステータスメッセージ）: 読み込み失敗を、フォーカスを移さずに
  // 支援技術へ伝える。role を "status" に落とすと落ちることを確認済み。
  it('role="alert" を持つ', () => {
    render(<ErrorState>失敗しました</ErrorState>)
    expect(screen.getByRole('alert')).toHaveTextContent('失敗しました')
  })

  // issue #467 のレビューコメント: 共通 ErrorState に再試行ボタンを足し、
  // 一覧の初回読み込み失敗をここへ寄せる。onRetry を消す・ボタンを
  // 常時出す（onRetry 未指定でも出す）のどちらの変異でも落ちることを確認済み。
  it('onRetry を渡すと再試行ボタンを出し、押すと呼ばれる', async () => {
    const onRetry = vi.fn()
    const user = userEvent.setup()
    render(<ErrorState onRetry={onRetry}>失敗しました</ErrorState>)

    await user.click(screen.getByRole('button', { name: '再試行' }))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })

  it('onRetry を渡さなければ再試行ボタンを出さない', () => {
    render(<ErrorState>失敗しました</ErrorState>)
    expect(screen.queryByRole('button', { name: '再試行' })).not.toBeInTheDocument()
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
