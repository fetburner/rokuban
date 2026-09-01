import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useSyncExternalStore } from 'react'

import {
  getGetEncodeQueueQueryKey,
  getGetStorageQueryKey,
  getListCapacityOveragesQueryKey,
  getListCircuitBreakersQueryKey,
  getListRecordingsQueryKey,
  getListReservationsQueryKey,
  getListSitesQueryKey,
} from '@/api/generated'

/**
 * operationalRefreshIntervalMs は運用状態（予約・録画・ブレーカー・容量超過）を
 * SSE 抜きで取り直す周期（ミリ秒）。
 *
 * 応答が小さく変化が速いグループなので短くできる。SSE の通知を全部落としても、
 * 前面タブで開いている画面はこの周期で REST から収束する
 * （テスト: events.test.tsx「SSE が来なくても運用状態のクエリは 60 秒周期で取り直す」）。
 */
export const operationalRefreshIntervalMs = 60_000

/**
 * epgRefreshIntervalMs は EPG（番組リスト・サービス一覧・重なり）を SSE 抜きで
 * 取り直す周期（ミリ秒）。
 *
 * 番組表は数十チャンネル x 24 時間の大きな時間窓を 1 回で取るうえ、EPG 同期
 * ジョブ自体が分オーダーでしか動かない。運用状態と同じ周期で回すと、得るものが
 * 無いまま最大の応答を繰り返し引くことになるので分ける
 * （テスト: events.test.tsx「EPG は運用状態より長い周期でしか取り直さない」）。
 */
export const epgRefreshIntervalMs = 600_000

/**
 * storageRefreshIntervalMs はストレージ残高（`/api/storage`）を取り直す周期
 * （ミリ秒、5 分）。
 *
 * `/api/storage` はディスクの statfs 観測の射影であって、worker の観測ループが
 * 書き換えたときにしか値が変わらない。運用状態（60 秒）と同じ周期で回しても、
 * 同じ値を余分に引くだけで得るものが無い。一方で EPG（10 分）ほど長くする理由も
 * ない（番組表グリッドのような大きな応答ではないので、短くする側のコストが
 * 小さい）。その 2 つの間に挟んで 5 分を選ぶ
 * （テスト: events.test.tsx「SSE が来なくてもストレージ残高は専用の周期で
 * 取り直す」）。
 *
 * worker の観測周期（`internal/worker/storage.go`）から輸入はしない ---
 * `lib/storage-forecast.ts` の `observationStaleAfterMs` と同じ立場で、
 * `GET /api/storage` の契約に入っていない実装詳細に結合すると、worker 側だけが
 * 変わってフロントが追随できなくなる。
 *
 * SSE トピックからの invalidate は持たない。`storage_sync` は行トリガーの対象に
 * していない（observed_at は毎パス全量 upsert されるだけで、行トリガーにすると
 * statfs の頻度そのものに通知量が結合する）ため。収束はこの定期 invalidate に
 * 加えて、再接続時の全グループ invalidate（{@link useServerEvents} 参照。topic が
 * `null` かどうかを見ずに queryGroups 全体を回す）と mount / window focus に依る
 * （テスト: events.test.tsx「再接続したら切断中の変更を全グループ取り直す」。
 * `docs/api/sse.md` 参照）。
 */
export const storageRefreshIntervalMs = 5 * 60_000

/**
 * reservationsQueryKeyPrefix / recordingsQueryKeyPrefix / capacityOveragesQueryKeyPrefix /
 * breakersQueryKeyPrefix / encodeQueueQueryKeyPrefix / storageQueryKeyPrefix /
 * sitesQueryKeyPrefix は orval 生成のクエリキー関数（先頭要素が URL）から
 * 導出した接頭辞。invalidate 用のリテラルをここと各ページで手書きに複製する
 * と、openapi.yaml のパスリネームにここだけが追随せず「操作後に一覧が
 * 更新されない」形で静かに壊れる。生成関数から引くことで自動追随させる
 * （先頭要素が URL 文字列である形式は orval の実装詳細への依存として残る）。
 *
 * `sitesQueryKeyPrefix` だけ末尾にスラッシュを足す --- `/api/sites` 自体
 * （サイト一覧）ではなく `/api/sites/{site}/...` 配下だけを epg グループで
 * 前方一致させたいため。
 *
 * ただしこの導出元（`getListSitesQueryKey`、サイト**一覧**の openapi パス）は、
 * epg グループが実際に前方一致させたい対象（`/api/sites/{site}/programs` /
 * `services` / `overlaps` という**別々の**サブリソースの openapi パス）とは
 * 別物。サブリソース側の生成関数はどれも `site` を引数に取るので、そこから
 * 接頭辞だけを安全に切り出せる生成関数は無い（これが唯一の安く手に入る導出元）。
 * そのため `/api/sites`（一覧）だけがリネームされても、このパスから見た
 * サブリソース側との対応が崩れているわけではないのに接頭辞は自動で追随して
 * しまい、epg グループが黙って前方一致を失うリスクは残る。手書き literal には
 * 無かったこの経路のリスクを、この導出は引き受けている。
 */
