import { useListRecordingDropStats, type Recording } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { cn } from '@/lib/utils'

// pidTypeLabels は PID 種別（M2-13）の表示名。
// 値の権威は Go 側（internal/tsstat）にあり、ここに無い値はそのまま表示する。
// 字幕と文字スーパーは stream_type だけでは区別できないので other にまとまる。
const pidTypeLabels: Record<string, string> = {
  video: '映像',
  audio: '音声',
  other: 'その他',
  pat: 'PAT',
  pmt: 'PMT',
  cat: 'CAT',
  nit: 'NIT',
  sdt: 'SDT',
  eit: 'EIT',
  tot: 'TOT',
}

function formatElapsedMs(ms: number): string {
  const totalSeconds = Math.floor(ms / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60
  const milliseconds = ms % 1000
  return `${hours.toString().padStart(2, '0')}:${minutes.toString().padStart(2, '0')}:${seconds
    .toString()
    .padStart(2, '0')}.${milliseconds.toString().padStart(3, '0')}`
}

export function DropStatsTable({ recordingId }: { recordingId: Recording['id'] }) {
  const query = useListRecordingDropStats(recordingId)
  const stats = unwrap(query.data) ?? []

  if (query.isPending) {
    return (
      <p role="status" className="text-muted-foreground">
        ドロップ統計を読み込み中…
      </p>
    )
  }
  if (query.isError) return <p className="text-destructive">ドロップ統計の取得に失敗しました</p>
  if (stats.length === 0) return null

  return (
    <section>
      <h4 className="mb-1 font-medium">PID 別ドロップ統計</h4>
      <div className="grid grid-cols-[auto_auto_1fr_1fr_1fr_1fr_minmax(0,2fr)] gap-x-3 gap-y-0.5">
        <span className="text-muted-foreground">PID</span>
        <span className="text-muted-foreground">種別</span>
        <span className="text-right text-muted-foreground">packets</span>
        <span className="text-right text-muted-foreground">ドロップ</span>
        <span className="text-right text-muted-foreground">エラー</span>
        <span className="text-right text-muted-foreground">スクランブル</span>
        <span className="text-muted-foreground">録画開始からの経過</span>
        {stats.map((s) => (
          <div key={s.pid} className="col-span-7 grid grid-cols-subgrid">
            <span>0x{s.pid.toString(16).padStart(4, '0')}</span>
            {/* 分類できなかった PID は種別なし（PID 番号だけで統計は成立する） */}
            <span className="text-muted-foreground">
              {s.pidType ? (pidTypeLabels[s.pidType] ?? s.pidType) : '—'}
            </span>
            <span className="text-right">{s.packets.toLocaleString()}</span>
            <span className={cn('text-right', s.drops > 0 && 'text-destructive')}>
              {s.drops.toLocaleString()}
            </span>
            <span className={cn('text-right', s.errors > 0 && 'text-destructive')}>
              {s.errors.toLocaleString()}
            </span>
            <span className={cn('text-right', s.scrambled > 0 && 'text-destructive')}>
              {s.scrambled.toLocaleString()}
            </span>
            <div className="min-w-0">
              <div className="flex flex-col">
                {(s.positions ?? []).map((position) => (
                  <span key={position.byteOffset}>
                    {position.elapsedMs == null ? '時刻不明' : formatElapsedMs(position.elapsedMs)}{' '}
                    <span className="text-muted-foreground">
                      （byte {position.byteOffset.toLocaleString()}）
                    </span>
                  </span>
                ))}
                {s.drops > (s.positions ?? []).length && (
                  <span className="text-muted-foreground">
                    {s.drops.toLocaleString()} 件中 {(s.positions ?? []).length.toLocaleString()} 件を表示
                  </span>
                )}
                {s.drops === 0 && (s.positions ?? []).length === 0 && '—'}
              </div>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}
