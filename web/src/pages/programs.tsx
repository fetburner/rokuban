import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'

import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { ProgramRow } from '@/components/program-row'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import {
  listPrograms,
  useCreateReservation,
  useDeleteReservation,
  useListReservations,
  useListServices,
  type ProgramListItem,
  type Service,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { dayKey, formatDate } from '@/lib/format'
import { cn } from '@/lib/utils'

/**
 * windowHours は 1 回のスクロールステップで取得する時間窓の幅。
 *
 * API はページネーショントークンを持たず、時間窓そのものがカーソルになる。
 * 「次のページ」= 前回の end を start にした次の窓。
 */
const windowHours = 6

/** 番組リストで一度に遡れる上限。EPG のローリングウィンドウ（8 日）に合わせる。 */
const maxWindows = (8 * 24) / windowHours

function windowAt(step: number): { start: Date; end: Date } {
  // 「今」ではなく直近の時刻境界を起点にすることで、再取得のたびに
  // クエリキーが変わって無限に fetch し続けるのを防ぐ。
  const base = new Date()
  base.setMinutes(0, 0, 0)
  const start = new Date(base.getTime() + step * windowHours * 3600_000)
  const end = new Date(start.getTime() + windowHours * 3600_000)
  return { start, end }
}

export function ProgramsPage() {
  const [serviceFilter, setServiceFilter] = useState<number | null>(null)

  const services = useListServices()
  const reservations = useListReservations()

  const query = useInfiniteQuery({
    queryKey: ['/api/programs', 'infinite', serviceFilter],
    initialPageParam: 0,
    queryFn: async ({ pageParam }) => {
      const { start, end } = windowAt(pageParam)
      const res = await listPrograms({
        start: start.toISOString(),
        end: end.toISOString(),
        ...(serviceFilter === null ? {} : { serviceId: serviceFilter }),
      })
      return { step: pageParam, programs: unwrap(res) ?? [] }
    },
    getNextPageParam: (last) => (last.step + 1 < maxWindows ? last.step + 1 : undefined),
  })

  // 窓は開区間なので境界をまたぐ番組が隣接する 2 つの窓に現れる。programId で潰す。
  const programs = useMemo(() => {
    const seen = new Map<number, ProgramListItem>()
    for (const page of query.data?.pages ?? []) {
      for (const p of page.programs) {
        if (!seen.has(p.programId)) seen.set(p.programId, p)
      }
    }
    return [...seen.values()].sort(
      (a, b) => new Date(a.startAt).getTime() - new Date(b.startAt).getTime(),
    )
  }, [query.data])

  const serviceById = useMemo(() => {
    const map = new Map<number, Service>()
    for (const s of unwrap(services.data) ?? []) map.set(s.serviceId, s)
    return map
  }, [services.data])

  // 予約状態は番組とは別クエリで取り、クライアント側で結合する。
  // 予約は頻繁に変わり番組はほとんど変わらないので、キャッシュの寿命を分ける。
  const reservationByProgramId = useMemo(() => {
    const map = new Map<number, number>()
    for (const r of unwrap(reservations.data) ?? []) map.set(r.programId, r.id)
    return map
  }, [reservations.data])

  // 番組が 1 件でもあるサービスだけをチップに出す（issue #17 の S3）。
  // マルチ編成のないサブサービスは番組を持たないので自動的に消える。
  const filterableServices = useMemo(() => {
    const withPrograms = new Set(programs.map((p) => p.serviceId))
    return (unwrap(services.data) ?? []).filter(
      (s) => withPrograms.has(s.serviceId) || s.serviceId === serviceFilter,
    )
  }, [programs, services.data, serviceFilter])

  return (
    <>
      <PageHeader title="番組">
        <ServiceChips
          services={filterableServices}
          selected={serviceFilter}
          onSelect={setServiceFilter}
        />
      </PageHeader>

      {query.isError ? (
        <ErrorState>番組の取得に失敗しました</ErrorState>
      ) : query.isPending ? (
        <ListSkeleton />
      ) : programs.length === 0 ? (
        <EmptyState>この時間帯の番組がありません</EmptyState>
      ) : (
        <ProgramList
          programs={programs}
          serviceById={serviceById}
          reservationByProgramId={reservationByProgramId}
        />
      )}

      {query.hasNextPage && !query.isPending && (
        <div className="px-4 py-6">
          <Button
            variant="outline"
            size="lg"
            className="w-full"
            disabled={query.isFetchingNextPage}
            onClick={() => void query.fetchNextPage()}
          >
            {query.isFetchingNextPage ? '読み込み中…' : 'さらに読み込む'}
          </Button>
        </div>
      )}
    </>
  )
}

function ServiceChips({
  services,
  selected,
  onSelect,
}: {
  services: Service[]
  selected: number | null
  onSelect: (serviceId: number | null) => void
}) {
  return (
    <div className="flex gap-2 overflow-x-auto px-4 pb-3">
      <Chip active={selected === null} onClick={() => onSelect(null)}>
        すべて
      </Chip>
      {services.map((s) => (
        <Chip
          key={`${s.networkId}-${s.serviceId}`}
          active={selected === s.serviceId}
          onClick={() => onSelect(s.serviceId)}
        >
          {s.name}
        </Chip>
      ))}
    </div>
  )
}

function Chip({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        'shrink-0 rounded-full border px-3 py-1.5 text-xs transition-colors',
        active
          ? 'border-primary bg-primary text-primary-foreground'
          : 'border-border text-muted-foreground hover:bg-muted',
      )}
    >
      {children}
    </button>
  )
}