export const reservationsQueryKeyPrefix = getListReservationsQueryKey()[0]
export const recordingsQueryKeyPrefix = getListRecordingsQueryKey()[0]
export const capacityOveragesQueryKeyPrefix = getListCapacityOveragesQueryKey()[0]
export const breakersQueryKeyPrefix = getListCircuitBreakersQueryKey()[0]
export const encodeQueueQueryKeyPrefix = getGetEncodeQueueQueryKey()[0]
export const storageQueryKeyPrefix = getGetStorageQueryKey()[0]
export const sitesQueryKeyPrefix = `${getListSitesQueryKey()[0]}/`

/**
 * programsQueryKeyPrefix は番組リスト（`pages/programs.tsx` の
 * useInfiniteQuery）が使う手書きキーの先頭要素。
 *
 * 一覧が `/api/sites/{site}/programs`（site ごと）なのに対し、無限リストは
 * 複数 site を跨いだ 1 つの時間窓として取得するため、orval 生成キーをその
 * まま使えない（`docs` 化していない、pages/programs.tsx 側の doc コメント
 * 参照）。導出元が無いのでリテラルのまま残すが、events.ts と
 * pages/programs.tsx の複製をこの 1 箇所に集約する。
 */
export const programsQueryKeyPrefix = '/api/programs'

/**
 * tunersQueryKeyPrefix は `GET /api/sites/{site}/tuners` のクエリキーの接頭辞。
 *
 * このエンドポイントだけ URL をキーにできない。URL は epg グループの接頭辞
 * （`/api/sites/`）にも前方一致するので、周期の違う 2 グループに同じキーが入り、
 * 両方のタイマーが発火する時刻に 2 回 invalidate されてしまう。
 *
 * **キーを組み立てる側（`components/tuner-status.tsx`）とグループの接頭辞が
 * 同じ定数を参照することで、片方だけ改名して取り直しが止まる drift を型で
 * 防ぐ**。テスト側はこの定数を参照せず literal を持つので、定数を改名すれば
 * events.test.tsx と live.test.tsx が落ちる。
 */
export const tunersQueryKeyPrefix = '/api/tuners'

/**
 * QueryGroup は SSE トピック（無い場合もある）と、それによって無効化するクエリ
 * キーの接頭辞、そして SSE が届かなかったときに取り直す周期の組。
 */
type QueryGroup = {
  /**
   * SSE のトピック名。`null` は「対応する SSE トピックを持たず、定期 invalidate
   * だけで収束させる」グループ（`storage` / `tuners` がこれ。理由は
   * {@link storageRefreshIntervalMs} 参照）。
   *
   * optional にはしない --- optional だと「トピックを書き忘れた」グループが型でも
   * テストでも通り、SSE の購読を静かに失う。`null` を明示的に書かせることで、
   * 省略は型エラーのまま、意図した「トピック無し」だけが書ける。
   */
  topic: string | null
  /** invalidate 対象のクエリキーの接頭辞（orval のキーは `[url, params]`）。 */
  prefixes: string[]
  /** SSE が 1 通も届かなくても、この周期で invalidate する。 */
  refreshIntervalMs: number
}

/**
 * queryGroups は SSE のトピックと、それによって無効化するクエリキーの接頭辞の対応。
 * トピックを持たないグループ（`storage` / `tuners`）は SSE を購読せず、定期 invalidate
 * （加えて再接続時の全グループ invalidate）だけで収束させる。
 *
 * サーバーが配るのは「どのデータが変わったか」のヒントだけで、変更内容は載っていない。
 * 受け取ったら該当クエリを invalidate し、真実は REST から取り直す（レベルトリガー）。
 * プッシュの中身を信じて手元の状態を書き換えることはしない。
 *
 * ヒントは落ちうる（notifier はクライアントのバッファが埋まっていたら通知を捨てる）
 * ので、各グループは「SSE が 1 通も来なくても取り直す周期」を併せ持つ。
 */
