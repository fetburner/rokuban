/**
 * オプション機能の有効/無効（`GET /api/capabilities`）をフロント側で読むための
 * 唯一の入口（issue #209）。
 *
 * **導線を出すかどうかの判断はここに集約する。** ライブへの導線は主ナビだけでは
 * なく、番組行の「ライブで見る」等にも増える（issue #229）。判断が導線ごとに
 * ばらけると、片方だけ直った状態（ナビからは消えたが番組行からは行ける）に
 * なりやすい。導線側は `useLiveEnabled()` を呼ぶだけでよい。
 *
 * **ただし「導線を出すか」と「原因を名指しするか」は別の問いなので、真偽値 1 つに
 * 潰さない。** 導線は未確定のとき出さない側に倒して構わない（代償は初回描画で
 * 1 つ遅れて現れることだけ）が、その同じ `false` を使って画面に
 * 「サーバーの設定で無効です」と書くと、**取得できていないだけの状態で誤った原因を
 * 名指しする**ことになる --- `live.enabled: true` のデプロイで能力 API が瞬断した
 * だけでも「設定が無効」と表示され、issue #209 が消したかった「運用者が原因に
 * たどり着けない」を別の顔で再演する（レビューでの指摘）。原因を出す側
 * （`/live` 画面）は `useLiveCapability()` の 4 値を見る。
 */

import { useGetCapabilities } from '@/api/generated'
import { unwrap } from '@/api/unwrap'

/**
 * LiveCapability はライブ視聴の可否を、**サーバーに聞けたかどうかまで含めて**表す。
 *
 * - `pending`: まだ聞けていない（読み込み中）
 * - `unknown`: 聞けなかった（能力 API が失敗）。**有効か無効かは分からない**
 * - `enabled` / `disabled`: サーバーの `live.enabled` をそのまま受け取った
 */
export type LiveCapability = 'pending' | 'unknown' | 'enabled' | 'disabled'

/**
 * useLiveCapability はライブ視聴の可否を 4 値で返す。
 *
 * **これは「今すぐ見られる」ではなく「config で有効になっている」。**
 * streamer が動いていない / チューナーが埋まっている場合は `enabled` のまま、
 * プレイリスト取得の 404 / 503 として `LivePlayer` 側に出る
 * （docs/frontend/live.md）。
 */
export function useLiveCapability(): LiveCapability {
  const query = useGetCapabilities()
  const capabilities = unwrap(query.data)
  if (capabilities !== undefined) return capabilities.live ? 'enabled' : 'disabled'
  return query.isPending ? 'pending' : 'unknown'
}

/**
 * useLiveEnabled はライブへの導線を出してよいかを返す。
 *
 * **`pending` / `unknown` は出さない側に倒す（fail-closed）。** 未確定を「有効」に
 * 倒すと、無効なデプロイでは導線が一瞬出てから消える --- 押せてしまえば行き先は
 * 「無効です」の画面なので、issue #209 が消したかった「機能しない導線」がその
 * 瞬間だけ復活する。逆に倒した場合の代償は、有効なデプロイで初回描画のあいだ
 * ナビ項目が 1 つ遅れて現れることだけで、`staleTime`（30 秒。`main.tsx`）に
 * 乗るのでページ遷移のたびには起きない。
 *
 * **画面の文言（原因の名指し）にこの真偽値を使ってはならない**
 * （このモジュール冒頭のコメント）。使うのは `useLiveCapability()`。
 */
export function useLiveEnabled(): boolean {
  return useLiveCapability() === 'enabled'
}
