import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { formatPlaybackTime } from '@/lib/format'

/**
 * RecordingPlaybackPoster は録画 HLSの再生前の面である。
 * セッションは ▶ を押して初めて始める。
 *
 * **枠の寸法は再生後のプレイヤー枠（`aspect-video w-full`）と同じにする。** 時間軸は枠の外に
 * 足さず、映像の下端に重ねる（再生後は操作バーが同じ位置にある）。枠の外に帯を足すと、押した
 * 瞬間にページが跳ぶ。
 */
export function RecordingPlaybackPoster({
  thumbnailUrl,
  resumeSeconds,
  onStart,
  onStartFromBeginning,
  watched,
}: {
  /** `Recording.media.thumbnailUrl`。 */
  thumbnailUrl: string
  /** 再生前に時間軸を描く場合だけ指定する。 */
  /** 保存された再生位置（秒）。無い・先頭に近いときは「再生」だけを出す。 */
  resumeSeconds?: number
  onStart: () => void
  /** 渡すと「先頭から見る」を出す。 */
  onStartFromBeginning?: () => void
  /** 原本 HLS の完了録画の視聴済み操作。枠の中に置く（枠の外に出すと押した後に消えて跳ぶ）。 */
  watched?: { value: boolean; pending: boolean; onToggle: () => void }
}) {
  // 録画中はサムネイルが未生成のことが普通。壊れた画像のアイコンを出さず、地のまま見せる。
  const [thumbnailFailed, setThumbnailFailed] = useState(false)
  const hasResume = resumeSeconds !== undefined && resumeSeconds >= 2
  const resumeLabel = hasResume ? `続きから（${formatPlaybackTime(resumeSeconds)}）` : undefined

  return (
    <div
      data-testid="recording-playback-poster"
      className="relative aspect-video w-full overflow-hidden rounded bg-neutral-950 text-white"
    >
      {!thumbnailFailed && (
        <img
          src={thumbnailUrl}
          alt=""
          className="absolute inset-0 size-full object-cover opacity-60"
          onError={() => setThumbnailFailed(true)}
        />
      )}
      <div className="absolute inset-0 bg-gradient-to-t from-black/75 via-black/20 to-black/50" />
      <div
        className="absolute inset-0 flex flex-col items-center justify-center gap-1 px-4 text-center"
      >
        <button
          type="button"
          data-testid="recording-playback-start"
          aria-label={resumeLabel ? `続きから再生（${formatPlaybackTime(resumeSeconds ?? 0)}）` : '再生'}
          onClick={onStart}
          className="flex flex-col items-center gap-1 md:gap-3"
        >
          <span className="grid size-12 place-items-center rounded-full bg-white/90 text-2xl text-black shadow-lg md:size-16 md:text-3xl">
            ▶
          </span>
          <span className="text-base font-semibold md:text-lg">{resumeLabel ?? '再生'}</span>
        </button>
        {onStartFromBeginning && hasResume && (
          <button
            type="button"
            data-testid="recording-playback-start-from-beginning"
            onClick={onStartFromBeginning}
            className="inline-flex min-h-8 items-center px-3 text-sm underline underline-offset-2"
          >
            先頭から見る
          </button>
        )}
      </div>
      {watched && (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-pressed={watched.value}
          disabled={watched.pending}
          onClick={watched.onToggle}
          className="absolute top-2 right-2 z-10 bg-black/45 text-white hover:bg-black/60 hover:text-white"
        >
          {watched.value ? '未視聴に戻す' : '視聴済みにする'}
        </Button>
      )}
    </div>
  )
}