const queryGroups: QueryGroup[] = [
  {
    topic: 'reservations',
    // 容量超過（チューナー不足）は予約集合からの導出値なので、予約が変わったら
    // 一緒に取り直す（docs/data.md §6.5）。専用のトピックは無い --- 導出値に
    // 独自の通知を持たせると、元データと導出値が別々の鮮度で並ぶことになる
    prefixes: [reservationsQueryKeyPrefix, capacityOveragesQueryKeyPrefix],
    refreshIntervalMs: operationalRefreshIntervalMs,
  },
  {
    topic: 'recordings',
    prefixes: [recordingsQueryKeyPrefix, encodeQueueQueryKeyPrefix],
    refreshIntervalMs: operationalRefreshIntervalMs,
  },
  {
    topic: 'breakers',
    prefixes: [breakersQueryKeyPrefix],
    refreshIntervalMs: operationalRefreshIntervalMs,
  },
  {
    topic: 'epg',
    // programs / services / overlaps はすべて /api/sites/{site}/... 配下。
    // 番組リスト（pages/programs.tsx の useInfiniteQuery）だけは URL をキーに
    // できず（ページの形が「取得した半開区間」なので）手書きの
    // [programsQueryKeyPrefix, 'infinite', ...] を使う。ここに書かないと、
    // 番組リストは SSE の epg イベントでも定期 invalidate でも取り直されない
    prefixes: [sitesQueryKeyPrefix, programsQueryKeyPrefix],
    refreshIntervalMs: epgRefreshIntervalMs,
  },
  {
    // トピックを持たない --- storageRefreshIntervalMs の doc コメント参照。
    topic: null,
    prefixes: [storageQueryKeyPrefix],
    refreshIntervalMs: storageRefreshIntervalMs,
  },
  {
    // トピックを持たない。tuner_sync の変更を知らせる SSE トピックは無く、
    // storage と同じ「使い捨てプロジェクションを定期 invalidate だけで
    // 収束させる」形（storageRefreshIntervalMs の doc コメント参照）。
    //
    // `GET /api/sites/{site}/tuners` の URL は epg の接頭辞（/api/sites/）に
    // 前方一致するので、キーは URL ではなく手書きにしてある
    // （{@link tunersQueryKeyPrefix}）。周期の違う 2 グループに同じキーが入ると、
    // 両方のタイマーが発火する時刻に別々の呼び出しで 2 回 invalidate される
    // （テスト: events.test.tsx「5 分と 10 分のタイマーが同時に発火しても
    // tuners は余分に取り直さない」）。
    topic: null,
    prefixes: [tunersQueryKeyPrefix],
    // storage と同じ周期を流用する（新しい数値は発明しない）。tuner_sync も
    // storage_sync と同じ「worker の定期全量同期でしか値が変わらない射影」
    // なので、性質が同じグループには同じ周期を割り当てる。
    refreshIntervalMs: storageRefreshIntervalMs,
  },
]

/** invalidateGroup は 1 グループ分のクエリキー接頭辞をまとめて invalidate する。 */
function invalidateGroup(queryClient: QueryClient, group: QueryGroup): void {
  for (const prefix of group.prefixes) {
    // orval のクエリキーは [url, params] の形なので、URL 接頭辞で照合する
    void queryClient.invalidateQueries({
      predicate: (query) => {
        const first = query.queryKey[0]
        return typeof first === 'string' && first.startsWith(prefix)
      },
    })
  }
}

type EncodeProgressSnapshot = ReadonlyMap<number, ReadonlyMap<string, number>>

const emptyEncodeProgress = new Map<string, number>()
let encodeProgressSnapshot: EncodeProgressSnapshot = new Map()
const encodeProgressListeners = new Set<() => void>()
const activeEncodeProfiles = new Map<number, Map<string, number>>()

function emitEncodeProgressChange(): void {
  for (const listener of encodeProgressListeners) listener()
}

function retainActiveEncodeProgress(recordingId: number): void {
  const current = encodeProgressSnapshot.get(recordingId)
  if (current === undefined) return
  const active = activeEncodeProfiles.get(recordingId)
  const retained = new Map([...current].filter(([profile]) => (active?.get(profile) ?? 0) > 0))
  if (retained.size === current.size) return
  const next = new Map(encodeProgressSnapshot)
  if (retained.size === 0) next.delete(recordingId)
  else next.set(recordingId, retained)
  encodeProgressSnapshot = next
  emitEncodeProgressChange()
}

