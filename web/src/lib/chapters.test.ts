import { afterEach, describe, expect, it } from 'vitest'

import type { ChapterSpan } from '@/api/generated'
import {
  CHAPTER_SKIP_STORAGE_KEY,
  FRAME_SECONDS,
  autoSkipSeekSeconds,
  chapterBoundaryMsToSeekSeconds,
  chapterBoundaries,
  chapterJumpTarget,
  chapterSpanIndexesAtBoundary,
  displayedFrameBoundaryMs,
  formatChaptersTime,
  loadChapterSkip,
  nudgeBoundary,
  normalizeChapterDraft,
  playbackSecondsToChapterBoundaryMs,
  saveChapterSkip,
  skipTarget,
} from '@/lib/chapters'

const cm = (startMs: number, endMs: number): ChapterSpan => ({ startMs, endMs, label: 'CM', cut: true })
const op = (startMs: number, endMs: number): ChapterSpan => ({ startMs, endMs, label: 'OP', cut: false })

afterEach(() => {
  localStorage.clear()
})

describe('skipTarget', () => {
  const spans = [cm(10_000, 20_000)]

  it('通常の再生で区間の先頭に差し掛かったら終端へ飛ぶ', () => {
    // 直前は 9.8 秒、今回 10.1 秒 = 区間の先頭を跨いだ。
    expect(skipTarget(spans, 9.8, 10.1, 300)).toBe(20)
  })

  it('手動のシークで区間の中に入ったときは飛ばさない', () => {
    // seeked の後は直前位置も区間の中になっている。
    expect(skipTarget(spans, 15, 15.1, 300)).toBeUndefined()
  })

  it('区間より後ろを再生し続けても飛ばさない', () => {
    expect(skipTarget(spans, 20, 20.1, 300)).toBeUndefined()
  })

  it('cut でない区間（OP / ED）は飛ばさない', () => {
    // ラベルだけの区間は目盛りに出すが、再生は止めない（切らずに印だけ付ける）。
    expect(skipTarget([op(10_000, 20_000)], 9.8, 10.1, 300)).toBeUndefined()
  })

  it('最後の区間が動画の終端を越えるときは終端で止める', () => {
    expect(skipTarget([cm(10_000, 99_000_000)], 9.8, 10.1, 300)).toBe(300)
  })

  it('duration が未確定でも終端へ飛ぶ', () => {
    expect(skipTarget(spans, 9.8, 10.1, Number.NaN)).toBe(20)
  })
})

describe('chapterJumpTarget', () => {
  const spans = [cm(10_000, 20_000), op(30_000, 40_000)]

  it('次の境界へ進む', () => {
    expect(chapterJumpTarget(spans, 0, 'next')).toBe(10)
    expect(chapterJumpTarget(spans, 15, 'next')).toBe(20)
    expect(chapterJumpTarget(spans, 20, 'next')).toBe(30)
  })

  it('前の境界へ戻る', () => {
    expect(chapterJumpTarget(spans, 35, 'prev')).toBe(30)
    expect(chapterJumpTarget(spans, 25, 'prev')).toBe(20)
  })

  it('境界の上に居るときは同じ場所を返さない', () => {
    // 0.5 秒の遊びを置く（境界の上で押し続けても動かない、を避ける）。
    expect(chapterJumpTarget(spans, 20.2, 'next')).toBe(30)
    expect(chapterJumpTarget(spans, 19.8, 'prev')).toBe(10)
  })

  it('端では undefined を返す', () => {
    expect(chapterJumpTarget(spans, 100, 'next')).toBeUndefined()
    expect(chapterJumpTarget(spans, 0, 'prev')).toBeUndefined()
  })
})

