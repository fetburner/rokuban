import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { RecordingPlaybackPoster } from '@/components/recording-playback-poster'
import { recordingThumbnailURL } from '@/lib/recording-media'

const timeline = {
  minSeconds: 0,
  maxSeconds: 3600,
  headSeconds: 0,
  recordedEndSeconds: 1500,
  plannedEndSeconds: 3540,
}

describe('RecordingPlaybackPoster', () => {
  it('保存位置があれば「続きから（時刻）」と「先頭から見る」を出す', () => {
    const onStart = vi.fn()
    const onStartFromBeginning = vi.fn()
    render(
      <RecordingPlaybackPoster
        recordingId={1}
        timeline={timeline}
        resumeSeconds={720}
        recordedSeconds={1500}
        onStart={onStart}
        onStartFromBeginning={onStartFromBeginning}
      />,
    )
    expect(screen.getByTestId('recording-playback-start')).toHaveAccessibleName('続きから再生（12:00）')
    fireEvent.click(screen.getByTestId('recording-playback-start'))
    expect(onStart).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: '先頭から見る' }))
    expect(onStartFromBeginning).toHaveBeenCalledTimes(1)
  })

  it('保存位置が無ければ「再生」だけで、同じ操作を 2 つ並べない', () => {
    render(
      <RecordingPlaybackPoster
        recordingId={1}
        timeline={timeline}
        recordedSeconds={1500}
        onStart={() => {}}
        onStartFromBeginning={() => {}}
      />,
    )
    expect(screen.getByTestId('recording-playback-start')).toHaveAccessibleName('再生')
    expect(screen.queryByRole('button', { name: '先頭から見る' })).not.toBeInTheDocument()
    expect(screen.queryByText(/続きから/)).not.toBeInTheDocument()
  })

  it('サムネイルが取れなければ壊れた画像を残さない', () => {
    const { container } = render(
      <RecordingPlaybackPoster recordingId={1} timeline={timeline} recordedSeconds={0} onStart={() => {}} />,
    )
    const image = container.querySelector('img')!
    expect(image).toHaveAttribute('src', recordingThumbnailURL(1))
    fireEvent.error(image)
    expect(container.querySelector('img')).toBeNull()
  })

  it('延長中は予定終端と先端を、そうでなければ予定終端だけを時間軸に書く', () => {
    const { rerender } = render(
      <RecordingPlaybackPoster recordingId={1} timeline={timeline} recordedSeconds={0} onStart={() => {}} />,
    )
    const preview = screen.getByTestId('recording-playback-preview-timeline')
    expect(preview).toHaveTextContent('59:00 まで（予定）')
    expect(preview).not.toHaveTextContent('延長中')
    rerender(
      <RecordingPlaybackPoster
        recordingId={1}
        timeline={{ ...timeline, plannedEndSeconds: 3000, recordedEndSeconds: 3612 }}
        recordedSeconds={0}
        onStart={() => {}}
      />,
    )
    expect(screen.getByTestId('recording-playback-preview-timeline')).toHaveTextContent('延長中 · 先端 60:12')
  })

  it('原本 HLS のポスターは視聴済みの操作を枠の中に持つ', () => {
    const onToggle = vi.fn()
    render(
      <RecordingPlaybackPoster
        recordingId={1}
        recordedSeconds={0}
        onStart={() => {}}
        watched={{ value: false, pending: false, onToggle }}
      />,
    )
    expect(screen.queryByTestId('recording-playback-preview-timeline')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '視聴済みにする' }))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })
})