function publishEncodeProgress(recordingId: number, profile: string, progress: number): void {
  if ((activeEncodeProfiles.get(recordingId)?.get(profile) ?? 0) === 0) return
  const profiles = new Map(encodeProgressSnapshot.get(recordingId) ?? [])
  profiles.set(profile, progress)
  const next = new Map(encodeProgressSnapshot)
  next.set(recordingId, profiles)
  encodeProgressSnapshot = next
  emitEncodeProgressChange()
}

function clearAllEncodeProgress(): void {
  if (encodeProgressSnapshot.size === 0) return
  encodeProgressSnapshot = new Map()
  emitEncodeProgressChange()
}

function subscribeEncodeProgress(
  recordingId: number,
  runningProfiles: readonly string[],
  listener: () => void,
): () => void {
  encodeProgressListeners.add(listener)
  const active = activeEncodeProfiles.get(recordingId) ?? new Map<string, number>()
  activeEncodeProfiles.set(recordingId, active)
  for (const profile of runningProfiles) active.set(profile, (active.get(profile) ?? 0) + 1)
  retainActiveEncodeProgress(recordingId)

  return () => {
    encodeProgressListeners.delete(listener)
    for (const profile of runningProfiles) {
      const count = (active.get(profile) ?? 0) - 1
      if (count > 0) active.set(profile, count)
      else active.delete(profile)
    }
    if (active.size === 0) activeEncodeProfiles.delete(recordingId)
    retainActiveEncodeProgress(recordingId)
  }
}

/** useEncodeProgress は durable 状態が running のプロファイルの揮発進捗を返す。 */
export function useEncodeProgress(
  recordingId: number,
  runningProfiles: readonly string[],
): ReadonlyMap<string, number> {
  const subscribe = useCallback(
    (listener: () => void) => subscribeEncodeProgress(recordingId, runningProfiles, listener),
    [recordingId, runningProfiles],
  )
  const snapshot = useSyncExternalStore(subscribe, () => encodeProgressSnapshot)
  return snapshot.get(recordingId) ?? emptyEncodeProgress
}

function receiveEncodeProgress(event: Event): void {
  if (!(event instanceof MessageEvent) || typeof event.data !== 'string') return
  try {
    const value: unknown = JSON.parse(event.data)
    if (typeof value !== 'object' || value === null) return
    const progress = value as Record<string, unknown>
    if (
      progress.type !== 'encode-progress' ||
      typeof progress.recordingId !== 'number' ||
      !Number.isSafeInteger(progress.recordingId) ||
      typeof progress.profile !== 'string' ||
      progress.profile === '' ||
      typeof progress.progress !== 'number' ||
      !Number.isFinite(progress.progress) ||
      progress.progress < 0 ||
      progress.progress > 1
    ) {
      return
    }
    publishEncodeProgress(progress.recordingId, progress.profile, progress.progress)
  } catch {
    // best-effort telemetry: malformed payload is ignored, durable state stays in REST.
  }
}

/** ConnectionStatus は /api/events（SSE）の接続状態。 */
export type ConnectionStatus =
  /** まだ一度も open していない（初回接続中、または再接続の待機中）。 */
  | 'connecting'
  /** 接続が開いている。 */
  | 'open'
  /** `error` を受けてから次の `open` までの間（EventSource は自分で再接続するが、
   * 再接続が成功するまでこの状態が続く）。 */
  | 'disconnected'

/** ConnectionState は {@link useConnectionState} が返す値。 */
export type ConnectionState = {
  status: ConnectionStatus
  /** 直近で `open` した時刻（ISO）。一度も繋がっていなければ null。 */
  lastConnectedAt: string | null
}

let connectionState: ConnectionState = { status: 'connecting', lastConnectedAt: null }
const connectionListeners = new Set<() => void>()

/**
 * setConnectionState は状態が実際に変わったときだけ購読者に通知する。
 *
 * `error` は切断が続く限り何度も飛んでくるが、ここで実質的な変化（status /
 * lastConnectedAt）が無ければ何もしない --- 帯を出す遅延タイマー
 * （{@link disconnectedBannerDelayMs}）を持つ側は「同じ status 値のままなら
 * 再レンダーされない」ことを前提に、error のたびにタイマーを再セットしない
 * 作りにできる。
 */
