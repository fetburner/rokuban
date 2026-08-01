import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'

import { CapacityBands } from '@/components/capacity-band'
import { ChannelPicker } from '@/components/channel-picker'
import { DayStrip } from '@/components/day-strip'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { ProgramGrid } from '@/components/program-grid'
import { ProgramList, type ReservationActions } from '@/components/program-list'
import { ProgramRow } from '@/components/program-row'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import {
  listPrograms,
  useListCapacityOverages,
  useListPrograms,
  useListReservations,
  useListServices,
  usePutProgramIntent,
  type CapacityOverage,
  type ProgramListItem,
  type Service,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { dayOrigin } from '@/lib/day-offset'
import { orderServices, type TimeAxis } from '@/lib/epg-grid'
import { DEFAULT_SITE } from '@/lib/site'
import { lgMediaQuery, useMediaQuery } from '@/lib/use-media-query'

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

/**
 * serviceIdParam は選択したサービス集合をサーバーへ渡す配列にする。
 *
 * 空集合は「すべて」なのでキーごと落とす（`undefined` を返す。docs/frontend.md
 * 「『問わない』次元はリクエストのキーごと落とす」）。ソートするのは、
 * `Set` の反復順は選び方の履歴に依存するため、そのまま渡すと同じ選択でも
 * URL / queryKey が変わって無限に再取得されるおそれがあるため。
 */
function serviceIdParam(selectedServiceIds: ReadonlySet<number>): number[] | undefined {
  if (selectedServiceIds.size === 0) return undefined
  return [...selectedServiceIds].sort((a, b) => a - b)
}

export function ProgramsPage() {
  // 空集合 = すべて表示。初期状態が空なので、これ以外の意味だと初回表示が
  // 空になってしまう。
  const [selectedServiceIds, setSelectedServiceIds] = useState<ReadonlySet<number>>(new Set())
  const [dayOffset, setDayOffset] = useState<number | null>(null)
  const [view, setView] = useState<ProgramView>('list')

  // グリッドは `lg` 以上でのみ出す。モバイルは常にリストのまま
  // （docs/frontend.md「リストを第一級に置く。グリッドはその上に足す」）。
  // view は画面幅で捨てないので、幅が戻ればグリッドに戻る。
  const wideScreen = useMediaQuery(lgMediaQuery)
  const showGrid = wideScreen && view === 'grid'

  const services = useListServices(DEFAULT_SITE)
  const reservations = useListReservations()

  // 起点は state から決める。queryKey に入るので、日付を変えるとページが
  // 積み直され、キャッシュ済みのページが古い窓のまま再利用されることもない。
  const originMs = dayOrigin(dayOffset).getTime()
  const limitMs = endOfSelection(dayOffset).getTime()
  const maxWindows = Math.max(1, Math.ceil((limitMs - originMs) / (windowHours * 3600_000)))

  // サーバー側で絞り込む。ソートするのは Set の反復順が選び方の履歴に依存する
  // ためで、順序が揺れると同じ選択でも queryKey / URL が変わってしまう
  // （serviceIdParam 参照）。
  const selectedServiceIdParam = useMemo(
    () => serviceIdParam(selectedServiceIds),
    [selectedServiceIds],
  )

  // サーバーが選択に応じて絞るようになったので、queryKey にも選択を入れる。
  // 入れないと別の選択で取得した結果をそのまま再利用してしまう（日付や時間窓と
  // 同じ「結果を左右するパラメータ」になったため）。
  const query = useInfiniteQuery({
    queryKey: ['/api/programs', 'infinite', originMs, limitMs, selectedServiceIdParam],
    initialPageParam: 0,
    // グリッド表示中はリストの窓を追いかけない（同じ時間帯を 2 つの形で
    // 同時に取りに行かない）。戻ったときはキャッシュがそのまま出る。
    enabled: !showGrid,
    queryFn: async ({ pageParam }) => {
      const start = new Date(originMs + pageParam * windowHours * 3600_000)
      const end = new Date(Math.min(start.getTime() + windowHours * 3600_000, limitMs))
      const res = await listPrograms(DEFAULT_SITE, {
        start: start.toISOString(),
        end: end.toISOString(),
        serviceId: selectedServiceIdParam,
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
    DEFAULT_SITE,
    {
      start: new Date(originMs).toISOString(),
      end: new Date(gridEndMs).toISOString(),
      serviceId: selectedServiceIdParam,
    },
    { query: { enabled: showGrid } },
  )
  // サーバーが選択済みの serviceId で絞るので、これ以上の適用点は要らない。
  const gridPrograms = useMemo(() => unwrap(gridQuery.data) ?? [], [gridQuery.data])
  const axis = useMemo<TimeAxis>(
    () => ({ startMs: originMs, endMs: gridEndMs, pxPerHour: gridPxPerHour }),
    [originMs, gridEndMs],
  )

  // チューナー不足の区間。グリッドの窓と同じ範囲を訊く（帯は軸の上に描かれるので
  // 軸の外の区間を持っていても使えない）。取得の失敗は帯が出ないだけに留める ---
  // 帯は補助であって番組表の本体ではなく、しかも沈黙は元から「収まる」ことの保証で
  // ないので、失敗を「不足していない」と表示するのと同じ意味にはならない
  // （docs/data.md §6.5）。予約の増減で内容が変わるため、予約の変更でも
  // invalidate する（useReservationActions / lib/events.ts）。
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(originMs).toISOString(),
      end: new Date(gridEndMs).toISOString(),
    },
    { query: { enabled: showGrid } },
  )
  const overages = useMemo(() => unwrap(overagesQuery.data) ?? [], [overagesQuery.data])

  // 窓は開区間なので境界をまたぐ番組が隣接する 2 つの窓に現れる。programId で潰す。
  // サーバーが選択済みの serviceId で絞るので、これ以上の適用点は要らない。
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

  // 絞り込む前の全サービスから作る。絞った側（filterableServices）から作ると、
  // hasPrograms が false の局の番組が来たとき（例えば選択直後にキャッシュが
  // まだ古い）名前が引けなくなる。
  const serviceById = useMemo(() => {
    const map = new Map<number, Service>()
    for (const s of unwrap(services.data) ?? []) map.set(s.serviceId, s)
    return map
  }, [services.data])

  // 予約状態は番組とは別クエリで取り、クライアント側で結合する。
  // 予約は頻繁に変わり番組はほとんど変わらないので、キャッシュの寿命を分ける。
  // リストとグリッドはこの同じ Set を見るので、表示形式で予約状態がずれない。
  //
  // 意図（PUT .../intent）は reservations 行を同期的に作らない（issue #29）ので、
  // サーバーの値だけを見ると予約直後の一覧に反映が数秒遅れる。actions.isReserved
  // が楽観的な上書きをこの Set の上に重ねる。
  const serverReservedProgramIds = useMemo(() => {
    const set = new Set<number>()
    for (const r of unwrap(reservations.data) ?? []) set.add(r.programId)
    return set
  }, [reservations.data])

  // 番組が 1 件でもあるサービスだけをチップに出す（issue #17 の S3）。
  // マルチ編成のないサブサービスは番組を持たないので自動的に消える。
  // 判断の材料は `hasPrograms`（EPG プロジェクション全体で 1 件でも番組を
  // 持つか）で、表示中の番組から推測しない。表示中の番組（サーバー側で
  // 絞り込んだ後）から導くと、1 局に絞った瞬間に候補がその 1 局だけになり、
  // 他局へ直接切り替えられなくなる（docs/frontend.md「番組リスト」）。
  const filterableServices = useMemo(
    () => (unwrap(services.data) ?? []).filter((s) => s.hasPrograms),
    [services.data],
  )

  // グリッドの列。番組を 1 つも持たないサービスは列にしない（空の列が数十本
  // 並ぶと、隣り合う番組の同時性が読み取れなくなる）。並び順は全順序なので
  // 再描画で列が入れ替わらない。
  const gridServices = useMemo(() => {
    const withPrograms = new Set(gridPrograms.map((p) => p.serviceId))
    return orderServices(
      (unwrap(services.data) ?? []).filter((s) => withPrograms.has(s.serviceId)),
    )
  }, [gridPrograms, services.data])

  const actions = useReservationActions(serverReservedProgramIds)

  return (
    <>
      <PageHeader
        title="番組"
        actions={
          <>
            {/* チャンネル絞り込みはグリッド表示中も出したままにする。選択は
                グリッドの列にも効くので、隠すと解除手段のない 1 列グリッドに
                なる（docs/frontend.md「番組リスト」）。 */}
            <ChannelPicker
              services={filterableServices}
              selected={selectedServiceIds}
              onChange={setSelectedServiceIds}
            />
            {/* 表示形式の切り替えは `lg` 以上でのみ出す。CSS で隠すのではなく
                出さないのは、モバイルに存在しない選択肢を読み上げさせないため */}
            {wideScreen && <ViewChips view={view} onSelect={setView} />}
          </>
        }
      >
        <DayStrip selected={dayOffset} days={selectableDays} onSelect={setDayOffset} />
      </PageHeader>

      {showGrid ? (
        <ProgramGridView
          axis={axis}
          programs={gridPrograms}
          services={gridServices}
          serviceById={serviceById}
          overages={overages}
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
            <ProgramList programs={programs} serviceById={serviceById} actions={actions} />
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

/**
 * useReservationActions は予約 / 取消の実行を組み立てる。
 *
 * リストとグリッドの両方が同じ経路を通るようページ側に持ち上げてある
 * （予約の見え方が表示形式で分岐すると、M2-9 の受け入れ条件「リストとグリッドで
 * 予約状態が一致する」がコード上で担保されない）。
 *
 * 意図（PUT/DELETE .../intent）は reservations 行を同期的に作らない（issue #29
 * の決定: reservations の書き手は ruler だけにする）。ruler_pass ヒントで実質
 * 秒オーダーではあるが、invalidate して取り直すだけでは一覧の反映がその間
 * 遅れる。**楽観的更新**で見た目を即時反映し、サーバー値（serverReservedIds）が
 * 追いついたら上書きを外す（自己修復。SSE の invalidate で最終的に一致する）。
 */
function useReservationActions(serverReservedIds: ReadonlySet<number>): ReservationActions {
  const queryClient = useQueryClient()
  const toast = useToast()
  const putIntent = usePutProgramIntent()

  // mutation の isPending は全行で共有されるため、操作中の番組だけを覚えておく。
  // これがないと 1 件予約する間にリスト全行のボタンが無効化される。
  const [busyProgramIds, setBusyProgramIds] = useState<ReadonlySet<number>>(new Set())
  // programId → 楽観的に見せたい予約状態（true=予約済み / false=未予約）。
  const [optimistic, setOptimistic] = useState<ReadonlyMap<number, boolean>>(new Map())

  // サーバー値が楽観的な予想に追いついたら、その上書きは要らなくなる。
  // 消し忘れると、後でユーザーが手動で戻した変更まで隠れてしまう。
  useEffect(() => {
    setOptimistic((current) => {
      if (current.size === 0) return current
      let changed = false
      const next = new Map(current)
      for (const [programId, want] of current) {
        if (serverReservedIds.has(programId) === want) {
          next.delete(programId)
          changed = true
        }
      }
      return changed ? next : current
    })
  }, [serverReservedIds])

  const reservedProgramIds = useMemo(() => {
    const set = new Set(serverReservedIds)
    for (const [programId, want] of optimistic) {
      if (want) set.add(programId)
      else set.delete(programId)
    }
    return set
  }, [serverReservedIds, optimistic])

  const setBusy = (programId: number, busy: boolean) => {
    setBusyProgramIds((current) => {
      const next = new Set(current)
      if (busy) next.add(programId)
      else next.delete(programId)
      return next
    })
  }

  const setOptimisticReserved = (programId: number, reserved: boolean | undefined) => {
    setOptimistic((current) => {
      const next = new Map(current)
      if (reserved === undefined) next.delete(programId)
      else next.set(programId, reserved)
      return next
    })
  }

  const invalidateReservations = () => {
    void queryClient.invalidateQueries({ queryKey: ['/api/reservations'] })
    // 容量超過は予約集合からの導出値なので、予約が増減すれば作り直させる。
    // 帯を古いまま残すと「予約したのに不足が消えない / 出ない」になる
    void queryClient.invalidateQueries({ queryKey: ['/api/capacity/overages'] })
  }

  const cancel = (programId: number) => {
    setBusy(programId, true)
    setOptimisticReserved(programId, false)
    putIntent.mutate(
      { site: DEFAULT_SITE, programId, data: { action: 'skip' } },
      {
        onSuccess: () => {
          invalidateReservations()
          toast({ message: '予約を取消しました' })
        },
        onError: () => {
          toast({ message: '予約の取消に失敗しました' })
          setOptimisticReserved(programId, undefined)
        },
        onSettled: () => setBusy(programId, false),
      },
    )
  }

  const reserve = (program: ProgramListItem) => {
    setBusy(program.programId, true)
    setOptimisticReserved(program.programId, true)
    putIntent.mutate(
      { site: DEFAULT_SITE, programId: program.programId, data: { action: 'record' } },
      {
        onSuccess: () => {
          invalidateReservations()
          // 確認ダイアログを挟まない代わりに、直後に取り返せるようにする
          toast({
            message: `予約しました: ${program.name}`,
            action: { label: '取消', onClick: () => cancel(program.programId) },
          })
        },
        onError: () => {
          toast({ message: '予約に失敗しました' })
          setOptimisticReserved(program.programId, undefined)
        },
        onSettled: () => setBusy(program.programId, false),
      },
    )
  }

  return {
    reserve,
    cancel,
    isBusy: (programId) => busyProgramIds.has(programId),
    reservedProgramIds,
  }
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
  overages,
  actions,
  isPending,
  isError,
}: {
  axis: TimeAxis
  programs: ProgramListItem[]
  services: Service[]
  serviceById: Map<number, Service>
  /** チューナーが不足している区間。番組ではなく区間として帯に描く（M2-10）。 */
  overages: readonly CapacityOverage[]
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
            reserved={actions.reservedProgramIds.has(selected.programId)}
            pending={actions.isBusy(selected.programId)}
            onReserve={() => actions.reserve(selected)}
            onCancel={() => actions.cancel(selected.programId)}
          />
        </div>
      )}
      <div className="min-h-0 flex-1">
        <ProgramGrid
          services={services}
          programs={programs}
          axis={axis}
          reservationByProgramId={actions.reservedProgramIds}
          selectedProgramId={selected?.programId ?? null}
          onSelect={(program) => setSelectedProgramId(program.programId)}
          // 帯はセルより上・ヘッダより下の層に入る。軸を受け取って同じ
          // spanToPx を通すので、帯と番組セルは同じ時刻で必ず同じ位置に来る
          overlay={(gridAxis) => <CapacityBands axis={gridAxis} overages={overages} />}
        />
      </div>
    </div>
  )
}
