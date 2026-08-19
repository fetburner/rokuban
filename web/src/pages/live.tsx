import { Link, useSearch as useRouteSearch } from '@tanstack/react-router'
import { Play } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'

import {
  useListPrograms,
  useListReservations,
  useListServices,
  type ProgramListItem,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { LiveInterruptionWarning } from '@/components/live-interruption-warning'
import { LivePlayer } from '@/components/live-player'
import { Button } from '@/components/ui/button'
import { useLiveCapability } from '@/lib/capabilities'
import { currentProgramWindow, liveServiceKey, pickInitialService } from '@/lib/live'
import { interruptionQueryWindow, upcomingInterruptingReservation } from '@/lib/live-interruption'
import { channelTypeLabel, groupByChannelType, orderServices } from '@/lib/epg-grid'
import { formatTime, isAiring } from '@/lib/format'
import { useCurrentSite } from '@/lib/site'
import { cn } from '@/lib/utils'

/**
 * nowPlayingRefetchMs は「いま放送中」表示を作り直す間隔。
 *
 * 番組が終わって次の番組に切り替わるタイミングを追いかけるためのポーリング。
 * SSE の `programs` 相当のトピックは無い（EPG 更新は元々 watcher の定期
 * ジョブなので、番組の切り替わり自体はサーバー側で NOTIFY されない）ため、
 * レベルトリガーの精神（不変条件 5）に沿って一定間隔で再取得する。
 *
 * **`useListPrograms` に `refetchInterval` は渡さない。** `nowMs`（tick）が
 * この間隔で更新されるたびに `currentProgramWindow` の結果が変わって
 * クエリキー自体が変わるので、それだけで同じ周期の再取得が起きる ---
 * `refetchInterval` を並置すると「キー変化による取得」と「タイマーによる
 * 取得」の同じ周期の再取得が二重に走るだけで、後者は何も追加しない
 * （レビュー #190 の指摘）。
 */
const nowPlayingRefetchMs = 30_000

/**
 * LivePage はライブ視聴画面（M4-4。選択と視聴開始の分離は M7-1。録画予約による
 * 中断予測は M7-2）。
 *
 * 「チャンネル一覧から選んでブラウザ再生、画質切り替え程度で良い」
 * （docs/frontend.md §ライブ視聴）という方針どおり、機能は絞る。プロファイル
 * （画質）を選ぶ UI は持たない --- `live.profiles` を列挙する API が無く
 * （M4-3 は OpenAPI 対象外。issue #92 着手前のコメント参照）、選択肢を UI に
 * 出すと「機能しないコントロール」になるため、既定プロファイル（先頭）に
 * 固定する。
 *
 * 選択中のチャンネルは `?networkId=&serviceId=` に持つ。SI の `serviceId` は
 * network をまたぐと一意でないため（`lib/live.ts` の `pickInitialService` の
 * doc コメント参照。issue #291）、同定には両方を使う。`networkId` を持たない
 * 旧 `?serviceId=` 単独のリンクは「その `serviceId` を持つ最初のサービス」へ
 * フォールバックする。これにより特定チャンネルへの直リンクが作れる。チャンネルを
 * 切り替えるナビゲーションは `replace` にし、ザッピングでブラウザ履歴が
 * 積み上がらないようにする。
 *
 * **「選ぶ」と「流す」を別のタップに分ける（issue #234 M7-1）。** チャンネルを
 * 選ぶこと自体は probe もセッション（チューナー確保 + ffmpeg 起動）も起こさない
 * ---「いま放送中」の表示とチャンネル種別だけを見せ、`LivePlayer` は「再生」ボタンを
 * 押すまでマウントしない。確認ダイアログは使わない --- 選択状態の画面そのものが
 * 値札であり、再生は 1 タップで足りる（ダイアログを重ねると、チューナーが有限で
 * ないデスクトップ LAN の利用者にまで摩擦を強いる。docs/frontend.md
 * §ライブ視聴「選択と視聴開始の分離」参照）。
 *
 * デバウンスは持たない。以前はザッピングのたびに probe / セッションが走っていた
 * ため「通り過ぎたチャンネル」を掴まないための緩和として 400ms のデバウンスを
 * 挟んでいたが、選択自体がコスト 0 になった今はデバウンスする対象（probe /
 * セッション開始）がチャンネル選択の瞬間には発生しない。デバウンスの存在理由が
 * 消えたので削除した（issue #234 の受け入れ 4）。
 */
export function LivePage() {
  const liveCapability = useLiveCapability()
  const site = useCurrentSite()
  const services = useListServices(site)
  const routeSearch = useRouteSearch({ from: '/live' })

  const orderedServices = useMemo(() => orderServices(unwrap(services.data) ?? []), [services.data])
  const groups = useMemo(() => groupByChannelType(orderedServices), [orderedServices])

  // **`serviceId` 単独では network をまたぐ同名 id を区別できない**ため、選択の
  // 同定は `Service` オブジェクトそのもの（`networkId` + `serviceId` の組）で行う
  // （`lib/live.ts` の `pickInitialService` の doc コメント。issue #291）。
  // `orderedServices.find((s) => s.serviceId === selectedServiceId)` のように
  // `serviceId` だけで一覧から再検索すると、同じ `serviceId` を持つ別 network の
  // サービスが先に一致し、選んだのと違う network のチャンネルを指してしまう
  // （aria-current のハイライトも 2 行に付いた）。
  const selectedService = pickInitialService(orderedServices, {
    networkId: routeSearch.networkId,
    serviceId: routeSearch.serviceId,
  })
  const selectedServiceId = selectedService?.serviceId
  const selectedKey =
    selectedService !== undefined
      ? liveServiceKey(selectedService.networkId, selectedService.serviceId)
      : undefined

  // playingKey は「再生」ボタンで明示的に視聴を始めたチャンネルの複合キー
  // （`liveServiceKey`。`serviceId` 単独ではなく `networkId` も含める --- 同じ
  // `serviceId` を持つ別 network のチャンネルへ切り替えたときに「同じチャンネル」
  // と誤認して再生状態を引き継がないため）。`null` なら未再生（選択状態）。
  // **選択（selectedKey）と一致するときだけ再生中とみなす** --- 直リンク・
  // ブックマークで来た場合（issue #234 の含むもの 3）も、チャンネル一覧で別の
  // チャンネルへ切り替えた場合も、同意はチャンネルごとに 1 回ずつ必要というのが
  // 構造で同意を取る設計の要点。
  //
  // **この判定は effect ではなくレンダー中に行う（React の「レンダー中に state を
  // 調整する」パターン）。** 当初は `useEffect(() => setIsPlaying(false),
  // [selectedServiceId])` で「選択が変わったら false に戻す」を実装していたが、
  // これは 1 コミットぶん透過的にバグる --- passive effect は子（LivePlayer）→親
  // （LivePage）の順に走るため、チャンネルを A→B へ切り替えた直後のコミットでは
  // 「`selectedServiceId` は B に変わっているが `isPlaying` はまだ前の値（true）」
  // という状態で一度レンダーされ、その 1 回だけ `<LivePlayer serviceId={B}>` が
  // マウントされて probe（チューナー確保 + ffmpeg 起動）を投げてしまう ---
  // その直後に `LivePage` 側の reset effect が走って `isPlaying` を false に戻し
  // `LivePlayer` は unmount されるが、**このコミットで実際に飛んだネットワーク要求
  // 自体は取り消せない**（`internal/streamer/live.go` のセッションは
  // `context.WithCancel(context.Background())` で回るため、クライアント側の
  // `AbortController.abort()` はセッション自体を止めない）。押していないチャンネルの
  // チューナー + ffmpeg が idle GC まで 30〜45 秒残る（レビューでの指摘。jsdom と
  // 実ブラウザ（`rokuban server --roles api` の実バイナリ）の両方で実際に
  // `/live/playlist.m3u8` への fetch が飛ぶことを確認された）。
  //
  // レンダー中に判定すれば、`selectedServiceId` が変わった**その場のレンダーで**
  // 「再生中でない」が確定するので、`<LivePlayer>` が異なる serviceId で透過的に
  // マウントされる中間コミット自体が存在しない。
  const [playingKey, setPlayingKey] = useState<string | null>(null)
  if (playingKey !== null && playingKey !== selectedKey) {
    setPlayingKey(null)
  }
  const isPlaying = playingKey !== null && playingKey === selectedKey

  // nowMs は「いま」を一定間隔で更新するティック。Date.now() を毎レンダー呼ぶだけでは
  // 再レンダーの理由にならず、番組が終わっても表示が切り替わらない。
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), nowPlayingRefetchMs)
    return () => clearInterval(id)
  }, [])

  const window_ = currentProgramWindow(nowMs)
  const nowPlayingQuery = useListPrograms(
    site,
    {
      start: window_.start,
      end: window_.end,
      // `networkId` も渡す --- `serviceId` 単独では同じ id を持つ別 network の
      // 番組も一致してしまう（issue #291 と同じ根）。
      networkId: selectedService?.networkId,
      serviceId: selectedServiceId !== undefined ? [selectedServiceId] : undefined,
    },
    { query: { enabled: selectedServiceId !== undefined } },
  )
  const nowPlaying = useMemo(() => {
    const programs: ProgramListItem[] = unwrap(nowPlayingQuery.data) ?? []
    return programs.find((p) => isAiring(p.startAt, p.endAt, nowMs))
  }, [nowPlayingQuery.data, nowMs])

  // 録画予約による中断予測（M7-2, issue #235）。
  //
  // `Reservation` はチャンネル種別を持たないので、視聴対象と同じチャンネル種別の
  // 予約を引くには EPG（`GET /api/sites/{site}/programs`）を経由して programId を
  // 突き合わせる必要がある。`serviceId` に同じ種別のサービスだけを渡すことで、
  // サーバー側に絞り込みを任せる（クライアントで全番組を持って channelType を
  // 引き直すより軽い）。
  const sameTypeServices = useMemo(
    () =>
      selectedService === undefined
        ? []
        : orderedServices.filter((s) => s.channelType === selectedService.channelType),
    [orderedServices, selectedService],
  )
  const sameTypeServiceIds = useMemo(
    () => sameTypeServices.map((s) => s.serviceId),
    [sameTypeServices],
  )
  // **クエリは `networkId` を持たない**（`serviceId` の配列だけをサーバーに渡す
  // ---複数 network の同じ種別のサービスを一度に問い合わせるので単一の
  // `networkId` では表現できない）。そのため `serviceId` が network をまたいで
  // 衝突すると（issue #291 と同じ根）、意図しない network の番組も応答に混入する
  // ---サーバーは `serviceId` だけを AND するため、選択中と別 network・別
  // channelType のサービスがたまたま同じ `serviceId` を持てば、その番組が
  // `sameTypeProgramIds` に入り込み、存在しない中断警告を出しうる。
  // `liveServiceKey`（`networkId` + `serviceId` の組）で応答側を絞り込むことで
  // 閉じる。
  const sameTypeKeys = useMemo(
    () => new Set(sameTypeServices.map((s) => liveServiceKey(s.networkId, s.serviceId))),
    [sameTypeServices],
  )
  // 窓は 10 分グリッドに丸める（`interruptionQueryWindow` 参照）。**丸めずに
  // `nowMs` から素直に組むと、`nowPlayingRefetchMs`（30 秒）ごとの tick で
  // クエリキーが毎回変わり、react-query が新しいキャッシュエントリとして扱う
  // ため、取得完了までの間 `sameTypeProgramIds` が空集合に戻って警告が一時的に
  // 消える**（実測: jsdom で 30038ms 後・実 Chromium で 28258ms 後に消失。
  // レビューでの指摘。`pages/live.test.tsx`「30 秒の tick を跨いでも警告が
  // 消えない」参照）。丸めることで 10 分間は値が変わらず、キャッシュも保たれる。
  const interruptionWindow = useMemo(() => interruptionQueryWindow(nowMs), [nowMs])
  const sameTypeProgramsQuery = useListPrograms(
    site,
    { start: interruptionWindow.start, end: interruptionWindow.end, serviceId: sameTypeServiceIds },
    { query: { enabled: sameTypeServiceIds.length > 0 } },
  )
  const sameTypeProgramIds = useMemo(
    () =>
      new Set(
        (unwrap(sameTypeProgramsQuery.data) ?? [])
          .filter((p) => sameTypeKeys.has(liveServiceKey(p.networkId, p.serviceId)))
          .map((p) => p.programId),
      ),
    [sameTypeProgramsQuery.data, sameTypeKeys],
  )
  // 予約一覧は絞り込みパラメータを持たない（`GET /api/reservations` は全サイトを
  // 返す。docs/api.md）。SSE の `reservations` トピックは既にこのクエリキーの
  // 接頭辞（`/api/reservations`）を invalidate するので、専用トピックは作らない
  // （`lib/events.ts` の `queryGroups` の reservations。容量バッジと同じ判断）。
  const reservationsQuery = useListReservations()
  const reservations = useMemo(() => unwrap(reservationsQuery.data) ?? [], [reservationsQuery.data])
  const interruptingReservation = useMemo(
    () =>
      selectedService === undefined
        ? null
        : upcomingInterruptingReservation(reservations, site, sameTypeProgramIds, nowMs),
    [reservations, site, sameTypeProgramIds, nowMs, selectedService],
  )

  return (
    <>
      <PageHeader title="ライブ" />

      {/* 能力 API の 4 状態を潰さずに出し分ける（issue #209 のレビュー指摘）。
          「無効です」と原因を名指しできるのは disabled を実際に受け取ったとき
          だけで、pending / unknown で同じ文言を出すと、live.enabled: true の
          デプロイで能力 API が瞬断しただけでも「設定が無効」と嘘をつく。 */}
      {liveCapability === 'pending' ? (
        <ListSkeleton />
      ) : liveCapability === 'unknown' ? (
        <ErrorState>ライブ視聴が利用できるかを確認できませんでした</ErrorState>
      ) : liveCapability === 'disabled' ? (
        // 主ナビからは消えているので通常ここには来ないが、直リンク・ブックマーク・
        // 戻る操作では来る（issue #209 の受け入れ 2）。何も言わずに再生を試すと
        // プレイリストが 404 になるだけで、原因（サーバー設定）に辿り着けない。
        <EmptyState>
          <p>この環境ではライブ視聴が無効です</p>
          <p className="mt-1 text-muted-foreground">
            サーバーの設定（<code>live.enabled</code>）で有効にすると使えます
          </p>
        </EmptyState>
      ) : services.isError ? (
        <ErrorState>チャンネル一覧の取得に失敗しました</ErrorState>
      ) : services.isPending ? (
        <ListSkeleton />
      ) : orderedServices.length === 0 ? (
        <EmptyState>チャンネルがありません</EmptyState>
      ) : selectedService === undefined ? (
        // pickInitialService はサービスが 1 件以上あれば必ず何かを返すので
        // ここには来ないはずだが、型上 undefined を許すため防御的に置く
        <EmptyState>チャンネルを選んでください</EmptyState>
      ) : (
        <div className="flex flex-col gap-4 p-4 lg:flex-row lg:items-start">
          <div className="flex min-w-0 flex-1 flex-col gap-2">
            {isPlaying ? (
              <LivePlayer
                site={site}
                networkId={selectedService.networkId}
                serviceId={selectedService.serviceId}
              />
            ) : (
              <LiveSelectionPreview
                serviceName={selectedService.name}
                onPlay={() =>
                  setPlayingKey(liveServiceKey(selectedService.networkId, selectedService.serviceId))
                }
              />
            )}
            <div>
              <div className="flex items-center gap-2">
                <p className="font-medium">{selectedService.name}</p>
                {/* チャンネル種別（GR/BS/CS）は選択のコストを判断する手がかり
                    （issue #234 の含むもの 1）。「チューナーが空いている」等の
                    保証はしない中立の事実表示 --- mirakc には Rokuban から見えない
                    消費者がいる（docs/data.md §6.5 と同じ下界主義。CLAUDE.md「罠」）。 */}
                <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[0.65rem] font-medium text-muted-foreground">
                  {channelTypeLabel(selectedService.channelType)}
                </span>
                {nowPlaying && <OnAirBadge />}
              </div>
              {nowPlaying ? (
                <p className="text-sm text-muted-foreground">
                  <span>
                    {formatTime(nowPlaying.startAt)}〜{formatTime(nowPlaying.endAt)}
                  </span>{' '}
                  <span>{nowPlaying.name}</span>
                </p>
              ) : !nowPlayingQuery.isPending ? (
                <p className="text-sm text-muted-foreground">いま放送中の番組の情報はありません</p>
              ) : null}
              {/* ライブ ⇄ 番組表の導線（issue #231）。番組表の `?serviceId=` は
                  `/recordings` と同じ形（複数可の配列）なので、1 局分の配列を渡す。
                  宛先は `/programs`（ホーム新設（M8-3）前は `/` だった）。 */}
              <Link
                to="/programs"
                search={{ serviceId: [selectedService.serviceId] }}
                className="mt-1 inline-block text-sm text-primary underline-offset-2 hover:underline"
              >
                この局の番組表
              </Link>
              {/* 録画予約による中断予測（issue #235 M7-2）。選択状態（値札）・
                  視聴中のどちらの画面でもこの情報欄は共通なので、1 箇所に置くだけで
                  両方の受け入れ条件（値札 / 視聴中画面への表示）を満たす。 */}
              <LiveInterruptionWarning reservation={interruptingReservation} />
            </div>
          </div>

          <nav aria-label="チャンネル一覧" className="w-full shrink-0 lg:w-72">
            <ul className="flex flex-col gap-3">
              {groups.map((group) => (
                <li key={group.channelType}>
                  <p className="px-1 pb-1 text-xs font-medium text-muted-foreground">
                    {channelTypeLabel(group.channelType)}
                  </p>
                  <ul className="flex flex-col gap-1">
                    {group.services.map((s) => (
                      <li key={`${s.networkId}-${s.serviceId}`}>
                        {/* チャンネルを選ぶこと自体はコスト 0（probe もセッションも
                            起こさない）なので、デバウンスも onClick での介入も無い
                            --- 通常のクリックナビゲーションのまま（issue #234）。
                            `replace` はザッピングでブラウザ履歴が積み上がらない
                            ようにするため。 */}
                        <Link
                          to="/live"
                          search={{ networkId: s.networkId, serviceId: s.serviceId }}
                          replace
                          // ハイライト・aria-current の同定も `networkId` +
                          // `serviceId` の組で行う --- `serviceId` 単独では、同じ
                          // `serviceId` を持つ別 network のサービスにも付いてしまう
                          // （issue #291。この一覧が実際に GR/BS/CS を混ぜて返す
                          // 構成では 2 行に aria-current が付いていた）。
                          aria-current={
                            liveServiceKey(s.networkId, s.serviceId) === selectedKey
                              ? 'page'
                              : undefined
                          }
                          className={cn(
                            'flex min-h-11 w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm transition-colors hover:bg-muted',
                            liveServiceKey(s.networkId, s.serviceId) === selectedKey &&
                              'bg-muted font-medium',
                          )}
                        >
                          {s.channelType === 'GR' && s.remoteControlKeyId > 0 && (
                            <span className="shrink-0 rounded bg-muted px-1 text-[10px] text-muted-foreground">
                              {s.remoteControlKeyId}
                            </span>
                          )}
                          <span className="truncate">{s.name}</span>
                        </Link>
                      </li>
                    ))}
                  </ul>
                </li>
              ))}
            </ul>
          </nav>
        </div>
      )}
    </>
  )
}

