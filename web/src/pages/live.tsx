import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useMemo, useRef, useState } from 'react'

import { useListPrograms, useListServices, type ProgramListItem, type Service } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { LivePlayer } from '@/components/live-player'
import { currentProgramWindow, pickInitialServiceId } from '@/lib/live'
import { orderServices } from '@/lib/epg-grid'
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
 * channelSwitchDebounceMs はチャンネル切り替えのデバウンス幅。
 *
 * 実配値（`live.idle_timeout` 既定 30 秒 / `live.max_sessions` 既定 4）では、
 * ザッピングのたびに前のセッションがすぐには解放されない（レビュー #190 の
 * 指摘。docs/frontend.md の該当節参照）。デバウンスは「本当に見たいチャンネル
 * が決まるまで、途中で通り過ぎたチャンネルの probe / セッションを起こさない」
 * ための緩和であり、idle GC の遅延自体は変えない。サーバー側の即時解放 API は
 * #191 へ切り出し済みで、この PR（UI のみ）の範囲外
 */
const channelSwitchDebounceMs = 400

type ChannelGroup = { channelType: string; services: Service[] }

/** channelTypeLabels は channelType の日本語表記（components/channel-picker.tsx と同じ表）。 */
const channelTypeLabels: Record<string, string> = {
  GR: '地上波',
  BS: 'BS',
  CS: 'CS',
  SKY: 'SKY',
}

function channelTypeLabel(channelType: string): string {
  return channelTypeLabels[channelType] ?? channelType
}

/** groupByChannelType は種別ごとの連続した塊にまとめる（orderServices 済みの入力を前提）。 */
function groupByChannelType(ordered: readonly Service[]): ChannelGroup[] {
  const groups: ChannelGroup[] = []
  for (const service of ordered) {
    const last = groups[groups.length - 1]
    if (last && last.channelType === service.channelType) {
      last.services.push(service)
    } else {
      groups.push({ channelType: service.channelType, services: [service] })
    }
  }
  return groups
}

/**
 * LivePage はライブ視聴画面（M4-4）。
 *
 * 「チャンネル一覧から選んでブラウザ再生、画質切り替え程度で良い」
 * （docs/frontend.md §ライブ視聴）という方針どおり、機能は絞る。プロファイル
 * （画質）を選ぶ UI は持たない --- `live.profiles` を列挙する API が無く
 * （M4-3 は OpenAPI 対象外。issue #92 着手前のコメント参照）、選択肢を UI に
 * 出すと「機能しないコントロール」になるため、既定プロファイル（先頭）に
 * 固定する。
 *
 * 選択中のチャンネルは `?serviceId=` に持つ（`pages/search.tsx` の `?ruleId` と
 * 同じ形）。これにより特定チャンネルへの直リンクが作れる。チャンネルを切り替える
 * ナビゲーションは `replace` にし、ザッピングでブラウザ履歴が積み上がらないように
 * する（実際のナビゲーションは `channelSwitchDebounceMs` だけデバウンスする。
 * 下記 `selectChannel` 参照）。
 */
export function LivePage() {
  const site = useCurrentSite()
  const services = useListServices(site)
  const routeSearch = useRouteSearch({ from: '/live' })
  const navigate = useNavigate()

  const orderedServices = useMemo(() => orderServices(unwrap(services.data) ?? []), [services.data])
  const groups = useMemo(() => groupByChannelType(orderedServices), [orderedServices])

  const selectedServiceId = pickInitialServiceId(orderedServices, routeSearch.serviceId)
  const selectedService = orderedServices.find((s) => s.serviceId === selectedServiceId)

  // pendingServiceId はチャンネル一覧のハイライト専用。LivePlayer に渡す実際の
  // selectedServiceId（= URL の ?serviceId=）とは切り離す --- クリック直後の
  // 見た目の反応と、実際に probe / セッションを起こすタイミング（デバウンス後）を
  // 分けるため。URL 側（selectedServiceId）が変わったら追従させる
  const [pendingServiceId, setPendingServiceId] = useState(selectedServiceId)
  useEffect(() => {
    setPendingServiceId(selectedServiceId)
  }, [selectedServiceId])

  const debounceTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (debounceTimer.current !== null) clearTimeout(debounceTimer.current)
    },
    [],
  )

  /**
   * selectChannel はチャンネル切り替えをデバウンスする。
   *
   * ハイライト（`pendingServiceId`）は即座に切り替えて操作感を保ちつつ、実際の
   * ナビゲーション（= `LivePlayer` への `serviceId` 反映 = probe / セッション
   * 開始）は `channelSwitchDebounceMs` の間に別のチャンネルが押されなかった
   * ときだけ行う（最後の選択だけが勝つ latest-wins）。連続してザップしても、
   * 通り過ぎたチャンネルの分だけセッションが積まれない
   */
  function selectChannel(serviceId: number) {
    setPendingServiceId(serviceId)
    if (debounceTimer.current !== null) clearTimeout(debounceTimer.current)
    debounceTimer.current = setTimeout(() => {
      debounceTimer.current = null
      void navigate({ to: '/live', search: { serviceId }, replace: true })
    }, channelSwitchDebounceMs)
  }

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
    { start: window_.start, end: window_.end, serviceId: selectedServiceId !== undefined ? [selectedServiceId] : undefined },
    { query: { enabled: selectedServiceId !== undefined } },
  )
  const nowPlaying = useMemo(() => {
    const programs: ProgramListItem[] = unwrap(nowPlayingQuery.data) ?? []
    return programs.find((p) => isAiring(p.startAt, p.endAt, nowMs))
  }, [nowPlayingQuery.data, nowMs])

  return (
    <>
      <PageHeader title="ライブ" />

      {services.isError ? (
        <ErrorState>チャンネル一覧の取得に失敗しました</ErrorState>
      ) : services.isPending ? (
        <ListSkeleton />
      ) : orderedServices.length === 0 ? (
        <EmptyState>チャンネルがありません</EmptyState>
      ) : selectedService === undefined ? (
        // pickInitialServiceId はサービスが 1 件以上あれば必ず何かを返すので
        // ここには来ないはずだが、型上 undefined を許すため防御的に置く
        <EmptyState>チャンネルを選んでください</EmptyState>
      ) : (
        <div className="flex flex-col gap-4 p-4 lg:flex-row lg:items-start">
          <div className="flex min-w-0 flex-1 flex-col gap-2">
            <LivePlayer
              site={site}
              networkId={selectedService.networkId}
              serviceId={selectedService.serviceId}
            />
            <div>
              <div className="flex items-center gap-2">
                <p className="font-medium">{selectedService.name}</p>
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
                        <Link
                          to="/live"
                          search={{ serviceId: s.serviceId }}
                          replace
                          onClick={(e) => {
                            // 修飾クリック（新規タブ等）・中クリック・右クリックは
                            // ブラウザの既定動作に任せる。デバウンスするのは
                            // 「その場で選び直す」通常のクリックだけ
                            if (e.defaultPrevented || e.button !== 0) return
                            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
                            e.preventDefault()
                            selectChannel(s.serviceId)
                          }}
                          aria-current={s.serviceId === pendingServiceId ? 'page' : undefined}
                          className={cn(
                            'flex min-h-11 w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm transition-colors hover:bg-muted',
                            s.serviceId === pendingServiceId && 'bg-muted font-medium',
                          )}
                        >
                          {s.channelType === 'GR' && s.remoteControlKeyId > 0 && (
                            <span className="shrink-0 rounded bg-muted px-1 text-[10px] tabular-nums text-muted-foreground">
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
