/**
 * ストレージ残高と満杯見込みを実測から導出する純関数群（issue #239 M7-6）。
 *
 * Rokuban は予約（desired state）と実測（録画の `sizeBytes` / `durationMs`）を
 * 両方持つので、「今後の予約を消化したらどれだけ減るか」を予測できる。ここに置くのは
 * 導出そのものだけで、`GET /api/storage` や `GET /api/recordings` /
 * `GET /api/reservations` の取得は呼び出し側（`components/storage-balance.tsx`）が
 * 行う（`lib/program-search.ts` / `lib/capacity.ts` と同じ「React・fetch に依存しない
 * 判定は lib に置く」方針）。
 *
 * ## すべて「見込み」であって「足りる」の肯定はしない
 *
 * `lib/capacity.ts` の「主張は下界に限る」と同じ精神。この予測は次の 2 つの近似の上に
 * 立っている:
 *
 * 1. **平均ビットレートは直近の標本の外挿。** 番組の種類（ドラマ / スポーツ中継 /
 *    アニメ）でビットレートは変わるため、直近の標本の平均が今後の録画にも当てはまる
 *    保証は無い。
 * 2. **消費見込みは今後 {@link forecastWindowDays} 日間に均等に分布すると仮定した
 *    線形外挿。** 実際の予約は曜日・時間帯に偏るが、日ごとの偏りを再現する根拠が
 *    無いための単純化。
 *
 * したがって「見込み消費が残量に収まる」ときは何も言わない（`fullAtMs` は
 * `undefined`）。**収まることを保証できるほどこの近似は正確ではない** ---
 * 「満杯見込み」を出すのは見込みが残量を超えたときだけで、超えていない側の沈黙を
 * 「足りる」という肯定として読ませない。
 */

import type { Recording, Reservation, StorageRoot } from '@/api/generated'

/**
 * forecastWindowDays は消費見込みの対象期間（今後 7 日）。
 *
 * ルールの時間帯条件・`lib/rule-cost.ts` の値札と同じ 1 週間周期に揃えている
 * （番組の再放送・帯番組は曜日単位で繰り返すため、月や日単位より直感的に
 * 多い/少ないを判断できる）。EPG のローリングウィンドウ（8 日）とは違い、
 * こちらは予約の対象期間そのものを絞り込む窓であって近似の分母ではないので、
 * 8 日にする理由が無い。
 */
export const forecastWindowDays = 7

/**
 * recentRecordingSampleLimit は平均ビットレート算出に使う母数（直近 N 件）。
 *
 * 日数ではなく件数で母数を決めている --- 録画頻度は運用によって大きく違うため
 * （毎日 10 本録る運用と週末しか録らない運用）、固定の日数（例: 直近 7 日）では
 * 少ない側の運用で標本が 0〜数件に落ち込み、多い側の運用では標本が数百件に膨らんで
 * 1 回のフェッチ（`limit` 上限 200）に収まらなくなる。20 件は
 * `GET /api/recordings`（既定 `limit=50`、上限 200）1 回の呼び出しで確実に収まり、
 * 番組ジャンルの偏り（同じチャンネル・同じ時間帯の録画が連続すると平均が偏る）を
 * 均すのに 1〜数件よりは十分な数、という判断（実測に基づく最適値ではない）。
 */
export const recentRecordingSampleLimit = 20

/**
 * observationStaleAfterMs は `observedAt` をこれより古いと「古い観測」として扱う
 * しきい値（1 時間）。
 *
 * worker のストレージ観測ループは既定 5 分間隔（`internal/worker/storage.go` の
 * `defaultStorageSyncInterval`）だが、この値は設定で変更可能で API には表れない
 * （`config.example.yml` の該当キーはフロントから見えない）。したがって
 * 「既定間隔の何倍か」という基準ではなく、**観測ループが実際に止まっていることを
 * 検知するための独立した余裕**として 1 時間を選んでいる --- 既定間隔に対しては
 * 12 倍の余裕があり、1 回の失敗パス・再起動直後の遅延程度では誤って「古い」と
 * 出ない一方、観測ループが本当に止まっていれば 1 時間以内に検知できる。
 * 設定で間隔を大きく延ばした運用では基準として弱くなるが、その場合の調整は
 * 別 issue（このしきい値を設定可能にする要求が実際に出てから）に回す。
 */
