import { describe, expect, it } from 'vitest'

import { selectRecordingPlaybackSource } from '@/lib/recording-playback-source'

describe('selectRecordingPlaybackSource', () => {
  it('uses recording HLS while a recording is in progress', () => {
    expect(selectRecordingPlaybackSource({
      status: 'recording', hasEncoded: false, hasNonCutEncoded: false, hasHlsPlayback: true,
    })).toBe('recording-hls')
  })

  it('falls back to an encoded file when recording HLS is not advertised', () => {
    expect(selectRecordingPlaybackSource({
      status: 'recording', hasEncoded: true, hasNonCutEncoded: true, hasHlsPlayback: false,
    })).toBe('encoded')
    expect(selectRecordingPlaybackSource({
      status: 'finished', hasEncoded: true, hasNonCutEncoded: true, hasHlsPlayback: false,
    })).toBe('encoded')
  })

  it('keeps a completed recording on its normal encoded version', () => {
    expect(selectRecordingPlaybackSource({
      status: 'finished', hasEncoded: true, hasNonCutEncoded: true, hasHlsPlayback: true,
    })).toBe('encoded')
  })

  it('uses recording HLS while a finished recording is still ingesting', () => {
    expect(selectRecordingPlaybackSource({
      status: 'finished', ingestState: 'transferring', hasEncoded: true,
      hasNonCutEncoded: true, hasHlsPlayback: true,
    })).toBe('recording-hls')
  })

  it('falls back to cut encoded media when HLS is unavailable', () => {
    expect(selectRecordingPlaybackSource({
      status: 'finished', hasEncoded: true, hasNonCutEncoded: false, hasHlsPlayback: false,
    })).toBe('encoded')
  })

  it('returns no source when neither HLS nor an encoded file is available', () => {
    expect(selectRecordingPlaybackSource({
      status: 'recording', hasEncoded: false, hasNonCutEncoded: false, hasHlsPlayback: false,
    })).toBe('none')
    expect(selectRecordingPlaybackSource({
      status: 'finished', hasEncoded: false, hasNonCutEncoded: false, hasHlsPlayback: false,
    })).toBe('none')
  })

  it.each(['failed', 'canceled'] as const)('does not play a %s recording', (status) => {
    expect(selectRecordingPlaybackSource({
      status, hasEncoded: true, hasNonCutEncoded: true, hasHlsPlayback: true,
    })).toBe('none')
  })

  it('does not play a trashed recording', () => {
    expect(selectRecordingPlaybackSource({
      status: 'finished', hasEncoded: true, hasNonCutEncoded: true,
      hasHlsPlayback: true, isTrashed: true,
    })).toBe('none')
  })
})
