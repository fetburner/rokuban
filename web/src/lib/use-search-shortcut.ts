import { useEffect } from 'react'

/**
 * useSearchShortcut は `/` で指定した検索欄へフォーカスする。
 *
 * 入力欄・選択欄・編集可能領域・ダイアログ／メニューの中と IME 変換中はキーを横取りしない。
 */
export function useSearchShortcut(selector: string) {
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (
        event.key !== '/' ||
        event.isComposing ||
        event.defaultPrevented ||
        event.altKey ||
        event.ctrlKey ||
        event.metaKey
      ) return

      const target = event.target
      if (
        target instanceof HTMLElement &&
        (
          target instanceof HTMLInputElement ||
          target instanceof HTMLTextAreaElement ||
          target instanceof HTMLSelectElement ||
          target.isContentEditable
        )
      ) return

      // モーダル・開いたメニューの中では、背面の検索欄へフォーカスを奪わない。
      if (target instanceof Element && target.closest('[role="dialog"], [role="alertdialog"], [role="menu"], [aria-modal="true"]') !== null) return

      const input = document.querySelector<HTMLInputElement>(selector)
      if (input === null || input.disabled || input.closest('[hidden]') !== null) return

      event.preventDefault()
      input.focus()
    }

    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [selector])
}
