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
 * `lib/capacity.ts` の「主張は下界に限る」と同じ精神。この予測は近似の上に立って
 * いる --- **列挙は閉じていない**（「近似は N 個」という言い方はしない。3 つ目は
 * レビューで発見されたもので、今後も増える可能性を否定しない）:
 *
 * 1. **平均ビットレートは直近の標本の外挿。** 番組の種類（ドラマ / スポーツ中継 /
 *    アニメ）でビットレートは変わるため、直近の標本の平均が今後の録画にも当てはまる
 *    保証は無い。
 * 2. **`sizeBytes` は原本 TS のみで、エンコード派生物を含まない。** `Recording.sizeBytes`
 *    は「原本の実サイズ。ingest 済みの場合のみ」（openapi.yaml）で、エンコード
 *    プロファイルを設定しているルール（`keepOriginal: always`、既定。
 *    [storage/retention.md](../../docs/storage/retention.md)）では実消費は
 *    原本 + 派生物なので、この見込みは**過小**に振れる。逆に `keepOriginal:
 *    until_encoded` を選んでいるルールでは、エンコード完了後に原本が削除され
 *    実消費が派生物サイズ（原本の 1/4〜1/10、同 doc）へ縮むため、原本サイズを
 *    今後も一定と仮定するこの見込みは**過大**に振れる。どちらの方向にも振れる
 *    ことを前提に見せており、一方向の保証は書かない
 *    （`lib/rule-cost.ts` の「見込み」と同じ立場）。
 * 3. **満杯見込み日は、重なる予約を直列（開始順に 1 本ずつ消費）として扱う。**
 *    `projectedFullAtMs` は `upcomingReservationSchedule` が `startMs` 昇順に
 *    並べた予約を順に積み上げるため、実際に並行して録画される予約（複数チューナー
 *    構成では普通）どうしの重なりを見ない。窓全体に均す一様分布の仮定は既に
 *    置いていないが（後述 `estimateStorageForecast` 参照）、この直列近似は
 *    別の問題として残る。両方向に振れる:
 *    - 同時に始まる複数予約では、直列近似は実際より**遅い**満杯見込みを出す
 *      （過小警告・危険な方向。実測: 同時開始の 6 時間予約 2 本が残量を消費する
 *      とき、直列近似は 2.78 時間後と報告するが、実際の合成レートでは 1.39 時間後
 *      に満杯になる --- `lib/storage-forecast.test.ts` の該当テスト参照）
 *    - 長い予約の途中に短い予約が重なるだけなら、直列近似は実際より**早い**満杯
 *      見込みを出す（過大警告・安全側だが不正確。実測: 24 時間予約に 30 分予約が
 *      重なるケースで、直列近似は 10 分後と報告するが、実際は約 23.67 時間後）
 *
 *    誤差の大きさは重なる予約のうち長い方の尺で頭打ちになる（実測で数時間〜
 *    約 1 日程度）。一様分布を仮定していた旧実装の系統誤差（最大で窓の長さ
 *    7 日そのもの）より小さいが、ゼロではない。一方向の保証は書かない
 *    （`lib/rule-cost.ts` と同じ立場）。
 *
 * したがって「見込み消費が残量に収まる」ときは何も言わない（`fullAtMs` は
 * `undefined`）。**収まることを保証できるほどこの近似は正確ではない** ---
 * 「満杯見込み」を出すのは見込みが残量を超えたときだけで、超えていない側の沈黙を
 * 「足りる」という肯定として読ませない。
 *
 * ## 欠損データは「0」ではなく「算出不能」として伝える
 *
 * `averageBitrate` / `upcomingSchedule` のどちらかが `undefined`（録画・予約の
 * どちらかの取得が未解決 or 失敗）なら `estimateStorageForecast` は
 * `hasEstimate: false` を返す。**`0` にフォールバックしない** ---
 * 「取得できていない」を `0`（=「これから何も消費しない」という肯定）に変換すると、
 * 欠損データから見込みを捏造することになり下界主義に反する。実際に予約が
 * 0 件（正当な `[]`）のときは `upcomingSchedule` が空配列になり `hasEstimate: true`
 * かつ `projectedConsumptionBytes: 0` になる --- こちらは「見込みが無い」ではなく
 * 「見込みを算出した結果 0 だった」なので区別する。
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
 * worker のストレージ観測ループの間隔は現在 **5 分固定**（`internal/worker/storage.go`
 * の `defaultStorageSyncInterval`）。`worker.Config.StorageSyncInterval` という
 * フィールドは存在するが、`cmd/rokuban/server.go` がこれを設定ファイルのどのキーからも
 * 埋めていない（`grep -rn StorageSyncInterval` が返すのは宣言・既定値へのフォール
 * バック・doc コメントだけで、代入は無い）。つまり**今は実質ハードコードの 5 分**で、
 * 「設定で変更可能」という以前の記述は誤り（実在しないキーへの参照だった）。
 *
 * それでも間隔の値をここへ輸入して「5 分の N 倍」と定義しないのは、この値が
 * worker 側の実装詳細であり `GET /api/storage` の契約に含まれていないため ---
 * フロントは API 契約だけを見て判定すべきで、worker の定数に結合すると、
 * 将来 worker 側だけが変わってフロントが追随できない依存になる。1 時間は
 * その代わりに置いた**独立した固定の安全マージン**: 現在の 5 分間隔に対しては
 * 12 倍の余裕があり、1 回の失敗パス・再起動直後の遅延程度では誤って「古い」と
 * 出ない一方、観測ループが本当に止まっていれば 1 時間以内に検知できる。
 * 将来 `StorageSyncInterval` が実際に設定可能になり、かつ既定より大きく延ばす
 * 運用が出てきた場合はこの余裕が相対的に薄くなるため、そのときはしきい値を
 * 見直す（現時点ではそのような運用要求が無いので固定値のままにする）。
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
 * **`sizeBytes` が無い録画（原本削除済み。`Recording.sizeBytes` のコメント
 * 「原本の実サイズ。ingest 済みの場合のみ」参照 --- 未 ingest も含む）も除く。**
 * 実測が残っていない標本を算出に混ぜる理由が無い。
 *
 * **`sizeBytes` は原本 TS のみでエンコード派生物を含まない。** これがそのまま
 * 今後の見込みの近似の一部になる（モジュール doc の近似 2 参照）。
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

