import { ChevronDown, TriangleAlert } from 'lucide-react'

import {
  useListRecordings,
  useListReservations,
  useGetStorage,
  type StorageRoot,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { formatBytes, formatDate, formatDateTime } from '@/lib/format'
import { useNow } from '@/lib/use-now'
import {
  estimateAverageBitrate,
  estimateStorageForecast,
  findMediaRoots,
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
        {`${root.root === 'media' ? 'アーカイブ' : 'スクラッチ'}（${root.store}）`}
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
 * `compact` では録画一覧の管理情報行へ収めるため、通常時の見込みは展開側へ
 * 常に移す（警告の有無に関わらない）。観測時刻は stale でない限り要約に出さず、
 * 展開すれば `StorageRootCapacity` が root ごとに観測時刻を出すのでそちらに
 * 譲る（展開側の追加行に二重で持たせない）。満杯見込みや観測 stale などの
 * 警告は要約に残す。
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
 *    規律と同じ。`docs/frontend/recordings.md`「ドロップ統計はバッジ + PID 表」）
 * 4. **見込み消費が残量に収まる**ときは満杯見込み日を出さない（下界主義）
 * 5. **置き場が複数あるときは見込みを出さない。** 予約がどの置き場へ入るかは
 *    `GET /api/reservations` からは決まらないので、残量と消費を対応づけられない。
 *    残高（「空き」）は置き場ごとに出す。
 */
export function StorageBalance({ compact = false }: { compact?: boolean }) {
  const storageQuery = useGetStorage()
  const recordingsQuery = useListRecordings({
    status: 'finished',
    limit: recentRecordingSampleLimit,
  })
  const reservationsQuery = useListReservations()

  const roots = unwrap(storageQuery.data)
  const mediaRoots = roots === undefined ? [] : findMediaRoots(roots)
  const scratchRoots = roots === undefined ? [] : roots.filter((root) => root.root === 'scratch')

  // 1 時間の観測しきい値を遅くとも 30 秒後に表示へ反映する。再取得結果が同じでも
  // 時計が進むため、ストレージ予測窓と古さの判定が止まらない。
  const nowMs = useNow({ intervalMs: 30_000, active: mediaRoots.length > 0 })

  // 観測なしで黙る（罠「観測時刻が古いときの表示を決める」の前段 --- 行そのものが
  // 無いときは古さの判定すら成立しない）。
  if (mediaRoots.length === 0) return null

  // 警告は置き場のどれかが古ければ出す。要約の観測時刻は最も古いものを出す。
  const stale = mediaRoots.some((root) => isObservationStale(root.observedAt, nowMs))
  const oldestObserved = mediaRoots.reduce((oldest, root) =>
    new Date(root.observedAt).getTime() < new Date(oldest.observedAt).getTime() ? root : oldest,
  )

  const recordings = unwrap(recordingsQuery.data)
  const reservations = unwrap(reservationsQuery.data)

  // 録画が未解決/失敗なら averageBitrate は undefined のまま
  // （estimateStorageForecast が hasEstimate: false を返す。「録画実績 0 件」と
  // 同じ沈黙の経路）。
  const averageBitrate =
    recordings === undefined ? undefined : estimateAverageBitrate(recentBitrateSamples(recordings))

  // 見込みは置き場が 1 つのときだけ算出する（ルール 5）。
  const singleMedia = mediaRoots.length === 1 ? mediaRoots[0] : undefined

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

  const forecast =
    singleMedia === undefined
      ? undefined
      : estimateStorageForecast({
          availableBytes: singleMedia.availableBytes,
          averageBitrate,
          upcomingSchedule,
          nowMs,
        })

  // 通常時の見込みは compact なら常に展開側へ移す（満杯見込み・stale の観測は
  // 警告なので別枠のまま summary に残る。この変数を hasWarning に依存させると
  // 「満杯見込みでも stale でもない compact」のときに観測時刻がどこにも出ない
  // 抜け穴になる）。
  const showForecastInSummary = !compact
  const showObservationInSummary = !compact || stale
  const forecastSummary =
    forecast !== undefined &&
    forecast.hasEstimate &&
    forecast.projectedConsumptionBytes !== undefined &&
    forecast.projectedConsumptionBytes > 0 ? (
      <span
        title={`直近 ${forecast.sampleSize} 件の録画実績（原本 TS の実測ビットレート。変換後のファイルは含まない）から算出した見込み`}
      >
        今後{forecastWindowDays}日の予約で約 +
        {formatBytes(forecast.projectedConsumptionBytes)} の見込み
      </span>
    ) : null

  return (
    <details
      className={cn(
        'min-w-0 text-xs text-muted-foreground',
        // compact は畳んだ状態では隣接するエンコードチップと 1 行に収まる幅
        // （flex-1）に振る舞うが、開いたときはその幅のまま 3 カラムの詳細
        // グリッドを押し込むと崩れる。open: variant で開いたときだけ自分の
        // 行を占有させる。
        compact ? 'group flex-1 open:basis-full' : 'border-t border-border',
      )}
    >
      <summary
        className={cn(
          'flex cursor-pointer list-none flex-wrap items-center gap-x-1 gap-y-1 sm:gap-x-3',
          compact ? 'py-1' : 'px-4 py-2',
          'pointer-coarse:min-h-11',
        )}
      >
        {mediaRoots.map((root) => (
          <span key={root.store}>
            {mediaRoots.length > 1 ? `${root.store} ` : ''}空き{' '}
            <span className="font-medium text-foreground">{formatBytes(root.availableBytes)}</span>
          </span>
        ))}

        {/* projectedConsumptionBytes > 0 を条件にする（0 のものは出さない。
            予約が正当に 0 件のときも「+0 B の見込み」を描かない）。 */}
        {showForecastInSummary && forecastSummary}

        {forecast !== undefined && forecast.exceedsAvailable && forecast.fullAtMs !== undefined && (
          <span className="flex items-center gap-1 text-warning">
            <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
            満杯見込み: {formatDate(new Date(forecast.fullAtMs).toISOString())}頃
          </span>
        )}

        {showObservationInSummary && (
          <span
            className={cn('flex items-center gap-1', stale && 'text-warning')}
            title={stale ? '観測ループが止まっている可能性があります' : undefined}
          >
            {stale && <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />}
            観測: {formatDateTime(oldestObserved.observedAt)}
            {stale && '（古い可能性）'}
          </span>
        )}
        <span className={cn(!compact && 'ml-auto', 'underline-offset-2 hover:underline')}>
          {compact && <span className="sr-only">ストレージ詳細</span>}
          {compact ? (
            <>
              <span className="hidden sm:inline" aria-hidden="true">詳細</span>
              <ChevronDown
                className="size-3 shrink-0 transition-transform group-open:rotate-180 sm:hidden"
                aria-hidden="true"
              />
            </>
          ) : 'ストレージ詳細'}
        </span>
      </summary>
      {/* compact のときだけ足す追加行。観測時刻は展開すると StorageRootCapacity が
          root ごとに既に「観測: …」を出しているので、ここでは二重にしない
          （forecastSummary だけを持つ）。 */}
      {compact && forecastSummary && (
        <div className="flex flex-wrap gap-x-3 gap-y-1 pb-2">{forecastSummary}</div>
      )}
      <div className={cn('grid gap-2 pb-3 sm:grid-cols-2', !compact && 'px-4')}>
        {mediaRoots.map((root) => (
          <StorageRootCapacity key={`media:${root.store}`} root={root} nowMs={nowMs} />
        ))}
        {scratchRoots.map((root) => (
          <StorageRootCapacity key={`scratch:${root.store}`} root={root} nowMs={nowMs} />
        ))}
      </div>
    </details>
  )
}
