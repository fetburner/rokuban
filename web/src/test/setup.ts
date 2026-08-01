import { cleanup } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'

// jsdom は ResizeObserver を実装していない。PageHeader
// （components/page.tsx）と CircuitBreakerBanner が使うため、
// テスト環境向けに no-op のスタブを用意する。
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

globalThis.ResizeObserver ??= ResizeObserverStub

// jsdom は window.scrollTo を実装していない。TanStack Virtual の
// useWindowVirtualizer（components/program-list.tsx）が初期位置合わせのために
// 呼ぶため、no-op スタブを用意する（無いとテストのたびに "Not implemented"
// エラーが標準エラーに出るだけで、テスト自体は落ちない。ログを汚さないための
// スタブで、可視判定の正しさ自体は lib/list-virtualization.ts 側で担保する）。
window.scrollTo = (() => {}) as typeof window.scrollTo

// vitest config で test.globals を有効にしていないため、
// @testing-library/react の自動クリーンアップ検出（グローバル afterEach の有無）
// が働かない。前のテストの DOM がそのまま残ると screen クエリが複数要素に
// マッチして誤検知するので、明示的に各テスト後に unmount する。
afterEach(() => {
  cleanup()
})
