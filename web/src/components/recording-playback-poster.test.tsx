import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { RecordingPlaybackPoster } from '@/components/recording-playback-poster'

describe('RecordingPlaybackPoster', () => {
  it('保存位置があれば「続きから（時刻）」と「先頭から見る」を出す', () => {
    const onStart = vi.fn()
    const onStartFromBeginning = vi.fn()
    render(
      <RecordingPlaybackPoster
        thumbnailUrl="/api/media/recordings/1/thumbnail?fixture=1"
        resumeSeconds={720}
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
        thumbnailUrl="/api/media/recordings/1/thumbnail?fixture=1"
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
      <RecordingPlaybackPoster thumbnailUrl="/api/media/recordings/1/thumbnail?fixture=1" onStart={() => {}} />,
    )
    const image = container.querySelector('img')!
    expect(image).toHaveAttribute('src', '/api/media/recordings/1/thumbnail?fixture=1')
    fireEvent.error(image)
    expect(container.querySelector('img')).toBeNull()
  })

  it('原本 HLS のポスターは視聴済みの操作を枠の中に持つ', () => {
    const onToggle = vi.fn()
    render(
      <RecordingPlaybackPoster
        thumbnailUrl="/api/media/recordings/1/thumbnail?fixture=1"
        onStart={() => {}}
        watched={{ value: false, pending: false, onToggle }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: '視聴済みにする' }))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })
})
