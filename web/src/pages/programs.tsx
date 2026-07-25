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

/** EPG のローリングウィンドウ（8 日）に合わせた、日付選択の選択肢の数。 */
const selectableDays = 8

/**
 * dayOrigin は日付選択に対応する時間窓の起点を返す。
 *
 * `dayOffset` が null なら「今」（時刻境界に切り捨てる。窓を時刻境界に揃えるため）。
 * 数値ならその日数だけ先の 0 時。日付を選べば「さらに読み込む」を何度も押さずに
 * 先の日付へ跳べる。
 */
function dayOrigin(dayOffset: number | null): Date {
  const origin = new Date()
  if (dayOffset === null) {
    origin.setMinutes(0, 0, 0)
    return origin
  }
  origin.setDate(origin.getDate() + dayOffset)
  origin.setHours(0, 0, 0, 0)
  return origin
}

/** windowEndOfDay は選択した日の終わり（翌 0 時）を返す。 */
function endOfSelection(dayOffset: number | null): Date {
  const origin = dayOrigin(dayOffset)
  const end = new Date(origin)
  if (dayOffset === null) {
    // 「今」は日付をまたいで先まで読めるようにする
    end.setDate(end.getDate() + selectableDays)
    end.setHours(0, 0, 0, 0)
    return end
  }
  end.setDate(end.getDate() + 1)
  return end
}

export function ProgramsPage() {
  const [serviceFilter, setServiceFilter] = useState<number | null>(null)
  const [dayOffset, setDayOffset] = useState<number | null>(null)

  const services = useListServices()
  const reservations = useListReservations()

  // 起点は state から決める。queryKey に入るので、日付を変えるとページが
  // 積み直され、キャッシュ済みのページが古い窓のまま再利用されることもない。
  const originMs = dayOrigin(dayOffset).getTime()
  const limitMs = endOfSelection(dayOffset).getTime()
  const maxWindows = Math.max(1, Math.ceil((limitMs - originMs) / (windowHours * 3600_000)))

  const query = useInfiniteQuery({
    queryKey: ['/api/programs', 'infinite', serviceFilter, originMs, limitMs],
    initialPageParam: 0,
    queryFn: async ({ pageParam }) => {
      const start = new Date(originMs + pageParam * windowHours * 3600_000)
      const end = new Date(Math.min(start.getTime() + windowHours * 3600_000, limitMs))
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
        <DayChips selected={dayOffset} onSelect={setDayOffset} />
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

function DayChips({
  selected,
  onSelect,
}: {
  selected: number | null
  onSelect: (dayOffset: number | null) => void
}) {
  const days = Array.from({ length: selectableDays }, (_, offset) => offset)

  return (
    <div className="flex gap-2 overflow-x-auto px-4 pb-2">
      <Chip active={selected === null} onClick={() => onSelect(null)}>
        今
      </Chip>
      {days.map((offset) => (
        <Chip key={offset} active={selected === offset} onClick={() => onSelect(offset)}>
          {formatDate(dayOrigin(offset).toISOString())}
        </Chip>
      ))}
    </div>
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

  // mutation の isPending は全行で共有されるため、操作中の番組だけを覚えておく。
  // これがないと 1 件予約する間にリスト全行のボタンが無効化される。
  const [busyProgramIds, setBusyProgramIds] = useState<ReadonlySet<number>>(new Set())

  const setBusy = (programId: number, busy: boolean) => {
    setBusyProgramIds((current) => {
      const next = new Set(current)
      if (busy) next.add(programId)
      else next.delete(programId)
      return next
    })
  }

  const invalidateReservations = () => {
    void queryClient.invalidateQueries({ queryKey: ['/api/reservations'] })
  }

  const cancel = (programId: number, reservationId: number) => {
    setBusy(programId, true)
    deleteReservation.mutate(
      { id: reservationId },
      {
        onSuccess: () => {
          invalidateReservations()
          toast({ message: '予約を取消しました' })
        },
        onError: () => toast({ message: '予約の取消に失敗しました' }),
        onSettled: () => setBusy(programId, false),
      },
    )
  }

  const reserve = (program: ProgramListItem) => {
    setBusy(program.programId, true)
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
              ? {
                  label: '取消',
                  onClick: () => cancel(program.programId, reservation.id),
                }
              : undefined,
          })
        },
        onError: () => toast({ message: '予約に失敗しました' }),
        onSettled: () => setBusy(program.programId, false),
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
            {/* 日付ヘッダの top は PageHeader が実測して書き出す高さ。
                ハードコードするとフィルタ行の増減や文字サイズでずれる */}
            {showDateHeader && (
              <h2 className="sticky top-[var(--page-header-height,0px)] z-[5] border-y border-border bg-muted/80 px-4 py-1.5 text-xs font-medium text-muted-foreground backdrop-blur">
                {formatDate(program.startAt)}
              </h2>
            )}
            <ProgramRow
              program={program}
              serviceName={serviceById.get(program.serviceId)?.name}
              reservationId={reservationId}
              pending={busyProgramIds.has(program.programId)}
              onReserve={() => reserve(program)}
              onCancel={() => reservationId && cancel(program.programId, reservationId)}
            />
          </li>
        )
      })}
    </ul>
  )
}
