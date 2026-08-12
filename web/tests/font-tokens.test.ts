import { readdirSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * 和文フォント基盤（Noto Sans JP）が index.css / src に正しく配線されているかを見るテスト。
 *
 * **フォントが実際に描画に使われるかは jsdom では測れない**（design-tokens.test.ts
 * と同じ理由 --- Tailwind のクラスは解決されず、フォントファイルも取得されない）。
 * 実描画の判定は `web/e2e/design.mjs`「③ フォントの判定」にある（CDP
 * `CSS.getPlatformFontsForNode` で番組リストの行の実使用フォントに Noto Sans JP
 * と Geist が両方出ることと、和文まじりの文字列で `font-variant-numeric:
 * tabular-nums` が実際に等幅を作ることを DOM の実測幅で見る）。ここで見るのは
 * 文字列として確かめられることだけ:
 *
 *   1. Noto Sans JP を import しているか（自前配布。CDN 参照ではない）
 *   2. `--font-sans` が Geist → Noto Sans JP → 和文システムフォントの順で並んでいるか
 *      （Geist は日本語グリフを持たないので、和文はフォールバックで Noto Sans JP に
 *      渡る。順序を間違えると和文だけでなく英数字まで Noto に落ちる）
 *   3. `tabular-nums` を `html` に一度だけ当てているか（全域適用）
 *   4. コンポーネント側に `tabular-nums` の個別指定が残っていないか
 *      （残っていると「全域適用に統合した」という決定と矛盾する）
 */

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const indexCss = readFileSync(path.join(webDir, 'src/index.css'), 'utf8')

/** fontSansValue は `--font-sans:` の値（次のセミコロンまで）を取り出す。 */
function fontSansValue(): string {
  const match = /--font-sans:\s*([^;]+);/.exec(indexCss)
  if (match === null) throw new Error('--font-sans が index.css に無い')
  return match[1]
}

/** htmlBaseBlock は `@layer base` 内の `html { ... }` ブロックの中身を返す。 */
function htmlBaseBlock(): string {
  const match = /html\s*\{([^}]*)\}/.exec(indexCss)
  if (match === null) throw new Error('html {} ブロックが index.css に無い')
  return match[1]
}

/** walk は dir 以下の .ts / .tsx を再帰的に集める（scripts/check-colors.mjs と同じ形）。 */
function walk(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else if (/\.tsx?$/.test(entry)) out.push(full)
  }
  return out
}

describe('Noto Sans JP の導入', () => {
  it('自前配布（fontsource）を import している', () => {
    // unicode-range 分割のある @fontsource-variable パッケージを使う。CDN 参照
    // （Google Fonts の <link> 等）は自前配布の要件（issue #225）に反するので、
    // node_modules 由来の import であることまで見る
    expect(indexCss).toMatch(/@import\s+["']@fontsource-variable\/noto-sans-jp["'];/)
  })

  it('--font-sans が Geist Variable → Noto Sans JP Variable の順で並ぶ', () => {
    const value = fontSansValue()
    const geistIndex = value.indexOf('Geist Variable')
    const notoIndex = value.indexOf('Noto Sans JP Variable')
    expect(geistIndex).toBeGreaterThanOrEqual(0)
    expect(notoIndex).toBeGreaterThanOrEqual(0)
    // 逆順にすると、フォールバックの解決順が変わって英数字まで Noto Sans JP に
    // 渡ってしまう（font-family は先頭から「そのグリフを持つ最初のフォント」を選ぶ）
    expect(geistIndex).toBeLessThan(notoIndex)
  })

  it('--font-sans に和文システムフォントのフォールバックがある', () => {
    // ダウンロード前後（font-display: swap）とダウンロード失敗時の保険。
    // ヒラギノ・游ゴシック・メイリオのいずれかがあればよい
    expect(fontSansValue()).toMatch(/Hiragino|Yu Gothic|Meiryo/)
  })

  it('--font-sans の末尾は総称フォントファミリーで終わる', () => {
    expect(fontSansValue().trim()).toMatch(/sans-serif$/)
  })
})

describe('tabular-nums の全域適用', () => {
  it('html に font-variant-numeric: tabular-nums が当たっている（@apply 経由）', () => {
    // Tailwind の `tabular-nums` ユーティリティは font-variant-numeric: tabular-nums
    // を生成する。@apply の対象として html ブロックに乗っているかを見る
    expect(htmlBaseBlock()).toMatch(/@apply[^;]*\btabular-nums\b/)
  })

  it('コンポーネント側に tabular-nums の個別指定が残っていない', () => {
    const offenders: string[] = []
    for (const file of walk(path.join(webDir, 'src'))) {
      const content = readFileSync(file, 'utf8')
      if (content.includes('tabular-nums')) offenders.push(path.relative(webDir, file))
    }
    // 全域適用に統合した後は、個別指定が生き残っていると「html 1 箇所で足りる」
    // という決定と矛盾する。復活したらここで拾う
    expect(offenders).toEqual([])
  })
})
