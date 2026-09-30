import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { RecordingRow } from '@/components/recording-row'
import { renderInRouter } from '@/test/router'

const base = { trash: false, showSite: false, view: 'list', liveEnabled: false } as const

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

  it('selecting だけ true で selected / onToggle を省略すると未選択のチェックボックスで、押しても落ちない', async () => {
    renderInRouter(<RecordingRow recording={recording} {...base} selecting />, {
      path: '/recordings',
    })

    const box = await screen.findByRole('checkbox', { name: '共有する録画を選択' })
    expect(box).not.toBeChecked()
    expect(screen.getByRole('option')).toHaveAttribute('aria-selected', 'false')
    expect(screen.queryByRole('link')).not.toBeInTheDocument()
    fireEvent.click(box)
  })

  it('サムネイルが 404 で error になるとプレースホルダに落ちる', async () => {
    const { container } = renderInRouter(<RecordingRow recording={recording} {...base} />, {
      path: '/recordings',
    })

    await screen.findByText('共有する録画')
    const img = container.querySelector('img')
    expect(img).toHaveAttribute('src', '/api/media/recordings/42/thumbnail')
    fireEvent.error(img!)
    expect(container.querySelector('img')).toBeNull()
  })

  it('ごみ箱ではサムネイルを要求せずプレースホルダ固定', async () => {
    const { container } = renderInRouter(<RecordingRow recording={recording} {...base} trash />, {
      path: '/recordings',
    })

    await screen.findByText('共有する録画')
    expect(container.querySelector('img')).toBeNull()
  })
})
