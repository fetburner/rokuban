import { describe, expect, it } from 'vitest'

import {
  selectRecordingPlaybackSource,
  transitionRecordingPlaybackSource,
  type RecordingPlaybackSource,
  type RecordingPlaybackSourceInput,
  type RecordingPlaybackTransitionTrigger,
} from '@/lib/recording-playback-source'

describe('selectRecordingPlaybackSource', () => {
  it.each([
    {
      name: 'recording with live capability selects chase',
      input: { status: 'recording', hasEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'chase',
    },
    {
      name: 'recording with live capability still selects chase when encoded media is present',
      input: { status: 'recording', hasEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'chase',
    },
    {
      name: 'recording without live capability has no browser source',
      input: { status: 'recording', hasEncoded: false, hasOriginal: true, liveEnabled: false },
      expected: 'none',
    },
    {
      name: 'finished recording prefers encoded even when original HLS is available',
      input: { status: 'finished', hasEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'encoded',
    },
    {
      name: 'finished recording with encoded does not require live capability',
      input: { status: 'finished', hasEncoded: true, hasOriginal: false, liveEnabled: false },
      expected: 'encoded',
    },
    {
      name: 'finished original without encoded selects original HLS when live is enabled',
      input: { status: 'finished', hasEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'original-vod',
    },
    {
      name: 'finished original without live capability has no browser source',
      input: { status: 'finished', hasEncoded: false, hasOriginal: true, liveEnabled: false },
      expected: 'none',
    },
    {
      name: 'finished recording without browser media has no source',
      input: { status: 'finished', hasEncoded: false, hasOriginal: false, liveEnabled: true },
      expected: 'none',
    },
    {
      name: 'failed or canceled recordings do not select a source',
      input: { status: 'failed', hasEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'none',
    },
    {
      name: 'canceled recording does not select a source',
      input: { status: 'canceled', hasEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'none',
    },
    {
      name: 'trashed recordings do not select a source',
      input: {
        status: 'finished',
        hasEncoded: true,
        hasOriginal: true,
        liveEnabled: true,
        isTrashed: true,
      },
      expected: 'none',
    },
    {
      name: 'trashed recording with live capability does not select chase',
      input: {
        status: 'recording',
        hasEncoded: false,
        hasOriginal: true,
        liveEnabled: true,
        isTrashed: true,
      },
      expected: 'none',
    },
  ] satisfies {
    name: string
    input: RecordingPlaybackSourceInput
    expected: RecordingPlaybackSource
  }[])(
    '$name',
    ({ input, expected }) => {
      expect(selectRecordingPlaybackSource(input)).toBe(expected)
    },
  )
})

describe('transitionRecordingPlaybackSource', () => {
  it('keeps the playing source and current position when only recording state updates', () => {
    const transition = transitionRecordingPlaybackSource({
      currentSource: 'chase',
      currentPositionSeconds: 1_234,
      trigger: 'recording-updated',
      recording: {
        status: 'finished',
        hasEncoded: true,
        hasOriginal: true,
        liveEnabled: true,
      },
    })

    expect(transition).toEqual({
      kind: 'keep-current',
      source: 'chase',
      positionSeconds: 1_234,
    })
  })

  it.each([
    'opened',
    'source-range-exit',
    'ended',
    'source-error',
    'reopened',
  ] satisfies RecordingPlaybackTransitionTrigger[])(
    '%s reselects from the latest recording state and carries the current source position',
    (trigger) => {
      const transition = transitionRecordingPlaybackSource({
        currentSource: 'chase',
        currentPositionSeconds: 1_234,
        trigger,
        recording: {
          status: 'finished',
          hasEncoded: true,
          hasOriginal: true,
          liveEnabled: true,
        },
      })

      expect(transition).toEqual({
        kind: 'reselect',
        source: 'encoded',
        positionSeconds: 1_234,
      })
    },
  )

  it('reselects on a range exit even when the selected source kind remains chase', () => {
    const transition = transitionRecordingPlaybackSource({
      currentSource: 'chase',
      currentPositionSeconds: 1_234,
      trigger: 'source-range-exit',
      recording: {
        status: 'recording',
        hasEncoded: false,
        hasOriginal: true,
        liveEnabled: true,
      },
    })

    expect(transition).toEqual({
      kind: 'reselect',
      source: 'chase',
      positionSeconds: 1_234,
    })
  })

  it('preserves an explicit zero position instead of treating it as absent', () => {
    const transition = transitionRecordingPlaybackSource({
      currentSource: 'chase',
      currentPositionSeconds: 0,
      trigger: 'source-error',
      recording: {
        status: 'finished',
        hasEncoded: true,
        hasOriginal: true,
        liveEnabled: true,
      },
    })

    expect(transition).toEqual({
      kind: 'reselect',
      source: 'encoded',
      positionSeconds: 0,
    })
  })
})
