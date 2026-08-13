import { TriangleAlert } from 'lucide-react'

import { useListRecordings, useListReservations, useGetStorage } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { formatBytes, formatDate, formatDateTime } from '@/lib/format'
import {
  estimateAverageBitrate,
  estimateStorageForecast,
  findMediaRoot,
  forecastWindowDays,
  isObservationStale,
  recentBitrateSamples,
  recentRecordingSampleLimit,
  upcomingReservationDurationMs,
} from '@/lib/storage-forecast'
import { cn } from '@/lib/utils'

/**
 * StorageBalance は「空き X GB / 今後 N 日の予約で約 +Y GB の見込み」を表示する
 * ヘッダー部品（issue #239 M7-6）。`components/` に独立させているのは M8 でホームへ
 * 移設予定のため（`pages/recordings.tsx` はこの部品の最初の設置先に過ぎない）。
 *
 * 導出（母数の取り方・線形外挿の前提）は `lib/storage-forecast.ts` にすべて集約する。
 * ここは 3 つの API（`GET /api/storage` / `GET /api/recordings` /
 * `GET /api/reservations`）の取得と、3 種類の沈黙の出し分けだけを持つ:
 *
 * 1. **ストレージ観測が無い**（`media` root が無い。初回観測前や statfs 失敗の継続で
 *    起きうる。openapi.yaml `getStorage` の説明参照）ときは何も描かない
 * 2. **直近の録画実績が 0 件**のときは見込み（「+Y GB」「満杯見込み」）を出さず、
 *    残高（「空き X GB」）だけ出す
 * 3. **見込み消費が残量に収まる**ときは満杯見込み日を出さない（下界主義。
 *    `lib/storage-forecast.ts` モジュール doc 参照）
 *
 * 録画・予約の取得が失敗した（`isError`）場合も見込みの算出には進まず、上記 2 と
 * 同じ「残高だけ出す」に落とす --- この部品はヘッダーの一角を占める補助情報であり、
 * 一覧本体の取得失敗のような専用のエラー表示（`ErrorState`）を割く対象ではないため、
 * 静かに縮退させる。
 */
export function StorageBalance() {
  const storageQuery = useGetStorage()
  const recordingsQuery = useListRecordings({
    status: 'finished',
    limit: recentRecordingSampleLimit,
  })
  const reservationsQuery = useListReservations()

  const roots = unwrap(storageQuery.data)
  const media = roots === undefined ? undefined : findMediaRoot(roots)

  // 観測なしで黙る（罠「観測時刻が古いときの表示を決める」の前段 --- 行そのものが
  // 無いときは古さの判定すら成立しない）。
  if (media === undefined) return null

  const nowMs = Date.now()
  const stale = isObservationStale(media.observedAt, nowMs)

  const recordings = unwrap(recordingsQuery.data)
  const reservations = unwrap(reservationsQuery.data)

  // 録画・予約のどちらかが未解決/失敗なら見込みは算出しない（残高だけ出す）。
  // averageBitrate を undefined のままにすると estimateStorageForecast が
  // hasEstimate: false を返すので、「録画実績 0 件」と同じ沈黙の経路に自然に乗る。
  const averageBitrate =
    recordings === undefined ? undefined : estimateAverageBitrate(recentBitrateSamples(recordings))

  const windowStartMs = nowMs
  const windowEndMs = nowMs + forecastWindowDays * 24 * 60 * 60 * 1000
  const upcomingDurationMs =
    reservations === undefined
      ? 0
      : upcomingReservationDurationMs(reservations, windowStartMs, windowEndMs)

  const forecast = estimateStorageForecast({
    availableBytes: media.availableBytes,
    averageBitrate,
    upcomingDurationMs,
    nowMs,
  })

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-4 py-2 text-xs text-muted-foreground">
      <span>
        空き <span className="font-medium text-foreground">{formatBytes(media.availableBytes)}</span>
      </span>

      {forecast.hasEstimate && forecast.projectedConsumptionBytes !== undefined && (
        <span title={`直近 ${forecast.sampleSize} 件の録画実績（実測ビットレート）から算出した見込み`}>
          今後{forecastWindowDays}日の予約で約 +
          {formatBytes(forecast.projectedConsumptionBytes)} の見込み
        </span>
      )}

      {forecast.exceedsAvailable && forecast.fullAtMs !== undefined && (
        <span className="flex items-center gap-1 text-warning">
          <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
          満杯見込み: {formatDate(new Date(forecast.fullAtMs).toISOString())}頃
        </span>
      )}

      <span
        className={cn('flex items-center gap-1', stale && 'text-warning')}
        title={stale ? '観測ループが止まっている可能性があります' : undefined}
      >
        {stale && <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />}
        観測: {formatDateTime(media.observedAt)}
        {stale && '（古い可能性）'}
      </span>
    </div>
  )
}
