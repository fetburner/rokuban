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
      <button onClick={() => toast({ message: '2 通目' })}>2 通目の info を出す</button>
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

  // jsdom はレイアウトを計算しないので、実際に複数行へ折り返るかどうかは
  // ここでは検証できない（測れないものを断言しない）。ここで固定するのは
  // クラス名だけ --- `truncate`（1 行省略）に戻す変異でも、`line-clamp-3` を
  // 消す変異でも落ちる。実際の折り返りは 409 の文言を `pnpm preview` で
  // 目視して確認済み（報告参照）。
  it('折り返り用のクラス名（truncate ではなく line-clamp-3）を固定する', () => {
    renderHarness()
    fireEvent.click(screen.getByText('info を出す'))
    const message = screen.getByText('保存しました')
    const classes = message.className.split(' ')
    expect(classes).not.toContain('truncate')
    expect(classes).toContain('line-clamp-3')
  })

  /**
   * 件数上限（slice(-3)）で古い方から落とす実装は、寿命の古さで判断するため
   * 未読の失敗トーストを後続の成功トーストが押し出してしまっていた
   * （kind: 'error' を足した動機そのものを打ち消す）。上限をやめてデデュープに
   * 変えたので、何件積んでも失敗トーストは残る。
   */
  it('未読の失敗トーストは、後続の成功トーストに押し出されない', () => {
    renderHarness()
    fireEvent.click(screen.getByText('error を出す'))
    fireEvent.click(screen.getByText('info を出す'))
    fireEvent.click(screen.getByText('2 通目の info を出す'))
    fireEvent.click(screen.getByText('action 付きを出す'))

    expect(screen.getByText('失敗しました')).toBeInTheDocument()
    expect(screen.getByText('保存しました')).toBeInTheDocument()
    expect(screen.getByText('2 通目')).toBeInTheDocument()
    expect(screen.getByText('予約しました')).toBeInTheDocument()
  })

  /**
   * デデュープ: 同じ文言で積み増さない代わりに、既存の分のタイマーを延長する。
   * 古いタイマーを消さずに延長すると、古いタイマーが先に発火して早く消えて
   * しまう（6500ms 時点のアサーションがそれを検出する）。
   */
  it('同じ文言の info は積み増さず、タイマーを延長する（デデュープ）', async () => {
    vi.useFakeTimers()
    renderHarness()
    fireEvent.click(screen.getByText('info を出す')) // t=0
    expect(screen.getAllByText('保存しました')).toHaveLength(1)

    await advance(5_000) // t=5000
    fireEvent.click(screen.getByText('info を出す')) // 同じ文言 → 積み増さず延長
    expect(screen.getAllByText('保存しました')).toHaveLength(1)

    // 最初の呼び出しからは 6000ms を超えたが、延長したのでまだ消えない
    await advance(1_500) // t=6500
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    // 2 回目の呼び出しから 6000ms（t=11000）で消える
    await advance(4_500) // t=11000
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })

  /**
   * blocker 1 の再現: ポインタが既にトースト領域上にある状態で届いた info は
   * 一時停止されず 6 秒で消えてはいけない。`show` が無条件に `schedule` する
   * 実装（修正前）だと、hover 中に届いた 2 通目だけが 6 秒後に消えて落ちる。
   */
  it('hover 中に届いた info は最初から一時停止されている', async () => {
    vi.useFakeTimers()
    const { container } = renderHarness()
    const region = liveRegion(container)
    fireEvent.mouseEnter(region)

    fireEvent.click(screen.getByText('info を出す'))
    await advance(6_001)
    // hover 中に届いたので、6 秒経っても消えない
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    fireEvent.mouseLeave(region)
    await advance(5_999)
    expect(screen.getByText('保存しました')).toBeInTheDocument()
    await advance(1)
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })

  /**
   * blocker 2 の再現: マウスがトースト上にあるまま内側の要素で focus が
   * 外れても（`onBlur` は `focusout` でバブルする）、hover が続いている限り
   * 一時停止のままでなければならない。hover / focus を単純に同じ
   * pause/resume につないだ実装（修正前）だと、ここで再開してしまい
   * 6 秒で消える。
   */
  it('hover 中に内側で focus が外れてもタイマーは再開しない', async () => {
    vi.useFakeTimers()
    const { container } = renderHarness()
    const region = liveRegion(container)

    fireEvent.click(screen.getByText('info を出す'))
    const closeButton = screen.getByRole('button', { name: '閉じる' })

    fireEvent.mouseEnter(region)
    fireEvent.focus(closeButton)
    fireEvent.blur(closeButton) // マウスは region 上のまま、focus だけ外れる

    await advance(6_001)
    // hover が続いているので消えない
    expect(screen.getByText('保存しました')).toBeInTheDocument()

    fireEvent.mouseLeave(region)
    await advance(6_000)
    expect(screen.queryByText('保存しました')).not.toBeInTheDocument()
  })
})
