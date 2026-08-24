import { keepPreviousData, useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'

import { CapacityBands } from '@/components/capacity-band'
import { ChannelPicker } from '@/components/channel-picker'
import { DayStrip } from '@/components/day-strip'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { ProgramGrid } from '@/components/program-grid'
import {
  ProgramList,
  type ProgramListHandle,
  type ReservationActions,
} from '@/components/program-list'
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
  usePatchProgramOverrides,
  usePutProgramIntent,
  type CapacityOverage,
  type ProgramListItem,
  type ProgramOverridesInput,
  type Service,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { shouldAutoLoadNextPage, shouldShowLoadMoreButton } from '@/lib/auto-load'
import { dayOffsetForMs, dayOrigin } from '@/lib/day-offset'
import { orderServices, type TimeAxis } from '@/lib/epg-grid'
import { formatDate } from '@/lib/format'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import { filterProgramsFromListStart } from '@/lib/program-list-window'
import {
  parseProgramServiceKey,
  pickerServiceDomain,
  programServiceKey,
  type ProgramsPageSearch,
} from '@/lib/programs-search'
import { previousDayWindow } from '@/lib/previous-day-window'
import { useCurrentSite } from '@/lib/site'
import { lgMediaQuery, useMediaQuery } from '@/lib/use-media-query'

/**
 * windowHours は、進行方向（下スクロールでの自動読み込み・「さらに読み込む」）
 * の 1 回のスクロールステップで取得する時間窓の幅。
 *
 * API はページネーショントークンを持たず、時間窓そのものがカーソルになる。
 * 「次のページ」= 前回の end を start にした次の窓。
 *
 * **遡行（「前を読み込む」）はこの定数を使わない** ---
 * 1 暦日（前日 0 時〜当日 0 時）単位で読む（`lib/previous-day-window.ts` の
 * `previousDayWindow`）。理由は同ファイルの doc コメント参照（日付ヘッダの
 * 帯の増減による位置ずれを、境界を暦日に揃えることで構造的に防ぐため）。
 * 進行方向は増分読み込みとして機能しているだけで日付ヘッダの帯とは無関係
 * なので、6 時間のまま変えていない。
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
 * ProgramsPage は番組表（`/programs`）。
 *
 * ホーム（`/`）の新設（M8-3, issue #242）で `/` を譲り、このページ自身は
 * `/programs` に移設した。裸の `/` はホームになり、旧 `?serviceId=` / `?at=` が
 * 付いた `/` だけ `/programs` へリダイレクトされる（`routes.tsx` の
 * `homeRoute`）。
 *
 * チャンネル絞り込みは URL に持つ。ライブ視聴からの 1 局リンクは既存の
 * `networkId + serviceId`、ピッカーからの複数選択は厳密な `service` 配列を使う。
 *
 * 容量不足バッジ（予約一覧）からは `?at=<epoch ms>` 付きで飛べる（issue #233
 * M6-5）。`lg` 以上ではグリッドへ自動で切り替えてその時刻へスクロールし、
 * それ以外（リスト・`lg` 未満）では「その時刻が属する日」への日付ジャンプに
 * 留める（下記 `at` 関連の 2 つの effect 参照）。
 *
 * **番組表からライブへの導線はここには置かない。** 放送中の番組の展開に
 * 「ライブで見る」を出す導線は行（`ProgramRow`）の展開領域の担当にする
 * （issue #229。この PR の時点では未実装・並行実装中）。
 * ページ全体に「視聴中チャンネルへ」のような 2 つ目のライブ導線を足すと、
 * どの番組が放送中か分からない状態でも押せてしまい行き先が不定になる
 * （複数チャンネルを絞り込んでいるときにどれへ飛ぶかを決める判断基準が無い）
 * うえ、個々の番組から飛べる導線と役割が重複する。issue #231 の決定。
 */
export function ProgramsPage() {
  const site = useCurrentSite()
  // チャンネル絞り込みは URL に持つ。新しい選択は厳密な `?service=`、旧
  // `?networkId=&serviceId=` は後方互換入力として読む。表示状態は component state。
  // 検証（不正な値・0 以下の除去・重複除去・昇順ソート）は
  // `routes.tsx` の `validateSearch`（`lib/programs-search.ts` の
  // `parseProgramsSearch`）で済んでいるので、ここでは信頼して使う。
  const search = useRouteSearch({ from: '/programs' })
  const navigate = useNavigate()
  const updateSearch = (updater: (prev: ProgramsPageSearch) => ProgramsPageSearch) => {
    // 選ぶたびに URL を書き換えるが、history は汚さない（`replace`。
    // `lib/recording-search.ts` の絞り込み更新と同じ規律）。
    //
    // **updater の引数は「全ルートの search を合成した型」で来る**
    // （TanStack Router の `ParamsReducerFn`）。`/live` が同じ名前の `serviceId`
    // を単数（`number`）で持つため、合成後は型上 `number | number[]` になり
    // `ProgramsPageSearch` にそのままは代入できない。この関数が呼ばれるのは
    // `/programs`（番組表）に居るときだけで、そのとき実際に入っているのは
    // `parseProgramsSearch` が検証した形なので、ここで絞ってから updater に渡す
    // （`pages/recordings.tsx` の `updateSearch` と同じ形）。
    void navigate({
      to: '/programs',
      search: (prev) => updater(prev as ProgramsPageSearch),
      replace: true,
    })
  }
  // dayOffset は「ジャンプ先」（DayStrip をタップして跳ぶ先）。0 以上
  // selectableDays 未満。0 は今日で、リストは常にここから連続フィードとして
  // 始まる（`今` という別枠の選択肢は無い）。
  const [dayOffset, setDayOffset] = useState(0)
  // visibleDay は「いま見ている日」（ProgramList がスクロール位置から導出して
  // 通知する）。DayStrip のハイライトはこちらを見る。ジャンプ直後は dayOffset と
  // 一致するが、その後リストをスクロールすればこちらだけが動く。
  const [visibleDay, setVisibleDay] = useState(0)
  const [view, setView] = useState<ProgramView>('list')

  // ProgramList への命令的 API（`components/program-list.tsx` の
  // `ProgramListHandle`）。「既にジャンプ先になっている日」を再タップしたときに
  // `scrollToDayOffset` を呼ぶために持つ。詳細は `selectDay` のコメント参照。
  const programListRef = useRef<ProgramListHandle>(null)

  // ジャンプ先を選んだら、ハイライトも即座にジャンプ先へ合わせる。ProgramList が
  // 新しい窓の可視範囲から改めて通知するまでの間、古い日をハイライトし続けない
  // ようにする。
  //
  // 既に `dayOffset` と同じ日をタップした場合は特別扱いする ---
  // `setDayOffset(同じ値)` は React が再レンダーの理由にしないため、クエリも
  // ProgramList への再マウントも起きず、素通しにすると「押しても無反応」に
  // なる（実機で確認した不具合）。ユーザーはスクロールでその日から離れた場所
  // （`visibleDay` が別の日）を見ていることがあるので、再タップは「読み込み
  // 済みならその日の先頭へ戻る」ことを意味する必要がある。読み込み済みかは
  // `ProgramList` 側（`programs` を持っている）でしか判定できないので、
  // `scrollToDayOffset`（ref 経由）に委ねる ---
  // 見つからなければ ProgramList 側が何もしない。
  const selectDay = (offset: number) => {
    if (offset === dayOffset) {
      setVisibleDay(offset)
      programListRef.current?.scrollToDayOffset(offset)
      return
    }
    setDayOffset(offset)
    setVisibleDay(offset)
  }

  // at は容量不足バッジ（`components/capacity-shortfall-badge.tsx`）からの
  // ジャンプ先の時刻（epoch ms。issue #233 M6-5、`lib/programs-search.ts` の
  // `parseProgramsSearch` が検証済み）。グリッドの初期スクロール位置に使う
  // （下記 `ProgramGridView` への `scrollToMs`）他に、グリッドが出ない・
  // 選ばれていない画面でも「その時刻が属する日」だけは合わせる（次項）。
  const at = search.at
  // atDayOffset は at が属する日（`lib/day-offset.ts` の `dayOffsetForMs`）。
  // `dayOffset`（いま実際に見ている日）と比較することで「at はまだ有効か」を
  // 判定する（下記 `scrollToMs` 参照）。
  const atDayOffset = at === undefined ? undefined : dayOffsetForMs(at, Date.now(), selectableDays)

  // at が指す日へ「いま見ている日」を合わせる。グリッドの有無・表示形式に
  // 関わらず効かせる --- リスト表示中・`lg` 未満（グリッドが出ない）画面では
  // 帯で「その時間帯」を直接見せる手段が無いため、次善として日だけ合わせる
  // のがこの導線の唯一の反映先になる。
  useEffect(() => {
    if (atDayOffset === undefined) return
    setDayOffset(atDayOffset)
    setVisibleDay(atDayOffset)
  }, [atDayOffset])

  // グリッドは `lg` 以上でのみ出す。モバイルは常にリストのまま
  // （docs/frontend.md「リストを第一級に置く。グリッドはその上に足す」）。
  // view は画面幅で捨てないので、幅が戻ればグリッドに戻る。
  const wideScreen = useMediaQuery(lgMediaQuery)
  const showGrid = wideScreen && view === 'grid'

  // at があり、かつグリッドが選べる画面幅なら自動でグリッドへ切り替える ---
  // バッジの目的は「その時間帯を帯で見る」ことなので、リストのままでは用が
  // 済まない。`useMediaQuery` は初回レンダーでは必ず false を返し（`window`
  // の購読は effect 経由なので、マウント直後の 1 回だけは実際の画面幅を
  // 反映できない）、遅れて true になった時点でこの effect が発火する。
  // `forcedGridForAtRef` で「この at には既に 1 回切り替えた」ことを覚えておき、
  // 切り替え後にユーザーが手動でリストへ戻した選択を、resize による
  // `wideScreen` の再評価で上書きしない。別のバッジ（別の at）を踏めばまた
  // 1 回だけ働く。
  //
  // **`at` を URL から消費・削除する方式は採らない**（レビュー指摘 nit 4 の
  // 素朴な実装で一度試して実機で退行を確認したため）。`navigate` で `at` を
  // 消す effect は非同期に解決し、グリッドが実際に軸を確定してスクロールを
  // 適用するより先に `at` が消えてしまうことがあった（`useMediaQuery` が
  // `false → true` になるのに最低 1 レンダー、`view` が `'grid'` になるのに
  // さらに 1 レンダーかかる一方、`navigate` の解決タイミングはそれより早く
  // 終わりうる）--- 結果、肝心の初回スクロールが「今」にしか効かなくなった
  // （e2e `web/e2e/badge-links.mjs` の②が実際に落ちて発覚した）。代わりに
  // 下記 `scrollToMs` を `dayOffset === atDayOffset` で条件付けることで、
  // 「at は現在地の日を離れたら自動的に効かなくなる」を URL を書き換えずに
  // 実現する --- 「今日」ボタンを押す（`dayOffset` が変わる）だけで scrollToMs
  // は自然に `undefined` に戻り、以後の軸変更は「今」へスクロールする既定の
  // 挙動に戻る。
  const forcedGridForAtRef = useRef<number | undefined>(undefined)
  useEffect(() => {
    if (at === undefined || !wideScreen) return
    if (forcedGridForAtRef.current === at) return
    forcedGridForAtRef.current = at
    setView('grid')
  }, [at, wideScreen])

  // scrollToMs はグリッドの初期スクロール先。**`dayOffset` が `at` の指す日と
  // 一致している間だけ** `at` を渡す --- 一致しなくなった（「今日」ボタンや
  // 日付ストリップで別の日へ移った）後まで古い `at` を渡し続けると、以後の
  // 軸変更のたびに「今」ではなく `at` の位置へ戻ってしまう（実測。上記コメント
  // 参照）。`at` 自体は URL に残ったままだが、実際に効くのは「その日を見ている
  // 間の最初の 1 回」に限られる。
  const scrollToMs = at !== undefined && dayOffset === atDayOffset ? at : undefined

  const services = useListServices(site)
  const allServices = useMemo(() => unwrap(services.data) ?? [], [services.data])
  const selectedServiceKeys = useMemo(() => {
    const selected = new Set(search.service ?? [])
    const legacyServiceIds = new Set(search.serviceId ?? [])
    const matchedLegacyServiceIds = new Set<number>()
    if (legacyServiceIds.size > 0 || search.networkId !== undefined) {
      for (const service of allServices) {
        if (search.networkId !== undefined && service.networkId !== search.networkId) continue
        if (legacyServiceIds.size > 0 && !legacyServiceIds.has(service.serviceId)) continue
        selected.add(programServiceKey(service.networkId, service.serviceId))
        matchedLegacyServiceIds.add(service.serviceId)
      }
    }
    for (const serviceId of legacyServiceIds) {
      if (!matchedLegacyServiceIds.has(serviceId)) {
        selected.add(programServiceKey(search.networkId ?? 0, serviceId))
      }
    }
    return selected
  }, [allServices, search.networkId, search.service, search.serviceId])
  const reservations = useListReservations()

  // nowMs はこのレンダーの間で一貫させる。起点・上限・下限をそれぞれ別々に
  // Date.now() を呼んで求めると、ミリ秒単位でずれた「今」が混ざりうる。
  const nowMs = Date.now()
  // 起点はジャンプ先（state）から決める。queryKey に入るので、日付を変えると
  // ページが積み直され、キャッシュ済みのページが古い窓のまま再利用されることもない。
  const originMs = dayOrigin(dayOffset, nowMs).getTime()
  // 上限はどの選択でも共通の「EPG のローリングウィンドウの終端」
  // （8 日先の 0 時）。日付を選んでも 24 時で打ち切らない —— 連続フィードなので、
  // 選んだ日から先もそのまま読み続けられる。
  const limitMs = dayOrigin(selectableDays, nowMs).getTime()
  // 下限は「now を時で切り捨てた時刻」。遡行（前の時間窓の読み込み）はここまで。
  // サーバーの EPG 保持期間の設定には依存させない —— 放送済み番組の閲覧は
  // 今回のスコープ外なので、クライアント側で now を不変条件として持つだけで足りる。
  const lowerBoundMs = dayOrigin(0, nowMs).getTime()

  // API へ渡すサービス絞り込み。新しい service は厳密な組、networkId / serviceId は
  // 後方互換入力であり、両方を同時には生成しない。
  const selectedServiceParam = search.service
  const legacyNetworkIdParam = search.networkId
  const legacyServiceIdParam = search.serviceId

  // サーバーが選択に応じて絞るようになったので、queryKey にも選択を入れる。
  // 入れないと別の選択で取得した結果をそのまま再利用してしまう（日付や時間窓と
  // 同じ「結果を左右するパラメータ」になったため）。
  //
  // pageParam / ページの形は「取得した半開区間 [startMs, endMs)」そのもの
  // （`step` のような抽象的なカーソルにしない）。進行方向（`getNextPageParam`）は
  // 常に `windowHours` 幅、遡行（`getPreviousPageParam`）は 1 暦日幅と、
  // 2 方向で窓の刻み方が異なる（`windowHours` の doc コメント参照）ため、
  // 共通の「窓の個数」では表現できない。
  const query = useInfiniteQuery({
    queryKey: [
      '/api/programs',
      'infinite',
      originMs,
      limitMs,
      legacyNetworkIdParam,
      legacyServiceIdParam,
      selectedServiceParam,
    ],
    initialPageParam: {
      startMs: originMs,
      endMs: Math.min(originMs + windowHours * 3600_000, limitMs),
    },
    // グリッド表示中はリストの窓を追いかけない（同じ時間帯を 2 つの形で
    // 同時に取りに行かない）。戻ったときはキャッシュがそのまま出る。
    enabled: !showGrid,
    // 日付ジャンプで originMs（＝ queryKey）が変わると infinite query が
    // 作り直される。未キャッシュの日だと `isPending` が即 true になり、
    // 下の分岐で `ProgramList` が `ListSkeleton` に挿し替わって文書高さが
    // ビューポートまで潰れる（レイアウトシフト）。前のリストを残したまま
    // 差し替えれば潰れない（`isPlaceholderData` の間は前の日のデータを見せ、
    // 新しい日が届いたら差し替わる）。着地時に先頭へ戻す処理は下の
    // `useLayoutEffect`（originMs 変化）が持つ。`pages/home.tsx` の容量超過
    // クエリと同じ手。
    placeholderData: keepPreviousData,
    queryFn: async ({ pageParam }) => {
      const { startMs, endMs } = pageParam
      const res = await listPrograms(site, {
        start: new Date(startMs).toISOString(),
        end: new Date(endMs).toISOString(),
        networkId: legacyNetworkIdParam,
        serviceId: legacyServiceIdParam,
        service: selectedServiceParam,
      })
      return { startMs, endMs, programs: unwrap(res) ?? [] }
    },
    // 進行方向は windowHours（6 時間）ぶんずつ。上限（EPG のローリングウィンドウの
    // 終端）に達したら打ち切る。
    getNextPageParam: (last) => {
      if (last.endMs >= limitMs) return undefined
      const startMs = last.endMs
      return { startMs, endMs: Math.min(startMs + windowHours * 3600_000, limitMs) }
    },
    // 遡行は明示的なボタンでのみ行う（ジェスチャにしない。理由は下の
    // 「前を読み込む」ボタンのコメント参照）。1 暦日（前日 0 時〜当日 0 時）
    // ぶんを 1 回で読む（`lib/previous-day-window.ts`）。下限（now を時で
    // 切り捨てた時刻）に達していたら `undefined`（`previousDayWindow` が
    // `null` を返す）でボタンごと消える。
    getPreviousPageParam: (first) => previousDayWindow(first.startMs, lowerBoundMs) ?? undefined,
  })

  // グリッドは 24 時間ぶんを 1 回で取る。リストのような窓の積み上げにしないのは、
  // 縦位置が時刻そのものなので途中まで積んだ状態が「番組がない時間帯」と
  // 見分けられないため。
  const gridEndMs = Math.min(originMs + gridWindowHours * 3600_000, limitMs)
  const gridQuery = useListPrograms(
    site,
    {
      start: new Date(originMs).toISOString(),
      end: new Date(gridEndMs).toISOString(),
      networkId: legacyNetworkIdParam,
      serviceId: legacyServiceIdParam,
      service: selectedServiceParam,
    },
    { query: { enabled: showGrid } },
  )
  // サーバーが選択済みのサービスで絞るので、これ以上の適用点は要らない。
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
  // 一覧 API は全サイトの区間を返すが、帯は現在サイトの EPG に重ねるため
  // 現在サイトだけに絞る。CapacityBands 自身は渡された区間を描くことに専念する。
  const overages = useMemo(
    () => (unwrap(overagesQuery.data) ?? []).filter((overage) => overage.site === site),
    [overagesQuery.data, site],
  )

  // 窓は開区間なので境界をまたぐ番組が隣接する 2 つの窓に現れる。programId で潰す。
  // サーバーが選択済みのサービスで絞るので、これ以上の適用点は要らない。
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

  // listStartMs は「読み込み済みの最も手前の窓の開始時刻を下限（now を時で
  // 切り捨てた時刻）で clamp したもの」。`query.data.pages` は
  // fetchPreviousPage で先頭に追加されるので、常に pages[0] が最も手前
  // （最小の startMs）の窓になる。`pages[0].startMs` は `queryFn` が返す
  // 時点で既に下限で clamp 済み（`previousDayWindow` 参照）だが、まだ
  // 一度も遡行していない（`pages` が無い）ときの `originMs` フォールバックは
  // clamp されていないことがある（`dayOffset` が 0 のとき `originMs` は
  // 既に下限と一致するので実質無害だが、意図を明示するため残す）。
  const listStartMs = useMemo(() => {
    const rawFirstStartMs = query.data?.pages[0]?.startMs ?? originMs
    return Math.max(rawFirstStartMs, lowerBoundMs)
  }, [query.data, originMs, lowerBoundMs])

  // 「前を読み込む」を押すと次に取得される窓（ボタンのラベルに日付を出す
  // ため。`query.hasPreviousPage`/`isFetchingPreviousPage` と同じ
  // `previousDayWindow` を入力に使うので、両者が指す「次の窓があるか」は
  // 常に一致する）。
  const previousWindow = useMemo(
    () => previousDayWindow(listStartMs, lowerBoundMs),
    [listStartMs, lowerBoundMs],
  )
  const previousDateLabel = previousWindow
    ? formatDate(new Date(previousWindow.startMs).toISOString())
    : null

  // API は問い合わせた時間窓に重なる番組を返す（`start_at < window_end AND
  // end_at > window_start`）ため、先頭の窓の開始時刻より前に始まった番組
  // （＝まだ読み込んでいない前の窓との重なり）がリストの先頭に混ざる。これを
  // 見せたままだと日付ヘッダと「いま見ている日」がどちらも前日を指す（実機で
  // 確認済みの不具合）。`listStartMs` が下限と一致するとき（＝今日を見ている、
  // または遡行が下限まで達したとき）は例外的に絞り込まない ---
  // 放送中の番組を隠さないため。判定は `lib/program-list-window.ts` の純関数。
  const visiblePrograms = useMemo(
    () => filterProgramsFromListStart(programs, listStartMs, lowerBoundMs),
    [programs, listStartMs, lowerBoundMs],
  )

  // 絞り込む前の全サービスから作る。絞った側（filterableServices）から作ると、
  // hasPrograms が false の局の番組が来たとき（例えば選択直後にキャッシュが
  // まだ古い）名前が引けなくなる。
  const serviceByKey = useMemo(() => {
    const map = new Map<string, Service>()
    for (const service of allServices) {
      map.set(programServiceKey(service.networkId, service.serviceId), service)
    }
    return map
  }, [allServices])

  // 予約状態は番組とは別クエリで取り、クライアント側で結合する。
  // 予約は頻繁に変わり番組はほとんど変わらないので、キャッシュの寿命を分ける。
  // リストとグリッドはこの同じ Set を見るので、表示形式で予約状態がずれない。
  //
  // 意図（PUT .../intent）は reservations 行を同期的に作らない（issue #29）ので、
  // サーバーの値だけを見ると予約直後の一覧に反映が数秒遅れる。actions.isReserved
  // が楽観的な上書きをこの Set の上に重ねる。
  //
  // 一覧は全サイトの予約を返す（不変条件 1）が、番組表は現在サイトの EPG を
  // 描くので現在サイトの予約だけを重ねる。programId は放送イベントから決まり
  // 2 サイトで一致しうるので、site で突き合わせないと別サイトの予約で「予約済み」に
  // なる（ライブの中断予測と同じ絞り込み。lib/live-interruption.ts。issue #324）。
  const serverReservedProgramIds = useMemo(() => {
    const set = new Set<number>()
    for (const r of unwrap(reservations.data) ?? []) {
      if (r.site === site) set.add(r.programId)
    }
    return set
  }, [reservations.data, site])

  // 番組が 1 件でもあるサービスだけをチップに出す（issue #17 の S3）。
  // マルチ編成のないサブサービスは番組を持たないので自動的に消える。
  // 判断の材料は `hasPrograms`（EPG プロジェクション全体で 1 件でも番組を
  // 持つか）で、表示中の番組から推測しない。表示中の番組（サーバー側で
  // 絞り込んだ後）から導くと、1 局に絞った瞬間に候補がその 1 局だけになり、
  // 他局へ直接切り替えられなくなる（docs/frontend.md「番組リスト」）。
  const filterableServices = useMemo(
    () => allServices.filter((service) => service.hasPrograms),
    [allServices],
  )

  // URL から入った選択は候補（hasPrograms=true）の外にありうるため、両者の和を
  // ピッカーへ渡す。旧 serviceId は一致する全 network の厳密キーへ表示上展開し、
  // 同じ serviceId の別 network を同じ候補へ潰さない。
  const pickerServices = useMemo(
    () => pickerServiceDomain(filterableServices, selectedServiceKeys, serviceByKey),
    [filterableServices, selectedServiceKeys, serviceByKey],
  )

  // グリッドの列。番組を 1 つも持たないサービスは列にしない（空の列が数十本
  // 並ぶと、隣り合う番組の同時性が読み取れなくなる）。並び順は全順序なので
  // 再描画で列が入れ替わらない。
  const gridServices = useMemo(() => {
    const withPrograms = new Set(
      gridPrograms.map((program) => programServiceKey(program.networkId, program.serviceId)),
    )
    return orderServices(
      allServices.filter((service) =>
        withPrograms.has(programServiceKey(service.networkId, service.serviceId)),
      ),
    )
  }, [allServices, gridPrograms])

  const actions = useReservationActions(serverReservedProgramIds)

  // autoLoadFailed: 直近の自動読み込み（進行方向）が失敗したか。失敗したら
  // ボタン + エラー表示に落とし、番兵が可視のままでも自動では再試行しない
  // （さもないと失敗したまま無限にリクエストを投げ続ける）。
  const [autoLoadFailed, setAutoLoadFailed] = useState(false)

  // クエリの窓（起点・上限・絞り込み）が変わったら新しいセッションとして扱い、
  // 前の窓での失敗を引きずらない。
  useEffect(() => {
    setAutoLoadFailed(false)
  }, [originMs, limitMs, legacyNetworkIdParam, legacyServiceIdParam, selectedServiceParam])

  // 日付ジャンプ（DayStrip・容量バッジ）で originMs が変わったら、リスト表示では
  // スクロールを先頭へ戻して選んだ日の先頭行に着地させる。上の
  // `placeholderData` で前のリストを残すようにしたぶん、以前スケルトン差し替えの
  // 副作用（文書高さがビューポートまで潰れ scrollY がクランプされる）で起きていた
  // 「先頭へ戻る」が起きなくなるので、ここで明示的に行う（さもないと前の日の
  // scrollY を引き継いだまま新しい日のリストの途中に着地する）。初回マウントでは
  // 動かさない（既に先頭）。グリッド表示はスクロール位置を `scrollToMs` が別に
  // 決めるので除外する。計測できない環境（jsdom）では仮想化そのものを
  // バイパスしているので何もしない（`components/program-list.tsx`）。
  //
  // `window.scrollTo` は `window.scrollY` を同期更新するが、`virtualizer` が
  // 可視範囲に使う内部スクロール位置は非同期の 'scroll' イベントでしか更新
  // されない（遡行のアンカー復元と同じ間隙。`docs/frontend/scroll.md`）。
  // 直後に 'scroll' を同期発火させて、ペイント前に `virtualizer` を y=0 へ
  // 追いつかせる。
  const previousOriginMsRef = useRef(originMs)
  useLayoutEffect(() => {
    if (previousOriginMsRef.current === originMs) return
    previousOriginMsRef.current = originMs
    if (showGrid || !domLayoutMeasurable()) return
    window.scrollTo(0, 0)
    window.dispatchEvent(new Event('scroll'))
  }, [originMs, showGrid])

  useEffect(() => {
    if (query.isFetchNextPageError) setAutoLoadFailed(true)
  }, [query.isFetchNextPageError])

  // IntersectionObserver のコールバックは、番兵を張り直さなくても常に最新の
  // 状態を読めるよう ref 経由で渡す（effect の再生成は showGrid が変わる
  // ときだけにしたい —— sentinel の DOM 有無が変わるタイミングと合わせる）。
  const autoLoadStateRef = useRef({
    hasNextPage: query.hasNextPage,
    isFetchingNextPage: query.isFetchingNextPage,
    autoLoadFailed,
    fetchNextPage: query.fetchNextPage,
  })
  useEffect(() => {
    autoLoadStateRef.current = {
      hasNextPage: query.hasNextPage,
      isFetchingNextPage: query.isFetchingNextPage,
      autoLoadFailed,
      fetchNextPage: query.fetchNextPage,
    }
  })

  const sentinelRef = useRef<HTMLDivElement>(null)

  // 番兵の <div> は一覧が実際に描かれたとき（!isPending && visiblePrograms.length
  // > 0）にしか存在しない。データ取得が終わる前に IntersectionObserver を
  // 組み立てる effect（`[showGrid]` だけに依存する形）だと、初回マウント時点では
  // sentinelRef.current がまだ null で、以後 showGrid が変わらない限り
  // 二度と組み立て直されない ---
  // つまり自動読み込みが永遠に発火しない。番兵が実際に DOM にあるかどうかを
  // 明示的な依存にして、描画されたタイミングで確実に組み立て直す。
  const sentinelMounted = !showGrid && !query.isPending && visiblePrograms.length > 0

  useEffect(() => {
    if (!sentinelMounted) return
    // 計測できない環境（jsdom 等）では番兵が常時可視と判定されるおそれが
    // あるので、IntersectionObserver そのものを作らない。この環境では
    // 「さらに読み込む」ボタンだけを受け皿にする
    // （`lib/list-virtualization.ts` の `domLayoutMeasurable()`）。
    if (!domLayoutMeasurable()) return
    const node = sentinelRef.current
    if (!node) return

    const observer = new IntersectionObserver((entries) => {
      const isIntersecting = entries.some((entry) => entry.isIntersecting)
      const state = autoLoadStateRef.current
      if (
        shouldAutoLoadNextPage({
          isIntersecting,
          autoLoadAvailable: true,
          autoLoadFailed: state.autoLoadFailed,
          hasNextPage: state.hasNextPage,
          isFetchingNextPage: state.isFetchingNextPage,
        })
      ) {
        void state.fetchNextPage()
      }
    })
    observer.observe(node)
    return () => observer.disconnect()
  }, [sentinelMounted])

  // 遡行（前の窓の読み込み）は `query.fetchPreviousPage()` を呼ぶだけ。
  // 先頭への挿入でスクロール位置がずれないようにする補正（アンカーの位置
  // 合わせ）は `ProgramList`（`components/program-list.tsx`）側が
  // `hasPreviousPage` / `onLoadPrevious` 経由で持つ ---
  // 復元に必要な情報（仮想化の添字・計測値・`virtualizer`）が全部そちらに
  // あるため。以前はここ（`pages/programs.tsx`）に DOM アンカーを
  // `document.querySelector` で挿入後に再取得する方式を置いていたが、
  // 仮想化の可視範囲の再計算でアンカー要素が DOM から消えてしまい機能しな
  // かった（`lib/scroll-preservation.ts` のコメント参照）。
  const loadPrevious = () => {
    void query.fetchPreviousPage()
  }

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
              services={pickerServices}
              selected={selectedServiceKeys}
              keyOf={(service) => programServiceKey(service.networkId, service.serviceId)}
              onChange={(next) => {
                const exact = [...next]
                  .map((key) => parseProgramServiceKey(key))
                  .filter((ref): ref is { networkId: number; serviceId: number } => ref !== undefined)
                  .sort((a, b) => a.networkId - b.networkId || a.serviceId - b.serviceId)
                  .map((ref) => programServiceKey(ref.networkId, ref.serviceId))
                updateSearch((s) => ({
                  ...s,
                  networkId: undefined,
                  service: exact.length > 0 ? exact : undefined,
                  serviceId: undefined,
                }))
              }}
            />
            {/* 表示形式の切り替えは `lg` 以上でのみ出す。CSS で隠すのではなく
                出さないのは、モバイルに存在しない選択肢を読み上げさせないため */}
            {wideScreen && <ViewChips view={view} onSelect={setView} />}
          </>
        }
      >
        {/* current はグリッド表示中はジャンプ先（dayOffset）をそのまま渡す
            （グリッドは軸が 24 時間固定で、スクロールで日をまたがないため）。
            リスト表示中はスクロール位置から導出した「いま見ている日」
            （visibleDay）を渡す。 */}
        <DayStrip
          current={showGrid ? dayOffset : visibleDay}
          days={selectableDays}
          onSelect={selectDay}
        />
      </PageHeader>

      {showGrid ? (
        <ProgramGridView
          axis={axis}
          programs={gridPrograms}
          services={gridServices}
          serviceByKey={serviceByKey}
          overages={overages}
          actions={actions}
          scrollToMs={scrollToMs}
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
          ) : (
            <>
              {/* 遡行の失敗表示。ボタン自体は ProgramList 側（下記）が持つが、
                  失敗の有無は query から直接分かるのでここに残す。 */}
              {query.isFetchPreviousPageError && (
                <p className="px-4 pt-4 text-center text-sm text-destructive">
                  前の読み込みに失敗しました
                </p>
              )}
              {visiblePrograms.length === 0 && (
                <EmptyState>この時間帯の番組がありません</EmptyState>
              )}
              {/* 遡行はボタンでのみ行う（上スワイプなどのジェスチャにしない）。
                  理由は 2 つ: (1) Android Chrome の pull-to-refresh がページ最上端
                  でのオーバースクロールを占有しており衝突する（前述「M2 のグリッドで
                  横スワイプによるナビゲーションを使わない」と同じ種類の衝突）、
                  (2) 上方向の自動読み込みは、先頭に差し込んだ直後も番兵が上端付近に
                  残るため境界（now）まで連鎖してしまう。下限に達すると
                  `hasPreviousPage` が false になりボタンごと消える。
                  ボタン自体とその押下時のスクロール位置復元は `ProgramList`
                  （仮想化を持っているコンポーネント）に移してある。 */}
              <ProgramList
                ref={programListRef}
                programs={visiblePrograms}
                serviceByKey={serviceByKey}
                actions={actions}
                // プレースホルダ表示中（未キャッシュ日へジャンプして新しい日の
                // データを待っている間）は前の日のデータが出ているので、その
                // 可視範囲から「いま見ている日」を通知させない ---
                // させると DayStrip のハイライトが跳んだ先から前の日へ一瞬
                // 戻ってしまう。ジャンプ先は既に `selectDay` が `visibleDay` に
                // 反映済みで、新しい日が届けば通知が再開して一致する。
                onVisibleDayChange={query.isPlaceholderData ? undefined : setVisibleDay}
                now={nowMs}
                hasPreviousPage={query.hasPreviousPage}
                isFetchingPreviousPage={query.isFetchingPreviousPage}
                previousDateLabel={previousDateLabel}
                onLoadPrevious={loadPrevious}
              />
            </>
          )}

          {/* 番兵。進行方向の自動読み込み（IntersectionObserver）はこれを見る。
              計測できない環境では監視対象を作らないだけで、要素自体は無害
              なので出したままにする。 */}
          {!query.isPending && visiblePrograms.length > 0 && (
            <div ref={sentinelRef} aria-hidden className="h-px" />
          )}

          {shouldShowLoadMoreButton({
            hasNextPage: query.hasNextPage,
            autoLoadAvailable: domLayoutMeasurable(),
            autoLoadFailed,
          }) && (
            <div className="px-4 py-6">
              {query.isFetchNextPageError && (
                <p className="pb-2 text-center text-sm text-destructive">
                  続きの取得に失敗しました
                </p>
              )}
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
 *
 * `reserve` は `overrides` を受け取ると intent の PUT に続けて
 * overrides の PATCH も呼ぶ（issue #132）。#29 の決定どおり intent の
 * ボディは `action` のみのまま変えず、overrides は別リクエストにする ---
 * ただし UI からは「予約」ボタン 1 回の操作に見える。overrides の PATCH が
 * 失敗しても予約自体（intent）は成立しているので、その旨を分けてトーストで示す。
 */
function useReservationActions(serverReservedIds: ReadonlySet<number>): ReservationActions {
  const site = useCurrentSite()
  const queryClient = useQueryClient()
  const toast = useToast()
  const putIntent = usePutProgramIntent()
  const patchOverrides = usePatchProgramOverrides()

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
      { site, programId, data: { action: 'skip' } },
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

  // 予約作成（PUT .../intent）自体は action のみのまま変更しない（issue #29 の
  // 決定）。overrides は別 PATCH のまま呼ぶが、ProgramRow の展開パネルで
  // encodeProfiles / keepOriginal を触っていなければ overrides は
  // `undefined` で渡ってくるので、この場合は PATCH 自体を呼ばない ---
  // 呼ぶと「既定のまま」という意味の無い override 行を作ってしまう
  // （不変条件 10）。UI 上は「予約」ボタン 1 回の操作に見せる（issue #132）。
  const reserve = (program: ProgramListItem, overrides?: ProgramOverridesInput) => {
    setBusy(program.programId, true)
    setOptimisticReserved(program.programId, true)
    void (async () => {
      try {
        await putIntent.mutateAsync({
          site,
          programId: program.programId,
          data: { action: 'record' },
        })
        invalidateReservations()

        if (overrides === undefined) {
          // 確認ダイアログを挟まない代わりに、直後に取り返せるようにする
          toast({
            message: `予約しました: ${program.name}`,
            action: { label: '取消', onClick: () => cancel(program.programId) },
          })
          return
        }

        // overrides の PATCH は `program_snapshots (site, programId)` への FK を
        // 要求する。EPG プロジェクションに無い番組（想定上は起こりにくいが、
        // ここに来る時点で番組表に出ているので通常は満たされる）だと 400 になり
        // うるので、予約自体の成功とは切り離してハンドルする ---
        // 予約は成立しているので「予約に失敗しました」にはしない。
        try {
          await patchOverrides.mutateAsync({
            site,
            programId: program.programId,
            data: overrides,
          })
          toast({
            message: `予約しました（エンコード設定つき）: ${program.name}`,
            action: { label: '取消', onClick: () => cancel(program.programId) },
          })
        } catch (err) {
          toast({
            message: `予約はできましたが、エンコード設定の保存に失敗しました: ${
              apiErrorMessage(err) ?? '不明なエラー'
            }`,
          })
        }
      } catch {
        toast({ message: '予約に失敗しました' })
        setOptimisticReserved(program.programId, undefined)
      } finally {
        setBusy(program.programId, false)
      }
    })()
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
  serviceByKey,
  overages,
  actions,
  isPending,
  isError,
  scrollToMs,
}: {
  axis: TimeAxis
  programs: ProgramListItem[]
  services: Service[]
  serviceByKey: Map<string, Service>
  /** チューナーが不足している区間。番組ではなく区間として帯に描く（M2-10）。 */
  overages: readonly CapacityOverage[]
  actions: ReservationActions
  isPending: boolean
  isError: boolean
  /** グリッドの初期スクロール先（issue #233 M6-5）。`ProgramGrid` にそのまま渡す。 */
  scrollToMs?: number
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
          {/* key を選択中の programId にする --- 番組を選び直しても同じ木の
              位置なのでコンポーネントは再マウントされず、`ProgramRow` が
              持つエンコード設定の下書き（issue #132）が前に選んでいた
              番組のまま残ってしまう。key で強制的に作り直す。 */}
          <ProgramRow
            key={selected.programId}
            program={selected}
            serviceName={serviceByKey.get(programServiceKey(selected.networkId, selected.serviceId))?.name}
            reserved={actions.reservedProgramIds.has(selected.programId)}
            pending={actions.isBusy(selected.programId)}
            onReserve={(overrides) => actions.reserve(selected, overrides)}
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
          scrollToMs={scrollToMs}
          // 帯はセルより上・ヘッダより下の層に入る。軸を受け取って同じ
          // spanToPx を通すので、帯と番組セルは同じ時刻で必ず同じ位置に来る
          overlay={(gridAxis) => <CapacityBands axis={gridAxis} overages={overages} />}
        />
      </div>
    </div>
  )
}