describe('nudgeBoundary', () => {
  it('一致する境界をすべて同じ量だけ動かす', () => {
    // 20 秒は前の区間の終端かつ次の区間の開始（境界を共有している）。
    const spans = [cm(10_000, 20_000), cm(20_000, 30_000)]
    const moved = nudgeBoundary(spans, 20, 1)
    expect(moved[0].endMs).toBe(21_000)
    expect(moved[1].startMs).toBe(21_000)
    // 触っていない境界はそのまま。
    expect(moved[0].startMs).toBe(10_000)
    expect(moved[1].endMs).toBe(30_000)
  })

  it('1 フレームぶんの刻みは 1001/30000 秒', () => {
    const spans = [cm(10_000, 20_000)]
    // 10000ms + 33.3667ms = 10033.37ms → round で 10033。
    expect(nudgeBoundary(spans, 10, FRAME_SECONDS)[0].startMs).toBe(10_033)
  })
})

describe('normalizeChapterDraft', () => {
  it('重なった cut 同士を同じラベルの区間へまとめる', () => {
    expect(normalizeChapterDraft([
      cm(10_000, 25_000),
      cm(20_000, 30_000),
    ], [1])).toEqual([cm(10_000, 30_000)])
  })

  it('cut 同士は同じラベルなら接してもまとめ、ラベルが違えば境界を残す', () => {
    expect(normalizeChapterDraft([cm(10_000, 20_000), cm(20_000, 30_000)])).toEqual([
      cm(10_000, 30_000),
    ])
    expect(normalizeChapterDraft([
      cm(10_000, 20_000),
      { ...cm(20_000, 30_000), label: '提供' },
    ])).toEqual([cm(10_000, 20_000), { ...cm(20_000, 30_000), label: '提供' }])
  })

  it('cut が非 cut 区間の内側にあると、非 cut を両側に分けてラベルを残す', () => {
    const opening: ChapterSpan = { startMs: 0, endMs: 30_000, label: 'OP', cut: false }
    expect(normalizeChapterDraft([opening, cm(10_000, 20_000)], [1])).toEqual([
      { ...opening, endMs: 10_000 },
      cm(10_000, 20_000),
      { ...opening, startMs: 20_000 },
    ])
  })

  it('cut が非 cut 区間を覆うと非 cut 区間を取り除く', () => {
    const opening: ChapterSpan = { startMs: 10_000, endMs: 20_000, label: 'OP', cut: false }
    expect(normalizeChapterDraft([opening, cm(0, 30_000)], [1])).toEqual([cm(0, 30_000)])
  })

  it('ラベルが違う cut 同士は操作した区間を残して、相手の重なりを削る', () => {
    expect(normalizeChapterDraft([
      cm(0, 15_000),
      { ...cm(10_000, 20_000), label: '提供' },
    ], [1])).toEqual([
      cm(0, 10_000),
      { ...cm(10_000, 20_000), label: '提供' },
    ])
  })

  it('共有境界を操作した区間の index を両側から返す', () => {
    expect(chapterSpanIndexesAtBoundary([cm(10_000, 20_000), cm(20_000, 30_000)], 20)).toEqual([0, 1])
  })

  it('切り取りで残る 1 フレーム未満の断片は捨て、1 フレーム以上は残す', () => {
    const opening: ChapterSpan = { startMs: 0, endMs: 10_005, label: 'OP', cut: false }
    expect(normalizeChapterDraft([opening, cm(5, 10_000)], [1])).toEqual([cm(5, 10_000)])
    expect(normalizeChapterDraft([{ ...opening, endMs: 10_034 }, cm(0, 10_000)], [1])).toEqual([
      cm(0, 10_000),
      { ...opening, startMs: 10_000, endMs: 10_034 },
    ])
  })

  it('利用者が元から持つ 1 フレーム未満の区間は落とさない', () => {
    expect(normalizeChapterDraft([cm(100, 105)])).toEqual([cm(100, 105)])
  })
})

describe('chapterBoundaries', () => {
  it('昇順・重複なしで返す', () => {
    expect(chapterBoundaries([cm(10_000, 20_000), cm(20_000, 30_000)])).toEqual([10, 20, 30])
  })
})

