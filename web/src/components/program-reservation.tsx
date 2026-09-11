import { Link } from '@tanstack/react-router'
import { Play } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { useGetProgram, type ProgramOverlaps, type ProgramOverridesInput } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EncodeSettingsFields } from '@/components/encode-settings-fields'
import { ProgramOverlapWarning } from '@/components/program-overlap-warning'
import { Button } from '@/components/ui/button'
import { useLiveEnabled } from '@/lib/capabilities'
import {
  defaultEncodeSettingsValue,
  encodeSettingsError,
  encodeSettingsOverridesBody,
  sameEncodeSettingsValue,
  type EncodeSettingsValue,
} from '@/lib/encode-settings'
import { formatDuration, formatTime, isAiring } from '@/lib/format'
import type { SiteProgram } from '@/lib/all-sites-services'
import { composeServiceId } from '@/lib/service-id'

/**
 * 番組の予約 UI が描画に必要とする最小形。
 *
 * 番組表は `endAt` / `description` を持つ `SiteProgram` を渡すが、検索 API の
 * 表示用射影にはその 2 つが含まれない。終了時刻は `startAt + durationMs` から
 * 導出し、説明は展開時に取得した詳細へフォールバックする。
 */
export type ProgramReservationProgram = Pick<
  SiteProgram,
  | 'site'
  | 'programId'
  | 'networkId'
  | 'serviceId'
  | 'startAt'
  | 'durationMs'
  | 'name'
  | 'isFree'
> & {
  endAt?: string
  description?: string
}

/**
 * 番組予約 UI の下書きと導出状態。
 *
 * リスト行とグリッドのダイアログは表示 chrome だけが異なる。同じ下書きと
 * `reserveBlocked` / `showLiveLink` を使うことで、encode 既定値・予約状態不明・
 * 放送中判定の片側だけがずれる経路を作らない。
 */
export type ProgramReservationDraft = {
  encodeValue: EncodeSettingsValue
  setEncodeValue: (value: EncodeSettingsValue) => void
  reserveBlocked: boolean
  handleReserve: () => void
  showLiveLink: boolean
}

/**
 * 予約操作に必要な状態を共有する。
 *
 * encode が既定値のままなら `undefined` を渡し、overrides の PATCH を発生させない。
 * 予約一覧が未取得の間は未予約側を止める。これはリストとダイアログで複製しては
 * ならない規則なので、表示側はこの hook の結果だけを使う。
 */
// oxlint-disable-next-line react/only-export-components -- 予約状態 hook と chrome なしの予約部品を同じ共有契約に置く
export function useProgramReservation({
  program,
  pending,
  reservationStateUnknown,
  onReserve,
}: {
  program: ProgramReservationProgram
  pending: boolean
  reservationStateUnknown: boolean
  onReserve: (overrides?: ProgramOverridesInput) => void
}): ProgramReservationDraft {
  const liveEnabled = useLiveEnabled()
  const [encodeValue, setEncodeValue] = useState<EncodeSettingsValue>(
    defaultEncodeSettingsValue(),
  )

  const encodeError = encodeSettingsError(encodeValue.keepOriginal, encodeValue.encodeProfiles)
  const encodeDirty = !sameEncodeSettingsValue(encodeValue, defaultEncodeSettingsValue())
  const reserveBlocked =
    pending || reservationStateUnknown || (encodeDirty && encodeError !== undefined)

  const handleReserve = () => {
    if (reserveBlocked) return
    onReserve(encodeSettingsOverridesBody(encodeValue, defaultEncodeSettingsValue()))
  }

  const endAt =
    program.endAt ?? new Date(Date.parse(program.startAt) + program.durationMs).toISOString()

  return {
    encodeValue,
    setEncodeValue,
    reserveBlocked,
    handleReserve,
    // 放送中判定は描画時＋その後の再レンダーの評価のみで、専用の tick を持たない。
    // それでも遷移先を誤らないのは、ライブ導線が programId を運ばずチャンネル
    // （networkId + serviceId）だけを渡すため --- /live が「いま何が流れているか」を
    // 自前で再取得する側に真実がある。
    showLiveLink: liveEnabled && isAiring(program.startAt, endAt),
  }
}

/**
 * 番組の要約。リストの行トグルとダイアログの見出しのどちらにも載せる chrome なしの
 * 共通部分で、時刻・題名・サービス/site・尺・有料・重なり警告の規則を共有する。
 * `title` を渡す経路では、その要素（ダイアログの `DialogTitle` など）を題名として使う。
 */
