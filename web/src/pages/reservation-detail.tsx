import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { useEffect, useState } from 'react'

import {
  getGetReservationQueryKey,
  useGetReservation,
  usePatchProgramOverrides,
  usePutProgramIntent,
  type ProgramOverridesInput,
  type Reservation,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import {
  EncodeSettingsFields,
  type EncodeSettingsValue,
} from '@/components/encode-settings-fields'
import { ErrorState, ListSkeleton } from '@/components/page'
import { ProgramOverlapWarning } from '@/components/program-overlap-warning'
import { ReservationSkipReason } from '@/components/reservation-skip-reason'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import {
  encodeSettingsError,
  keepOriginalLabel,
  type KeepOriginal,
} from '@/lib/encode-settings'
import { formatDateTime, formatDuration } from '@/lib/format'

/**
 * ReservationDetailPage は予約 1 件の詳細。
 *
 * base（ルールが書く）と overrides（ユーザーが書く）は同形の jsonb なので、
 * ルール由来予約と手動予約を 1 画面で扱える（EPGStation は編集画面が分裂している）。
 *
 * encodeProfiles / keepOriginal は M3 で worker が消費するため編集可能
 * （issue #68）。priority など mirakc 差分が必要な項目は #19 解決後に足す。
 */
export function ReservationDetailPage() {
  const { reservationId } = useParams({ from: '/reservations/$reservationId' })
  const navigate = useNavigate()
  const toast = useToast()
  const queryClient = useQueryClient()

  const id = Number(reservationId)
  const query = useGetReservation(id)
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
          void queryClient.invalidateQueries({ queryKey: getGetReservationQueryKey(id) })
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
              {formatDateTime(reservation.startAt)} · {formatDuration(reservation.durationMs)}
            </p>
            {/* この予約が作られたあとで他の予約が増え、重なりが生じることもあるので
                詳細画面でも常に出す（issue #24 M2-8。件数だけ・断定なし）。 */}
            <div className="mt-2">
              <ProgramOverlapWarning programId={reservation.programId} />
            </div>
          </section>

          <Fields title="予約">
            <Field label="状態" value={reservation.state} />
            <Field label="種別" value={reservation.source === 'manual' ? '手動' : 'ルール'} />
            {/* 予約行が残っているのに録画されない状態は、それ自体が説明を要する。
                重複排除なら根拠（録画 id と類似度）まで出す（issue #24 M2-6）。 */}
            {reservation.skip && (
              <Field label="録画" value={<ReservationSkipReason reservation={reservation} />} />
            )}
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

          <section>
            <h3 className="mb-2 text-xs font-medium text-muted-foreground">エンコードと保持</h3>
            <EncodeOverridesEditor
              reservation={reservation}
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

/**
 * EncodeOverridesEditor は予約 overrides の encodeProfiles / keepOriginal を編集する。
 *
 * 予約 GET は effective ではなく overrides jsonb だけを返すので、ここに出るのは
 * 「ユーザーが上書きした値」。未設定なら既定（always / 空）をフォーム初期値にし、
 * 保存時に PATCH で書く。
 */
function EncodeOverridesEditor({
  reservation,
  isPending,
  onSave,
}: {
  reservation: Reservation
  isPending: boolean
  onSave: (body: ProgramOverridesInput) => void
}) {
  const fromOverrides = encodeFromOverrides(reservation)
  const [value, setValue] = useState<EncodeSettingsValue>(fromOverrides)
  // サーバー側の overrides が変わったらフォームを同期する（保存後の invalidate など）。
  useEffect(() => {
    setValue(encodeFromOverrides(reservation))
  }, [reservation])

  const error = encodeSettingsError(value.keepOriginal, value.encodeProfiles)
  const dirty =
    value.keepOriginal !== fromOverrides.keepOriginal ||
    !sameStringSet(value.encodeProfiles, fromOverrides.encodeProfiles)

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border p-3">
      <p className="text-xs text-muted-foreground">
        現在の上書き:{' '}
        {hasEncodeOverride(reservation)
          ? `${keepOriginalLabel(fromOverrides.keepOriginal)} / ${
              fromOverrides.encodeProfiles.length === 0
                ? 'プロファイルなし'
                : fromOverrides.encodeProfiles.join(', ')
            }`
          : 'なし（ルールまたは既定）'}
      </p>
      <EncodeSettingsFields
        value={value}
        onChange={setValue}
        disabled={isPending}
        note="この予約だけを上書きします。"
      />
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          size="lg"
          disabled={!dirty || error !== undefined || isPending}
          onClick={() =>
            onSave({
              keepOriginal: value.keepOriginal,
              encodeProfiles: value.encodeProfiles,
            })
          }
        >
          {isPending ? '保存中…' : '上書きを保存'}
        </Button>
        {hasEncodeOverride(reservation) && (
          <Button
            type="button"
            variant="outline"
            size="lg"
            disabled={isPending}
            onClick={() =>
              onSave({
                reset: ['keepOriginal', 'encodeProfiles'],
              })
            }
          >
            ルールに戻す
          </Button>
        )}
      </div>
    </div>
  )
}

function encodeFromOverrides(reservation: Reservation): EncodeSettingsValue {
  const o = reservation.overrides
  const keepRaw = o?.keepOriginal
  const keepOriginal: KeepOriginal =
    keepRaw === 'until_encoded' || keepRaw === 'always' ? keepRaw : 'always'
  const profilesRaw = o?.encodeProfiles
  const encodeProfiles = Array.isArray(profilesRaw)
    ? profilesRaw.filter((p): p is string => typeof p === 'string')
    : []
  return { keepOriginal, encodeProfiles }
}

function hasEncodeOverride(reservation: Reservation): boolean {
  const o = reservation.overrides
  if (o === undefined || o === null) return false
  return o.keepOriginal !== undefined || o.encodeProfiles !== undefined
}

function sameStringSet(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false
  const set = new Set(b)
  return a.every((x) => set.has(x))
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
