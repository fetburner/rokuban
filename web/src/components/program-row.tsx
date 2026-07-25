import { ChevronDown, Loader2 } from 'lucide-react'
import { useState } from 'react'

import { useGetProgram, type ProgramListItem } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { Button } from '@/components/ui/button'
import { formatDuration, formatTime, isAiring } from '@/lib/format'
import { cn } from '@/lib/utils'

/**
 * ProgramRow は番組リストの 1 行。
 *
 * 行本体のタップで詳細を展開し、予約ボタンは右端に固定幅で分離する。
 * スクロール中に予約ボタンへ誤って触れないようタップ領域を分けている。
 */
export function ProgramRow({
  program,
  serviceName,
  reservationId,
  pending,
  onReserve,
  onCancel,
}: {
  program: ProgramListItem
  serviceName?: string
  reservationId?: number
  pending: boolean
  onReserve: () => void
  onCancel: () => void
}) {
  const [expanded, setExpanded] = useState(false)
  const reserved = reservationId !== undefined

  return (
    <div className="flex items-stretch border-b border-border">
      <button
        type="button"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
        className="flex min-h-14 min-w-0 flex-1 items-center gap-3 px-4 py-2.5 text-left hover:bg-muted/50"
      >
        <div className="w-11 shrink-0 text-sm tabular-nums">
          <div className={cn(isAiring(program.startAt, program.endAt) && 'text-primary')}>
            {formatTime(program.startAt)}
          </div>
        </div>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm">{program.name}</div>
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            {serviceName && <span className="truncate">{serviceName}</span>}
            <span className="shrink-0">{formatDuration(program.durationMs)}</span>
            {!program.isFree && <span className="shrink-0">有料</span>}
          </div>
          {expanded && <ProgramDetail program={program} />}
        </div>
        <ChevronDown
          className={cn(
            'size-4 shrink-0 text-muted-foreground transition-transform',
            expanded && 'rotate-180',
          )}
        />
      </button>

      {/* 予約ボタンは行本体と分離した固定幅。最小 44px のタップ領域を確保する */}
      <div className="flex w-20 shrink-0 items-center justify-center border-l border-border">
        <Button
          variant={reserved ? 'destructive' : 'default'}
          size="sm"
          disabled={pending}
          onClick={reserved ? onCancel : onReserve}
          className="min-h-11 w-full rounded-none"
        >
          {pending ? (
            <Loader2 className="size-4 animate-spin" />
          ) : reserved ? (
            '取消'
          ) : (
            '予約'
          )}
        </Button>
      </div>
    </div>
  )
}

/**
 * ProgramDetail は展開時に表示する詳細。
 *
 * 説明・出演者・映像音声属性は一覧レスポンスに含まれないため、
 * 展開したときに GET /api/programs/{id} で取得する（段階的開示）。
 */
function ProgramDetail({ program }: { program: ProgramListItem }) {
  const detail = useGetProgram(program.programId)
  const d = unwrap(detail.data)

  return (
    <div className="mt-2 flex flex-col gap-2 text-xs">
      {program.description && (
        <p className="whitespace-pre-wrap text-muted-foreground">{program.description}</p>
      )}

      {detail.isPending && <p className="text-muted-foreground">詳細を読み込み中…</p>}
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
