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
          toast({ message: '予約しました', actions: [{ label: '取消', onClick: () => {} }] })
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

  it('複数の action を並べ、押された action だけを実行してトーストを閉じる', () => {
    const selected: string[] = []
    function MultipleActionsHarness() {
      const toast = useToast()
      return (
        <button
          onClick={() =>
            toast({
              message: '予約しました',
              actions: [
                { label: '取消', onClick: () => selected.push('cancel') },
                { label: '設定', onClick: () => selected.push('settings') },
              ],
            })
          }
        >
          予約する
        </button>
      )
    }

    render(
      <ToastProvider>
        <MultipleActionsHarness />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByText('予約する'))
    expect(screen.getByRole('button', { name: '取消' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '設定' }))
    expect(selected).toEqual(['settings'])
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
   * デデュープを文言だけで判定すると、action を持つトーストの古いクロージャが
   * 再利用され、間違った対象を復元してしまう（実測: recordings.tsx の
   * 「ごみ箱に移しました」+「元に戻す」。文言は定数で、`onClick` だけが録画
   * ごとに違う。A → B と連続でごみ箱へ送ると B がデデュープされて出ず、
   * 残った「元に戻す」を押すと A が復元されて B が残ってしまっていた）。
   * デデュープを error 限定にし、action 付きは対象外にしたので、同じ文言でも
   * 両方出て、それぞれの Undo がそれぞれの対象に効く。
   */
  it('同じ文言で action が違う 2 件を続けて出すと、2 件目の Undo は 2 件目の対象に効く', () => {
    const undone: string[] = []
    function TrashHarness() {
      const toast = useToast()
      return (
        <>
          <button
            onClick={() =>
              toast({
                message: 'ごみ箱に移しました',
                actions: [{ label: '元に戻す', onClick: () => undone.push('A') }],
              })
            }
          >
            A をごみ箱へ
          </button>
          <button
            onClick={() =>
              toast({
                message: 'ごみ箱に移しました',
                actions: [{ label: '元に戻す', onClick: () => undone.push('B') }],
              })
            }
          >
            B をごみ箱へ
          </button>
        </>
      )
    }

    render(
      <ToastProvider>
        <TrashHarness />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByText('A をごみ箱へ'))
    fireEvent.click(screen.getByText('B をごみ箱へ'))

    // 文言が同じでも、action 付きはデデュープされず両方出る
    expect(screen.getAllByText('ごみ箱に移しました')).toHaveLength(2)

    const undoButtons = screen.getAllByRole('button', { name: '元に戻す' })
    expect(undoButtons).toHaveLength(2)
    fireEvent.click(undoButtons[1]) // 2 件目（B）の Undo を押す

    expect(undone).toEqual(['B'])
  })

  /**
   * 文言だけでデデュープすると、既存の分（この場合は error）の id にタイマーが
   * 付け替えられ、未読の失敗トーストが info の 6 秒タイマーで消えてしまう
   * （実測）。デデュープを error 同士に限定したので、info はそれ自身の
   * タイマーで消え、error は残る。
   */
  it('未読の失敗トーストは、同じ文言の info によって消えない', async () => {
    vi.useFakeTimers()
    function SameMessageHarness() {
      const toast = useToast()
      return (
        <>
          <button onClick={() => toast({ message: '同じ文言', kind: 'error' })}>
            error を出す
          </button>
          <button onClick={() => toast({ message: '同じ文言' })}>同じ文言の info を出す</button>
        </>
      )
    }

    render(
      <ToastProvider>
        <SameMessageHarness />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByText('error を出す'))
    fireEvent.click(screen.getByText('同じ文言の info を出す'))
    expect(screen.getAllByText('同じ文言')).toHaveLength(2)

    // info 自身の 6 秒タイマーで info だけが消え、error は残る
    await advance(6_001)
    expect(screen.getAllByText('同じ文言')).toHaveLength(1)

    fireEvent.click(screen.getByRole('button', { name: '閉じる' }))
    expect(screen.queryByText('同じ文言')).not.toBeInTheDocument()
  })

  it('同一文言の error は積み増さない（デデュープが効く唯一のケース）', () => {
    renderHarness()
    fireEvent.click(screen.getByText('error を出す'))
    fireEvent.click(screen.getByText('error を出す'))
    expect(screen.getAllByText('失敗しました')).toHaveLength(1)
  })

  /**
   * デデュープは message + kind === 'error' だけで判定すると、action 付きの
   * error も対象に入ってしまう（「action は個体ごとにクロージャが違うので
   * デデュープしない」というコメントが嘘になる --- action が違う error に
   * 「再試行」を足した瞬間、同じ「古いクロージャが再利用される」バグが戻る）。
   * action 付きは error でも常にデデュープ対象外にする。
   */
  it('同じ文言の error でも action が違えば 2 件出て、2 件目の action は 2 件目の対象に効く', () => {
    const retried: string[] = []
    function RetryHarness() {
      const toast = useToast()
      return (
        <>
          <button
            onClick={() =>
              toast({
                message: '失敗しました',
                kind: 'error',
                actions: [{ label: '再試行', onClick: () => retried.push('A') }],
              })
            }
          >
            A を失敗させる
          </button>
          <button
            onClick={() =>
              toast({
                message: '失敗しました',
                kind: 'error',
                actions: [{ label: '再試行', onClick: () => retried.push('B') }],
              })
            }
          >
            B を失敗させる
          </button>
        </>
      )
    }

    render(
      <ToastProvider>
        <RetryHarness />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByText('A を失敗させる'))
    fireEvent.click(screen.getByText('B を失敗させる'))

    expect(screen.getAllByText('失敗しました')).toHaveLength(2)

    const retryButtons = screen.getAllByRole('button', { name: '再試行' })
    expect(retryButtons).toHaveLength(2)
    fireEvent.click(retryButtons[1]) // 2 件目（B）の再試行を押す

    expect(retried).toEqual(['B'])
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
