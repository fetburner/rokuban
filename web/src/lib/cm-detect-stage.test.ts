import { describe, expect, it } from 'vitest'

import { cmDetectStageMessage } from '@/lib/cm-detect-stage'

describe('cmDetectStageMessage', () => {
  it('explains resolution and match failures', () => {
    expect(cmDetectStageMessage('resolution')).toBe(
      '覚えたロゴは別の解像度の録画から作られたため、この録画には使えませんでした。',
    )
    expect(cmDetectStageMessage('match')).toBe(
      '覚えたロゴがこの録画にほとんど映っておらず、CM を検出できませんでした。局のロゴが変わった可能性があります。',
    )
  })
})