export const observationStaleAfterMs = 60 * 60 * 1000

/** BitrateSample は平均ビットレート算出の入力 1 件。 */
export type BitrateSample = {
  sizeBytes: number
  durationMs: number
}

/**
 * recentBitrateSamples は `Recording[]` から平均ビットレート算出に使える標本を
 * 取り出す。
 *
 * **`status === 'finished'` のものだけを使う。** `durationMs` は番組の放送時間
 * （`program_start_at` からの尺）であって実際に録画できた時間ではないため、
 * 録画が途中で終わった（`failed`）録画を含めると「途中までの `sizeBytes`」を
 * 「全尺の `durationMs`」で割ることになり、実際より低いビットレートに偏る
 * （桁を外す近似ではないが方向が偏るため、確実に除ける失敗録画は除く）。
 * `canceled` はそもそも録画されていないので `sizeBytes` が無い。
 *
 * **`sizeBytes` が無い録画（原本削除済み。`Recording.sizeBytes` のコメント参照）も
 * 除く。** 実測が残っていない標本を算出に混ぜる理由が無い。
 */
export function recentBitrateSamples(recordings: readonly Recording[]): BitrateSample[] {
  return recordings
    .filter(
      (r): r is Recording & { sizeBytes: number } =>
        r.status === 'finished' && r.sizeBytes !== undefined && r.durationMs > 0,
    )
    .map((r) => ({ sizeBytes: r.sizeBytes, durationMs: r.durationMs }))
}

/** AverageBitrate は `estimateAverageBitrate` の出力。 */
export type AverageBitrate = {
  /** バイト/ミリ秒の平均ビットレート。 */
  bytesPerMs: number
  /** 算出に使った標本数。 */
  sampleSize: number
}

/**
 * estimateAverageBitrate は Σ sizeBytes / Σ durationMs で平均ビットレートを算出する。
 *
 * 標本が 0 件（**録画実績が無い** --- 罠「録画実績が 0 件のときは平均ビットレートが
 * 出せない」）なら `undefined`。でっち上げの既定値は置かない --- 呼び出し側は
 * この `undefined` を「見込みを出さない」の判定にそのまま使う。
 */
export function estimateAverageBitrate(samples: readonly BitrateSample[]): AverageBitrate | undefined {
  if (samples.length === 0) return undefined
  const totalBytes = samples.reduce((sum, s) => sum + s.sizeBytes, 0)
  const totalDurationMs = samples.reduce((sum, s) => sum + s.durationMs, 0)
  if (totalDurationMs <= 0) return undefined
  return { bytesPerMs: totalBytes / totalDurationMs, sampleSize: samples.length }
}

/** UpcomingReservation は `upcomingReservationDurationMs` の入力 1 件。 */
export type UpcomingReservation = Pick<Reservation, 'startAt' | 'durationMs' | 'skip'>

/**
 * upcomingReservationDurationMs は `[windowStartMs, windowEndMs)` に開始する、
 * 実際に mirakc へ同期される見込みの予約（`skip === false`）の合計時間。
 *
 * **`skip === true` の予約は除く。** `skip` は `effective.skip`
 * （`docs/recording/reservation-model.md` §4.3「同期の可否を決めるのは state では
 * なく effective.skip である」）で、true の間 reconciler は mirakc に同期しない ---
 * つまりディスクを消費しない予約。`state`（active/detached/orphaned）は導出値の
 * 表示用マーカーであって同期可否のフィルタに使ってはならないので、ここでは見ない。
 *
 * 区間は半開区間 `[windowStartMs, windowEndMs)`（`lib/capacity.ts` の区間規約と
 * 揃える）。
 */
export function upcomingReservationDurationMs(
  reservations: readonly UpcomingReservation[],
  windowStartMs: number,
  windowEndMs: number,
): number {
  let total = 0
  for (const r of reservations) {
    if (r.skip) continue
    const startMs = new Date(r.startAt).getTime()
    if (startMs < windowStartMs || startMs >= windowEndMs) continue
    total += r.durationMs
  }
  return total
}

