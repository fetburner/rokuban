import { keepPreviousData } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { TriangleAlert } from 'lucide-react'

import {
  useListCapacityOverages,
  useListCircuitBreakers,
  useListRecordings,
  useListReservations,
  type CapacityOverage,
  type CircuitBreaker,
  type Recording,
  type Reservation,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { ReservationSkipBadge } from '@/components/reservation-skip-reason'
import { describeBreakerName } from '@/lib/breaker'
import { shortageRangeMessage } from '@/lib/capacity'
import { dayOrigin } from '@/lib/day-offset'
import { formatDateTime, formatDuration } from '@/lib/format'
import { cn } from '@/lib/utils'

/**
 * RESERVATION_LIMIT / RECENT_FINISHED_LIMIT は「1 セクションが画面を占有しない」
 * ための恣意的な上限（実測ではない。issue #242 着手宣言コメント）。窓の方
 * （今夜〜明日の予約の時間窓）は既存の日境界（`lib/day-offset.ts`）に揃えたという
 * 根拠があるが、この 2 つの件数はそうではない。
 */
const RESERVATION_LIMIT = 10
const RECENT_FINISHED_LIMIT = 6

/**
 * DROP_WARNING_SCAN_LIMIT はドロップ警告の材料を取る範囲。**「直近の完了」の
 * 表示件数（`RECENT_FINISHED_LIMIT`）とは独立の定数にしてある** ---
 * 表示（何行見せるか = レイアウトの都合）と検出（どこまで遡って異常を拾うか =
 * 正しさの都合）は別の関心事で、同じ値に乗せるとレイアウト都合で表示件数を
 * 下げただけで警告の遡り幅まで黙って縮む。この値も上と同じく恣意的な上限
 * （実測ではない）。
 *
 * **問い合わせは 1 本にまとめる**（`limit=DROP_WARNING_SCAN_LIMIT` で取り、
 * 表示はその先頭 `RECENT_FINISHED_LIMIT` 件へ切る）。同じ絞り込み・同じ既定順
 * （`program_start_at` 降順）なので `limit=6` の集合は `limit=20` の先頭 6 件と
 * 一致し、2 本目の問い合わせは何も新しい情報を持ってこない。定数 2 つの独立性は
 * スライスでも保たれる（テスト「表示上限（6 件）の外にある録画のドロップも警告
 * には出る」がそれを固定している）。
 *
 * 以前は本当に 2 本のクエリに分けていた（`dropScanQuery`）。分ける根拠として
 * 「表示件数を変える変更と警告を壊す変更が同じ 1 行の定数変更に潰れる」と
 * 書いていたが、それはスライスでも潰れないのでレビューで根拠にならないと
 * 指摘された。実際に払っていた代償は: 問い合わせが 1 本増える（マウント時も
 * `recordings` の SSE invalidate のたびも 2 本走る）/ 表示ゲート
 * （`warningsPending` / `allSettled`）が待つクエリが 1 本増える /
 * **2 つの応答の間に録画が 1 本完了すると、表示リストと警告の検出リストが
 * 食い違いうる**（同じ画面の中で「直近の完了」に出ていない録画のドロップ警告が
 * 出る、または逆）。
 */
const DROP_WARNING_SCAN_LIMIT = 20

/**
 * FAILED_RECORDING_SCAN_LIMIT は警告に出す失敗録画（`status=failed`）を取る
 * 範囲。**「直近の完了」「ドロップ警告」の限度とは無関係の別の定数にする**
 * （issue #301）--- 失敗はホームに専用の表示欄を持たず「警告」セクションへの
 * 追加項目として出すだけなので、表示件数と検出範囲を分ける理由（上記
 * `DROP_WARNING_SCAN_LIMIT` の doc コメント）はここには無いが、他の 2 つの
 * 上限と値だけ共有すると「表示件数を変えたら失敗の遡り幅まで連動する」将来の
 * 罠を先に埋めてしまう。値自体は他の上限と同じく実測ではない恣意的な上限。
 */
const FAILED_RECORDING_SCAN_LIMIT = 20

/**
 * FAILED_RECORDING_WARNING_WINDOW_MS は警告に出す失敗録画の recency 窓（レビュー
 * 指摘）。他の警告材料（ブレーカーは発動中のみ・容量超過は今夜〜明日の窓・
 * ドロップは「直近 20 件の完了」で実質 recency がある）はどれも自然に消えるが、
 * 失敗だけは `FAILED_RECORDING_SCAN_LIMIT` 件に収まる限り**いつの失敗でも
 * 出続けてしまう**。稼働の長いサーバーでは警告セクションが古い失敗で常時
 * 埋まり、警告全体の情報価値が下がる（issue #301 の受け入れ基準も「直近の」
 * 失敗録画と言っている）。窓は録画の `startAt`（番組の放送開始。失敗の場合も
 * 必ず持つ --- `startedAt` と違い欠けることが無い）で判定する。値（7 日）は
 * 実測ではなく「1〜2 週間動かして異常が無いか確認する」運用サイクルに対して
 * 「今日気付くべき失敗」を残す側に振った恣意的な上限（他の上限と同じ性質）。
 */
const FAILED_RECORDING_WARNING_WINDOW_MS = 7 * 24 * 3_600_000

/**
 * HomePage はホーム（`/`。M8-3, issue #242）。
 *
 * 起動して最初に見えるのが番組表（「これから録るもの」）だと、運用が安定した
 * 録画サーバーへの再訪の大半が知りたいこと（録れているか・今夜なにが録れるか・
 * 見るものはあるか・異常はないか）に 1 画面で答えられない。部品（録画中の状態・
 * 予約一覧・サーキットブレーカーバナー・容量バッジ・ドロップ統計）は既存のまま、
 * ここでは**新しい API を作らず**それらを集約するだけにする（issue #242 の
 * 着手宣言コメントの決定）。
 *
 * セクションは 4 つ: いま録画中 / 今夜〜明日の予約 / 警告 / 直近の完了。
 * **0 件のセクションは文言も出さずセクションごと消し、全セクションが空のときだけ
 * ホーム全体で 1 つの空状態を出す**（一覧画面の「条件に合う録画がありません」の
 * ような「探した結果の報告」とは意味が違う --- ホームの空は「何も主張しない」
 * ことそのものなので、肯定的な文言（「異常なし」）に転ばないよう沈黙を選ぶ）。
 *
 * **セクションごとの可視性はそのセクション自身のクエリの解決だけを待つ。**
 * 「全セクションが空」（`allEmpty`）の判定だけが全クエリの解決を待つ ---
 * 6 本のうち最も遅い 1 本（絞り込みを持たない `GET /api/reservations` など）に
 * 「いま録画中」のような最も見たいセクションまで引きずられて隠れる半径を
 * 小さくするため（レビュー指摘）。一方で「まだ解決していないセクションを
 * 0 件として隠す」ことはしない --- 個別のクエリが解決する前に「空だから隠す」を
 * 判定すると、読み込み中の一瞬を「セクションが無い」と誤読する（CLAUDE.md
 * 「非同期の空虚な成功」）。未解決のセクションは「解決するまで存在を主張しない」
 * （消えているのではなく、まだ何も言っていない）。
 *
 * 取得が失敗した場合は空扱いにせず、そのセクションだけ取得失敗を表示する
 * （空白のセクションを「異常なし」と取り違えさせないため）。ただし警告
 * セクションの材料（サーキットブレーカー・容量超過・完了録画のドロップ統計・
 * 失敗録画）は、他の画面（`CircuitBreakerBanner` / 予約一覧の容量バッジ）と
 * 同じ「取得失敗は警告が無いことにする」流儀に揃える --- `docs/data.md` §6.5 が
 * 言う「既知の盲点は警告を見逃す方向に偏っている」を承知のうえで、既存の
 * 踏襲先が同じ判断をしている。完了録画の一覧は「直近の完了」の表示と警告の
 * 材料を兼ねるので、それが失敗したときは前者にエラーを出し、後者は黙って
 * 警告なしに縮退する。
 *
 * **失敗録画（`status=failed`）はホームに専用の一覧を持たず、「警告」への
 * 追加項目としてのみ出す**（issue #301）。「直近の完了」は
 * `status=finished` の絞り込みなので failed 行はそもそも混ざらず、既存の
 * 4 セクション構成を変えずに済む。行では予定尺（`durationMs`。番組の放送尺の
 * スナップショット）と実際に録れた尺（`startedAt`〜`endedAt`）を区別する ---
 * 録画が実際には開始しなかった失敗（`startedAt`/`endedAt` が無い）と、
 * 開始した直後に終わった失敗（両方あるが差が小さい）を同じ「予定尺」表示に
 * 潰すと、後者が「ほぼ予定通り録れた」ように見えてしまう。失敗理由は
 * `qualityEvents`（失敗系イベントの最後の要素の `reason`）にあれば出し、
 * 無ければ「理由不明」と沈黙を区別する（`failureReasonText` 参照）。
 */
export function HomePage() {
  // nowMs はこのレンダーの間で一貫させる（`pages/programs.tsx` と同じ規律。
  // 起点・上限を別々に Date.now() を呼んで求めると、ミリ秒単位でずれた「今」が
  // 混ざりうる）。
  // 予約・容量の窓を同じ瞬間の観測で組み立てる。時刻を state 初期値にすると、
  // クエリ再取得後の「いま」と予測窓が古いままになる。
  // oxlint-disable-next-line react/purity -- 各レンダーで一貫した現在時刻スナップショットが必要
  const nowMs = Date.now()

  // **容量超過クエリの `start` は生の `nowMs` を渡さない。** レンダーごとの
  // 生ミリ秒をクエリのパラメータ（延いては TanStack Query のキャッシュキー）に
  // 直接載せると、レンダーのたびに新しいキーになり「未解決 → 即解決 → 再描画 →
  // また未解決」が閉じない無限再取得になる（レビューで実測: 4 秒で 37 回、
  // 実サーバー相当の遅延では 4 秒間ずっと全画面スケルトンのまま収束しなかった）。
  //
  // **既存 2 ファイルの前例に倣い、量子化してからキーに渡す**（`useRef`/`useState`
  // で「now を固定する」ような対症療法は採らない --- それは症状を消すだけで
  // 「キーに入る値は答えが変わる粒度まで量子化する」という規律の欠落が残る）。
  // `pages/programs.tsx` は `Date.now()` を `dayOrigin(0, ...)` で時境界へ量子化
  // してからキーに渡している（同ファイルの `dayOrigin` の doc コメント参照。
  // 「今日」の起点を「now を時で切り捨てた時刻」にしているのはこの目的も兼ねる）。
  // ここでも同じ関数で「今」を時境界へ丸めてから `start` に渡す。
  const overagesStartMs = dayOrigin(0, nowMs).getTime()
  // 今夜〜明日の予約セクションの窓の終端。「明日の暦日の終わり」= 明後日の 0 時
  // （`dayOrigin` が返す「dayOffset 日先の 0 時」を dayOffset=2 で呼ぶと明後日の
  // 0 時になり、これが明日の終わりと一致する。番組表の日境界（0 時基準）に
  // 揃えた窓であり、根拠はここだけ実測ではなく既存の日境界との整合）。
  // こちらは元から日単位に量子化済みなので上記の無限再取得は起きない。
  const reservationsWindowEndMs = dayOrigin(2, nowMs).getTime()

  const recordingQuery = useListRecordings({ status: 'recording' })
  // 「直近の完了」の表示とドロップ警告の検出を兼ねる 1 本。取る範囲は広い方
  // （`DROP_WARNING_SCAN_LIMIT`）に合わせ、表示だけを先頭 `RECENT_FINISHED_LIMIT`
  // 件に切る（`DROP_WARNING_SCAN_LIMIT` の doc コメント参照）。
  const finishedQuery = useListRecordings({
    status: 'finished',
    limit: DROP_WARNING_SCAN_LIMIT,
  })
  // 失敗録画（issue #301）。表示専用のセクションは持たず「警告」への追加項目
  // としてのみ使うので、`finishedQuery` のような「表示 + 検出の兼用」は無い ---
  // 取る範囲がそのまま警告に出す範囲になる。
  const failedQuery = useListRecordings({
    status: 'failed',
    limit: FAILED_RECORDING_SCAN_LIMIT,
  })
  const reservationsQuery = useListReservations()
  const breakersQuery = useListCircuitBreakers()
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(overagesStartMs).toISOString(),
      end: new Date(reservationsWindowEndMs).toISOString(),
    },
    {
      // **時境界を越えた瞬間に警告セクションを消さない。** 上の量子化により
      // キーは毎時 0 分に 1 回変わる。新しいキーにはまだデータが無いので、
      // 素のままだと `isPending` → `warningsPending` → 警告セクションが 1 RTT
      // だけ消える（警告だけが可視だった場合はページ全体がスケルトンに戻る）。
      // この画面の主題は「セクションが理由なく消えないこと」なので、キーが
      // 進んでいる間は前のキーのデータを見せ続ける（`isPending` は false のまま
      // になる）。判定はテスト「時境界を越えてキーが変わっても警告は消えない」。
      query: { placeholderData: keepPreviousData },
    },
  )

  const recordingsInProgress = unwrap(recordingQuery.data) ?? []
  // 以下の導出は `useMemo` を使わない。**この関数の中で最も頻繁に変わる依存は
  // `nowMs`（= 生の `Date.now()`）で、レンダーごとに必ず変わる。** それを deps に
  // 持つ `useMemo` は毎レンダー再計算されるので何も買っていない（レビュー指摘。
  // 以前は `activeOverages` / `upcomingReservations` / `warnings` を `useMemo` で
  // 包んでおり、後者 2 つは前者が毎レンダー新しい配列になることで連鎖して
  // 再計算されていた）。録画は API の `limit`、超過区間は窓幅で上界があるが、
  // **予約だけは上界が無い**（`GET /api/reservations` は絞り込みパラメータを
  // 持たない全件取得で `limit` も無い）。それでも `useMemo` は上記のとおり
  // `nowMs` 依存で効かないので、素朴に毎レンダー計算する。
  const finishedRecordings = unwrap(finishedQuery.data) ?? []
  const recentFinished = finishedRecordings.slice(0, RECENT_FINISHED_LIMIT)
  const failedRecordings = unwrap(failedQuery.data) ?? []

  const upcomingReservations = (unwrap(reservationsQuery.data) ?? [])
    .filter((r) => {
      const startMs = new Date(r.startAt).getTime()
      return startMs >= nowMs && startMs < reservationsWindowEndMs
    })
    .sort((a, b) => new Date(a.startAt).getTime() - new Date(b.startAt).getTime())
  const reservationsOverflow = upcomingReservations.length > RESERVATION_LIMIT
  const shownReservations = upcomingReservations.slice(0, RESERVATION_LIMIT)

  // `overagesStartMs` を時境界へ丸めた分だけ、実際の「今」より前に始まって
  // **既に終わった**区間まで返ってきうる（`openapi.yaml` の `start` は「この時刻
  // より後に終わる区間が対象」なので、`start` を時頭まで後退させて増えるのは
  // ちょうど「[時頭, now] に終わった区間」だけ）。ここで「実際の今より後に
  // 終わる」区間だけへ絞り、量子化前と同じ主張の強さに戻す（量子化はキャッシュ
  // キーの安定のためだけの手段で、表示する内容の正しさを緩めてよい理由には
  // しない）。これが無いと「もう終わったチューナー不足」が最大 59 分ぶん警告に
  // 出続ける。判定はテスト「既に終わった超過区間は警告に出さない」/「時境界より
  // 前に始まって進行中の超過区間は警告に出す」の両方向。
  const activeOverages = (unwrap(overagesQuery.data) ?? []).filter(
    (o) => new Date(o.endAt).getTime() > nowMs,
  )

  // 失敗録画は `FAILED_RECORDING_SCAN_LIMIT` 件に収まる限りいつの失敗でも警告に
  // 出続けてしまうので、ここで recency 窓へ絞る
  // （`FAILED_RECORDING_WARNING_WINDOW_MS` の doc コメント参照）。
  const recentFailedRecordings = failedRecordings.filter(
    (r) => new Date(r.startAt).getTime() >= nowMs - FAILED_RECORDING_WARNING_WINDOW_MS,
  )

  const warnings = buildWarnings({
    breakers: unwrap(breakersQuery.data) ?? [],
    overages: activeOverages,
    dropCandidates: finishedRecordings,
    failedRecordings: recentFailedRecordings,
  })

  // セクションごとの可視性はそのセクション自身のクエリの解決だけを待つ
  // （上記 doc コメント参照）。警告は 4 本のクエリの合成なので、そのいずれかが
  // 未解決なら「まだ言わない」。
  const recordingSectionVisible =
    !recordingQuery.isPending && (recordingQuery.isError || recordingsInProgress.length > 0)
  const reservationSectionVisible =
    !reservationsQuery.isPending && (reservationsQuery.isError || shownReservations.length > 0)
  const warningsPending =
    breakersQuery.isPending ||
    overagesQuery.isPending ||
    finishedQuery.isPending ||
    failedQuery.isPending
  const warningSectionVisible = !warningsPending && warnings.length > 0
  const finishedSectionVisible =
    !finishedQuery.isPending && (finishedQuery.isError || recentFinished.length > 0)

  const anyVisible =
    recordingSectionVisible ||
    reservationSectionVisible ||
    warningSectionVisible ||
    finishedSectionVisible

  // 「全セクションが空」の判定だけは全クエリの解決を待つ ---
  // 一部がまだ未解決のうちは「空である」とまだ言い切れない。
  const allSettled =
    !recordingQuery.isPending &&
    !reservationsQuery.isPending &&
    !warningsPending &&
    !finishedQuery.isPending

  // どのセクションもまだ何も言えることが無い（1 つも解決していない、または
  // 解決済みの分がすべて 0 件）間はスケルトンを出す。何か 1 つでも言えることが
  // 出てくれば、他がまだ解決していなくても表示を始める（最も遅いクエリに
  // 引きずられない）。
  if (!anyVisible && !allSettled) {
    return (
      <>
        <PageHeader title="ホーム" />
        <PageContent>
          <ListSkeleton />
        </PageContent>
      </>
    )
  }

  const allEmpty = allSettled && !anyVisible

  return (
    <>
      <PageHeader title="ホーム" />

      <PageContent>
        {allEmpty ? (
        // 「異常なし」「予約がありません」のような肯定/報告の文言にしない ---
        // これは検索結果の 0 件ではなく、4 セクションすべてが沈黙した結果の
        // 集約なので、何も主張しない言い方に留める。
        <EmptyState>表示できる項目がありません</EmptyState>
      ) : (
        <div className="flex flex-col divide-y divide-border">
          {recordingSectionVisible && (
            <section aria-labelledby="home-recording">
              <h2 id="home-recording" className="px-4 pt-4 pb-2 text-sm font-semibold">
                いま録画中
              </h2>
              {recordingQuery.isError ? (
                <p className="px-4 pb-4 text-sm text-destructive">録画中の取得に失敗しました</p>
              ) : (
                <ul>
                  {recordingsInProgress.map((r) => (
                    <RecordingRow key={r.id} recording={r} />
                  ))}
                </ul>
              )}
            </section>
          )}

          {reservationSectionVisible && (
            <section aria-labelledby="home-reservations">
              <h2 id="home-reservations" className="px-4 pt-4 pb-2 text-sm font-semibold">
                今夜〜明日の予約
              </h2>
              {reservationsQuery.isError ? (
                <p className="px-4 pb-4 text-sm text-destructive">予約の取得に失敗しました</p>
              ) : (
                <>
                  <ul>
                    {shownReservations.map((r) => (
                      <ReservationRow key={`${r.site}:${r.programId}`} reservation={r} />
                    ))}
                  </ul>
                  {reservationsOverflow && (
                    <div className="px-4 pb-4">
                      <Link
                        to="/reservations"
                        className="text-sm text-primary underline-offset-2 hover:underline"
                      >
                        予約をすべて見る
                      </Link>
                    </div>
                  )}
                </>
              )}
            </section>
          )}

          {warningSectionVisible && (
            <section aria-labelledby="home-warnings">
              <h2 id="home-warnings" className="px-4 pt-4 pb-2 text-sm font-semibold">
                警告
              </h2>
              <ul className="flex flex-col gap-2 px-4 pb-4">
                {warnings.map((w) => (
                  <WarningRow key={w.key} warning={w} />
                ))}
              </ul>
            </section>
          )}

          {finishedSectionVisible && (
            <section aria-labelledby="home-finished">
              <h2 id="home-finished" className="px-4 pt-4 pb-2 text-sm font-semibold">
                直近の完了
              </h2>
              {finishedQuery.isError ? (
                <p className="px-4 pb-4 text-sm text-destructive">
                  直近の完了録画の取得に失敗しました
                </p>
              ) : (
                <ul>
                  {recentFinished.map((r) => (
                    <RecordingRow key={r.id} recording={r} />
                  ))}
                </ul>
              )}
            </section>
          )}
        </div>
      )}
      </PageContent>
    </>
  )
}

