import { Link, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useMemo, useState } from 'react'

import { useListPrograms, useListServices, type ProgramListItem, type Service } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { LivePlayer } from '@/components/live-player'
import { currentProgramWindow, pickInitialServiceId } from '@/lib/live'
import { orderServices } from '@/lib/epg-grid'
import { formatTime, isAiring } from '@/lib/format'
import { DEFAULT_SITE } from '@/lib/site'
import { cn } from '@/lib/utils'

/**
 * nowPlayingRefetchMs は「いま放送中」表示の再取得間隔。
 *
 * 番組が終わって次の番組に切り替わるタイミングを追いかけるためのポーリング。
 * SSE の `programs` 相当のトピックは無い（EPG 更新は元々 watcher の定期
 * ジョブなので、番組の切り替わり自体はサーバー側で NOTIFY されない）ため、
 * レベルトリガーの精神（不変条件 5）に沿って一定間隔で再取得する。
 */
const nowPlayingRefetchMs = 30_000

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
 * リンクは `replace` にし、ザッピングでブラウザ履歴が積み上がらないようにする。
 */
export function LivePage() {
  const services = useListServices(DEFAULT_SITE)
  const routeSearch = useRouteSearch({ from: '/live' })

  const orderedServices = useMemo(() => orderServices(unwrap(services.data) ?? []), [services.data])
  const groups = useMemo(() => groupByChannelType(orderedServices), [orderedServices])

  const selectedServiceId = pickInitialServiceId(orderedServices, routeSearch.serviceId)
  const selectedService = orderedServices.find((s) => s.serviceId === selectedServiceId)

  // nowMs は「いま」を一定間隔で更新するティック。Date.now() を毎レンダー呼ぶだけでは
  // 再レンダーの理由にならず、番組が終わっても表示が切り替わらない。
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), nowPlayingRefetchMs)
    return () => clearInterval(id)
  }, [])

  const window_ = currentProgramWindow(nowMs)
  const nowPlayingQuery = useListPrograms(
    DEFAULT_SITE,
    { start: window_.start, end: window_.end, serviceId: selectedServiceId !== undefined ? [selectedServiceId] : undefined },
    { query: { enabled: selectedServiceId !== undefined, refetchInterval: nowPlayingRefetchMs } },
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
            <LivePlayer site={DEFAULT_SITE} serviceId={selectedService.serviceId} />
            <div>
              <p className="font-medium">{selectedService.name}</p>
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
                          aria-current={s.serviceId === selectedService.serviceId ? 'true' : undefined}
                          className={cn(
                            'flex min-h-11 w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm transition-colors hover:bg-muted',
                            s.serviceId === selectedService.serviceId && 'bg-muted font-medium',
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
