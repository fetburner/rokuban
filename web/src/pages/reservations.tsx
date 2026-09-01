import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import { useMemo } from 'react'

import { useListCapacityOverages, useListReservations, type Reservation } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { CapacityShortfallBadge } from '@/components/capacity-shortfall-badge'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { ReservationSkipBadge } from '@/components/reservation-skip-reason'
import { Chip } from '@/components/ui/chip'
import { coveringWindow } from '@/lib/capacity'
import { formatDateTime, formatDuration } from '@/lib/format'
import {
  reservationNeedsAttention,
  stateLabels,
  type ReservationsPageSearch,
} from '@/lib/reservation-labels'
import { cn } from '@/lib/utils'

export function ReservationsPage() {
  const search = useRouteSearch({ from: '/reservations' })
  const navigate = useNavigate()
  const query = useListReservations()
  const reservations = useMemo(() => unwrap(query.data) ?? [], [query.data])

  // 一覧に出ている予約すべてを覆う窓で超過区間を訊く。窓を固定幅にすると、
  // その外に出た予約のバッジが黙って消える。予約が無ければ問い合わせない
  // （窓が null。パラメータは必須なので値は入れるが enabled で止める）。
  const listedWindow = useMemo(() => coveringWindow(reservations), [reservations])
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(listedWindow?.startMs ?? 0).toISOString(),
      end: new Date(listedWindow?.endMs ?? 0).toISOString(),
    },
    { query: { enabled: listedWindow !== null } },
  )
  // 取得の失敗・未完了は「バッジが出ない」に落ちる。元から沈黙は「収まる」ことの
  // 保証ではないので（docs/data.md §6.5）、予約一覧そのものをエラーにはしない
  const overages = useMemo(() => unwrap(overagesQuery.data) ?? [], [overagesQuery.data])
  const attentionReady = listedWindow === null || !overagesQuery.isPending
  const attentionReservations = useMemo(
    () => reservations.filter((reservation) => reservationNeedsAttention(reservation, overages)),
    [overages, reservations],
  )
  const visibleReservations =
    search.only === 'attention' ? attentionReservations : reservations
  const selectOnly = (only: ReservationsPageSearch['only']) => {
    void navigate({ to: '/reservations', search: { only }, replace: true })
  }

  return (
    <>
      <PageHeader title="予約">
        {!query.isPending && !query.isError && attentionReady && (
          <div role="group" aria-label="予約の絞り込み" className="flex flex-wrap gap-2 px-4 pb-3">
            <Chip active={search.only === undefined} onClick={() => selectOnly(undefined)}>
              すべて（{reservations.length}）
            </Chip>
            {attentionReservations.length > 0 && (
              <Chip active={search.only === 'attention'} onClick={() => selectOnly('attention')}>
                要確認（{attentionReservations.length}）
              </Chip>
            )}
          </div>
        )}
      </PageHeader>

      <PageContent>
        {query.isError ? (
        <ErrorState onRetry={() => void query.refetch()}>予約の取得に失敗しました</ErrorState>
      ) : query.isPending || (search.only === 'attention' && !attentionReady) ? (
        <ListSkeleton />
      ) : visibleReservations.length === 0 ? (
        <EmptyState>
          {search.only === 'attention' ? '確認が要る予約はありません' : '予約がありません'}
        </EmptyState>
      ) : (
        <ul>
          {visibleReservations.map((r) => {
            // 行本体のリンクの accessible name。子要素を持たない絶対配置の
            // リンク（下記）にするため、children から自動で組めない分を明示する。
            // 採否の基準は「行を一意に識別できるか」--- 局名は同タイトル・別局
            // （同名ニュースの裏かぶり）を分ける唯一の情報なので、時刻・尺・state と
            // 並べてここに入れる（issue #302）。skip / 容量バッジの文言は識別情報
            // ではなく、かつ行の中の通常フロー要素として残ってブラウズ（矢印キー
            // 走査）では読めるので、1 つの長いリンク名に押し込む必要はない。
            const rowLabel = [
              r.title || '（番組名なし）',
              r.serviceName,
              formatDateTime(r.startAt),
              formatDuration(r.durationMs),
              r.state === 'active' ? null : stateLabels[r.state],
            ]
              // 空文字も落とす（`serviceName` は API では required だが空文字を
              // 禁じてはいないので、裸の区切りが名前に残らないようにする）
              .filter((s): s is string => s !== null && s !== '')
              .join(' ')

            return (
              <li
                key={r.id}
                className="relative flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5 hover:bg-muted/50"
              >
                {/* 行本体のリンクは絶対配置で行全体を覆う「面」にし、通常フローから
                    外す（`position: relative` を li に置いて containing block に
                    する）。**入れ子を解く方向を反転させた**（issue #233 のレビュー
                    指摘）--- 最初の実装は容量バッジを行本体リンクの外（兄弟）へ
                    移して <a> の入れ子を消したが、それは配置文法（バッジの位置・
                    chevron の終端性・モバイルでのタイトル幅）を壊した。壊れていた
                    のは「バッジが行の中にある」ことではなく「行本体そのものが
                    子要素を抱えた <a> で、バッジという別の対話要素と競合する」こと
                    --- 行の中身（タイトル・バッジ列・chevron）は元の配置のまま
                    通常フローに残し、行本体のリンクだけを見えない全面カバーの層に
                    退避させる。
                    子要素を持たないため accessible name は aria-label で渡す
                    （children から計算できない）。 */}
                <Link
                  to="/reservations/$site/$programId"
                  params={{ site: r.site, programId: String(r.programId) }}
                  aria-label={rowLabel}
                  className="absolute inset-0"
                />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-base">{r.title || '（番組名なし）'}</div>
                  <div
                    data-testid="reservation-secondary"
                    className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground"
                  >
                    <span className="shrink-0">{r.serviceName}</span>
                    <span className="shrink-0">{formatDateTime(r.startAt)}</span>
                    <span className="shrink-0">{formatDuration(r.durationMs)}</span>
                    <StateBadge state={r.state} />
                    <ReservationSkipBadge reservation={r} />
                    {/* 容量バッジは番組表への別の Link（issue #233 M6-5）。
                        バッジ自身が `relative z-10` を持ち、行全面の
                        `absolute inset-0` リンクより手前で 24px の当たり判定を保つ。
                        判定はサイトごとに独立している（docs/data.md §6.5）ので
                        予約自身の site を渡す。定数を持たない。 */}
                    <CapacityShortfallBadge
                      overages={overages}
                      site={r.site}
                      startMs={new Date(r.startAt).getTime()}
                      endMs={new Date(r.startAt).getTime() + r.durationMs}
                    />
                  </div>
                </div>
                <ChevronRight
                  data-testid="reservation-chevron"
                  className="size-4 shrink-0 text-muted-foreground"
                />
              </li>
            )
          })}
        </ul>
      )}
      </PageContent>
    </>
  )
}

/**
 * StateBadge の `detached` の文字色は `text-foreground`（bg-muted 小バッジの
 * 合成後コントラスト対策。docs/frontend/design.md「コントラストは毎回測る」）。
 */
function StateBadge({ state }: { state: Reservation['state'] }) {
  if (state === 'active') return null
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-xs',
        state === 'orphaned' ? 'bg-destructive/10 text-destructive' : 'bg-muted text-foreground',
      )}
    >
      {stateLabels[state]}
    </span>
  )
}