/** UpcomingReservation は `upcomingReservationSchedule` の入力 1 件。 */
export type UpcomingReservation = Pick<Reservation, 'startAt' | 'durationMs' | 'skip'>

/** ScheduledConsumption は 1 予約ぶんの消費イベント（開始時刻・尺）。 */
export type ScheduledConsumption = {
  startMs: number
  durationMs: number
}

/**
 * upcomingReservationSchedule は `[windowStartMs, windowEndMs)` に開始する、
 * 実際に mirakc へ同期される見込みの予約（`skip === false`）を `startMs` 昇順に
 * 整列した消費イベント列にする。
 *
 * **`skip === true` の予約は除く。** `skip` は `effective.skip`
 * （`docs/recording/reservation-model.md` §4.3「同期の可否を決めるのは state では
 * なく effective.skip である」）で、true の間 reconciler は mirakc に同期しない ---
 * つまりディスクを消費しない予約。`state`（active/detached/orphaned）は導出値の
 * 表示用マーカーであって同期可否のフィルタに使ってはならないので、ここでは見ない。
 *
 * 区間は半開区間 `[windowStartMs, windowEndMs)`（`lib/capacity.ts` の区間規約と
 * 揃える）。
 *
 * `startMs` 昇順に整列するのは、`estimateStorageForecast` が満杯見込み日を
 * 「取得済みの各予約の実際の開始時刻」に沿って累積消費曲線を辿って算出するため
 * （一様分布の仮定を置かない。モジュール doc の近似 3）。
 */
export function upcomingReservationSchedule(
  reservations: readonly UpcomingReservation[],
  windowStartMs: number,
  windowEndMs: number,
): ScheduledConsumption[] {
  const events: ScheduledConsumption[] = []
  for (const r of reservations) {
    if (r.skip) continue
    const startMs = new Date(r.startAt).getTime()
    if (startMs < windowStartMs || startMs >= windowEndMs) continue
    events.push({ startMs, durationMs: r.durationMs })
  }
  return events.sort((a, b) => a.startMs - b.startMs)
}

/**
 * isObservationStale は `observedAt` が {@link observationStaleAfterMs} より古いかを
 * 返す。
 *
 * `GET /api/storage` の `observedAt` は「観測ループが止まっていても行は消えない」
 * ため鮮度の手がかりとして必ず使う契約（openapi.yaml `StorageRoot.observedAt`）。
 * 古い観測を新しい顔で見せないための唯一の判定点をここに集約する。
 *
 * しきい値ちょうど（`nowMs - observedAt === staleAfterMs`）は古いとしない
 * （`>` であって `>=` ではない --- 境界値テストで固定してある）。
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
   * 見込みを算出できたか。次のいずれかで `false`: 録画実績が 0 件
   * （`averageBitrate` が `undefined`）、または予約の取得が未解決/失敗
   * （`upcomingSchedule` が `undefined`）。呼び出し側はこれを見て「見込み」欄
   * そのものを描かない。
   */
  hasEstimate: boolean
  /** 見込みに使った標本数（`hasEstimate` が `false` なら 0）。 */
  sampleSize: number
  /**
   * 今後 {@link forecastWindowDays} 日ぶんの消費見込み（バイト）。
   * `hasEstimate` が `false` なら `undefined`。予約が正当に 0 件のときは
   * `0`（算出不能の `undefined` とは区別する。モジュール doc 参照）。
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
 * estimateStorageForecast は残量・平均ビットレート・今後の予約スケジュールから
 * 「空き X / +Y の見込み / (超える場合のみ) 満杯見込み日」を導出する。
 *
 * **満杯見込み日は「7 日間に均等分布」という一様分布を仮定しない。**
 * `upcomingSchedule`（`upcomingReservationSchedule` の結果。`startMs` 昇順）を
 * 先頭から辿り、各予約の消費（`durationMs × bytesPerMs`）を積み上げて
 * `availableBytes` を最初に超える瞬間を報告する（`projectedFullAtMs`）。
 * 各予約の開始時刻・尺は `GET /api/reservations` で既に取得済みなので、
 * 7 日間に均等に散らすという単純化を置く理由が無い --- 窓の後半に固まった
 * 予約を前寄せで警告したり（過大）、窓の直後に固まった予約を後ろ倒しで警告する
 * （過小・危険な方向）ことを避ける。
 *
 * **それでも各予約を直列（重なりを見ない）として積み上げるので、並行する予約
 * どうしの近似は残る**（モジュール doc の近似 3。両方向に振れ、誤差は重なる
 * 予約のうち長い方の尺で頭打ちになる）。7 日間の一様分布よりは狭い範囲の近似だが、
 * ゼロではない。
 */
