import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'

import {
  useDeleteReservation,
  useGetReservation,
  type Reservation,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ErrorState, ListSkeleton } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { formatDateTime, formatDuration } from '@/lib/format'

/**
 * ReservationDetailPage は予約 1 件の詳細。
 *
 * base（ルールが書く）と overrides（ユーザーが書く）は同形の jsonb なので、
 * ルール由来予約と手動予約を 1 画面で扱える（EPGStation は編集画面が分裂している）。
 * M1 は全予約が manual なので overrides のみが存在する。
 *
 * 編集コントロールは置かない。reconciler が予約オプションの差分を反映しないため、
 * 編集できても mirakc に伝わらない（issue #19）。解決後にここへ差し込む。
 */
export function ReservationDetailPage() {
  const { reservationId } = useParams({ from: '/reservations/$reservationId' })
  const navigate = useNavigate()
  const toast = useToast()

  const id = Number(reservationId)
  const query = useGetReservation(id)
  const deleteReservation = useDeleteReservation()

  const reservation = unwrap(query.data)

  const cancel = () => {
    deleteReservation.mutate(
      { id },
      {
        onSuccess: () => {
          toast({ message: '予約を取消しました' })
          void navigate({ to: '/reservations' })
        },
        onError: () => toast({ message: '予約の取消に失敗しました' }),
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
              {formatDateTime(reservation.startAt)} · {formatDuration(reservation.durationMs)}
            </p>
          </section>

          <Fields title="予約">
            <Field label="状態" value={reservation.state} />
            <Field label="種別" value={reservation.source === 'manual' ? '手動' : 'ルール'} />
            {reservation.ruleId !== undefined && (
              <Field label="ルール" value={`#${reservation.ruleId}`} />
            )}
            <Field label="programId" value={String(reservation.programId)} />
          </Fields>

          <Fields title="録画の設定">
            <Field
              label="優先度"
              value={overrideValue(reservation, 'priority') ?? '既定'}
              note="#19 の解決後に編集できるようにする"
            />
            <Field label="保存先パス" value="自動生成" />
          </Fields>

          <Fields title="エンコードと保持">
            <Field label="エンコード" value="なし" note="M3" />
            <Field label="原本の保持" value="常に保持" note="M3" />
          </Fields>

          <Button
            variant="destructive"
            size="lg"
            className="w-full"
            disabled={deleteReservation.isPending}
            onClick={cancel}
          >
            {deleteReservation.isPending ? '取消中…' : '予約を取消'}
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
  value: string
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
