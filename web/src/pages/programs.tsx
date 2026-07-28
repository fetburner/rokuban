import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'

import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { ProgramGrid } from '@/components/program-grid'
import { ProgramRow } from '@/components/program-row'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import {
  listPrograms,
  useCreateReservation,
  useDeleteReservation,
  useListPrograms,
  useListReservations,
  useListServices,
  type ProgramListItem,
  type Service,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { orderServices, type TimeAxis } from '@/lib/epg-grid'
import { dayKey, formatDate } from '@/lib/format'
import { lgMediaQuery, useMediaQuery } from '@/lib/use-media-query'
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

/** グリッドが一度に描く時間の幅。M2-9 の受け入れ条件が「全サービス x 24 時間」。 */
const gridWindowHours = 24

/**
 * グリッドの縦の縮尺。30 分番組が 60px になるので、開始時刻とタイトルの 2 行が入る。
 * これより詰めると 15 分番組が読めず、広げると 24 時間の全長が伸びすぎる。
 */
const gridPxPerHour = 120

/** ProgramView は番組の表示形式。グリッドは `lg` 以上でのみ選べる。 */
type ProgramView = 'list' | 'grid'

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
  const [view, setView] = useState<ProgramView>('list')

  // グリッドは `lg` 以上でのみ出す。モバイルは常にリストのまま
  // （docs/frontend.md「リストを第一級に置く。グリッドはその上に足す」）。
  // view は画面幅で捨てないので、幅が戻ればグリッドに戻る。
  const wideScreen = useMediaQuery(lgMediaQuery)
  const showGrid = wideScreen && view === 'grid'

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
    // グリッド表示中はリストの窓を追いかけない（同じ時間帯を 2 つの形で
    // 同時に取りに行かない）。戻ったときはキャッシュがそのまま出る。
    enabled: !showGrid,
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

  // グリッドは 24 時間ぶんを 1 回で取る。リストのような窓の積み上げにしないのは、
  // 縦位置が時刻そのものなので途中まで積んだ状態が「番組がない時間帯」と
  // 見分けられないため。
  const gridEndMs = Math.min(originMs + gridWindowHours * 3600_000, limitMs)
  const gridQuery = useListPrograms(
    {
      start: new Date(originMs).toISOString(),
      end: new Date(gridEndMs).toISOString(),
      ...(serviceFilter === null ? {} : { serviceId: serviceFilter }),
    },
    { query: { enabled: showGrid } },
  )
  const gridPrograms = useMemo(() => unwrap(gridQuery.data) ?? [], [gridQuery.data])
  const axis = useMemo<TimeAxis>(
    () => ({ startMs: originMs, endMs: gridEndMs, pxPerHour: gridPxPerHour }),
    [originMs, gridEndMs],
  )

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
  // リストとグリッドはこの同じ Map を見るので、表示形式で予約状態がずれない。
  const reservationByProgramId = useMemo(() => {
    const map = new Map<number, number>()
    for (const r of unwrap(reservations.data) ?? []) map.set(r.programId, r.id)
    return map
  }, [reservations.data])

  // 番組が 1 件でもあるサービスだけをチップに出す（issue #17 の S3）。
  // マルチ編成のないサブサービスは番組を持たないので自動的に消える。
  // 判断の材料はいま見ている表示形式の番組（グリッドは 24 時間、リストは積んだ窓）。
  const visiblePrograms = showGrid ? gridPrograms : programs
  const filterableServices = useMemo(() => {
    const withPrograms = new Set(visiblePrograms.map((p) => p.serviceId))
    return (unwrap(services.data) ?? []).filter(
      (s) => withPrograms.has(s.serviceId) || s.serviceId === serviceFilter,
    )
  }, [visiblePrograms, services.data, serviceFilter])

  // グリッドの列。番組を 1 つも持たないサービスは列にしない（空の列が数十本
  // 並ぶと、隣り合う番組の同時性が読み取れなくなる）。並び順は全順序なので
  // 再描画で列が入れ替わらない。
  const gridServices = useMemo(() => {
    const withPrograms = new Set(gridPrograms.map((p) => p.serviceId))
    return orderServices(
      (unwrap(services.data) ?? []).filter((s) => withPrograms.has(s.serviceId)),
    )
  }, [gridPrograms, services.data])

  const actions = useReservationActions()

  return (
    <>
      <PageHeader title="番組">
        <DayChips selected={dayOffset} onSelect={setDayOffset} />
        <ServiceChips
          services={filterableServices}
          selected={serviceFilter}
          onSelect={setServiceFilter}
        />
        {/* 表示形式の切り替えは `lg` 以上でのみ出す。CSS で隠すのではなく
            出さないのは、モバイルに存在しない選択肢を読み上げさせないため */}
        {wideScreen && <ViewChips view={view} onSelect={setView} />}
      </PageHeader>

      {showGrid ? (
        <ProgramGridView
          axis={axis}
          programs={gridPrograms}
          services={gridServices}
          serviceById={serviceById}
          reservationByProgramId={reservationByProgramId}
          actions={actions}
          // グリッドではサービスが列そのもの（構造）なので、リストと違って
          // サービスの取得失敗を「名前が出ないだけ」に落とせない。列が 0 本の
          // グリッドは「番組がない」と見分けがつかないので、取得状態を合わせる
          isPending={gridQuery.isPending || services.isPending}
          isError={gridQuery.isError || services.isError}
        />
      ) : (
        <>
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
              actions={actions}
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
      )}
    </>
  )
}

