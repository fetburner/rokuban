import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { recordingMedia } from '@/test/recording-media'
import { recordingVerdict } from '@/lib/recording-verdict'

const base: Recording = {
  id: 42,
  site: 'default',
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '結論テスト',
  startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always',
  hlsPlayback: [],
  media: recordingMedia(42),
  cmDetection: { state: 'disabled' },
  createdAt: '2026-01-01T12:30:00Z',
}

describe('recordingVerdict', () => {
  it.each([
    ['recording', { status: 'recording' }, 'recording'],
    ['failed', { status: 'failed' }, 'failed'],
    ['encoded asset', { encodedAssets: [{ profile: 'web', sizeBytes: 500 }] }, 'viewable'],
    ['ingest pending', { ingest: { state: 'pending' } }, 'preparing'],
    ['no playback source', {}, 'unavailable'],
    ['original without recording HLS', { sizeBytes: 1_000 }, 'unavailable'],
    ['recording HLS', {
      sizeBytes: 1_000,
      hlsPlayback: [{ profile: 'hd', masterUrl: '/api/media/stores/main/recordings/42/hls/v1/hd/master.m3u8' }],
    }, 'viewable'],
  ] as const)('returns %s verdict', (_name, recording, expected) => {
    expect(recordingVerdict({ recording: { ...base, ...recording } as Recording })).toBe(expected)
  })

  it('keeps a stalled ingest in preparing', () => {
    expect(recordingVerdict({
      recording: {
        ...base,
        ingest: { state: 'transferring', writtenBytes: 12, observedAt: '2026-01-01T12:00:00Z' },
      },
    })).toBe('preparing')
  })

  it.each(['queued', 'running'] as const)('keeps encode %s in preparing', (state) => {
    expect(recordingVerdict({
      recording: { ...base, encodeStatus: [{ profile: 'web', state }] },
    })).toBe('preparing')
  })

  it('does not report a verdict for recordings in trash', () => {
    expect(recordingVerdict({ recording: { ...base, status: 'failed' }, isTrashed: true })).toBeUndefined()
  })
})
