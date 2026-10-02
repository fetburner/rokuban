import type { CapacityOverage } from '@/api/generated'

export type HomeTimelineKind = 'finished' | 'recording' | 'failed' | 'reservation'

/** 1 件の既存録画または予約を、時間軸に置くための値。日時は epoch ms。 */
export type HomeTimelineEvent = {
  key: string
  site: string
  channelType: string
  startMs: number
  endMs: number
  kind: HomeTimelineKind
  title: string
  /** 完了録画にドロップ/エラー/スクランブルが記録されている。 */
  hasDrop: boolean
  /** 個々の時間軸ブロックから既存詳細へ移るための宛先。 */
  href: { to: '/recordings/$id'; id: number } | {
    to: '/reservations/$site/$programId'
    site: string
    programId: number
  }
}

export type PackedHomeTimelineEvent = HomeTimelineEvent & { track: number }

export type HomeTimelineOverageSpan = {
  key: string
  site: string
  channelType: string
  startMs: number
  endMs: number
  shortfall: number
}

export type HomeTimelineRow = {
  key: string
  site: string
  channelType: string
  events: PackedHomeTimelineEvent[]
  overages: HomeTimelineOverageSpan[]
  trackCount: number
}

const CHANNEL_TYPE_ORDER = ['GR', 'BS', 'CS', 'SKY'] as const

/**
 * packHomeTimelineEvents は開始順に並べ、終了済みの最初の段へ項目を置く。
 * 等しい境界（end === next.start）は交差しない半開区間として同じ段を再利用する。
 * 段は重なりを見やすくするためだけの配置で、チューナー割当を表さない。
 */
export function packHomeTimelineEvents(
  events: readonly HomeTimelineEvent[],
): PackedHomeTimelineEvent[] {
  const ordered = events
    .filter(
      (event) =>
        Number.isFinite(event.startMs) &&
        Number.isFinite(event.endMs) &&
        event.endMs > event.startMs,
    )
    .slice()
    .sort(
      (left, right) =>
        left.startMs - right.startMs ||
        left.endMs - right.endMs ||
        left.key.localeCompare(right.key),
    )

  const trackEnds: number[] = []
  return ordered.map((event) => {
    let track = trackEnds.findIndex((endMs) => endMs <= event.startMs)
    if (track === -1) track = trackEnds.length
    trackEnds[track] = event.endMs
    return { ...event, track }
  })
}

/**
 * buildHomeTimelineRows は観測した項目を site × channelType で分ける。
 * 容量超過は `jammedTypes` のすべてに置き、site も一致する行だけへ載せる。
 * Overages は event に付与しないため、個別予約を勝者/敗者として描かない。
 */
export function buildHomeTimelineRows(
  events: readonly HomeTimelineEvent[],
  capacityOverages: readonly CapacityOverage[],
): HomeTimelineRow[] {
  const eventGroups = new Map<string, HomeTimelineEvent[]>()
  const rowIdentity = (site: string, channelType: string) => `${site}\u0000${channelType}`

  for (const event of events) {
    if (!isValidSpan(event.startMs, event.endMs)) continue
    const key = rowIdentity(event.site, event.channelType)
    const group = eventGroups.get(key)
    if (group === undefined) eventGroups.set(key, [event])
    else group.push(event)
  }

  const overagesByRow = new Map<string, HomeTimelineOverageSpan[]>()
  for (const overage of capacityOverages) {
    const startMs = new Date(overage.startAt).getTime()
    const endMs = new Date(overage.endAt).getTime()
    if (!isValidSpan(startMs, endMs)) continue
    for (const channelType of overage.jammedTypes) {
      const key = rowIdentity(overage.site, channelType)
      const spans = overagesByRow.get(key)
      const span = {
        key: `${overage.site}:${overage.startAt}:${overage.endAt}:${channelType}`,
        site: overage.site,
        channelType,
        startMs,
        endMs,
        shortfall: overage.shortfall,
      }
      if (spans === undefined) overagesByRow.set(key, [span])
      else spans.push(span)
    }
  }

  const keys = new Set([...eventGroups.keys(), ...overagesByRow.keys()])
  return [...keys]
    .map((key) => {
      const separator = key.indexOf('\u0000')
      const site = key.slice(0, separator)
      const channelType = key.slice(separator + 1)
      const rowEvents = packHomeTimelineEvents(eventGroups.get(key) ?? [])
      const overages = overagesByRow.get(key) ?? []
      return {
        key,
        site,
        channelType,
        events: rowEvents,
        overages,
        trackCount: Math.max(1, ...rowEvents.map((event) => event.track + 1)),
      }
    })
    .sort((left, right) => {
      const siteOrder = left.site.localeCompare(right.site)
      if (siteOrder !== 0) return siteOrder
      const leftIndex = CHANNEL_TYPE_ORDER.indexOf(left.channelType as (typeof CHANNEL_TYPE_ORDER)[number])
      const rightIndex = CHANNEL_TYPE_ORDER.indexOf(right.channelType as (typeof CHANNEL_TYPE_ORDER)[number])
      return (leftIndex < 0 ? CHANNEL_TYPE_ORDER.length : leftIndex) -
        (rightIndex < 0 ? CHANNEL_TYPE_ORDER.length : rightIndex) ||
        left.channelType.localeCompare(right.channelType)
    })
}

export function homeTimelineChannelLabel(channelType: string): string {
  switch (channelType) {
    case 'GR':
      return '地デジ'
    case 'BS':
      return 'BS'
    case 'CS':
      return 'CS'
    case 'SKY':
      return 'スカパー!'
    default:
      return channelType
  }
}

function isValidSpan(startMs: number, endMs: number): boolean {
  return Number.isFinite(startMs) && Number.isFinite(endMs) && endMs > startMs
}
