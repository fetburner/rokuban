import { describe, expect, it } from 'vitest'

import { isRecordingHlsGenerationGap, selectRecordingPlaybackSource } from '@/lib/recording-playback-source'

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

describe('isRecordingHlsGenerationGap', () => {
  const empty = { hasHlsPlayback: false }

  it.each([
    ['録画中', { status: 'recording' as const }],
    ['完了後の ingest が pending', { status: 'finished' as const, ingestState: 'pending' as const }],
    ['完了後の ingest が transferring', { status: 'finished' as const, ingestState: 'transferring' as const }],
  ])('%s で hlsPlayback が空なら世代の作り直しとして待つ', (_label, input) => {
    expect(isRecordingHlsGenerationGap({ ...empty, ...input })).toBe(true)
  })

  it.each([
    ['失敗', { status: 'failed' as const }],
    ['キャンセル', { status: 'canceled' as const }],
    ['取り込み済み', { status: 'finished' as const, ingestState: 'committed' as const }],
    ['ingest の行なし', { status: 'finished' as const }],
    ['ごみ箱の録画中', { status: 'recording' as const, isTrashed: true }],
  ])('%s で空なら次の世代は来ないので待たない', (_label, input) => {
    expect(isRecordingHlsGenerationGap({ ...empty, ...input })).toBe(false)
  })

  it('hlsPlayback があれば空き状態ではない', () => {
    expect(isRecordingHlsGenerationGap({ status: 'recording', hasHlsPlayback: true })).toBe(false)
  })
})
