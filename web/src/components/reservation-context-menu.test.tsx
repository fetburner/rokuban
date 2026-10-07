import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Reservation } from '@/api/generated'
import { ReservationContextMenu } from '@/components/reservation-context-menu'
import { renderInRouter } from '@/test/router'

const originalMatchMedia = window.matchMedia

afterEach(() => {
  if (originalMatchMedia) {
    Object.defineProperty(window, 'matchMedia', { configurable: true, value: originalMatchMedia })
  } else {
    Reflect.deleteProperty(window, 'matchMedia')
  }
})

// 最小の Reservation。メニューが読むのは site / programId だけ。
const reservation = { site: 'default', programId: 7 } as Reservation

function renderMenu(cancelPending: boolean, onCancel = vi.fn()) {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: vi.fn((query: string) => ({
      matches: query === '(pointer: fine)',
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  })
  renderInRouter(
    <ReservationContextMenu
      reservation={reservation}
      rowRef={{ current: null }}
      onCancel={onCancel}
      cancelPending={cancelPending}
    >
      <div data-testid="row">行</div>
    </ReservationContextMenu>,
    { path: '/reservations' },
  )
  return onCancel
}

describe('ReservationContextMenu', () => {
  it('処理中は「予約を取消」が無効で、押しても取消を呼ばない', async () => {
    const onCancel = renderMenu(true)
    fireEvent.contextMenu(await screen.findByTestId('row'), { clientX: 10, clientY: 10 })
    const item = await screen.findByRole('menuitem', { name: '予約を取消' })
    expect(item).toHaveAttribute('aria-disabled', 'true')
    await userEvent.setup().click(item)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('処理中でなければ「予約を取消」で取消を呼ぶ', async () => {
    const onCancel = renderMenu(false)
    fireEvent.contextMenu(await screen.findByTestId('row'), { clientX: 10, clientY: 10 })
    const item = await screen.findByRole('menuitem', { name: '予約を取消' })
    expect(item).not.toHaveAttribute('aria-disabled', 'true')
    await userEvent.setup().click(item)
    expect(onCancel).toHaveBeenCalledTimes(1)
  })
})