/**
 * RecordingRow はホームの「いま録画中」「直近の完了」セクションの 1 行。
 *
 * **警告に値する事実（ドロップ統計）は行に重ねない。** どちらのセクションも
 * 録画は `status` で既に絞り込まれている（前者は `recording`、後者は
 * `finished`）ため状態バッジ自体が見出しと重複するので出さない。ドロップの
 * 有無は「警告」セクションが該当録画へのリンク付きで一覧化する唯一の場所
 * （`ReservationRow` の doc コメントと同じ「二重に主張しない」規律。以前は
 * ここにも `DropBadges` を出しており、同じ事実が同一画面に 2 回出ていた ---
 * レビュー指摘で統一した）。行の役目はタイトル・時刻等の識別に絞り、詳細は
 * 遷移先（録画単体ページ）か警告セクションに譲る。
 */
function RecordingRow({ recording }: { recording: Recording }) {
  return (
    <li className="border-b border-border last:border-b-0">
      <Link
        to="/recordings/$id"
        params={{ id: String(recording.id) }}
        className="flex min-h-14 flex-col justify-center gap-0.5 px-4 py-2.5 hover:bg-muted/50"
      >
        <span className="truncate text-base">{recording.title || '（番組名なし）'}</span>
        <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
          <span className="shrink-0">{recording.serviceName}</span>
          <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
          <span className="shrink-0">{formatDuration(recording.durationMs)}</span>
        </span>
      </Link>
    </li>
  )
}