export function ProgramReservationSummary({
  program,
  serviceName,
  siteName,
  reserved,
  overlaps,
  title,
}: {
  program: ProgramReservationProgram
  serviceName?: string
  siteName?: string
  reserved: boolean
  overlaps?: ProgramOverlaps
  title?: ReactNode
}) {
  const endAt =
    program.endAt ?? new Date(Date.parse(program.startAt) + program.durationMs).toISOString()
  const airing = isAiring(program.startAt, endAt)

  return (
    <div className="flex min-w-0 flex-1 items-center gap-3">
      <div className="w-11 shrink-0 text-sm">
        <div data-testid="program-row-time" className={airing ? 'font-medium' : undefined}>
          {formatTime(program.startAt)}
        </div>
      </div>
      <div className="min-w-0 flex-1">
        {title ?? <div className="truncate text-base">{program.name}</div>}
        <div
          data-testid="program-row-meta"
          className="flex items-center gap-2 text-sm text-muted-foreground"
        >
          {siteName && <span className="shrink-0">{siteName}</span>}
          {serviceName && <span className="truncate">{serviceName}</span>}
          <span className="shrink-0">{formatDuration(program.durationMs)}</span>
          {!program.isFree && <span className="shrink-0">有料</span>}
        </div>
        {!reserved && <ProgramOverlapWarning overlaps={overlaps} />}
      </div>
    </div>
  )
}

/**
 * 予約 / 取消 / ライブのボタンそのもの。配置と幅のアニメーションは親の chrome に
 * 任せるため、ここではボタンの意味と最小タップ領域だけを共有する。
 */
export function ProgramReservationActions({
  program,
  reserved,
  pending,
  reserveBlocked,
  showLiveLink,
  onReserve,
  onCancel,
}: {
  program: ProgramReservationProgram
  reserved: boolean
  pending: boolean
  reserveBlocked: boolean
  showLiveLink: boolean
  onReserve: () => void
  onCancel: () => void
}) {
  return (
    <>
      {showLiveLink && (
        <div data-program-action="live" className="shrink-0">
          <Button
            variant="default"
            size="icon"
            aria-label="ライブで見る"
            render={
              <Link
                to="/live"
                search={{
                  service: composeServiceId(program.networkId, program.serviceId),
                  site: program.site,
                }}
              />
            }
            className="min-h-11 min-w-11 w-full rounded-none"
          >
            <Play />
          </Button>
        </div>
      )}
      <div data-program-action="reserve" className="min-w-0 flex-1">
        <Button
          variant={reserved ? 'destructive' : 'default'}
          size="sm"
          disabled={reserved ? pending : reserveBlocked}
          onClick={reserved ? onCancel : onReserve}
          className="min-h-11 w-full rounded-none"
        >
          {/* 楽観更新でラベルを即時確定させるため、送信中もスピナーは重ねない。 */}
          {reserved ? '取消' : '予約'}
        </Button>
      </div>
    </>
  )
}

/**
 * 詳細本文と、未予約時の encode 下書き / 予約済み時の設定リンク。
 * リストの行区切りやダイアログの余白は持たず、両方の chrome から同じ意味の中身を
 * 使えるようにする。
 */
export function ProgramReservationBody({
  program,
  reserved,
  pending,
  draft,
}: {
  program: ProgramReservationProgram
  reserved: boolean
  pending: boolean
  draft: ProgramReservationDraft
}) {
  return (
    <>
      <ProgramReservationDetails program={program} />

      {reserved && (
        <div className="mt-3 flex flex-wrap gap-4 text-xs">
          <Link
            to="/reservations/$site/$programId"
            params={{ site: program.site, programId: String(program.programId) }}
            className="text-primary underline-offset-2 hover:underline"
          >
            予約の設定
          </Link>
        </div>
      )}

      {!reserved && (
        <div className="mt-3 border-t border-border pt-3">
          <EncodeSettingsFields
            value={draft.encodeValue}
            onChange={draft.setEncodeValue}
            disabled={pending}
            note="ここでの変更はこの画面に保存ボタンを持ちません。「予約」を押した時点で反映されます。"
          />
        </div>
      )}
    </>
  )
}

/**
 * 説明・出演者・映像音声属性は一覧レスポンスに含まれないため、表示した時だけ詳細を
 * 取得する（段階的開示）。
 */
function ProgramReservationDetails({ program }: { program: ProgramReservationProgram }) {
  const detail = useGetProgram(program.site, program.programId)
  const d = unwrap(detail.data)
  const description = program.description ?? d?.description

  return (
    <div className="flex flex-col gap-2 text-xs">
      {description && (
        <p className="whitespace-pre-wrap text-muted-foreground">{description}</p>
      )}

      {detail.isPending && (
        <p role="status" className="text-muted-foreground">
          詳細を読み込み中…
        </p>
      )}
      {detail.isError && <p className="text-destructive">詳細の取得に失敗しました</p>}

      {d?.extended && Object.keys(d.extended).length > 0 && (
        <dl className="flex flex-col gap-1">
          {Object.entries(d.extended).map(([key, value]) => (
            <div key={key}>
              <dt className="font-medium">{key}</dt>
              <dd className="whitespace-pre-wrap text-muted-foreground">{value}</dd>
            </div>
          ))}
        </dl>
      )}

      {(d?.video || d?.audios) && (
        <p className="text-muted-foreground">
          {[
            d.video?.resolution,
            d.audios?.length ? `音声 ${d.audios.length}` : undefined,
            d.audios?.flatMap((a) => a.langs ?? []).join('/') || undefined,
          ]
            .filter(Boolean)
            .join(' · ')}
        </p>
      )}
    </div>
  )
}
