import { describe, expect, it } from 'vitest'

import {
  selectRecordingPlaybackSource,
  type RecordingPlaybackSource,
  type RecordingPlaybackSourceInput,
} from '@/lib/recording-playback-source'

describe('selectRecordingPlaybackSource', () => {
  it.each([
    {
      name: 'recording with live capability selects chase',
      input: { status: 'recording', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'chase',
    },
    {
      name: 'recording with live capability still selects chase when encoded media is present',
      input: { status: 'recording', hasEncoded: true, hasNonCutEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'chase',
    },
    {
      name: 'recording without live capability has no browser source',
      input: { status: 'recording', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: false },
      expected: 'none',
    },
    {
      name: 'finished recording with pending ingest selects chase',
      input: { status: 'finished', ingestState: 'pending', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: false, liveEnabled: true },
      expected: 'chase',
    },
    {
      name: 'finished recording with transferring ingest selects chase even when progress is stale',
      input: { status: 'finished', ingestState: 'transferring', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: false, liveEnabled: true },
      expected: 'chase',
    },
    {
      name: 'finished recording with pending ingest needs live capability',
      input: { status: 'finished', ingestState: 'pending', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: false, liveEnabled: false },
      expected: 'none',
    },
    {
      name: 'finished recording prefers encoded even when original HLS is available',
      input: { status: 'finished', hasEncoded: true, hasNonCutEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'encoded',
    },
    {
      name: 'finished recording with encoded does not require live capability',
      input: { status: 'finished', hasEncoded: true, hasNonCutEncoded: true, hasOriginal: false, liveEnabled: false },
      expected: 'encoded',
    },
    {
      name: 'finished original without encoded selects original HLS when live is enabled',
      input: { status: 'finished', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'original-vod',
    },
    {
      name: 'finished recording with committed ingest follows the normal original HLS selection',
      input: { status: 'finished', ingestState: 'committed', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'original-vod',
    },
    {
      name: 'finished recording with unknown ingest follows the normal original HLS selection',
      input: { status: 'finished', ingestState: 'unknown', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'original-vod',
    },
    {
      name: 'finished cut-only recording prefers original HLS for review when live is enabled',
      input: { status: 'finished', hasEncoded: true, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: true },
      expected: 'original-vod',
    },
    {
      name: 'finished cut-only recording falls back to its cut version after original deletion',
      input: { status: 'finished', hasEncoded: true, hasNonCutEncoded: false, hasOriginal: false, liveEnabled: true },
      expected: 'encoded',
    },
    {
      name: 'finished original without live capability has no browser source',
      input: { status: 'finished', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: true, liveEnabled: false },
      expected: 'none',
    },
    {
      name: 'finished recording without browser media has no source',
      input: { status: 'finished', hasEncoded: false, hasNonCutEncoded: false, hasOriginal: false, liveEnabled: true },
      expected: 'none',
    },
    {
      name: 'failed or canceled recordings do not select a source',
      input: { status: 'failed', hasEncoded: true, hasNonCutEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'none',
    },
    {
      name: 'canceled recording does not select a source',
      input: { status: 'canceled', hasEncoded: true, hasNonCutEncoded: true, hasOriginal: true, liveEnabled: true },
      expected: 'none',
    },
    {
      name: 'trashed recordings do not select a source',
      input: {
        status: 'finished',
        hasEncoded: true,
        hasNonCutEncoded: true,
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
        hasNonCutEncoded: false,
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
