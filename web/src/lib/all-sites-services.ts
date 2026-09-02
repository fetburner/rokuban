import { useQueries } from '@tanstack/react-query'

import { getListServicesQueryOptions, useListSites, type ProgramListItem, type Service } from '@/api/generated'
import { unwrap } from '@/api/unwrap'

/** SiteService はサービス射影に、それを取得した site を付与した値。 */
export type SiteService = Service & { site: string }

/** SiteProgram は番組射影に、それを取得した site を付与した値。 */
export type SiteProgram = ProgramListItem & { site: string }

/** siteServiceKey は site 内の network/service と site を合わせた identity を返す。 */
export function siteServiceKey(site: string, networkId: number, serviceId: number): string {
  return `${site}:${networkId}:${serviceId}`
}

/** programIdentity は site を含む番組の identity を返す。 */
export function programIdentity(site: string, programId: number): string {
  return `${site}:${programId}`
}

export type AllSitesServices = {
  /** services は `Service.id` で畳んだ全 site のサービス一覧。順不同。 */
  services: Service[]
  /** siteServices は site ごとのサービス一覧。列・ライブの出所を保持する。 */
  siteServices: SiteService[]
  /** sites は `GET /api/sites` のレジストリ（順序はサーバー応答のまま）。 */
  sites: string[]
  /** isPending は取得中と失敗の区別に使う（空を「サービスが無い」と読ませない）。 */
  isPending: boolean
  isError: boolean
}

/**
 * useAllSitesServices は「全 site から引いて `Service.id` で畳んだ」サービス一覧。
 *
 * サービスの識別子は `Service.id`（`networkId * 100000 + serviceId`。放送から
 * 合成される値で、mirakc インスタンスが採番したものではない）であって、site は
 * 識別子の一部ではなく「その site がそのチャンネルを受信しているか」という
 * 存在のスコープでしかない。「条件として何を名指しできるか」（= 識別子の問い）
 * に答える選択肢は、1 つの site の観測ではなく識別子の集合で答える必要がある
 * （issue #290。`docs/frontend/shell.md`「サイトの扱い」の「レジストリが運ぶ」）。
 *
 * `recording-filters.tsx`（録画検索のチャンネル選択肢）と
 * `condition-fields.tsx`（検索・ルール条件フォームのサービス選択肢）の両方が
 * 同じ fetch + dedupe を必要とするため、ここに 1 本化する（同じ dedupe を
 * 2 箇所に書かない）。ラベル付け（`serviceDisambiguator`）は呼び出し側ごとに
 * 使い方が違う（`recording-filters.tsx` は `Map<id, label>` を組む、
 * `condition-fields.tsx` はチップごとに直接呼ぶ）ので、ここには含めない。
 *
 * **`isPending` は `sitesQuery` を含む全クエリの OR。** site ごとに結果が
 * 届くたびに一覧が伸びる部分描画を避けるため（`recording-filters.tsx` と同じ
 * `some(isPending)` の単一 swap）。
 *
 * **`isError` も同じ OR で畳む。** ただし壊れ方は小さい: `GET /api/sites` が
 * 返す site 名はそのままレジストリなので、ここから作る `getListServicesQueryOptions`
 * の呼び先が未知の site になることはない（`internal/api/epg.go` の
 * `h.knownSite(req.Site)` も同じレジストリで検証しており、一覧に無い site を
 * 404 にする経路自体が無い）。EPG をまだ同期していない site は空応答
 * `200 []` を返すだけでエラーにはならないので、**mirakc が実際に落ちている
 * site が 1 つあっても、それだけでは `isError` は立たない**（その site の
 * 取得が実際に失敗した場合だけ立ち、その場合は選択肢全体を「取得に失敗」に
 * 倒す --- 部分的に欠けた選択肢を黙って見せない）。
 *
 * **単一サイト構成では挙動が変わらない。** site が 1 件なら
 * `useQueries` も要素 1 個になり、以前の `useListServices(site)` 相当の結果に
 * 一致する。
 */
export function useAllSitesServices(): AllSitesServices {
  const sitesQuery = useListSites()
  const sites = unwrap(sitesQuery.data) ?? []
  const serviceQueries = useQueries({
    queries: sites.map((site) => getListServicesQueryOptions(site)),
  })

  const serviceById = new Map<number, Service>()
  const siteServices: SiteService[] = []
  for (const [index, query] of serviceQueries.entries()) {
    const site = sites[index]
    if (site === undefined) continue
    for (const service of unwrap(query.data) ?? []) {
      siteServices.push({ ...service, site })
      if (!serviceById.has(service.id)) serviceById.set(service.id, service)
    }
  }

  return {
    services: [...serviceById.values()],
    siteServices,
    sites,
    isPending: sitesQuery.isPending || serviceQueries.some((q) => q.isPending),
    isError: sitesQuery.isError || serviceQueries.some((q) => q.isError),
  }
}