function setConnectionState(next: ConnectionState): void {
  if (
    next.status === connectionState.status &&
    next.lastConnectedAt === connectionState.lastConnectedAt
  ) {
    return
  }
  connectionState = next
  for (const listener of connectionListeners) listener()
}

function subscribeConnectionState(listener: () => void): () => void {
  connectionListeners.add(listener)
  return () => connectionListeners.delete(listener)
}

/** useConnectionState は /api/events（{@link useServerEvents}）の接続状態を返す。 */
export function useConnectionState(): ConnectionState {
  return useSyncExternalStore(subscribeConnectionState, () => connectionState)
}

/**
 * disconnectedBannerDelayMs は「切断中」になってから帯（`ConnectionBanner`）を
 * 出すまでの遅延（ミリ秒）。
 *
 * EventSource は瞬断でも自分で再接続するので、遅延なしで出すと瞬断のたびに
 * 帯が点滅する。10 秒は運用状態の定期 invalidate（60 秒）より十分短く
 * （帯無しで放置される時間を長くしない）、かつ大半の瞬断がここに達する前に
 * 自己回復する値として選んだ（実測ではなく判断）。
 */
export const disconnectedBannerDelayMs = 10_000

/**
 * useServerEvents は /api/events を購読し、届いたトピックに対応するクエリを
 * invalidate する。加えて、SSE に依存しない 2 つのレベル経路を張る。
 *
 *   - グループごとの定期 invalidate（`refreshIntervalMs`）。接続が生きたまま
 *     個別の通知だけ捨てられたケースを回復する唯一の経路
 *   - 再接続時の全グループ invalidate。切断中に飛んだ通知は再送されないので、
 *     周期を待たずに取り直す
 *
 * `staleTime` は「stale と判定する期限」であって再取得を起こすタイマーではない
 * ので、これらが無いと再 mount・focus・別操作のどれも起きない画面は古いまま残る。
 */
export function useServerEvents() {
  const queryClient = useQueryClient()

  useEffect(() => {
    const source = new EventSource('/api/events')
    const listeners: { type: string; handler: (event: Event) => void }[] = []
    const listen = (type: string, handler: (event: Event) => void) => {
      source.addEventListener(type, handler)
      listeners.push({ type, handler })
    }

    for (const group of queryGroups) {
      // topic が null のグループ（storage / tuners）は SSE を購読しない ---
      // 定期 invalidate だけで収束させる（storageRefreshIntervalMs の doc
      // コメント参照）
      if (group.topic === null) continue
      listen(group.topic, () => invalidateGroup(queryClient, group))
    }
    // encode-progress だけは次の値で上書きされる揮発テレメトリなので、REST を
    // invalidate せず payload を直接表示する。
    listen('encode-progress', receiveEncodeProgress)

    // EventSource は切断されると自分で繋ぎ直すが、切断中に飛んだ通知は二度と
    // 来ない。error（= 切断ないし失敗）を見た後の open でだけ全部取り直す。
    // 初回接続で取り直すと、各クエリの mount 時の取得と二重になる
    let disconnected = false
    listen('error', () => {
      disconnected = true
      clearAllEncodeProgress()
      // 「切断中」は error 後 open 前で定義する（readyState は再接続中も
      // CONNECTING を返し続けるので使わない）
      setConnectionState({ status: 'disconnected', lastConnectedAt: connectionState.lastConnectedAt })
    })
    listen('open', () => {
      setConnectionState({ status: 'open', lastConnectedAt: new Date().toISOString() })
      if (!disconnected) return
      disconnected = false
      for (const group of queryGroups) invalidateGroup(queryClient, group)
    })

    return () => {
      for (const { type, handler } of listeners) source.removeEventListener(type, handler)
      source.close()
      clearAllEncodeProgress()
      setConnectionState({ status: 'connecting', lastConnectedAt: connectionState.lastConnectedAt })
    }
  }, [queryClient])

  useEffect(() => {
    const intervals = [...new Set(queryGroups.map((group) => group.refreshIntervalMs))]
    const timers = intervals.map((intervalMs) =>
      setInterval(() => {
        // 背面タブでは投げない。復帰時は refetchOnWindowFocus が拾う
        // （main.tsx の QueryClient 既定）
        if (document.visibilityState === 'hidden') return
        for (const group of queryGroups) {
          if (group.refreshIntervalMs === intervalMs) invalidateGroup(queryClient, group)
        }
      }, intervalMs),
    )

    return () => {
      for (const timer of timers) clearInterval(timer)
    }
  }, [queryClient])
}
