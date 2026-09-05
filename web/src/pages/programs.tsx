import { keepPreviousData, useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'

import { CapacityBandLabels, CapacityBands } from '@/components/capacity-band'
import { ChannelPicker } from '@/components/channel-picker'
import { DayStrip } from '@/components/day-strip'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { GenreLegend, ProgramGrid } from '@/components/program-grid'
import {
  ProgramList,
  type ProgramListHandle,
  type ReservationActions,
} from '@/components/program-list'
import { ProgramRow } from '@/components/program-row'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import {
  listPrograms,
  useListCapacityOverages,
  useListReservations,
  type CapacityOverage,
  type Reservation,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { shouldAutoLoadNextPage, shouldShowLoadMoreButton } from '@/lib/auto-load'
import { dayOffsetForMs, dayOrigin } from '@/lib/day-offset'
import { orderServices, type TimeAxis } from '@/lib/epg-grid'
import {
  programsQueryKeyPrefix,
} from '@/lib/events'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import { useReservationActions } from '@/lib/reservation-actions'
import {
  programIdentity,
  siteServiceKey,
  useAllSitesServices,
  type SiteProgram,
  type SiteService,
} from '@/lib/all-sites-services'
import {
  pickerServiceDomain,
  programsDayForOffset,
  programsDayOffset,
  programsSelectableDays as selectableDays,
  type ProgramsPageSearch,
} from '@/lib/programs-search'
import { filterProgramsFromListStart } from '@/lib/program-list'
import { lgMediaQuery, useMediaQuery } from '@/lib/use-media-query'

/**
 * windowHours は、進行方向（下スクロールでの自動読み込み・「さらに読み込む」）
 * の 1 回のスクロールステップで取得する時間窓の幅。
 *
 * API はページネーショントークンを持たず、時間窓そのものがカーソルになる。
 * 「次のページ」= 前回の end を start にした次の窓。
 */
const windowHours = 6

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
 * 容量不足バッジ（予約一覧）からは `?view=grid&at=<epoch ms>` 付きで飛べる
 * （issue #233 M6-5、`view` の URL 化は issue #437）。`lg` 以上ではその `view`
 * がそのままグリッドへの切り替えになりその時刻へスクロールし、それ以外
 * （リスト・`lg` 未満）では「その時刻が属する日」への日付ジャンプに留める
 * （下記 `at` 関連の effect 参照）。
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
  // `at` の日判定・取得窓・子リストの判定を同じレンダーの時刻で揃える。
  // oxlint-disable-next-line react/purity -- EPG の現在時刻スナップショットが必要
  const nowMs = Date.now()
  const {
    sites,
    services: serviceList,
    siteServices,
    isPending: servicesPending,
    isError: servicesError,
    refetch: refetchServices,
  } = useAllSitesServices()
  // チャンネル絞り込み・表示形式・ジャンプ先の日付は URL に持つ。
  // 検証（不正値・範囲外の除去・配列の重複除去・昇順ソート）は `routes.tsx` の
  // `validateSearch`（`lib/programs-search.ts` の `parseProgramsSearch`）で済んで
  // いるので、ここでは信頼して使う。
  const search = useRouteSearch({ from: '/programs' })
  const navigate = useNavigate()
  const updateSearch = (updater: (prev: ProgramsPageSearch) => ProgramsPageSearch) => {
    // 選ぶたびに URL を書き換えるが、history は汚さない（`replace`。
    // `lib/recording-search.ts` の絞り込み更新と同じ規律）。
    //
    // **updater の引数は「全ルートの search を合成した型」で来る**
    // （TanStack Router の `ParamsReducerFn`）。`/live` が同じ名前の `service`
    // を単数（`number`）で持つため（issue #438）、合成後は型上
    // `number | number[]` になり `ProgramsPageSearch` にそのままは代入できない。
    // この関数が呼ばれるのは `/programs`（番組表）に居るときだけで、そのとき
    // 実際に入っているのは `parseProgramsSearch` が検証した形なので、ここで
    // 絞ってから updater に渡す（`pages/recordings.tsx` の `updateSearch` と
    // 同じ形）。
    void navigate({
      to: '/programs',
      search: (prev) => updater(prev as ProgramsPageSearch),
      replace: true,
    })
  }
  // dayOffset は「ジャンプ先」（DayStrip をタップして跳ぶ先）。0 以上
  // selectableDays 未満。0 は今日で、リストは常にここから連続フィードとして
  // 始まる（`今` という別枠の選択肢は無い）。URL の `day` から初期化し、
  // `at` があれば下の effect がその日で上書きする。
  const [dayOffset, setDayOffset] = useState(() => programsDayOffset(search.day, nowMs))
  // visibleDay は「いま見ている日」（ProgramList がスクロール位置から導出して
  // 通知する）。DayStrip のハイライトはこちらを見る。ジャンプ直後は dayOffset と
  // 一致するが、その後リストをスクロールすればこちらだけが動く。
  const [visibleDay, setVisibleDay] = useState(dayOffset)
  // view は表示形式（グリッド / リスト）。URL 化してある（`search.view`）。
  // 既定はリスト --- 容量不足バッジが `view: 'grid'` を明示したときはこの値が
  // 直ちに 'grid' になるが、実際にグリッドが出るかは `showGrid`（下記）が
  // 決める。`showGrid` は `wideScreen`（`useMediaQuery`）の判定を待つため、
  // グリッドのマウント自体は初回レンダーより 1 レンダー遅れる
  // （docs/frontend/programs.md「番組表への `at` 導線」参照）。
  const view: ProgramView = search.view ?? 'list'

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
    updateSearch((s) => ({ ...s, day: programsDayForOffset(offset, Date.now()) }))
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
  const atDayOffset = at === undefined ? undefined : dayOffsetForMs(at, nowMs, selectableDays)

  // at が指す日へ「いま見ている日」を合わせる。グリッドの有無・表示形式に
  // 関わらず効かせる --- リスト表示中・`lg` 未満（グリッドが出ない）画面では
  // 帯で「その時間帯」を直接見せる手段が無いため、次善として日だけ合わせる
  // のがこの導線の唯一の反映先になる。
  // at が指す日へ「いま見ている日」を合わせる。`day` より at を優先する
  // （容量不足バッジは「この時間帯を見たい」という要求そのものなので、共有 URL の
  // `day` と矛盾しても at の日を勝たせる。`day` は URL からは消さない）。
  useEffect(() => {
    if (atDayOffset === undefined) return
    // URL の at を画面状態へ反映する同期。URL を正本としており、render 中に
    // state を書くと hooks の render 更新になるため effect が必要。
    // oxlint-disable-next-line react/set-state-in-effect -- URL のジャンプ先を画面状態へ同期する
    setDayOffset(atDayOffset)
    // oxlint-disable-next-line react/set-state-in-effect -- URL のジャンプ先を画面状態へ同期する
    setVisibleDay(atDayOffset)
  }, [atDayOffset])

  // グリッドは `lg` 以上でのみ出す。モバイルは常にリストのまま
  // （docs/frontend.md「リストを第一級に置く。グリッドはその上に足す」）。
  // view は画面幅で捨てないので、幅が戻ればグリッドに戻る。
  const wideScreen = useMediaQuery(lgMediaQuery)
  const showGrid = wideScreen && view === 'grid'

  // `at` を URL から消費・削除する方式は採らない（レビュー指摘 nit 4 の素朴な
  // 実装で一度試して実機で退行を確認したため）。`navigate` で `at` を消す
  // effect は非同期に解決し、グリッドが実際に軸を確定してスクロールを適用する
  // より先に `at` が消えてしまうことがあった --- 結果、肝心の初回スクロールが
  // 「今」にしか効かなくなった（e2e `web/e2e/badge-links.mjs` の②が実際に
  // 落ちて発覚した）。代わりに下記 `scrollToMs` を `dayOffset === atDayOffset`
  // で条件付けることで、「at は現在地の日を離れたら自動的に効かなくなる」を
  // URL を書き換えずに実現する --- 「今日」ボタンを押す（`dayOffset` が変わる）
  // だけで scrollToMs は自然に `undefined` に戻り、以後の軸変更は「今」へ
  // スクロールする既定の挙動に戻る。
  //
  // scrollToMs はグリッドの初期スクロール先。**`dayOffset` が `at` の指す日と
  // 一致している間だけ** `at` を渡す --- 一致しなくなった（「今日」ボタンや
  // 日付ストリップで別の日へ移った）後まで古い `at` を渡し続けると、以後の
  // 軸変更のたびに「今」ではなく `at` の位置へ戻ってしまう（実測。上記コメント
  // 参照）。`at` 自体は URL に残ったままだが、実際に効くのは「その日を見ている
  // 間の最初の 1 回」に限られる。
  const scrollToMs = at !== undefined && dayOffset === atDayOffset ? at : undefined

  const allServices = serviceList
  const selectedServiceIds = useMemo(() => new Set(search.service ?? []), [search.service])
  const reservations = useListReservations()

  // nowMs はこのレンダーの間で一貫させる。起点・上限・下限をそれぞれ別々に
  // Date.now() を呼んで求めると、ミリ秒単位でずれた「今」が混ざりうる。
  // 起点はジャンプ先（state）から決める。queryKey に入るので、日付を変えると
  // ページが積み直され、キャッシュ済みのページが古い窓のまま再利用されることもない。
  const originMs = dayOrigin(dayOffset, nowMs).getTime()
  // 上限はどの選択でも共通の「EPG のローリングウィンドウの終端」
  // （8 日先の 0 時）。日付を選んでも 24 時で打ち切らない —— 連続フィードなので、
  // 選んだ日から先もそのまま読み続けられる。
  const limitMs = dayOrigin(selectableDays, nowMs).getTime()
  // 下限は「now を時で切り捨てた時刻」。放送済み番組の閲覧は今回のスコープ外
  // なので、それより前の窓は取りに行かない。`filterProgramsFromListStart` の
  // 「今日は絞り込まない」判定に使う。
  const lowerBoundMs = dayOrigin(0, nowMs).getTime()

  // API へ渡すサービス絞り込み（`Service.id`。複数は OR）。
  const selectedServiceParam = search.service

  // サーバーが選択に応じて絞るようになったので、queryKey にも選択を入れる。
  // 入れないと別の選択で取得した結果をそのまま再利用してしまう（日付や時間窓と
  // 同じ「結果を左右するパラメータ」になったため）。
  //
  // pageParam / ページの形は「取得した半開区間 [startMs, endMs)」そのもの
  // （`step` のような抽象的なカーソルにしない）。
  const query = useInfiniteQuery({
    queryKey: [
      programsQueryKeyPrefix,
      'infinite',
      sites,
      originMs,
      limitMs,
      selectedServiceParam,
    ],
    initialPageParam: {
      startMs: originMs,
      endMs: Math.min(originMs + windowHours * 3600_000, limitMs),
    },
    // グリッド表示中はリストの窓を追いかけない（同じ時間帯を 2 つの形で
    // 同時に取りに行かない）。戻ったときはキャッシュがそのまま出る。
    enabled: !showGrid && sites.length > 0,
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
      const responses = await Promise.all(
        sites.map((site) =>
          listPrograms(site, {
            start: new Date(startMs).toISOString(),
            end: new Date(endMs).toISOString(),
            service: selectedServiceParam,
          }).then((res) =>
            (unwrap(res) ?? []).map((program): SiteProgram => ({ ...program, site })),
          ),
        ),
      )
      return { startMs, endMs, programs: responses.flat() }
    },
    // 進行方向は windowHours（6 時間）ぶんずつ。上限（EPG のローリングウィンドウの
    // 終端）に達したら打ち切る。
    getNextPageParam: (last) => {
      if (last.endMs >= limitMs) return undefined
      const startMs = last.endMs
      return { startMs, endMs: Math.min(startMs + windowHours * 3600_000, limitMs) }
    },
  })

  // グリッドは 24 時間ぶんを 1 回で取る。リストのような窓の積み上げにしないのは、
  // 縦位置が時刻そのものなので途中まで積んだ状態が「番組がない時間帯」と
  // 見分けられないため。
  const gridEndMs = Math.min(originMs + gridWindowHours * 3600_000, limitMs)
  const gridQuery = useQuery({
    queryKey: [programsQueryKeyPrefix, 'grid', sites, originMs, gridEndMs, selectedServiceParam],
    enabled: showGrid && sites.length > 0,
    queryFn: async () => {
      const responses = await Promise.all(
        sites.map((site) =>
          listPrograms(site, {
            start: new Date(originMs).toISOString(),
            end: new Date(gridEndMs).toISOString(),
            service: selectedServiceParam,
          }).then((res) =>
            (unwrap(res) ?? []).map((program): SiteProgram => ({ ...program, site })),
          ),
        ),
      )
      return responses.flat()
    },
  })
  // サーバーが選択済みのサービスで絞るので、これ以上の適用点は要らない。
  const gridPrograms = useMemo(() => gridQuery.data ?? [], [gridQuery.data])
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
  // 一覧 API は全サイトの区間を返す。各帯は同じ site の番組列に重ねるので
  // site を落とさず CapacityBands へ渡す。
  const overages = useMemo(() => unwrap(overagesQuery.data) ?? [], [overagesQuery.data])

  // 窓は開区間なので境界をまたぐ番組が隣接する 2 つの窓に現れる。programId で潰す。
  // サーバーが選択済みのサービスで絞るので、これ以上の適用点は要らない。
  const programs = useMemo(() => {
    const seen = new Map<string, SiteProgram>()
    for (const page of query.data?.pages ?? []) {
      for (const p of page.programs) {
        if (!seen.has(programIdentity(p.site, p.programId))) {
          seen.set(programIdentity(p.site, p.programId), p)
        }
      }
    }
    return [...seen.values()].sort(
      (a, b) => new Date(a.startAt).getTime() - new Date(b.startAt).getTime(),
    )
  }, [query.data])

  // listStartMs は「読み込み済みの最も手前の窓の開始時刻を下限（now を時で
  // 切り捨てた時刻）で clamp したもの」。**`originMs` をそのまま渡すのは誤り**
  // ---
  // `getPreviousPageParam` が無い今、定常状態では `pages[0].startMs` は常に
  // `originMs` と一致するが、日付ジャンプ直後の `placeholderData`
  // （`keepPreviousData`）中は `programs`（`query.data.pages` 由来）がまだ
  // ジャンプ前の日の番組のままなのに `originMs` だけが新しい日へ先に進む。
  // ここで `originMs` を直接使うと、`filterProgramsFromListStart` が
  // 「まだ前の日のままの programs」を「新しい日の originMs」で絞り込むことになり、
  // 前の日の番組が（新しい日の起点よりずっと前に始まっているため）全滅して
  // 一瞬 `EmptyState` 相当の高さ（800px = viewport）まで潰れる回帰を実機の
  // `web/e2e/checks.mjs`（①）で確認した。`query.data` 由来の
  // `pages[0].startMs` を経由すれば、placeholder 中は「まだ前の日を表示している」
  // という事実と足並みが揃う。
  const listStartMs = useMemo(() => {
    const rawFirstStartMs = query.data?.pages[0]?.startMs ?? originMs
    return Math.max(rawFirstStartMs, lowerBoundMs)
  }, [query.data, originMs, lowerBoundMs])

  // API は問い合わせた時間窓に重なる番組を返す（`start_at < window_end AND
  // end_at > window_start`）ため、先頭の窓の開始時刻より前に始まった番組
  // （＝前日からの重なり）がリストの先頭に混ざる。これを見せたままだと
  // 日付ヘッダと「いま見ている日」がどちらも前日を指す（実機で確認済みの
  // 不具合）。`listStartMs` が下限（`lowerBoundMs`）と一致するとき
  // （＝今日を見ている）は例外的に絞り込まない --- 放送中の番組を隠さないため。
  // 判定は `lib/program-list.ts` の純関数。
  const visiblePrograms = useMemo(
    () => filterProgramsFromListStart(programs, listStartMs, lowerBoundMs),
    [programs, listStartMs, lowerBoundMs],
  )

  // 絞り込む前の全サービスから作る。絞った側（filterableServices）から作ると、
  // hasPrograms が false の局の番組が来たとき（例えば選択直後にキャッシュが
  // まだ古い）名前が引けなくなる。
  const serviceById = useMemo(() => {
    const map = new Map<number, SiteService>()
    for (const service of siteServices) {
      if (!map.has(service.id)) map.set(service.id, service)
    }
    return map
  }, [siteServices])
  const siteServiceByKey = useMemo(() => {
    const map = new Map<string, SiteService>()
    for (const service of siteServices) {
      map.set(siteServiceKey(service.site, service.networkId, service.serviceId), service)
    }
    return map
  }, [siteServices])

  // 予約状態は番組とは別クエリで取り、クライアント側で結合する。
  // 予約は頻繁に変わり番組はほとんど変わらないので、キャッシュの寿命を分ける。
  // リストとグリッドはこの同じ Set を見るので、表示形式で予約状態がずれない。
  //
  // 意図（PUT .../intent）は reservations 行を同期的に作らない（issue #29）ので、
  // サーバーの値だけを見ると予約直後の一覧に反映が数秒遅れる。actions.isReserved
  // が楽観的な上書きをこの Set の上に重ねる。
  //
  // 一覧は全サイトの予約を返す（不変条件 1）。番組表の行も site を運ぶので、
  // 予約状態は site:programId で突き合わせる。
  const serverReservedProgramIds = useMemo(() => {
    const set = new Set<string>()
    for (const r of unwrap(reservations.data) ?? []) {
      set.add(programIdentity(r.site, r.programId))
    }
    return set
  }, [reservations.data])

  // Undo（取消の打ち消し）がルール由来か手動かで送る intent を変える必要が
  // あるため（`useReservationActions` の `revive` 参照）、site:programId から
  // `reservation.source` を引けるようにしておく。同じ `reservations.data` から
  // 作るので、`serverReservedProgramIds` と同じ identity の規則を揃える。
  const reservationSourceByProgramId = useMemo(() => {
    const map = new Map<string, Reservation['source']>()
    for (const r of unwrap(reservations.data) ?? []) {
      map.set(programIdentity(r.site, r.programId), r.source)
    }
    return map
  }, [reservations.data])

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
  // ピッカーへ渡す。選択中の id は候補に無くても表示上は残し、
  // 同じ serviceId の別 network を同じ候補へ潰さない。
  const pickerServices = useMemo(
    () => pickerServiceDomain(filterableServices, selectedServiceIds, serviceById),
    [filterableServices, selectedServiceIds, serviceById],
  )

  // グリッドの列。番組を 1 つも持たないサービスは列にしない（空の列が数十本
  // 並ぶと、隣り合う番組の同時性が読み取れなくなる）。並び順は全順序なので
  // 再描画で列が入れ替わらない。
  const gridServices = useMemo(() => {
    const withPrograms = new Set(
      gridPrograms.map((program) => siteServiceKey(program.site, program.networkId, program.serviceId)),
    )
    return orderServices(
      siteServices.filter((service) =>
        withPrograms.has(siteServiceKey(service.site, service.networkId, service.serviceId)),
      ),
    )
  }, [gridPrograms, siteServices])

  const actions = useReservationActions(serverReservedProgramIds, reservationSourceByProgramId)

  // autoLoadFailed: 直近の自動読み込み（進行方向）が失敗したか。失敗したら
  // ボタン + エラー表示に落とし、番兵が可視のままでも自動では再試行しない
  // （さもないと失敗したまま無限にリクエストを投げ続ける）。
  // クエリの窓（起点・上限・絞り込み）が変わったら新しいセッションとして扱い、
  // 前の窓での失敗を引きずらない。
  const autoLoadFailed = query.isFetchNextPageError

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
  // 可視範囲に使う内部スクロール位置はブラウザが発火する 'scroll' イベント
  // （`window.scrollTo` に対して非同期。早くても次のフレーム）を受けてはじめて
  // 更新される。直後に 'scroll' を同期発火させることで、イベントリスナー
  // （`virtualizer` が登録している）をその場で呼び、ペイント前に `virtualizer`
  // を y=0 へ追いつかせる。
  const previousOriginMsRef = useRef(originMs)
  useLayoutEffect(() => {
    if (previousOriginMsRef.current === originMs) return
    previousOriginMsRef.current = originMs
    if (showGrid || !domLayoutMeasurable()) return
    window.scrollTo(0, 0)
    window.dispatchEvent(new Event('scroll'))
  }, [originMs, showGrid])

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
              selected={selectedServiceIds}
              onChange={(next) => {
                // 昇順に正準化する（`Set` の反復順は選び方の履歴に依存し、
                // 順序が揺れると同じ選択でも queryKey / URL が変わる）。
                const ids = [...next].sort((a, b) => a - b)
                updateSearch((s) => ({ ...s, service: ids.length > 0 ? ids : undefined }))
              }}
            />
            {/* 表示形式の切り替えは `lg` 以上でのみ出す。CSS で隠すのではなく
                出さないのは、モバイルに存在しない選択肢を読み上げさせないため */}
            {wideScreen && (
              <ViewChips
                view={view}
                onSelect={(next) => updateSearch((s) => ({ ...s, view: next }))}
              />
            )}
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
          serviceById={siteServiceByKey}
          overages={overages}
          actions={actions}
          scrollToMs={scrollToMs}
          showSite={sites.length > 1}
          // グリッドではサービスが列そのもの（構造）なので、リストと違って
          // サービスの取得失敗を「名前が出ないだけ」に落とせない。列が 0 本の
          // グリッドは「番組がない」と見分けがつかないので、取得状態を合わせる
          isPending={gridQuery.isPending || servicesPending}
          isError={gridQuery.isError || servicesError}
          onRetry={() => {
            void gridQuery.refetch()
            void refetchServices()
          }}
        />
      ) : (
        <PageContent>
          {query.isError || servicesError ? (
            <ErrorState
              onRetry={() => {
                void query.refetch()
                void refetchServices()
              }}
            >
              番組の取得に失敗しました
            </ErrorState>
          ) : query.isPending || servicesPending ? (
            <ListSkeleton />
          ) : (
            <>
              {visiblePrograms.length === 0 && (
                <EmptyState>この時間帯の番組がありません</EmptyState>
              )}
              <ProgramList
                ref={programListRef}
                programs={visiblePrograms}
                serviceById={siteServiceByKey}
                showSite={sites.length > 1}
                actions={actions}
                // プレースホルダ表示中（未キャッシュ日へジャンプして新しい日の
                // データを待っている間）は前の日のデータが出ているので、その
                // 可視範囲から「いま見ている日」を通知させない ---
                // させると DayStrip のハイライトが跳んだ先から前の日へ一瞬
                // 戻ってしまう。ジャンプ先は既に `selectDay` が `visibleDay` に
                // 反映済みで、新しい日が届けば通知が再開して一致する。
                onVisibleDayChange={query.isPlaceholderData ? undefined : setVisibleDay}
                now={nowMs}
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
        </PageContent>
      )}
    </>
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
  onRetry,
  scrollToMs,
  showSite,
}: {
  axis: TimeAxis
  programs: SiteProgram[]
  services: SiteService[]
  serviceById: Map<string, SiteService>
  /** チューナーが不足している区間。番組ではなく区間として帯に描く（M2-10）。 */
  overages: readonly CapacityOverage[]
  actions: ReservationActions
  isPending: boolean
  isError: boolean
  /** isError のときの再試行（番組・チャンネルの両方の取得を取り直す）。 */
  onRetry: () => void
  /** グリッドの初期スクロール先（issue #233 M6-5）。`ProgramGrid` にそのまま渡す。 */
  scrollToMs?: number
  showSite: boolean
}) {
  const [selectedProgramId, setSelectedProgramId] = useState<string | null>(null)

  // 日付やサービスを変えると選択中の番組が消えることがある。id ではなく
  // 実体を引き直して、消えていれば選択も無かったことにする。
  const selected = programs.find((p) => programIdentity(p.site, p.programId) === selectedProgramId)

  if (isError) return <ErrorState onRetry={onRetry}>番組の取得に失敗しました</ErrorState>
  if (isPending) return <ListSkeleton />
  if (programs.length === 0) return <EmptyState>この時間帯の番組がありません</EmptyState>

  return (
    <div
      className="flex flex-col"
      // 高さの予算はここで決める。ページ全体がスクロールするとグリッドの
      // ヘッダ（sticky）が画面外へ出てしまう。
      style={{
        height:
          'calc(100dvh - var(--page-header-height, 0px) - var(--sticky-banners-height, 0px))',
      }}
    >
      <GenreLegend />
      {selected && (
        <div className="shrink-0 border-b border-border bg-card">
          {/* key を選択中の programId にする --- 番組を選び直しても同じ木の
              位置なのでコンポーネントは再マウントされず、`ProgramRow` が
              持つエンコード設定の下書き（issue #132）が前に選んでいた
              番組のまま残ってしまう。key で強制的に作り直す。 */}
          <ProgramRow
            key={programIdentity(selected.site, selected.programId)}
            program={selected}
            siteName={showSite ? selected.site : undefined}
            serviceName={
              serviceById.get(
                siteServiceKey(selected.site, selected.networkId, selected.serviceId),
              )?.name
            }
            reserved={actions.reservedProgramIds.has(
              programIdentity(selected.site, selected.programId),
            )}
            pending={actions.isBusy(selected)}
            onReserve={(overrides) => actions.reserve(selected, overrides)}
            onCancel={() => actions.cancel(selected)}
          />
        </div>
      )}
      <div className="min-h-0 flex-1">
        <ProgramGrid
          services={services}
          programs={programs}
          axis={axis}
          reservationByProgramId={actions.reservedProgramIds}
          selectedProgramId={selected ? programIdentity(selected.site, selected.programId) : null}
          onSelect={(program) =>
            setSelectedProgramId(programIdentity(program.site, program.programId))
          }
          scrollToMs={scrollToMs}
          showSite={showSite}
          // 帯はセルより上・ヘッダより下の層に入る。軸を受け取って同じ
          // spanToPx を通すので、帯と番組セルは同じ時刻で必ず同じ位置に来る。
          // `announce` は site の最初の走だけ true --- GR + BS を両方持つ site は
          // 走が 2 本に分かれるため、両方 true のままだと sr-only が重複する。
          siteOverlay={(gridAxis, site, isFirstRunForSite) => (
            <CapacityBands
              axis={gridAxis}
              overages={overages}
              site={site}
              showSite={showSite}
              announce={isFirstRunForSite}
            />
          )}
          // 帯の見えるラベルは時間軸列に出す（局の列の番組セルと重ならない。
          // issue #460。docs/frontend/programs.md「容量超過の帯とバッジ」）
          gutterOverlay={(gridAxis) => (
            <CapacityBandLabels axis={gridAxis} overages={overages} showSite={showSite} />
          )}
        />
      </div>
    </div>
  )
}
