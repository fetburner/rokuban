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

// jsdom は window.scrollTo を実装していない。呼び手は 2 つある。
//   - TanStack Virtual の useWindowVirtualizer（components/program-list.tsx）が
//     初期位置合わせのために呼ぶ
//   - TanStack Router のスクロール復元がナビゲーションのたびに呼ぶ（ルーターを
//     使うテスト全般。routes.test.tsx が個別に置いていたスタブをここに集約し、
//     test/router.tsx の renderInRouter を使うテストにも効かせる）
// どちらも無いとテストのたびに "Not implemented" が標準エラーに出るだけで
// テスト自体は落ちないが、無関係な例外でログが埋まる。可視判定の正しさ自体は
// lib/list-virtualization.ts 側で担保する。
// jsdom がすでに「投げるだけ」の実装を持っているため `??=` では上書きできない。
window.scrollTo = (() => {}) as typeof window.scrollTo

// jsdom は HTMLMediaElement.prototype.load を実装していない。LivePlayer
// （components/live-player.tsx）が破棄時に呼ぶ（次のチャンネルへセグメント要求が
// 残らないようにするため）。window.scrollTo と同じ理由でスタブする。
HTMLMediaElement.prototype.load = function (this: HTMLMediaElement) {}

// vitest config で test.globals を有効にしていないため、
// @testing-library/react の自動クリーンアップ検出（グローバル afterEach の有無）
// が働かない。前のテストの DOM がそのまま残ると screen クエリが複数要素に
// マッチして誤検知するので、明示的に各テスト後に unmount する。
afterEach(() => {
  cleanup()
})
