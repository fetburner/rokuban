import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { RecordingRow } from '@/components/recording-row'
import { recordingThumbnailURL } from '@/lib/recording-media'
import { renderInRouter } from '@/test/router'

const base = { liveCapability: 'enabled', trash: false, showSite: false, view: 'list' } as const

const recording: Recording = {
  id: 42,
  site: 'default',
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '共有する録画',
  startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: '2026-01-01T12:30:00Z',
}

describe('RecordingRow', () => {
  it('録画中でも一覧に追っかけ再生の導線を出さない', async () => {
    renderInRouter(
      <RecordingRow recording={{ ...recording, status: 'recording' }} {...base} />,
      { path: '/recordings' },
    )

    await screen.findByText('共有する録画')
    expect(await screen.findByText('録画中')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '共有する録画を追っかけ再生' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: '共有する録画' })).toHaveAttribute(
      'href',
      '/recordings/42',
    )
  })

  it('選択 props を渡さなくても詳細への全面リンクとして描ける', async () => {
    renderInRouter(<RecordingRow recording={recording} {...base} />, { path: '/recordings' })

    await screen.findByText('共有する録画')
    expect(screen.getByRole('link', { name: '共有する録画' })).toHaveAttribute(
      'href',
      '/recordings/42',
    )
    expect(screen.queryByRole('option')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })

  it('selecting だけ true で selected / onToggle を省略すると未選択のチェックボックスになる', async () => {
    renderInRouter(<RecordingRow recording={recording} {...base} selecting />, {
      path: '/recordings',
    })

    const box = await screen.findByRole('checkbox', { name: '共有する録画を選択' })
    expect(box).not.toBeChecked()
    expect(screen.getByRole('option')).toHaveAttribute('aria-selected', 'false')
    expect(screen.queryByRole('link')).not.toBeInTheDocument()
  })

  it('サムネイルが 404 で error になるとプレースホルダに落ちる', async () => {
    const { container } = renderInRouter(<RecordingRow recording={recording} {...base} />, {
      path: '/recordings',
    })

    await screen.findByText('共有する録画')
    const img = container.querySelector('img')
    expect(img).toHaveAttribute('src', recordingThumbnailURL(42))
    fireEvent.error(img!)
    expect(container.querySelector('img')).toBeNull()
  })

  it('ごみ箱ではサムネイルを要求せずプレースホルダ固定', async () => {
    const { container } = renderInRouter(<RecordingRow recording={recording} {...base} trash />, {
      path: '/recordings',
    })

    await screen.findByText('共有する録画')
    expect(container.querySelector('img')).toBeNull()
    expect(screen.queryByText('再生不可')).not.toBeInTheDocument()
    expect(screen.queryByText('完了')).not.toBeInTheDocument()
  })

  it('finished の録画に完了バッジを出さず、取り込み中は準備中と出す', async () => {
    renderInRouter(
      <RecordingRow
        recording={{ ...recording, ingest: { state: 'pending' } }}
        {...base}
      />,
      { path: '/recordings' },
    )

    await screen.findByText('共有する録画')
    expect(await screen.findByText('準備中')).toBeInTheDocument()
    expect(screen.queryByText('完了')).not.toBeInTheDocument()
  })

  it('再生元がある録画は結論バッジを出さない', async () => {
    renderInRouter(
      <RecordingRow recording={{ ...recording, sizeBytes: 1_000 }} {...base} />,
      { path: '/recordings' },
    )

    await screen.findByText('共有する録画')
    expect(screen.queryByText('準備中')).not.toBeInTheDocument()
    expect(screen.queryByText('完了')).not.toBeInTheDocument()
    expect(screen.queryByText('再生不可')).not.toBeInTheDocument()
  })
})
