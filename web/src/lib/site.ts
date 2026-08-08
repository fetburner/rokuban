import { createContext, useContext } from 'react'

/**
 * SiteContext は UI が対象とする現在の site 名を配る。
 *
 * 値の解決（`GET /api/sites` の取得と「どのサイトを使うか」の決定）は
 * `<SiteGate>`（components/site-gate.tsx）が担い、この Context 自体は値を
 * 右から左に流すだけの同期的な Provider/Consumer のペアに留める ---
 * テストが `/api/sites` の fetch を経由せずに
 * `<SiteContext value="default">` で直接値を注入できるようにするため
 * （CLAUDE.md テスト規律「非同期の空虚な成功に注意する」。SiteGate 越しだと
 * この Context を消費する全コンポーネントのテストが site 一覧のフェッチ完了を
 * 待つ必要が出てしまう）。
 */
export const SiteContext = createContext<string | undefined>(undefined)

/**
 * useCurrentSite は UI が対象とする現在の site 名を返す。`<SiteGate>` の外で
 * 呼ぶとエラーになる（値が未解決のまま使われるのを防ぐ）。
 */
export function useCurrentSite(): string {
  const site = useContext(SiteContext)
  if (site === undefined) {
    throw new Error('useCurrentSite() must be called within <SiteGate>')
  }
  return site
}
