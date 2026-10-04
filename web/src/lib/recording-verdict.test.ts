import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import type { LiveCapability } from '@/lib/capabilities'
import { recordingVerdict, type RecordingVerdict } from '@/lib/recording-verdict'

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
  cmDetection: { state: 'disabled' },
  createdAt: '2026-01-01T12:30:00Z',
}

const nowMs = Date.parse('2026-01-01T14:00:00Z')
const verdictCases: {
  name: string
  recording: Partial<Recording>
  liveCapability: LiveCapability
  expected: RecordingVerdict
}[] = [
  { name: 'recording', recording: { status: 'recording' }, liveCapability: 'enabled', expected: 'recording' },
  { name: 'failed', recording: { status: 'failed' }, liveCapability: 'enabled', expected: 'failed' },
  {
    name: 'viewable through an encoded asset',
    recording: { encodedAssets: [{ profile: 'web', sizeBytes: 500 }] },
    liveCapability: 'unknown',
    expected: 'viewable',
  },
  {
    name: 'preparing while ingest is pending',
    recording: { ingest: { state: 'pending' } },
    liveCapability: 'enabled',
    expected: 'preparing',
  },
  {
    name: 'unavailable with no source or active processing',
    recording: {},
    liveCapability: 'enabled',
    expected: 'unavailable',
  },
]

describe('recordingVerdict', () => {
  it.each(verdictCases)('returns $expected for $name', ({ recording, liveCapability, expected }) => {
      expect(
        recordingVerdict({
          recording: { ...base, ...recording },
          liveCapability,
          nowMs,
        }),
      ).toBe(expected)
  })

  it('原本の転送が停滞していても準備中にする', () => {
    expect(
      recordingVerdict({
        recording: {
          ...base,
          ingest: {
            state: 'transferring',
            writtenBytes: 12,
            observedAt: '2026-01-01T12:00:00Z',
          },
        },
        liveCapability: 'enabled',
        nowMs,
      }),
    ).toBe('preparing')
  })

  it.each(['queued', 'running'] as const)('エンコード %s を準備中にする', (state) => {
    expect(
      recordingVerdict({
        recording: { ...base, encodeStatus: [{ profile: 'web', state }] },
        liveCapability: 'enabled',
        nowMs,
      }),
    ).toBe('preparing')
  })

  it.each(['pending', 'unknown'] as const)(
    '原本のみの録画は capability が %s の間、再生不可と断定しない',
    (liveCapability) => {
      expect(
        recordingVerdict({
          recording: { ...base, sizeBytes: 1_000 },
          liveCapability,
          nowMs,
        }),
      ).toBeUndefined()
    },
  )

  it.each(['queued', 'running'] as const)(
    '原本のみ + エンコード %s は capability 未確定なら結論を出さず、disabled なら準備中',
    (state) => {
      const recording = { ...base, sizeBytes: 1_000, encodeStatus: [{ profile: 'web', state }] }
      for (const liveCapability of ['pending', 'unknown'] as const) {
        expect(recordingVerdict({ recording, liveCapability, nowMs })).toBeUndefined()
      }
      expect(recordingVerdict({ recording, liveCapability: 'disabled', nowMs })).toBe('preparing')
    },
  )

  it('原本のみの録画はライブ capability が disabled なら再生不可', () => {
    expect(
      recordingVerdict({
        recording: { ...base, sizeBytes: 1_000 },
        liveCapability: 'disabled',
        nowMs,
      }),
    ).toBe('unavailable')
  })

  it('原本のみの録画はライブ capability が有効なら見られる', () => {
    expect(
      recordingVerdict({
        recording: { ...base, sizeBytes: 1_000 },
        liveCapability: 'enabled',
        nowMs,
      }),
    ).toBe('viewable')
  })

  it('ごみ箱の録画には結論を出さない', () => {
    expect(
      recordingVerdict({
        recording: { ...base, status: 'failed' },
        liveCapability: 'enabled',
        isTrashed: true,
        nowMs,
      }),
    ).toBeUndefined()
  })
})