/**
 * ReservationRow はホームの「今夜〜明日の予約」セクションの 1 行。
 *
 * **警告に値する事実（容量不足）は行に重ねない。** 同じ超過区間は本ページの
 * 「警告」セクションで既に一覧化しているので、二重に主張しない（`RecordingRow`
 * の doc コメントと同じ規律 --- 警告セクションが「その事実の一覧化」を一手に
 * 引き受け、他の行は識別情報だけを持つ）。行自体は 2 つ目の対話要素を持たない
 * ので、`pages/reservations.tsx` のような「行本体を絶対配置の面にする」入れ子
 * 回避の手当ても不要 --- `<li>` をそのまま 1 つの `Link` にできる。
 */
function ReservationRow({ reservation }: { reservation: Reservation }) {
  return (
    <li className="border-b border-border last:border-b-0">
      <Link
        to="/reservations/$site/$programId"
        params={{ site: reservation.site, programId: String(reservation.programId) }}
        className="flex min-h-14 flex-col justify-center gap-0.5 px-4 py-2.5 hover:bg-muted/50"
      >
        <span className="truncate text-base">{reservation.title || '（番組名なし）'}</span>
        <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
          <span className="shrink-0">{reservation.serviceName}</span>
          <span className="shrink-0">{formatDateTime(reservation.startAt)}</span>
          <span className="shrink-0">{formatDuration(reservation.durationMs)}</span>
          <ReservationSkipBadge reservation={reservation} />
        </span>
      </Link>
    </li>
  )
}

