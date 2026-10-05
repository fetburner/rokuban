import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { formatDuration, formatPlaybackTime } from '@/lib/format'
import type { PlaybackTimeline } from '@/lib/playback-timeline'
import { recordingThumbnailURL } from '@/lib/recording-media'

/** PosterTimeline は再生前のポスターに描く追っかけ時間軸の表示項目。 */
export type PosterTimeline = Pick<
  Extract<PlaybackTimeline, { kind: 'chase' }>,
  'minSeconds' | 'maxSeconds' | 'headSeconds' | 'recordedEndSeconds' | 'plannedEndSeconds'
>

/**
 * RecordingPlaybackPoster は変換を伴う再生元（追っかけ・原本 HLS）の再生前の面である。
 * セッションは ▶ を押して初めて始める。
 *
 * **枠の寸法は再生後のプレイヤー枠（`aspect-video w-full`）と同じにする。** 時間軸は枠の外に
 * 足さず、映像の下端に重ねる（再生後は操作バーが同じ位置にある）。枠の外に帯を足すと、押した
 * 瞬間にページが跳ぶ。
 */
export function RecordingPlaybackPoster({
  recordingId,
  timeline,
  resumeSeconds,
  recordedSeconds,
  onStart,
  onStartFromBeginning,
  watched,
}: {
  recordingId: number
  /** 追っかけのときだけ。無ければ時間軸を描かない（原本 HLS）。 */
  timeline?: PosterTimeline
  /** 保存された再生位置（秒）。無い・先頭に近いときは「再生」だけを出す。 */
  resumeSeconds?: number
  recordedSeconds: number
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
  const span = timeline ? Math.max(1, timeline.maxSeconds - timeline.minSeconds) : 1
  const fraction = (value: number) => (timeline ? Math.max(0, Math.min(1, (value - timeline.minSeconds) / span)) : 0)
  const time = (value: number) => formatPlaybackTime(value, false)
  const extended = timeline !== undefined && timeline.recordedEndSeconds > timeline.plannedEndSeconds

  return (
    <div
      data-testid="recording-playback-poster"
      className="relative aspect-video w-full overflow-hidden rounded bg-neutral-950 text-white"
    >
      {!thumbnailFailed && (
        <img
          src={recordingThumbnailURL(recordingId)}
          alt=""
          className="absolute inset-0 size-full object-cover opacity-60"
          onError={() => setThumbnailFailed(true)}
        />
      )}
      <div className="absolute inset-0 bg-gradient-to-t from-black/75 via-black/20 to-black/50" />
      <div
        className={
          timeline
            ? 'absolute inset-x-0 top-0 bottom-14 flex flex-col items-center justify-center gap-1 px-4 text-center md:bottom-16 md:gap-2'
            : 'absolute inset-0 flex flex-col items-center justify-center gap-1 px-4 text-center'
        }
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
          {timeline && (
            <span className="hidden text-sm text-white/80 sm:block">
              録画済み {formatDuration(recordedSeconds * 1000)} · 押すと追っかけ再生を始めます
            </span>
          )}
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
      {timeline && (
        <div
          data-testid="recording-playback-preview-timeline"
          role="img"
          aria-label={`録画時間: ${time(timeline.minSeconds)} から ${time(timeline.plannedEndSeconds)} まで（予定）、録画済み ${time(timeline.recordedEndSeconds)}`}
          className="absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/95 via-black/70 to-transparent px-3 pt-8 pb-2 md:pb-3"
        >
          <div data-testid="recording-playback-preview-track" className="relative h-6" aria-hidden="true">
            <div className="absolute inset-x-0 top-1/2 h-1 -translate-y-1/2 overflow-hidden rounded-full bg-white/30">
              <div
                data-testid="recording-playback-preview-recorded"
                className="absolute inset-y-0 bg-white/55"
                style={{
                  left: `${fraction(timeline.headSeconds) * 100}%`,
                  width: `${Math.max(0, fraction(timeline.recordedEndSeconds) - fraction(timeline.headSeconds)) * 100}%`,
                }}
              />
            </div>
            <div
              className="absolute top-1/2 z-[2] h-4 -translate-y-1/2 border-l-2 border-dashed border-white"
              style={{ left: `${fraction(timeline.plannedEndSeconds) * 100}%` }}
            />
            <div
              data-testid="recording-playback-preview-live-edge"
              className="absolute top-1/2 z-[3] h-4 w-0.5 -translate-x-1/2 -translate-y-1/2 bg-red-500"
              style={{ left: `${fraction(timeline.recordedEndSeconds) * 100}%` }}
            />
          </div>
          <div className="relative mt-1 h-4 text-xs whitespace-nowrap text-white/75">
            <span className="absolute left-0 font-mono">{time(timeline.minSeconds)}</span>
            {extended ? (
              <>
                <span
                  className="absolute hidden md:inline"
                  style={{ right: `${(1 - fraction(timeline.plannedEndSeconds)) * 100}%`, transform: 'translateX(50%)' }}
                >
                  予定 <span className="font-mono">{time(timeline.plannedEndSeconds)}</span> ¦
                </span>
                <span className="absolute right-0 text-red-400">
                  延長中 · 先端 <span className="font-mono">{time(timeline.recordedEndSeconds)}</span>
                </span>
              </>
            ) : (
              <span className="absolute right-0">
                <span className="font-mono">{time(timeline.plannedEndSeconds)}</span> まで（予定）
              </span>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
