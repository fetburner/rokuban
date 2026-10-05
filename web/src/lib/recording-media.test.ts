import { describe, expect, it } from 'vitest'

import { recordingThumbnailURL } from '@/lib/recording-media'

describe('recordingThumbnailURL', () => {
  it('録画 id をサムネイル配信 URL に埋め込む', () => {
    expect(recordingThumbnailURL(42)).toBe(
      '/api/media/recordings/42/thumbnail',
    )
  })
})
