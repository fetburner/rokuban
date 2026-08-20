import { describe, expect, it } from 'vitest'

import { encodeJobStatusLabel } from './encode-status'

describe('encodeJobStatusLabel', () => {
  it('queued / running / failed をそれぞれ別の文言に落とす', () => {
    expect(encodeJobStatusLabel('queued')).toBe('エンコード待ち')
    expect(encodeJobStatusLabel('running')).toBe('エンコード中')
    expect(encodeJobStatusLabel('failed')).toBe('エンコード失敗')
  })

  it('3 つの文言はすべて異なる（取り違えを検出できる形にする）', () => {
    const labels = new Set([
      encodeJobStatusLabel('queued'),
      encodeJobStatusLabel('running'),
      encodeJobStatusLabel('failed'),
    ])
    expect(labels.size).toBe(3)
  })
})
