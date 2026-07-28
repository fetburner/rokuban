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

// vitest config で test.globals を有効にしていないため、
// @testing-library/react の自動クリーンアップ検出（グローバル afterEach の有無）
// が働かない。前のテストの DOM がそのまま残ると screen クエリが複数要素に
// マッチして誤検知するので、明示的に各テスト後に unmount する。
afterEach(() => {
  cleanup()
})