/** WarningKind はホーム「警告」セクションの項目の種別。表示色と、色を選ぶ判断の両方をこれ 1 つに一本化する。 */
type WarningKind = 'breaker' | 'overage' | 'drop' | 'failed'

/** WarningItem はホーム「警告」セクションの 1 件（サーキットブレーカー / チューナー不足 / ドロップ / 失敗録画）。 */
type WarningItem = {
  key: string
  /**
   * 表示色・将来の出し分けの判断はすべてこの値を経由させる（文字列の `key` を
   * 前方一致で覗いて種別を推測する実装は、`key` の書式を変えただけで表示が
   * 黙って壊れる。レビュー指摘で `buildWarnings` が組む時点の種別をそのまま
   * 持たせる形に直した）。
   */
  kind: WarningKind
  message: string
  /** 遷移先。サーキットブレーカーは対応する専用画面が無い（`CircuitBreakerBanner` が同じページの上部で扱う）ので省略。 */
  link?: { to: '/programs'; search: { at: number } } | { to: '/recordings/$id'; id: number }
}

/**
 * buildWarnings はサーキットブレーカー・容量超過・ドロップ統計・失敗録画から
 * ホームの「警告」セクションの項目を組む（issue #242 着手宣言コメントの決定：
 * 新しい API を作らず、既存の取得結果だけを材料にする。失敗録画も既存の
 * `GET /api/recordings?status=failed` の絞り込みだけで足りる。issue #301）。
 *
 * `dropCandidates` は `limit=DROP_WARNING_SCAN_LIMIT` で取った完了録画の全件で、
 * 「直近の完了」に**表示する分（先頭 `RECENT_FINISHED_LIMIT` 件）に切る前**の
 * リスト --- 表示件数を絞っても警告の検出範囲まで連動して狭まらないようにする
 * ため（呼び出し元の doc コメント参照）。`failedRecordings` は
 * `limit=FAILED_RECORDING_SCAN_LIMIT` で取った失敗録画のうち、呼び出し元で
 * さらに `FAILED_RECORDING_WARNING_WINDOW_MS` の recency 窓へ絞り込んだもの
 * （表示専用セクションを持たないので表示/検出の区別は無いが、警告としての
 * recency は要る）。
 *
 * 容量超過・失敗録画はいずれも呼び出し元で時間フィルタ済みなので、ここでは
 * 追加の時間フィルタはしない。
 */
