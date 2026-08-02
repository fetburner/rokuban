import { describe, expect, it } from 'vitest'

import {
  defaultEncodeSettingsValue,
  encodeSettingsError,
  encodeSettingsOverridesBody,
  encodeSettingsValueFromOverrides,
  hasEncodeOverride,
  keepOriginalLabel,
  sameEncodeSettingsValue,
  toggleProfile,
} from './encode-settings'

describe('encodeSettingsError', () => {
  it('until_encoded でプロファイル空ならエラー', () => {
    expect(encodeSettingsError('until_encoded', [])).toMatch(/プロファイル/)
  })

  it('until_encoded でプロファイルありなら通る', () => {
    expect(encodeSettingsError('until_encoded', ['h264'])).toBeUndefined()
  })

  it('always ならプロファイル空でも通る', () => {
    expect(encodeSettingsError('always', [])).toBeUndefined()
  })
})

describe('toggleProfile', () => {
  it('未選択なら末尾に足す', () => {
    expect(toggleProfile(['h264'], 'hevc')).toEqual(['h264', 'hevc'])
  })

  it('選択済みなら外す', () => {
    expect(toggleProfile(['h264', 'hevc'], 'h264')).toEqual(['hevc'])
  })
})

describe('keepOriginalLabel', () => {
  it('ラベルを返す', () => {
    expect(keepOriginalLabel('always')).toBe('常に保持')
    expect(keepOriginalLabel('until_encoded')).toBe('エンコード後に削除')
  })
})

describe('encodeSettingsValueFromOverrides（issue #132: Reservation 型に依存しない切り出し）', () => {
  it('overrides が undefined / null なら既定値（always / 空配列）', () => {
    expect(encodeSettingsValueFromOverrides(undefined)).toEqual(defaultEncodeSettingsValue())
    expect(encodeSettingsValueFromOverrides(null)).toEqual(defaultEncodeSettingsValue())
  })

  it('overrides に値があればそれを反映する', () => {
    expect(
      encodeSettingsValueFromOverrides({ keepOriginal: 'until_encoded', encodeProfiles: ['h264'] }),
    ).toEqual({ keepOriginal: 'until_encoded', encodeProfiles: ['h264'] })
  })

  it('片方だけ設定されていれば、もう片方は既定値で補う', () => {
    expect(encodeSettingsValueFromOverrides({ keepOriginal: 'until_encoded' })).toEqual({
      keepOriginal: 'until_encoded',
      encodeProfiles: [],
    })
  })

  it('未知の形（不正な keepOriginal・配列でない encodeProfiles）は既定値に落とす', () => {
    expect(
      encodeSettingsValueFromOverrides({ keepOriginal: 'bogus', encodeProfiles: 'h264' }),
    ).toEqual(defaultEncodeSettingsValue())
  })
})

describe('hasEncodeOverride（Reservation 型ではなく overrides の値そのものを引数に取る）', () => {
  it('undefined / null は override なし', () => {
    expect(hasEncodeOverride(undefined)).toBe(false)
    expect(hasEncodeOverride(null)).toBe(false)
  })

  it('keepOriginal / encodeProfiles のどちらかが有れば override あり', () => {
    expect(hasEncodeOverride({ keepOriginal: 'always' })).toBe(true)
    expect(hasEncodeOverride({ encodeProfiles: [] })).toBe(true)
  })

  it('無関係なキーしか無ければ override なし', () => {
    expect(hasEncodeOverride({ priority: 1 })).toBe(false)
  })
})

describe('sameEncodeSettingsValue', () => {
  it('keepOriginal が違えば別扱い', () => {
    expect(
      sameEncodeSettingsValue(
        { keepOriginal: 'always', encodeProfiles: [] },
        { keepOriginal: 'until_encoded', encodeProfiles: [] },
      ),
    ).toBe(false)
  })

  it('encodeProfiles の順序違いは同じ扱い（集合として比較する）', () => {
    expect(
      sameEncodeSettingsValue(
        { keepOriginal: 'always', encodeProfiles: ['h264', 'hevc'] },
        { keepOriginal: 'always', encodeProfiles: ['hevc', 'h264'] },
      ),
    ).toBe(true)
  })

  it('件数が違えば別扱い', () => {
    expect(
      sameEncodeSettingsValue(
        { keepOriginal: 'always', encodeProfiles: ['h264'] },
        { keepOriginal: 'always', encodeProfiles: ['h264', 'hevc'] },
      ),
    ).toBe(false)
  })
})

describe('encodeSettingsOverridesBody（不変条件 10: 既定のままなら PATCH ボディを作らない）', () => {
  it('baseline と同じなら undefined（PATCH を呼ばせない）', () => {
    expect(
      encodeSettingsOverridesBody(defaultEncodeSettingsValue(), defaultEncodeSettingsValue()),
    ).toBeUndefined()
  })

  it('baseline と違えば PATCH ボディを返す', () => {
    expect(
      encodeSettingsOverridesBody(
        { keepOriginal: 'always', encodeProfiles: ['h264'] },
        defaultEncodeSettingsValue(),
      ),
    ).toEqual({ keepOriginal: 'always', encodeProfiles: ['h264'] })
  })
})