describe('チャプター境界と再生位置の変換', () => {
  it('ms 境界を対応するフレームの中央へ写す（ms が切り下がる場合と切り上がる場合）', () => {
    // frame 66 は 2202.2ms → 2202ms、frame 67 は 2235.566…ms → 2236ms。
    expect(chapterBoundaryMsToSeekSeconds(2202)).toBeCloseTo(66.5 * FRAME_SECONDS, 12)
    expect(chapterBoundaryMsToSeekSeconds(2236)).toBeCloseTo(67.5 * FRAME_SECONDS, 12)
  })

  it('表示区間の後半でも表示中フレームの境界へ戻す', () => {
    expect(playbackSecondsToChapterBoundaryMs(66.75 * FRAME_SECONDS)).toBe(2202)
    expect(playbackSecondsToChapterBoundaryMs(67.75 * FRAME_SECONDS)).toBe(2236)
  })

  it('境界ちょうどの浮動小数丸めで前のフレームに戻らない', () => {
    const justBefore = 66 * FRAME_SECONDS - 4 * Number.EPSILON
    expect(justBefore / FRAME_SECONDS).toBeLessThan(66)
    expect(playbackSecondsToChapterBoundaryMs(justBefore)).toBe(2202)
  })
})

describe('表示フレームの mediaTime からの境界', () => {
  it('mediaTime のフレーム番号を round で求める（切り下がる k=66 と切り上がる k=67）', () => {
    expect(displayedFrameBoundaryMs(66 * FRAME_SECONDS, 0)).toBe(2202)
    expect(displayedFrameBoundaryMs(67 * FRAME_SECONDS, 0)).toBe(2236)
  })

  it('mediaTime が取れていれば currentTime が前のフレームを指していても mediaTime を使う', () => {
    // 一時停止直後の currentTime は表示フレームの開始より手前（-0.2 フレーム）にある。
    const current = (67 - 0.2) * FRAME_SECONDS
    expect(playbackSecondsToChapterBoundaryMs(current)).toBe(2202)
    expect(displayedFrameBoundaryMs(67 * FRAME_SECONDS, current)).toBe(2236)
  })

  it('mediaTime が無ければ currentTime の floor に fallback する', () => {
    expect(displayedFrameBoundaryMs(null, 66.75 * FRAME_SECONDS)).toBe(2202)
    expect(displayedFrameBoundaryMs(Number.NaN, 67.75 * FRAME_SECONDS)).toBe(2236)
  })

  it('境界 → シーク秒 → 境界が往復で一致する', () => {
    for (const ms of [0, 2202, 2236, 67_367, 1_199_999]) {
      const seek = chapterBoundaryMsToSeekSeconds(ms)
      expect(displayedFrameBoundaryMs(null, seek)).toBe(ms)
    }
  })
})

describe('autoSkipSeekSeconds', () => {
  it('境界秒をフレーム中央へ写す', () => {
    expect(autoSkipSeekSeconds(2.236, 120)).toBeCloseTo(67.5 * FRAME_SECONDS, 12)
  })

  it('動画の長さで頭打ちにする。長さが未確定なら頭打ちしない', () => {
    expect(autoSkipSeekSeconds(20, 20)).toBe(20)
    expect(autoSkipSeekSeconds(20, Number.NaN)).toBeCloseTo(599.5 * FRAME_SECONDS, 12)
  })
})

describe('chapter skip の設定', () => {
  it('既定は有効', () => {
    expect(loadChapterSkip()).toBe(true)
  })

  it('OFF を保存すると OFF で読める', () => {
    saveChapterSkip(false)
    expect(localStorage.getItem(CHAPTER_SKIP_STORAGE_KEY)).toBe('off')
    expect(loadChapterSkip()).toBe(false)
  })
})

describe('formatChaptersTime', () => {
  it('h:mm:ss で返す', () => {
    expect(formatChaptersTime(0)).toBe('0:00:00')
    expect(formatChaptersTime(3725)).toBe('1:02:05')
  })

  it('不正な値は 0:00:00 に落とす', () => {
    expect(formatChaptersTime(Number.NaN)).toBe('0:00:00')
    expect(formatChaptersTime(-1)).toBe('0:00:00')
  })
})
