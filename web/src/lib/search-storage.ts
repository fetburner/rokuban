/**
 * 最後に実行した検索条件を localStorage に保存する。
 *
 * 置き場所の判断は [docs/frontend/design.md](../../../docs/frontend/design.md)
 * §個人化 —— 端末ごとの好みなのでサーバーには持たない。**URL の条件が優先**で、
 * ここは URL が条件を持たないときの初期値にしか使わない。復元するのは
 * フォームだけで、検索そのものは実行しない（開いた瞬間に前回の問い合わせが
 * 飛ぶのは、押していない操作が起きたのと同じ）。
 */

import type { ProgramSearchRequest } from '@/api/generated'
import { SearchProgramsBody } from '@/api/zod'
import { validValue } from '@/lib/url-search'

const KEY = 'rokuban:search:last'

/**
 * loadLastSearchConditions は保存済みの条件を返す。無い・壊れているなら undefined。
 *
 * **読むたびに検証する。** 保存したあとに openapi の条件が変わることもあれば、
 * 手で書き換えられることもある。検証は URL と同じ openapi 由来のスキーマ
 * （`api/zod.ts`）に委ね、制約をここに書き写さない（`lib/url-search.ts`）。
 */
export function loadLastSearchConditions(): ProgramSearchRequest | undefined {
  try {
    const raw = localStorage.getItem(KEY)
    if (raw === null) return undefined
    return validValue<ProgramSearchRequest>(SearchProgramsBody, JSON.parse(raw))
  } catch {
    // private mode で localStorage が使えない / JSON として壊れている
    return undefined
  }
}

/** saveLastSearchConditions は条件を保存する。条件なし（空）はキーごと消す。 */
export function saveLastSearchConditions(request: ProgramSearchRequest): void {
  try {
    if (Object.keys(request).length === 0) localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, JSON.stringify(request))
  } catch {
    // ignore
  }
}
