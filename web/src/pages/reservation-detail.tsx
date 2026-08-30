import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'

import {
  useDeleteProgramIntent,
  useGetProgramReservation,
  useListRules,
  usePatchProgramOverrides,
  usePutProgramIntent,
  type ProgramOverridesInput,
  type Reservation,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { EncodeOverridesEditor } from '@/components/encode-settings-fields'
import { ErrorState, ListSkeleton } from '@/components/page'
import { ProgramOverlapWarning } from '@/components/program-overlap-warning'
import { ReservationSkipReason } from '@/components/reservation-skip-reason'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { formatDateTime, formatDuration } from '@/lib/format'
import { mutationErrorMessage } from '@/lib/mutation-error-message'
import { stateLabels } from '@/lib/reservation-labels'

/**
 * reservationDetailQueryKey は単体ページ自身のクエリキー。
 *
 * orval が生成する `getGetProgramReservationQueryKey`
 * （`['/api/sites/{site}/programs/{programId}/reservation']`、1 要素）は使わない。
 * TanStack Query の既定の前方一致はフィルタキーの要素を前から順に比較するので、
 * 生成キーは一覧側・SSE 側が使う `['/api/reservations']` に**掛からない**。
 * その結果このページは
 *
 *   - `reservations` トピック（`lib/events.ts`）の invalidate が届かず
 *   - 定期 invalidate では `'/api/sites/'` の方に掛かって EPG グループ
 *     （10 分）に落ちる
 *
 * という状態だった。宛先が `(site, programId)` であること（`reservations.id` を
 * URL・キーに使わない）は変えずに、**先頭要素だけを一覧と揃える**
 * （`pages/recording-detail.tsx` の `recordingDetailQueryKey` と同じ手）。
 * site と programId をキーの要素として持つので、資源の同定は生成キーと等価。
 */
function reservationDetailQueryKey(site: string, programId: number) {
  return ['/api/reservations', 'detail', site, programId] as const
}

/**
 * ReservationDetailPage は予約 1 件の詳細。
 *
 * base（ルールが書く）と overrides（ユーザーが書く）は同形の jsonb なので、
 * ルール由来予約と手動予約を 1 画面で扱える（EPGStation は編集画面が分裂している）。
 *
 * encodeProfiles / keepOriginal は M3 で worker が消費するため編集可能
 * （issue #68）。priority は reconciler が mirakc への差分反映を持つが
 * （優先度差分での schedules 再作成）、編集 UI をまだ作っていないため
 * 表示のみに留めている。UI を足すこと自体は別タスク。
 *
 * ルートとクエリは `(site, programId)` を宛先にする（issue #99）。
 * `reservations.id` は ruler の導出削除・再実体化で変わりうる不安定な値なので、
 * それを URL・クエリキーに使うとブックマーク・共有した URL やキャッシュが
 * 予約の再実体化で無効になる。`GET /api/sites/{site}/programs/{programId}/reservation`
 * は `UNIQUE (site, program_id)` をキーにするので、id が変わっても同じ URL で引ける。
 */
export function ReservationDetailPage() {
  const { site, programId } = useParams({ from: '/reservations/$site/$programId' })
  const navigate = useNavigate()
  const toast = useToast()
  const queryClient = useQueryClient()

  const programIdNum = Number(programId)
  const query = useGetProgramReservation(site, programIdNum, {
    query: { queryKey: reservationDetailQueryKey(site, programIdNum) },
  })
  const putIntent = usePutProgramIntent()
  const deleteIntent = useDeleteProgramIntent()
  const patchOverrides = usePatchProgramOverrides()

  const reservation = unwrap(query.data)

  // 取消は (site, programId) を宛先に intent{skip} を書くだけ（issue #29）。
  // reservations 行には触れない --- ただし ruler は次パスで **行そのものを
  // 削除する**（`insertRulerPassHint` が積むヒントを受けて評価し直し、
  // 予約の根拠が「明示 skip」だけになった行を落とす）。上書き（overrides）が
  // 残っていて detached として残るケースを除き、この画面の GET は 404 に
  // 変わる。だからこの画面に留まる形は取れない --- 404 を待たず、一覧
  // （`/reservations`）へ遷移する。`ToastProvider` は `main.tsx` で
  // `RouterProvider` の外側にあるため、遷移後もトースト自体と Undo の
  // クロージャは生き続ける。一覧・グリッド（`pages/programs.tsx`）と同じ
  // 「ワンタップ + トーストの Undo」を詳細にも対称に持たせる（issue #453）。
  const cancel = () => {
    if (!reservation) return
    const source = reservation.source
    putIntent.mutate(
      { site: reservation.site, programId: reservation.programId, data: { action: 'skip' } },
      {
        onSuccess: () => {
          toast({
            message: '予約を取消しました',
            action: { label: '元に戻す', onClick: () => revive(source) },
          })
          void navigate({ to: '/reservations' })
        },
        onError: (err) => toast({ message: mutationErrorMessage('予約の取消に失敗しました', err) }),
      },
    )
  }

  // revive はトーストの「元に戻す」から呼ぶ。`cancel` が遷移するため、
  // クリック時点でこの画面（と `putIntent`/`deleteIntent` の観測者）は
  // 既にアンマウント済みのことが前提になる --- `.mutate(vars, {onSuccess,
  // onError})` の第 2 引数コールバックは `MutationObserver` に購読者
  // （＝マウント中のコンポーネント）が居るときしか呼ばれない
  // （`@tanstack/query-core` の `MutationObserver#mutate` → `#notify` の
  // `hasListeners()` 判定。実測: アンマウント後に `.mutate` を呼ぶと
  // リクエスト自体は飛ぶが `onSuccess`/`onError` は一度も呼ばれない）。
  // `mutateAsync` は `Mutation#execute` の Promise をそのまま返すのでこの
  // 判定を経由せず、遷移後でも解決・reject する。だから遷移をまたぐ Undo は
  // `mutateAsync` + 自前の try/catch にする（`.mutate` のコールバックには
  // 依存しない）。site / programId は URL のパラメータ（route の宛先
  // そのもの）を使う。
  //
  // `source` で分岐する: 手動予約の逆操作は `PUT intent{record}` でよいが、
  // ルール由来の予約に対して同じ PUT を送ると `program_intents` に record 行が
  // 残り、以後ルールがマッチしなくなっても予約が残る「種別: 手動」の予約に
  // 恒久的に変わってしまう（`internal/api/handler.go` の source 導出、
  // `TestGetReservation_SourceManualDespiteRuleMatch`）。ルール由来の厳密な
  // 逆操作は `DELETE .../intent`（明示的な意見を取り下げ、ルール評価に戻す）。
  const revive = (source: Reservation['source']) => {
    void (async () => {
      try {
        if (source === 'rule') {
          await deleteIntent.mutateAsync({ site, programId: programIdNum })
        } else {
          await putIntent.mutateAsync({
            site,
            programId: programIdNum,
            data: { action: 'record' },
          })
        }
        void queryClient.invalidateQueries({ queryKey: ['/api/reservations'] })
        toast({ message: '予約を元に戻しました' })
      } catch (err) {
        toast({ message: mutationErrorMessage('予約への復帰に失敗しました', err) })
      }
    })()
  }

  const saveEncode = (body: ProgramOverridesInput) => {
    if (!reservation) return
    patchOverrides.mutate(
      { site: reservation.site, programId: reservation.programId, data: body },
      {
        onSuccess: () => {
          toast({ message: 'エンコード設定を更新しました' })
          void queryClient.invalidateQueries({
            queryKey: reservationDetailQueryKey(site, programIdNum),
          })
        },
        onError: (err) =>
          toast({ message: apiErrorMessage(err) ?? 'エンコード設定の更新に失敗しました' }),
      },
    )
  }

  return (
    <>
      <header
        className="sticky z-10 flex items-center gap-2 border-b border-border bg-background/95 px-2 py-2 backdrop-blur"
        style={{ top: 'var(--sticky-banners-height, 0px)' }}
      >
        <Button variant="ghost" size="icon" aria-label="戻る" render={<Link to="/reservations" />}>
          <ArrowLeft />
        </Button>
        <h1 className="text-base font-semibold tracking-tight">予約の詳細</h1>
      </header>

      {query.isError ? (
        <ErrorState>予約が見つかりません</ErrorState>
      ) : query.isPending || !reservation ? (
        <ListSkeleton rows={4} />
      ) : (
        <div className="flex flex-col gap-6 px-4 py-4">
          <section>
            <h2 className="text-lg font-medium">{reservation.title || '（番組名なし）'}</h2>
            {/* 局名・開始時刻・尺を中点でつなぐ。`serviceName` は API では required
                だが空文字を禁じてはいないので、空の成分を落としてから join する
                （無条件連結だと先頭に裸の `·` が残る）。 */}
            <p className="mt-1 text-sm text-muted-foreground">
              {[
                reservation.serviceName,
                formatDateTime(reservation.startAt),
                formatDuration(reservation.durationMs),
              ]
                .filter((s) => s !== '')
                .join(' · ')}
            </p>
            {/* この予約が作られたあとで他の予約が増え、重なりが生じることもあるので
                詳細画面でも常に出す（issue #24 M2-8。件数だけ・断定なし）。 */}
            <div className="mt-2">
              <ProgramOverlapWarning site={site} programId={reservation.programId} />
            </div>
          </section>

          <Fields title="予約">
            <Field label="状態" value={stateLabels[reservation.state]} />
            <Field label="種別" value={reservation.source === 'manual' ? '手動' : 'ルール'} />
            {/* 予約行が残っているのに録画されない状態は、それ自体が説明を要する。
                重複排除なら根拠（録画 id と類似度）まで出す（issue #24 M2-6）。 */}
            {reservation.skip && (
              <Field label="録画" value={<ReservationSkipReason reservation={reservation} />} />
            )}
            {reservation.ruleId !== undefined && (
              <Field label="ルール" value={<RuleName ruleId={reservation.ruleId} />} />
            )}
          </Fields>

          <Fields title="録画の設定">
            <Field label="優先度" value={overrideValue(reservation, 'priority') ?? '既定'} />
            <Field label="保存先パス" value="自動生成" />
          </Fields>

          <section>
            <h3 className="mb-2 text-xs font-medium text-muted-foreground">エンコードと保持</h3>
            <EncodeOverridesEditor
              overrides={reservation.overrides}
              isPending={patchOverrides.isPending}
              onSave={saveEncode}
            />
          </section>

          <Button
            variant="destructive"
            size="lg"
            className="w-full"
            disabled={putIntent.isPending}
            onClick={cancel}
          >
            {putIntent.isPending ? '取消中…' : '予約を取消'}
          </Button>
        </div>
      )}
    </>
  )
}

/** overrideValue は overrides jsonb から 1 フィールドを文字列で取り出す。 */
function overrideValue(reservation: Reservation, key: string): string | undefined {
  const value = reservation.overrides?.[key]
  return value === undefined || value === null ? undefined : String(value)
}

/**
 * RuleName はこの予約を生んだルールへの導線（issue #300）。
 *
 * `pages/recordings.tsx` の `RuleSection` と同じ手を使う ---
 * `useListRules()`（パラメータなし = 常に全件）のキャッシュから名前を引く。
 * `/rules` に単一ルートは無く、ルールの実質的な編集画面は `/search?ruleId=N`
 * （`RulesPage` の「検索しながら編集」と同じ着地先）なので、リンク先もそこに揃える。
 *
 * `rules.find` が見つからない間（一覧が未解決・失敗、または一覧にまだ無い）は
 * `#N` に落とす --- ルールが削除された場合は `reservations.rule_id` の FK が
 * `ON DELETE SET NULL` なので `Reservation.ruleId` 自体が省略され、呼び出し側
 * （`reservation.ruleId !== undefined` の分岐）がこのフィールドごと出さない。
 */
function RuleName({ ruleId }: { ruleId: number }) {
  const query = useListRules()
  const rules = unwrap(query.data) ?? []
  const rule = rules.find((r) => r.id === ruleId)
  const label = rule?.name ?? `#${ruleId}`

  return (
    <Link
      to="/search"
      search={{ ruleId }}
      className="text-primary underline-offset-2 hover:underline"
    >
      {label}
    </Link>
  )
}

function Fields({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section>
      <h3 className="mb-2 text-xs font-medium text-muted-foreground">{title}</h3>
      <dl className="divide-y divide-border rounded-lg border border-border">{children}</dl>
    </section>
  )
}

function Field({
  label,
  value,
  note,
}: {
  label: string
  /** 文字列だけでなく要素も置ける（重複排除の根拠のように構造を持つ値があるため）。 */
  value: React.ReactNode
  note?: string
}) {
  return (
    <div className="flex items-baseline justify-between gap-3 px-3 py-2.5">
      <dt className="shrink-0 text-sm text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-right text-sm">
        <span className="break-all">{value}</span>
        {note && <span className="ml-2 text-xs text-muted-foreground">({note})</span>}
      </dd>
    </div>
  )
}
