import { describe, expect, it } from 'vitest'

import { parsePositiveIntId } from '@/lib/positive-id'

describe('parsePositiveIntId', () => {
  it.each<[string, unknown]>([
    ['空文字列', ''],
    ['空白のみ', '   '],
    ['0', 0],
    ['0（文字列）', '0'],
    ['負値', -1],
    ['負値（文字列）', '-1'],
    ['安全整数の外', 1e30],
    ['安全整数の外（文字列）', '1e30'],
    // 数値リテラルとして書くと oxlint(no-loss-of-precision) に引っかかる
    // （リテラル自体が構文解析時に丸まる、というこのテストの主張と同じ理由）ので
    // `Number()` 経由で同じ丸め後の値を作る。
    ['MAX_SAFE_INTEGER を 1 超える値（黙って別の値に丸まる経路）', Number('9007199254740993')],
    ['同上（文字列）', '9007199254740993'],
    ['非整数', 1.5],
    ['非整数（文字列）', '1.5'],
    ['数値化できない文字列', 'not-a-number'],
    ['undefined', undefined],
    ['null', null],
    ['真偽値', true],
    ['オブジェクト', {}],
    ['配列', [1]],
  ])('%s は undefined に落ちる', (_label, raw) => {
    expect(parsePositiveIntId(raw)).toBeUndefined()
  })

  it.each<[string, unknown, number]>([
    ['整数', 1, 1],
    ['整数（文字列）', '1', 1],
    ['大きめの整数', 5168, 5168],
    ['指数表記', 1e3, 1000],
    ['指数表記（文字列）', '1e3', 1000],
    ['MAX_SAFE_INTEGER そのもの', Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER],
  ])('%s は通す', (_label, raw, expected) => {
    expect(parsePositiveIntId(raw)).toBe(expected)
  })
})
