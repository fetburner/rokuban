import type { Reservation } from '@/api/generated'

/**
 * stateLabels は reservations.state の表示名（docs/schema.md §3）。
 *
 * 一覧（`pages/reservations.tsx`）と詳細（`pages/reservation-detail.tsx`）の
 * 両方が使う --- 同じ状態が画面によって違う表記（生の enum 値など）で出ると
 * 利用者が混乱するので、ここに定義を集約する（issue #300）。
 *
 * `pages/*.tsx` に置かず独立したファイルにするのは、ページコンポーネントの
 * ファイルが値と（React Fast Refresh が要求する）コンポーネントのみの export
 * を混在させないため（`lib/recording-search.ts` の `statusLabels` /
 * `sourceLabels` と同じ手）。
 */
export const stateLabels: Record<Reservation['state'], string> = {
  active: '有効',
  detached: 'ルール外',
  orphaned: 'EPG から消失',
}