/** ReservationActions は番組からの予約 / 取消と、番組ごとの実行中状態。 */
type ReservationActions = {
  reserve: (program: ProgramListItem) => void
  cancel: (programId: number, reservationId: number) => void
  isBusy: (programId: number) => boolean
}

/**
 * useReservationActions は予約 / 取消の実行を組み立てる。
 *
 * リストとグリッドの両方が同じ経路を通るようページ側に持ち上げてある
 * （予約の見え方が表示形式で分岐すると、M2-9 の受け入れ条件「リストとグリッドで
 * 予約状態が一致する」がコード上で担保されない）。
 *
 * 楽観的更新はしない。invalidate して REST から取り直す（レベルトリガー）。
 */
function useReservationActions(): ReservationActions {
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

  return { reserve, cancel, isBusy: (programId) => busyProgramIds.has(programId) }
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

function ViewChips({
  view,
  onSelect,
}: {
  view: ProgramView
  onSelect: (view: ProgramView) => void
}) {
  return (
    <div role="group" aria-label="表示形式" className="flex gap-2 px-4 pb-3">
      <Chip active={view === 'list'} onClick={() => onSelect('list')}>
        リスト
      </Chip>
      <Chip active={view === 'grid'} onClick={() => onSelect('grid')}>
        番組表
      </Chip>
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

/**
 * ProgramGridView はグリッド表示。選択した番組をグリッドの上にリストの行として出す。
 *
 * セルの中に予約ボタンや詳細を作り込まない。セルの高さは放送時間そのもの
 * （5 分番組は 10px）なので、そこに操作を置くと押せない番組ができる。
 * リスト行（ProgramRow）をそのまま再利用すれば、予約・取消・詳細の展開・
 * 重なり警告がリストと同一の実装になり、見え方が表示形式で分岐しない。
 */
function ProgramGridView({
  axis,
  programs,
  services,
  serviceById,
  reservationByProgramId,
  actions,
  isPending,
  isError,
}: {
  axis: TimeAxis
  programs: ProgramListItem[]
  services: Service[]
  serviceById: Map<number, Service>
  reservationByProgramId: Map<number, number>
  actions: ReservationActions
  isPending: boolean
  isError: boolean
}) {
  const [selectedProgramId, setSelectedProgramId] = useState<number | null>(null)

  // 日付やサービスを変えると選択中の番組が消えることがある。id ではなく
  // 実体を引き直して、消えていれば選択も無かったことにする。
  const selected = programs.find((p) => p.programId === selectedProgramId)

  if (isError) return <ErrorState>番組の取得に失敗しました</ErrorState>
  if (isPending) return <ListSkeleton />
  if (programs.length === 0) return <EmptyState>この時間帯の番組がありません</EmptyState>

  const selectedReservationId = selected
    ? reservationByProgramId.get(selected.programId)
    : undefined

  return (
    <div
      className="flex flex-col"
      // 高さの予算はここで決める。ページ全体がスクロールするとグリッドの
      // ヘッダ（sticky）が画面外へ出てしまう。
      style={{
        height:
          'calc(100dvh - var(--page-header-height, 0px) - var(--breaker-banner-height, 0px))',
      }}
    >
      {selected && (
        <div className="shrink-0 border-b border-border bg-card">
          <ProgramRow
            program={selected}
            serviceName={serviceById.get(selected.serviceId)?.name}
            reservationId={selectedReservationId}
            pending={actions.isBusy(selected.programId)}
            onReserve={() => actions.reserve(selected)}
            onCancel={() =>
              selectedReservationId !== undefined &&
              actions.cancel(selected.programId, selectedReservationId)
            }
          />
        </div>
      )}
      <div className="min-h-0 flex-1">
        <ProgramGrid
          services={services}
          programs={programs}
          axis={axis}
          reservationByProgramId={reservationByProgramId}
          selectedProgramId={selected?.programId ?? null}
          onSelect={(program) => setSelectedProgramId(program.programId)}
        />
      </div>
    </div>
  )
}

function ProgramList({
  programs,
  serviceById,
  reservationByProgramId,
  actions,
}: {
  programs: ProgramListItem[]
  serviceById: Map<number, Service>
  reservationByProgramId: Map<number, number>
  actions: ReservationActions
}) {
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
              pending={actions.isBusy(program.programId)}
              onReserve={() => actions.reserve(program)}
              onCancel={() => reservationId && actions.cancel(program.programId, reservationId)}
            />
          </li>
        )
      })}
    </ul>
  )
}
