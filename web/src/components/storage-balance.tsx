import { TriangleAlert } from 'lucide-react'

import {
  useListRecordings,
  useListReservations,
  useGetStorage,
  type StorageRoot,
} from '@/api/generated'
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
  upcomingReservationSchedule,
} from '@/lib/storage-forecast'
import { cn } from '@/lib/utils'

function StorageRootCapacity({ root, nowMs }: { root: StorageRoot; nowMs: number }) {
  const stale = isObservationStale(root.observedAt, nowMs)
  return (
    <section className="rounded-md border border-border p-3">
      <h3 className="mb-2 text-sm font-medium text-foreground">
        {root.root === 'media' ? 'アーカイブ' : 'スクラッチ'}
      </h3>
      <dl className="grid grid-cols-3 gap-3 text-xs">
        <div>
          <dt className="text-muted-foreground">総容量</dt>
          <dd className="font-medium text-foreground">{formatBytes(root.totalBytes)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">使用済み</dt>
          <dd className="font-medium text-foreground">{formatBytes(root.usedBytes)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">空き</dt>
          <dd className="font-medium text-foreground">{formatBytes(root.availableBytes)}</dd>
        </div>
      </dl>
      <p className={cn('mt-2 flex items-center gap-1 text-xs text-muted-foreground', stale && 'text-warning')}>
        {stale && <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />}
        観測: {formatDateTime(root.observedAt)}
        {stale && '（古い可能性）'}
      </p>
    </section>
  )
}

/**
 * StorageBalance は「空き X GB / 今後 N 日の予約で約 +Y GB の見込み」を要約に出し、
 * 展開時はアーカイブとスクラッチの総容量・使用済み・空きを表示する。
 *
 * 導出（母数の取り方・満杯見込み日の算出）は `lib/storage-forecast.ts` にすべて
 * 集約する。ここは 3 つの API（`GET /api/storage` / `GET /api/recordings` /
 * `GET /api/reservations`）の取得と、4 種類の沈黙の出し分けだけを持つ:
 *
 * 1. **ストレージ観測が無い**（`media` root が無い。初回観測前や statfs 失敗の継続で
 *    起きうる。openapi.yaml `getStorage` の説明参照）ときは何も描かない
 * 2. **直近の録画実績が 0 件、または予約の取得が未解決/失敗**のときは見込み
 *    （「+Y GB」「満杯見込み」）を出さず、残高（「空き X GB」）だけ出す ---
 *    どちらも `estimateStorageForecast` の `hasEstimate: false` に落ちる
 *    （`lib/storage-forecast.ts` モジュール doc「欠損データは『0』ではなく
 *    『算出不能』として伝える」）。**予約クエリが失敗しても `+0 B` を描かない
 *    ことをテストで固定してある**（`upcomingSchedule` を `undefined` のまま渡す
 *    --- 空配列 `[]` にフォールバックすると「予約 0 件」と区別できなくなる）
 * 3. **予約が正当に 0 件**（`upcomingSchedule` が空配列。取得は成功したが窓の中に
 *    予約が無い）ときも「+0 B」は出さない --- `projectedConsumptionBytes` が `0`
 *    以下なら見込みの行自体を描かない（ドロップ統計バッジの「0 のものは出さない」
 *    規律と同じ。`docs/frontend/recordings.md`「ドロップ統計はバッジ + 展開」）
 * 4. **見込み消費が残量に収まる**ときは満杯見込み日を出さない（下界主義）
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
  const scratch = roots?.find((root) => root.root === 'scratch')

  // 観測なしで黙る（罠「観測時刻が古いときの表示を決める」の前段 --- 行そのものが
  // 無いときは古さの判定すら成立しない）。
  if (media === undefined) return null

  const nowMs = Date.now()
  const stale = isObservationStale(media.observedAt, nowMs)

  const recordings = unwrap(recordingsQuery.data)
  const reservations = unwrap(reservationsQuery.data)

  // 録画が未解決/失敗なら averageBitrate は undefined のまま
  // （estimateStorageForecast が hasEstimate: false を返す。「録画実績 0 件」と
  // 同じ沈黙の経路）。
  const averageBitrate =
    recordings === undefined ? undefined : estimateAverageBitrate(recentBitrateSamples(recordings))

  const windowStartMs = nowMs
  const windowEndMs = nowMs + forecastWindowDays * 24 * 60 * 60 * 1000
  // 予約が未解決/失敗なら upcomingSchedule は undefined のまま渡す。
  // ここで [] にフォールバックすると「予約取得の失敗」と「予約が正当に 0 件」が
  // estimateStorageForecast から見て同じ入力になり、後者だけが起こるべき
  // 「hasEstimate: true, projectedConsumptionBytes: 0」の経路を失敗時にも
  // 誤って取ってしまう（= 見込みの取得に失敗したのに「+0 B の見込み」という
  // 肯定を描く。指摘 1 の再発防止）。
  const upcomingSchedule =
    reservations === undefined
      ? undefined
      : upcomingReservationSchedule(reservations, windowStartMs, windowEndMs)

  const forecast = estimateStorageForecast({
    availableBytes: media.availableBytes,
    averageBitrate,
    upcomingSchedule,
    nowMs,
  })

  return (
    <details className="border-t border-border text-xs text-muted-foreground">
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2">
        <span>
          空き <span className="font-medium text-foreground">{formatBytes(media.availableBytes)}</span>
        </span>

        {/* projectedConsumptionBytes > 0 を条件にする（0 のものは出さない。
            予約が正当に 0 件のときも「+0 B の見込み」を描かない）。 */}
        {forecast.hasEstimate &&
          forecast.projectedConsumptionBytes !== undefined &&
          forecast.projectedConsumptionBytes > 0 && (
            <span
              title={`直近 ${forecast.sampleSize} 件の録画実績（原本 TS の実測ビットレート。変換後のファイルは含まない）から算出した見込み`}
            >
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
        <span className="ml-auto underline-offset-2 hover:underline">ストレージ詳細</span>
      </summary>
      <div className="grid gap-2 px-4 pb-3 sm:grid-cols-2">
        <StorageRootCapacity root={media} nowMs={nowMs} />
        {scratch !== undefined && <StorageRootCapacity root={scratch} nowMs={nowMs} />}
      </div>
    </details>
  )
}
