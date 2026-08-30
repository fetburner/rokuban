import { describe, expect, it } from 'vitest'

import { ApiError } from '@/api/client'
import { mutationErrorMessage } from '@/lib/mutation-error-message'

describe('mutationErrorMessage', () => {
  it('サーバー本文があれば末尾に「: 本文」を付ける', () => {
    const err = new ApiError(409, 'Conflict', { error: 'already exists' })
    expect(mutationErrorMessage('予約の取消に失敗しました', err)).toBe(
      '予約の取消に失敗しました: already exists',
    )
  })

  it('本文が無ければ generic のみ返し、末尾に「: 」を残さない', () => {
    const err = new Error('network error')
    expect(mutationErrorMessage('予約の取消に失敗しました', err)).toBe(
      '予約の取消に失敗しました',
    )
  })
})
