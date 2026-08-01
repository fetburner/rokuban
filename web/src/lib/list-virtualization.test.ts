import { describe, expect, it } from 'vitest'

import { isDomLayoutMeasurable, probeDomLayout } from '@/lib/list-virtualization'

describe('isDomLayoutMeasurable', () => {
  it('計測できている（高さが正）なら true を返す（間引く側に倒れる）', () => {
    expect(isDomLayoutMeasurable(1)).toBe(true)
    expect(isDomLayoutMeasurable(56)).toBe(true)
  })

  it('未計測（高さ 0 以下）なら false を返す（全部描く側に倒れる）', () => {
    expect(isDomLayoutMeasurable(0)).toBe(false)
    expect(isDomLayoutMeasurable(-1)).toBe(false)
  })
})

describe('probeDomLayout', () => {
  it('jsdom はレイアウトエンジンを持たないため、既知の高さを与えても 0 を読み返す', () => {
    // このテスト自体が「計測できない環境」であることの根拠になっている。
    // 実ブラウザではここが正の値になり、isDomLayoutMeasurable が true 側に倒れる。
    expect(probeDomLayout()).toBe(0)
  })

  it('一時的に差し込んだ要素を残さない', () => {
    probeDomLayout()
    expect(document.body.querySelectorAll('div').length).toBe(0)
  })
})
