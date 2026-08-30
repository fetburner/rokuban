import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { ToastProvider, useToast } from '@/components/toaster'

/**
 * トーストの閉じる・一時停止・折り返し（issue #455 U-3）。
 *
 * **タイマーは fake にする。** `show` はタイマーを ref に持ち（toasts 配列を
 * 依存にした useEffect では再スケジュールしない設計 --- toaster.tsx の
 * コメント参照）、`Date.now()` は `pause` の残り時間計算にだけ使うので、
 * fake timers と組み合わせても `storage-balance.test.tsx` が踏んだ
 * `findByText` 由来の詰まりは起きない（`waitFor`/`findByText` を使わず、
 * `act` で timer を進めたあと同期にアサートする）。
 */

/** Harness はボタン経由で toast を出す。実際の呼び出し側（onError 等）を模す。 */
function Harness() {
  const toast = useToast()
  return (
    <>
      <button onClick={() => toast({ message: '保存しました' })}>info を出す</button>
      <button onClick={() => toast({ message: '失敗しました', kind: 'error' })}>
        error を出す
      </button>
      <button
        onClick={() =>
          toast({ message: '予約しました', action: { label: '取消', onClick: () => {} } })
        }
      >
        action 付きを出す
      </button>
    </>
  )
}

function renderHarness() {
  return render(
    <ToastProvider>
      <Harness />
    </ToastProvider>,
  )
}

/** liveRegion は toast を包む aria-live コンテナ（hover / focus の対象）。 */
function liveRegion(container: HTMLElement): HTMLElement {
  const el = container.querySelector('[aria-live="polite"]')
  if (!el) throw new Error('aria-live region not found')
  return el as HTMLElement
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

afterEach(() => {
  vi.useRealTimers()
})

describe('ToastProvider', () => {
  it('閉じるボタンで消える', () => {
    renderHarness()
    fireEvent.click(screen.getByText('info を出す'))
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '閉じる' }))
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })

  it('info は 6 秒で自動的に消える（hover なし）', async () => {
    vi.useFakeTimers()
    renderHarness()
    fireEvent.click(screen.getByText('info を出す'))
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    await advance(5_999)
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    await advance(1)
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })

  it('hover 中は消えず、離れると残り時間で消える（両方向）', async () => {
    vi.useFakeTimers()
    const { container } = renderHarness()
    fireEvent.click(screen.getByText('info を出す'))
    const region = liveRegion(container)

    await advance(3_000)
    fireEvent.mouseEnter(region)

    // hover 中は 6 秒を大きく超えても消えない
    await advance(10_000)
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    // 離れると残り（6000 - 3000 = 3000ms 分）で消える
    fireEvent.mouseLeave(region)
    await advance(2_999)
    expect(screen.getByText('保存しました')).toBeInTheDocument()
    await advance(1)
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })

  it('focus 中は消えず、外れると残り時間で消える', async () => {
    vi.useFakeTimers()
    renderHarness()
    fireEvent.click(screen.getByText('info を出す'))
    const closeButton = screen.getByRole('button', { name: '閉じる' })

    await advance(3_000)
    fireEvent.focus(closeButton)

    await advance(10_000)
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    fireEvent.blur(closeButton)
    await advance(2_999)
    expect(screen.getByText('保存しました')).toBeInTheDocument()
    await advance(1)
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })

  it('action 付きは 10 秒で消える', async () => {
    vi.useFakeTimers()
    renderHarness()
    fireEvent.click(screen.getByText('action 付きを出す'))
    expect(screen.getByText('予約しました')).toBeInTheDocument()

    await advance(9_999)
    expect(screen.getByText('予約しました')).toBeInTheDocument()
    await advance(1)
    expect(screen.queryByText('予約しました')).not.toBeInTheDocument()
  })

  it('kind: error は自動で消えない。閉じるボタンでのみ消える', async () => {
    vi.useFakeTimers()
    renderHarness()
    fireEvent.click(screen.getByText('error を出す'))
    expect(screen.getByText('失敗しました')).toBeInTheDocument()

    await advance(60_000)
    expect(screen.getByText('失敗しました')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '閉じる' }))
    expect(screen.queryByText('失敗しました')).not.toBeInTheDocument()
  })

  it('truncate ではなく複数行に折り返す（line-clamp）', () => {
    renderHarness()
    fireEvent.click(screen.getByText('info を出す'))
    const message = screen.getByText('保存しました')
    const classes = message.className.split(' ')
    expect(classes).not.toContain('truncate')
    expect(classes).toContain('line-clamp-3')
  })
})