/**
 * LiveSelectionPreview は「選択」段階の表示（issue #234 M7-1）。
 *
 * `LivePlayer` と同じ寸法（`aspect-video` の黒地）にして、再生ボタンを押した
 * 瞬間のレイアウトシフトを避ける。probe もセッションもここでは起こさない ---
 * 「再生」ボタンを押した後に呼び出し側が `LivePlayer` をマウントするまで、
 * ネットワーク要求は一切発生しない（チャンネル切り替えの中間コミットも含む。
 * `playingKey` の判定をレンダー中に落とす必要があった理由も同じ ---
 * `LivePage` のコメント参照）。`pages/live.test.tsx`「再生中に別チャンネルへ
 * 切り替えると選択状態に戻る（同意はチャンネルごとに必要）」の
 * `playlistFetchCallCount()` の assertion と `web/e2e/live.mjs` ⓪' で実測済み。
 */
function LiveSelectionPreview({
  serviceName,
  onPlay,
}: {
  serviceName: string
  onPlay: () => void
}) {
  return (
    <div className="relative flex aspect-video w-full max-w-3xl items-center justify-center rounded bg-black">
      <Button type="button" size="lg" aria-label={`${serviceName}を再生`} onClick={onPlay}>
        <Play data-icon="inline-start" />
        再生
      </Button>
    </div>
  )
}

/**
 * OnAirBadge は「いま電波に乗っている」ことを示すバッジ（M4-4 のライブ視聴専用）。
 *
 * **走査線は 3 箇所限定の使用箇所の 1 つ**（ON AIR。docs/frontend/design.md
 * 「走査線は 3 箇所限定」）。タリーレッドの塗り（`tally-scanlines` /
 * `text-tally-foreground`）は録画中バッジと同じ組み合わせをベースにしており、
 * AA を満たすことを確認済み（`e2e/design.mjs`）。選択中チャンネルに `nowPlaying`
 * （いま放送中の番組）があるときだけ呼び出し側が描画する。
 */
function OnAirBadge() {
  return (
    <span className="tally-scanlines shrink-0 rounded px-1.5 py-0.5 text-[0.65rem] font-medium text-tally-foreground">
      ON AIR
    </span>
  )
}
