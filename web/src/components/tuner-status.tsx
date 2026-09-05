import { TriangleAlert } from 'lucide-react'

import { useQueries } from '@tanstack/react-query'

import { getListTunersQueryOptions, useListSites, type Tuner } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { tunersQueryKeyPrefix } from '@/lib/events'
import { isObservationStale } from '@/lib/storage-forecast'

/**
 * TunerStatus はライブ画面のチャンネル一覧の脇に「チューナー n 本（故障 m）」の
 * site ごとの行を出す（issue #474 の判定 (b)）。
 *
 * **site ごとに集計する。** ライブ画面のチャンネル一覧が全サイトの和集合に
 * なったため、状態表示も選択可能な全サイトの tuner_sync を対象にする。各 site の
 * 射影行は別々の状態表示にし、未取得/空のサイトは何も主張しない。複数 site の
 * ときだけ site 名を表示し、単一 site では従来の見た目を維持する。
 *
 * 「いまどの局を掴んでいるか」「ライブ視聴が何本か」は `tuner_sync` に無いため
 * 出さない（別の判断）。
 *
 * **n は射影の全行数ではなく `isAvailable && !isFault` の本数にする**
 * （`internal/capacity` の `countable` と揃える。docs/data/capacity.md §6.5）。
 * 故障の本数（m）は n に含めない別枠の警告として添える --- n は「いま使える
 * 本数」、m は「n に入っていない故障」という独立した警告。
 *
 * **現行の mirakc に対してこの絞り込みは恒真**（`isAvailable` は常に true、
 * `isFault` は常に false。設定で無効化したチューナーはそもそも一覧に現れない）。
 * Mirakurun 互換 API の契約に揃えて書いてあり、mirakc が実装したら効く。
 * 詳細と根拠は docs/data/capacity.md §6.5。
 *
 * **射影が 0 行のサイトは何も主張しない**
 * （docs/data/capacity.md §6.5「射影が 1 行も無いサイトは何も主張しない」と一貫
 * させる）。取得が未解決/失敗のときも同じ（`unwrap` が `undefined` を返す）。
 *
 * **鮮度は `observedAt`（射影内で最も古いもの）が `isObservationStale` の既定
 * しきい値（`observationStaleAfterMs`。1 時間）より古いかで見る。** 専用の
 * しきい値は作らない --- `tuner_sync` は worker の定期全量同期でしか値が
 * 変わらない使い捨てプロジェクションで、ストレージ観測（`GET /api/storage`）と
 * 性質が同じであり、クライアント側の取り直しも同じ周期（`lib/events.ts` の
 * `tuners` グループが `storageRefreshIntervalMs` を流用する。クエリキーは
 * そのグループに入るよう手書きにしてある --- 下記）に揃えてある。
 * 同じ性質のものに別の数字を発明する理由が無いので、既存のしきい値をそのまま
 * 再利用する。
 */
export function TunerStatus() {
  const sitesQuery = useListSites()
  const sites = unwrap(sitesQuery.data) ?? []
  const tunerQueries = useQueries({
    queries: sites.map((site) =>
      getListTunersQueryOptions(site, { query: { queryKey: [tunersQueryKeyPrefix, site] } }),
    ),
  })
  // 生成キー（`/api/sites/${site}/tuners`）ではなく手書きにする --- URL のままだと
  // epg グループの接頭辞（`/api/sites/`）にも一致し、周期の違う 2 グループに同じ
  // キーが入る（理由は {@link tunersQueryKeyPrefix}）。接頭辞は `lib/events.ts` の
  // グループ定義と同じ定数を参照する --- 片方だけ改名して取り直しが止まる drift を
  // 防ぐため。手書きキーの前例は番組リストの
  // `['/api/programs', 'infinite', ...]`（`pages/programs.tsx`）。
  if (sitesQuery.isPending || sitesQuery.isError) return null
  const statuses = sites.flatMap((site, index) => {
    const query = tunerQueries[index]
    if (query === undefined || query.isPending || query.isError) return []
    const tuners = unwrap(query.data) ?? []
    return tuners.length > 0 ? [{ site, tuners }] : []
  })
  if (statuses.length === 0) return null

  const showSite = sites.length > 1
  if (!showSite) {
    return <TunerStatusLine site={statuses[0]!.site} tuners={statuses[0]!.tuners} showSite={false} />
  }

  return (
    <div className="flex flex-col gap-1">
      {statuses.map(({ site, tuners }) => (
        <TunerStatusLine key={site} site={site} tuners={tuners} showSite />
      ))}
    </div>
  )
}

function TunerStatusLine({
  site,
  tuners,
  showSite,
}: {
  site: string
  tuners: readonly Tuner[]
  showSite: boolean
}) {
  const availableCount = tuners.filter((t) => t.isAvailable && !t.isFault).length
  const faultCount = tuners.filter((t) => t.isFault).length
  const oldestObservedAt = tuners.reduce(
    (oldest, t) => (Date.parse(t.observedAt) < Date.parse(oldest) ? t.observedAt : oldest),
    tuners[0]!.observedAt,
  )
  // tuner_sync は定期再取得で再描画される。時刻を mount 時に固定すると、古い観測が
  // stale になっても表示が変わらないため、描画時の観測時刻を使う。
  // oxlint-disable-next-line react/purity -- 定期再取得ごとの現在時刻スナップショットが必要
  const stale = isObservationStale(oldestObservedAt, Date.now())

  return (
    <p
      className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground"
      data-site={site}
      data-testid={showSite ? `tuner-status-${site}` : 'tuner-status'}
    >
      {showSite && <span className="font-medium">{site}</span>}
      <span>
        チューナー{availableCount}本
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