export function estimateStorageForecast(input: {
  /** `GET /api/storage` の archive root（`root === 'media'`）の残量。 */
  availableBytes: number
  averageBitrate: AverageBitrate | undefined
  /**
   * `upcomingReservationSchedule` の結果。`undefined` は「予約の取得が
   * 未解決/失敗」を表し、正当な 0 件（`[]`）とは区別する
   * （モジュール doc「欠損データは『0』ではなく『算出不能』として伝える」）。
   */
  upcomingSchedule: ScheduledConsumption[] | undefined
  nowMs: number
}): StorageForecast {
  const { availableBytes, averageBitrate, upcomingSchedule, nowMs } = input

  if (averageBitrate === undefined || upcomingSchedule === undefined) {
    return {
      hasEstimate: false,
      sampleSize: 0,
      projectedConsumptionBytes: undefined,
      exceedsAvailable: false,
      fullAtMs: undefined,
    }
  }

  const bytesPerMs = averageBitrate.bytesPerMs
  const projectedConsumptionBytes = upcomingSchedule.reduce(
    (sum, event) => sum + event.durationMs * bytesPerMs,
    0,
  )
  const exceedsAvailable = projectedConsumptionBytes > availableBytes

  const fullAtMs = exceedsAvailable
    ? projectedFullAtMs(upcomingSchedule, bytesPerMs, availableBytes, nowMs)
    : undefined

  return {
    hasEstimate: true,
    sampleSize: averageBitrate.sampleSize,
    projectedConsumptionBytes,
    exceedsAvailable,
    fullAtMs,
  }
}

/**
 * projectedFullAtMs は実際の予約開始時刻に沿った累積消費曲線を辿り、残量
 * （`availableBytes`）を最初に超える瞬間を返す。
 *
 * `schedule` は `startMs` 昇順（`upcomingReservationSchedule` の契約）が前提。
 * 超える予約が見つかったら、その予約の途中で交差する正確な時刻を予約内で
 * 比例配分して返す（予約の消費が瞬間的ではなく尺に沿って一定速度で進むと
 * 仮定する --- 予約の中でだけ置く単純化で、予約の外（窓の一様分布）には
 * 拡張しない）。
 *
 * **予約は重なりを考慮せず直列に積み上げる。** 実際に並行して録画される予約
 * （複数チューナー構成では普通）があっても合成レートにはしない（モジュール
 * doc の近似 3。両方向に振れることは同 doc と `lib/storage-forecast.test.ts`
 * の「既知の近似」テストで固定してある）。
 *
 * 呼び出し側（`estimateStorageForecast`）は `exceedsAvailable` を確認した
 * 後にだけこれを呼ぶが、`schedule` が空、または浮動小数点誤差で 1 件も
 * 超えなかった場合の保険として、最後の予約の終了時刻（`schedule` が空なら
 * `nowMs`）にフォールバックする。
 */
function projectedFullAtMs(
  schedule: readonly ScheduledConsumption[],
  bytesPerMs: number,
  availableBytes: number,
  nowMs: number,
): number {
  let cumulative = 0
  for (const event of schedule) {
    const eventBytes = event.durationMs * bytesPerMs
    if (cumulative + eventBytes > availableBytes) {
      const remaining = availableBytes - cumulative
      const fraction = eventBytes > 0 ? remaining / eventBytes : 0
      return event.startMs + fraction * event.durationMs
    }
    cumulative += eventBytes
  }
  const last = schedule[schedule.length - 1]
  return last === undefined ? nowMs : last.startMs + last.durationMs
}

/** findMediaRoot は `GET /api/storage` の結果からアーカイブ root（`media`）を探す。 */
export function findMediaRoot(roots: readonly StorageRoot[]): StorageRoot | undefined {
  return roots.find((r) => r.root === 'media')
}
