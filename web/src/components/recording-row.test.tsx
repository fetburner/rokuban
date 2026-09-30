import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { RecordingRow } from '@/components/recording-row'
import { renderInRouter } from '@/test/router'

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
    renderInRouter(<RecordingRow recording={recording} />, { path: '/recordings' })

    await screen.findByText('共有する録画')
    expect(screen.getByRole('link', { name: '共有する録画' })).toHaveAttribute(
      'href',
      '/recordings/42',
    )
    expect(screen.queryByRole('option')).not.toBeInTheDocument()
  })
})
