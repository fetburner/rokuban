import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { useSearchShortcut } from './use-search-shortcut'

function Harness({ wrapper }: { wrapper?: { role?: string; 'aria-modal'?: 'true' } }) {
  useSearchShortcut('input[aria-label="検索"]')
  return (
    <>
      <input aria-label="検索" />
      <div {...wrapper}>
        <button type="button">中のボタン</button>
      </div>
      <button type="button">外のボタン</button>
    </>
  )
}

/** press はボタンにフォーカスして / を送り、keydown が preventDefault されたかを返す。 */
function press(name: string) {
  const button = screen.getByRole('button', { name })
  button.focus()
  return fireEvent.keyDown(button, { key: '/' }) === false
}

describe('useSearchShortcut', () => {
  it('通常の画面では / で検索欄へ移る', () => {
    render(<Harness />)
    expect(press('外のボタン')).toBe(true)
    expect(document.activeElement).toBe(screen.getByLabelText('検索'))
  })

  it.each<{ role?: string; 'aria-modal'?: 'true' }>([
    { role: 'dialog' },
    { role: 'alertdialog' },
    { role: 'menu' },
    { 'aria-modal': 'true' },
  ])('%o の内側では / を奪わない', (wrapper) => {
    render(<Harness wrapper={wrapper} />)
    expect(press('中のボタン')).toBe(false)
    expect(document.activeElement).toBe(screen.getByRole('button', { name: '中のボタン' }))
  })
})
