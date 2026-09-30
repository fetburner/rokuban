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

  it('explains that an adoption-stage attempt needs a station decision', () => {
    expect(cmDetectStageMessage('adopt')).toBe(
      'この局はロゴの採用待ちです。局の画面で候補を確かめて採用してください。',
    )
  })
})