function buildWarnings({
  breakers,
  overages,
  dropCandidates,
  failedRecordings,
}: {
  breakers: readonly CircuitBreaker[]
  overages: readonly CapacityOverage[]
  dropCandidates: readonly Recording[]
  failedRecordings: readonly Recording[]
}): WarningItem[] {
  const items: WarningItem[] = []

  for (const breaker of breakers) {
    items.push({
      key: `breaker:${breaker.site}:${breaker.name}`,
      kind: 'breaker',
      message: `${describeBreakerName(breaker.name)}が停止中（保留 ${breaker.pending} 件）`,
    })
  }

  for (const recording of failedRecordings) {
    const reason = failureReasonText(recording)
    // 材料が無い（undefined）ときは理由そのものが言えていないので、「理由:」
    // というラベルを付けない（付けると「理由: 理由不明」という二重表現になる）。
    const reasonSegment = reason === undefined ? '理由不明' : `理由: ${reason}`
    items.push({
      key: `failed:${recording.id}`,
      kind: 'failed',
      message: `${recording.title || '（番組名なし）'}: 録画失敗（${failedDurationText(recording)} / ${reasonSegment}）`,
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  for (const overage of overages) {
    items.push({
      key: `overage:${overage.site}:${overage.startAt}:${overage.endAt}`,
      kind: 'overage',
      message: shortageRangeMessage(overage),
      link: { to: '/programs', search: { at: new Date(overage.startAt).getTime() } },
    })
  }

  for (const recording of dropCandidates) {
    const summary = recording.dropSummary
    if (summary === undefined) continue
    if (summary.drops === 0 && summary.errors === 0 && summary.scrambled === 0) continue
    const parts = [
      { label: 'ドロップ', value: summary.drops },
      { label: 'エラー', value: summary.errors },
      { label: 'スクランブル', value: summary.scrambled },
    ]
      .filter((b) => b.value > 0)
      .map((b) => `${b.label} ${b.value.toLocaleString()}`)
      .join(' / ')
    items.push({
      key: `drop:${recording.id}`,
      kind: 'drop',
      message: `${recording.title || '（番組名なし）'}: ${parts}`,
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  return items
}

/**
 * failedDurationText は失敗録画の警告メッセージに載せる尺の文言。
 *
 * **予定尺（`durationMs`。番組の放送尺のスナップショット）と実際に録れた尺
 * （`startedAt`〜`endedAt`）を区別する**（issue #301）。区別しないと、録画が
 * 開始した直後に終わった失敗（実際は 0 分に近いのに `durationMs` は番組の
 * 予定尺のまま）が「ほぼ予定通り録れた」ように見えてしまう。
 *
 * `startedAt` と `endedAt` は独立に書かれるので、`startedAt` だけが立って
 * `endedAt` が無い行がある（レビューで発覚。以前のコメントは「両方揃ってから
 * 書く」と逆を断言していた）。`UpdateRecordingStatus`
 * （`internal/db/queries/recordings.sql`）は `started_at` を無条件に
 * `COALESCE` で埋め、`ended_at` は渡された値が非 NULL のときだけ書く。呼び
 * 出し元の `Watcher.updateRecordingStatus`（`internal/watcher/watcher.go`）は
 * `record.Recording.EndTime`（`*mirakc.Milliseconds` で nil を取りうる）を
 * そのまま渡すので、mirakc の failed record に `endTime` が無ければ failed
 * 行でも `startedAt` だけが立つ。したがって 3 通りを区別する: 両方あり
 * （実際尺が定義できる）/ `startedAt` のみ（開始した事実はあるが終了時刻が無く、
 * 実際尺は主張できない）/ 両方無し（**rokuban が録画の開始を観測していない**。
 * 「未開始」と出す）。
 *
 * 3 つ目で mirakc 側の事実（「mirakc が録画を開始しなかった」）は主張しない
 * （レビュー指摘）。`started_at` を書くのは record を観測した
 * `Watcher.updateRecordingStatus` だけで、`CreateFailedRecording`
 * （`internal/db/queries/recordings.sql`）は書かないので、**record の観測より
 * 先に `recording.failed` の SSE が届いた**窓でも両方無しの形になる。データが
 * 支えているのは「rokuban が開始を観測していない」までで、その先は測っていない。
 *
 * 3 通りの中の区切りはどれも `・` に揃える（レビュー指摘）。警告メッセージ全体が
 * `（尺 / 理由: …）` の形で ` / ` を「尺と理由の境目」に使っているので、尺の中でも
 * ` / ` を使うと 1 行に 2 種類の意味のスラッシュが並ぶ。
 */
function failedDurationText(recording: Recording): string {
  if (recording.startedAt && recording.endedAt) {
    const actualMs = new Date(recording.endedAt).getTime() - new Date(recording.startedAt).getTime()
    return `実際 ${formatDuration(actualMs)}・予定 ${formatDuration(recording.durationMs)}`
  }
  if (recording.startedAt) {
    return `予定 ${formatDuration(recording.durationMs)}・開始のみ記録（終了未記録）`
  }
  return `予定 ${formatDuration(recording.durationMs)}・未開始`
}

/**
 * failureReasonText は失敗録画の理由を `qualityEvents` から取り出す。
 *
 * **材料が無ければ `undefined` を返し、沈黙（理由が言えない）と実際の理由文を
 * 型で区別する**（issue #301 / #454）。呼び出し側はこれを見て「理由:」という
 * ラベルを付けるかどうかを決める --- 文字列の中身（`'理由不明'`）で判定すると、
 * 将来この語を言い換えたときに呼び出し側の分岐が黙って外れる。
 * `qualityEvents` は追記専用の履歴（`recordings.quality_events`。
 * `docs/schema/recordings.md` §5）で `recording.failed` /
 * `recording.record-broken` / `bcas_anomaly` が混ざるので、**最後の要素では
 * なく失敗系イベントの最後の要素**を見る（末尾が `bcas_anomaly` だと最後の
 * 失敗理由を読み飛ばす）。
 *
 * `reason` の形は書き手（`event` の値）で決まり、いずれもオブジェクトで
 * 素の文字列を書く経路は無い（以前のコメントは「`recording.failed` は文字列」
 * と書いていたが、書き手を辿ると逆でどちらもオブジェクト）:
 * - `recording.failed`: `internal/watcher/watcher.go` の
 *   `handleRecordingFailed` が `json.Marshal(data.Reason)` で書く。
 *   `data.Reason` は `mirakc.FailedReason`
 *   （`internal/mirakc/types.go`。discriminated union で `type` フィールドを
 *   持つ）なので `reason.type` を読む。
 * - `recording.record-broken`: 同ファイルの `handleRecordBroken` が
 *   `map[string]string{"reason": data.Reason}` で書くので `reason.reason`
 *   を読む。
 *
 * 上記どちらでもない `event`、または期待した形（`type` / `reason` フィールド
 * が無い）は `components/recording-detail-panel.tsx` の「品質イベント」欄と
 * 同じ流儀（`JSON.stringify`）で読める形にフォールバックする。
 *
 * **読んだフィールドが空文字なら `undefined` に寄せる**（レビュー指摘）。
 * `mirakc.FailedReason.Type` に `omitempty` は無いので `{"type":""}` は
 * あり得る形だが、それを `JSON.stringify` でそのまま出すと「理由: {"type":""}」に
 * なり、材料が無い（沈黙）ことと区別できる文言にならない。
 */
function failureReasonText(recording: Recording): string | undefined {
  const events = recording.qualityEvents
  if (events === undefined || events.length === 0) return undefined
  const failureEvent = events.findLast(
    (e) => e['event'] === 'recording.failed' || e['event'] === 'recording.record-broken',
  )
  if (failureEvent === undefined) return undefined
  const reason = failureEvent['reason']
  if (reason === undefined || reason === null) return undefined

  if (typeof reason === 'object' && !Array.isArray(reason)) {
    const record = reason as Record<string, unknown>
    // 上の findLast が `event` を 2 種類に絞っているので、`recording.failed`
    // でなければ `recording.record-broken`。
    const field = failureEvent['event'] === 'recording.failed' ? 'type' : 'reason'
    const value = record[field]
    if (typeof value === 'string') return value === '' ? undefined : value
  }
  return JSON.stringify(reason)
}

/**
 * WarningRow は 1 件の警告。サーキットブレーカー・直近のドロップ・失敗録画は
 * 「取り返しがつかない/止まっている」意味の destructive、チューナー不足は容量
 * バッジ（`components/capacity-shortfall-badge.tsx`）と同じ warning（琥珀）に
 * 揃える（docs/frontend/design.md「色は信号のみ」。同じ事実は同じ色で言う）。
 *
 * **失敗録画（`kind: 'failed'`）は destructive 側。** design.md の表が
 * destructive を「取り返しがつかない・壊れた（失敗・ドロップ・…）」と定めており、
 * 録画が失われたことは後から取り返せない --- 琥珀（「これから足りない」の予告）
 * とは別の事実なので、色でも分ける（種別 × 色は `pages/home.test.tsx`
 * 「警告項目は種別ごとに固定の色クラスを持つ」と `e2e/design.mjs` ①'' が固定する）。
 */
function WarningRow({ warning }: { warning: WarningItem }) {
  const amber = warning.kind === 'overage'
  const content = (
    <>
      <TriangleAlert className="size-4 shrink-0" aria-hidden="true" />
      <span>{warning.message}</span>
    </>
  )
  const rowClassName = cn(
    'flex items-center gap-2 rounded-md px-2 py-1.5 text-sm',
    amber ? 'bg-warning/10 text-warning' : 'text-destructive',
  )

  if (warning.link === undefined) {
    return <li className={rowClassName}>{content}</li>
  }

  if (warning.link.to === '/programs') {
    return (
      <li>
        <Link
          to="/programs"
          search={warning.link.search}
          className={cn(rowClassName, 'hover:underline')}
        >
          {content}
        </Link>
      </li>
    )
  }

  return (
    <li>
      <Link
        to="/recordings/$id"
        params={{ id: String(warning.link.id) }}
        className={cn(rowClassName, 'hover:underline')}
      >
        {content}
      </Link>
    </li>
  )
}