/**
 * isObservationStale は `observedAt` が {@link observationStaleAfterMs} より古いかを
 * 返す。
 *
 * `GET /api/storage` の `observedAt` は「観測ループが止まっていても行は消えない」
 * ため鮮度の手がかりとして必ず使う契約（openapi.yaml `StorageRoot.observedAt`）。
 * 古い観測を新しい顔で見せないための唯一の判定点をここに集約する。
 */
export function isObservationStale(
  observedAt: string,
  nowMs: number,
  staleAfterMs: number = observationStaleAfterMs,
): boolean {
  return nowMs - new Date(observedAt).getTime() > staleAfterMs
}

/** StorageForecast は `estimateStorageForecast` の出力。 */
export type StorageForecast = {
  /**
   * 見込みを算出できたか。録画実績が 0 件（`averageBitrate` が `undefined`）なら
   * `false` --- 呼び出し側はこれを見て「見込み」欄そのものを描かない。
   */
  hasEstimate: boolean
  /** 見込みに使った標本数（`hasEstimate` が `false` なら 0）。 */
  sampleSize: number
  /**
   * 今後 {@link forecastWindowDays} 日ぶんの消費見込み（バイト）。
   * `hasEstimate` が `false` なら `undefined`。
   */
  projectedConsumptionBytes: number | undefined
  /** 見込み消費が残量（`availableBytes`）を超えるか。 */
  exceedsAvailable: boolean
  /**
   * 超える場合のみ満杯に達する見込み時刻（epoch ms）。収まる場合・見込みを
   * 算出できない場合は `undefined`（下界主義 --- 「足りる」は主張しない。
   * モジュール doc 冒頭「すべて『見込み』であって『足りる』の肯定はしない」）。
   */
  fullAtMs: number | undefined
}

/**
 * estimateStorageForecast は残量・平均ビットレート・今後の予約時間から
 * 「空き X / +Y の見込み / (超える場合のみ) 満杯見込み日」を導出する。
 *
 * 満杯見込み日は、見込み消費（`projectedConsumptionBytes`）を
 * {@link forecastWindowDays} 日間に均等に分布すると仮定した線形外挿
 * （モジュール doc の近似 2 を参照）。`availableBytes` が既に 0 以下でも
 * `msUntilFull` が 0 以下になり `nowMs` 以前を指すことがあるが、それも
 * 「すでに満杯（見込みを過ぎている）」という事実の表現として扱う
 * （呼び出し側で `Math.max(nowMs, fullAtMs)` のような下駄を履かせる必要は無い
 * --- 表示側が formatDate に渡せば過去日として素直に出る）。
 */
export function estimateStorageForecast(input: {
  /** `GET /api/storage` の archive root（`root === 'media'`）の残量。 */
  availableBytes: number
  averageBitrate: AverageBitrate | undefined
  /** `upcomingReservationDurationMs` の結果。 */
  upcomingDurationMs: number
  nowMs: number
  /** 既定 {@link forecastWindowDays}。テストで窓を変えられるようにするための穴。 */
  windowDays?: number
}): StorageForecast {
  const { availableBytes, averageBitrate, upcomingDurationMs, nowMs } = input
  const windowDays = input.windowDays ?? forecastWindowDays
  const windowMs = windowDays * 24 * 60 * 60 * 1000

  if (averageBitrate === undefined) {
    return {
      hasEstimate: false,
      sampleSize: 0,
      projectedConsumptionBytes: undefined,
      exceedsAvailable: false,
      fullAtMs: undefined,
    }
  }

  const projectedConsumptionBytes = averageBitrate.bytesPerMs * upcomingDurationMs
  const exceedsAvailable = projectedConsumptionBytes > availableBytes

  let fullAtMs: number | undefined
  if (exceedsAvailable && projectedConsumptionBytes > 0) {
    const bytesPerMsOverWindow = projectedConsumptionBytes / windowMs
    const msUntilFull = availableBytes / bytesPerMsOverWindow
    fullAtMs = nowMs + msUntilFull
  }

  return {
    hasEstimate: true,
    sampleSize: averageBitrate.sampleSize,
    projectedConsumptionBytes,
    exceedsAvailable,
    fullAtMs,
  }
}

/** findMediaRoot は `GET /api/storage` の結果からアーカイブ root（`media`）を探す。 */
export function findMediaRoot(roots: readonly StorageRoot[]): StorageRoot | undefined {
  return roots.find((r) => r.root === 'media')
}