function ProgramList({
  programs,
  serviceById,
  reservationByProgramId,
}: {
  programs: ProgramListItem[]
  serviceById: Map<number, Service>
  reservationByProgramId: Map<number, number>
}) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const createReservation = useCreateReservation()
  const deleteReservation = useDeleteReservation()

  const invalidateReservations = () => {
    void queryClient.invalidateQueries({ queryKey: ['/api/reservations'] })
  }

  const cancel = (reservationId: number) => {
    deleteReservation.mutate(
      { id: reservationId },
      {
        onSuccess: () => {
          invalidateReservations()
          toast({ message: '予約を取消しました' })
        },
        onError: () => toast({ message: '予約の取消に失敗しました' }),
      },
    )
  }

  const reserve = (program: ProgramListItem) => {
    createReservation.mutate(
      {
        data: {
          programId: program.programId,
          title: program.name,
          startAt: program.startAt,
          durationMs: program.durationMs,
        },
      },
      {
        onSuccess: (created) => {
          invalidateReservations()
          const reservation = unwrap(created)
          // 確認ダイアログを挟まない代わりに、直後に取り返せるようにする
          toast({
            message: `予約しました: ${program.name}`,
            action: reservation
              ? { label: '取消', onClick: () => cancel(reservation.id) }
              : undefined,
          })
        },
        onError: () => toast({ message: '予約に失敗しました' }),
      },
    )
  }

  let lastDay = ''

  return (
    <ul>
      {programs.map((program) => {
        const day = dayKey(program.startAt)
        const showDateHeader = day !== lastDay
        lastDay = day
        const reservationId = reservationByProgramId.get(program.programId)

        return (
          <li key={program.programId}>
            {showDateHeader && (
              <h2 className="sticky top-[6.5rem] z-[5] border-y border-border bg-muted/80 px-4 py-1.5 text-xs font-medium text-muted-foreground backdrop-blur">
                {formatDate(program.startAt)}
              </h2>
            )}
            <ProgramRow
              program={program}
              serviceName={serviceById.get(program.serviceId)?.name}
              reservationId={reservationId}
              pending={createReservation.isPending || deleteReservation.isPending}
              onReserve={() => reserve(program)}
              onCancel={() => reservationId && cancel(reservationId)}
            />
          </li>
        )
      })}
    </ul>
  )
}
