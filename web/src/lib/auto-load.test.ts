import { describe, expect, it } from 'vitest'

import { shouldAutoLoadNextPage, shouldShowLoadMoreButton } from '@/lib/auto-load'

const base = {
  isIntersecting: true,
  autoLoadAvailable: true,
  autoLoadFailed: false,
  hasNextPage: true,
  isFetchingNextPage: false,
}

describe('shouldAutoLoadNextPage', () => {
  it('可視・自動可・未失敗・次ページあり・未取得中なら読む', () => {
    expect(shouldAutoLoadNextPage(base)).toBe(true)
  })

  it('可視でなければ読まない', () => {
    expect(shouldAutoLoadNextPage({ ...base, isIntersecting: false })).toBe(false)
  })

  it('計測できない環境（autoLoadAvailable=false）では読まない', () => {
    expect(shouldAutoLoadNextPage({ ...base, autoLoadAvailable: false })).toBe(false)
  })

  it('直近の自動読み込みが失敗していれば読まない（無限リトライを防ぐ）', () => {
    expect(shouldAutoLoadNextPage({ ...base, autoLoadFailed: true })).toBe(false)
  })

  it('次ページが無ければ読まない', () => {
    expect(shouldAutoLoadNextPage({ ...base, hasNextPage: false })).toBe(false)
  })

  it('取得中に重ねて読まない', () => {
    expect(shouldAutoLoadNextPage({ ...base, isFetchingNextPage: true })).toBe(false)
  })
})

describe('shouldShowLoadMoreButton', () => {
  it('自動が効いている通常時（自動可・未失敗）は出さない', () => {
    expect(
      shouldShowLoadMoreButton({ hasNextPage: true, autoLoadAvailable: true, autoLoadFailed: false }),
    ).toBe(false)
  })

  it('次ページが無ければ出さない', () => {
    expect(
      shouldShowLoadMoreButton({ hasNextPage: false, autoLoadAvailable: true, autoLoadFailed: false }),
    ).toBe(false)
  })

  it('計測できない環境では次ページがある限り出す', () => {
    expect(
      shouldShowLoadMoreButton({ hasNextPage: true, autoLoadAvailable: false, autoLoadFailed: false }),
    ).toBe(true)
  })

  it('自動読み込みが失敗した後は出す', () => {
    expect(
      shouldShowLoadMoreButton({ hasNextPage: true, autoLoadAvailable: true, autoLoadFailed: true }),
    ).toBe(true)
  })
})
