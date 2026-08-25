import { describe, expect, it } from 'vitest'

import { encodeJobStatusLabel } from './encode-status'

describe('encodeJobStatusLabel', () => {
  it('queued / running / failed をそれぞれ別の文言に落とす', () => {
    expect(encodeJobStatusLabel('queued')).toBe('エンコード待ち')
    expect(encodeJobStatusLabel('running')).toBe('エンコード中')
    expect(encodeJobStatusLabel('failed')).toBe('エンコード失敗')
  })

  it('running だけは受信済みの揮発進捗を百分率で出す', () => {
    expect(encodeJobStatusLabel('running', 0.429)).toBe('エンコード中 42%')
    expect(encodeJobStatusLabel('running')).toBe('エンコード中')
    expect(encodeJobStatusLabel('failed', 0.429)).toBe('エンコード失敗')
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
