import { useQueries } from '@tanstack/react-query'
import { TriangleAlert } from 'lucide-react'

import { getListTunersQueryOptions, useListSites, type Tuner } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { isObservationStale } from '@/lib/storage-forecast'

/**
 * tunerStatusStaleAfterMs は `observedAt`（サイト内の最古の観測）をこれより古いと
 * 「観測が止まっています」と表示するしきい値。
 *
 * worker のチューナー射影ループの間隔は既定 10 分
 * （`internal/worker/worker.go` の `defaultTunerSyncInterval`）。ここではその 3 倍
 * （30 分）を安全マージンにする --- `lib/storage-forecast.ts` の
 * `observationStaleAfterMs`（ストレージ観測 5 分間隔の 12 倍）と同じ考え方で、
 * 1 回の失敗パス・再起動直後の遅延程度では誤って「古い」と出ない一方、
 * 観測ループが本当に止まっていれば早めに検知できる値を選んだ。
 */
export const tunerStatusStaleAfterMs = 3 * 10 * 60 * 1000

/**
 * TunerStatus はライブ画面のチャンネル一覧の脇に「チューナー n 本（故障 m）」の
 * 1 行を出す（issue #474 の判定 (b)）。
 *
 * 「いまどの局を掴んでいるか」「ライブ視聴が何本か」は `tuner_sync` に無いため
 * 出さない（別の判断。issue のコメント参照）。故障を知る手段が今は無いので、
 * 「警告が無い = 大丈夫」という誤読を避けるためにここへ出す。
 *
 * `GET /api/sites/{site}/tuners` は site スコープ（`GET /api/capacity/overages` の
 * ような全サイト版は無い）なので、サイトごとに `useQueries` で問い合わせる
 * （`pages/search.tsx` の値札と同じ形）。
 *
 * **射影が 0 行のサイトは何も主張しない**
 * （docs/data/capacity.md §6.5「射影が 1 行も無いサイトは何も主張しない」と一貫
 * させる）。取得が未解決/失敗のサイトも同じ扱い（`unwrap` が `undefined` を返す）。
 */
export function TunerStatus() {
  const sitesQuery = useListSites()
  const sites = unwrap(sitesQuery.data) ?? []
  const showSite = sites.length > 1

  const tunerQueries = useQueries({
    queries: sites.map((site) => getListTunersQueryOptions(site)),
  })

  const nowMs = Date.now()
  const lines = sites
    .map((site, i) => {
      const tuners = unwrap(tunerQueries[i]?.data)
      if (tuners === undefined || tuners.length === 0) return undefined
      return <TunerStatusLine key={site} site={site} showSite={showSite} tuners={tuners} nowMs={nowMs} />
    })
    .filter((line) => line !== undefined)

  if (lines.length === 0) return null

  return <div className="flex flex-col gap-1">{lines}</div>
}

function TunerStatusLine({
  site,
  showSite,
  tuners,
  nowMs,
}: {
  site: string
  showSite: boolean
  tuners: Tuner[]
  nowMs: number
}) {
  const faultCount = tuners.filter((t) => t.isFault).length
  const oldestObservedAt = tuners.reduce(
    (oldest, t) => (t.observedAt < oldest ? t.observedAt : oldest),
    tuners[0]!.observedAt,
  )
  const stale = isObservationStale(oldestObservedAt, nowMs, tunerStatusStaleAfterMs)

  return (
    <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
      {showSite && <span className="font-medium text-foreground">{site}</span>}
      <span>
        チューナー{tuners.length}本
        {faultCount > 0 && (
          <span className="ml-1 rounded bg-destructive/10 px-1.5 py-0.5 text-xs font-medium text-destructive">
            （故障{faultCount}）
          </span>
        )}
      </span>
      {stale && (
        <span className="flex items-center gap-1 text-warning">
          <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
          観測が止まっています
        </span>
      )}
    </p>
  )
}
