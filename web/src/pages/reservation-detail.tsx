import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'

import {
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
  const patchOverrides = usePatchProgramOverrides()

  const reservation = unwrap(query.data)

  // 取消は (site, programId) を宛先に intent{skip} を書くだけ（issue #29）。
  // reservations 行には触れない。ruler が次パスで行を落とすまでの間、
  // 一覧側は楽観更新で見た目を反映する（この画面はナビゲーションで離れるので
  // 楽観表示は不要）。
  const cancel = () => {
    if (!reservation) return
    putIntent.mutate(
      { site: reservation.site, programId: reservation.programId, data: { action: 'skip' } },
      {
        onSuccess: () => {
          toast({ message: '予約を取消しました' })
          void navigate({ to: '/reservations' })
        },
        onError: () => toast({ message: '予約の取消に失敗しました' }),
      },
    )
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
        style={{ top: 'var(--breaker-banner-height, 0px)' }}
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
            <p className="mt-1 text-sm text-muted-foreground">
              {reservation.serviceName} · {formatDateTime(reservation.startAt)} ·{' '}
              {formatDuration(reservation.durationMs)}
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
