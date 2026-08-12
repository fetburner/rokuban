import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * デザイントークンの土台が崩れていないことを見るテスト。
 *
 * **色そのものの合否は jsdom では出せない**（レイアウトと同じで、jsdom は
 * Tailwind のクラスを解決しないし oklch も計算しない）。実際の画素での判定は
 * `web/e2e/design.mjs` にある。ここで見るのは、実ブラウザを起動しなくても
 * 文字列として確かめられることだけ:
 *
 *   1. 地の 3 値がファビコンの 3 値と一致しているか（別々のファイルにある同じ
 *      決定なので、片方だけ動くと黙って食い違う）
 *   2. 意味を持つトークン（地と字）がその 3 値を参照しているか --- 3 値を
 *      定義しただけで誰も参照していなければ何も変わらない
 *   3. 信号色がライト・ダークの両方に定義され、色相を取り違えていないか
 *      （片側を忘れる事故がいちばん起きやすい）
 *
 * `src/` ではなくここに置くのは、対象がファイルの中身そのものだから ---
 * `src/index.css?raw` は vitest が CSS を丸ごとスタブする（既定の `css: false`）
 * ので空文字列になり、**全部の判定が素通りする**。`node:fs` で読むには Node の
 * 型が要り、それを `tsconfig.app.json`（アプリのコード）に足したくない。
 */

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const indexCss = readFileSync(path.join(webDir, 'src/index.css'), 'utf8')
const faviconScript = readFileSync(path.join(webDir, 'scripts/gen-favicon.mjs'), 'utf8')

/** oklchToHex は `oklch(L C H)` を sRGB の 16 進に落とす（Björn Ottosson の変換）。 */
function oklchToHex(L: number, C: number, hDeg: number): string {
  const h = (hDeg * Math.PI) / 180
  const a = C * Math.cos(h)
  const b = C * Math.sin(h)
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3
  const linear = [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s,
  ]
  return (
    '#' +
    linear
      .map((u) => {
        const encoded = u <= 0.0031308 ? 12.92 * u : 1.055 * Math.pow(u, 1 / 2.4) - 0.055
        const byte = Math.round(Math.min(1, Math.max(0, encoded)) * 255)
        return byte.toString(16).padStart(2, '0')
      })
      .join('')
  )
}

/**
 * block はセレクタのブロックの中身を返す（同じセレクタが複数あれば連結する。
 * `:root` は「3 値の定義」と「ライトのトークン」の 2 つに分かれている）。
 */
function block(selector: ':root' | '.dark'): string {
  const escaped = selector.replace('.', '\\.')
  const matches = [...indexCss.matchAll(new RegExp(`${escaped}\\s*\\{([^}]*)\\}`, 'g'))]
  if (matches.length === 0) throw new Error(`${selector} が index.css に無い`)
  return matches.map((m) => m[1]).join('\n')
}

/**
 * cssToken は指定したテーマの custom property の値を取り出す。
 *
 * **スコープを必ず指定する。** ファイル全体から最初のマッチを拾う実装にすると
 * `--warning` は常にライトの値を返し、ダーク側を青にしても検査が通ってしまう。
 */
function cssToken(name: string, scope: ':root' | '.dark' = ':root'): string {
  const match = new RegExp(`--${name}:\\s*([^;]+);`).exec(block(scope))
  if (match === null) throw new Error(`--${name} が ${scope} に無い`)
  return match[1].trim()
}

/** tokenHex は oklch で書かれたトークンを 16 進に落とす。 */
function tokenHex(name: string): string {
  const value = cssToken(name)
  const match = /oklch\(\s*([\d.]+)\s+([\d.]+)\s+([\d.]+)\s*\)/.exec(value)
  if (match === null) throw new Error(`--${name} が oklch(L C H) の形ではない: ${value}`)
  return oklchToHex(Number(match[1]), Number(match[2]), Number(match[3]))
}

/** faviconConst は gen-favicon.mjs の `const NAME = "#rrggbb";` を読む。 */
function faviconConst(name: string): string {
  const match = new RegExp(`const ${name} = "(#[0-9a-fA-F]{6})"`).exec(faviconScript)
  if (match === null) throw new Error(`${name} が gen-favicon.mjs に無い`)
  return match[1].toLowerCase()
}

/** channels は `#rrggbb` を [r, g, b] に開く。 */
function channels(hex: string): [number, number, number] {
  return [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16)) as [number, number, number]
}

describe('地の 3 値', () => {
  // ファビコンの輝線 / 間隙 / 字と、アプリの紙白 / 走査線グレー / 墨は同じ決定。
  // 別ファイルに 2 度書いてあるので、片方を動かしたらここで落ちる
  // （docs/frontend/design.md「地は「イ」の 3 値」）。
  it.each([
    ['paper', 'LIT'],
    ['scanline', 'GAP'],
    ['sumi', 'INK'],
  ])('--%s はファビコンの %s と同じ色', (token, constant) => {
    // 完全一致ではなく 1/255 の許容を置く。oklch は 16 進を往復しない
    // （`#71717a` → oklch へ丸めて戻すと `#71717b`）ので、厳密比較にすると
    // 「同じ色を指している」ことではなく「丸め方が同じ」ことを検査してしまう。
    const actual = channels(tokenHex(token))
    const expected = channels(faviconConst(constant))
    for (let i = 0; i < 3; i++) {
      expect(Math.abs(actual[i] - expected[i])).toBeLessThanOrEqual(1)
    }
  })

  it('地の 3 値はほぼ無彩（chroma <= 0.02）', () => {
    for (const token of ['paper', 'scanline', 'sumi']) {
      const chroma = Number(/oklch\(\s*[\d.]+\s+([\d.]+)/.exec(cssToken(token))![1])
      expect(chroma).toBeLessThanOrEqual(0.02)
    }
  })

  // 3 値を定義しただけで、意味を持つトークンがそれを参照していなければ
  // 何も変わらない。地と字が 3 値の側から来ていることを両テーマで見る
  it.each([
    [':root' as const, '--background', '--paper'],
    [':root' as const, '--foreground', '--sumi'],
    ['.dark' as const, '--foreground', '--paper'],
    ['.dark' as const, '--card', '--sumi'],
  ])('%s の %s は %s を参照する', (scope, token, expected) => {
    expect(cssToken(token.slice(2), scope)).toBe(`var(${expected})`)
  })
})

describe('信号色', () => {
  // 「ダーク側を忘れない」。`:root` で定義した custom property は `.dark`
  // （どちらも <html>）にも継承されるので**未定義にはならない** ---
  // 実際に起きるのは「ダークでもライトの値がそのまま出る」で、
  // 気付けないまま暗い地に明るい信号が乗る（あるいはその逆になる）
  it.each(['tally', 'tally-foreground', 'warning'])(
    '--%s はライトとダークの両方にある',
    (token) => {
      expect(block(':root')).toMatch(new RegExp(`--${token}:`))
      expect(block('.dark')).toMatch(new RegExp(`--${token}:`))
    },
  )

  it.each(['tally', 'warning'])('--color-%s が @theme に載っている（utility として使える）', (token) => {
    expect(indexCss).toMatch(new RegExp(`--color-${token}:\\s*var\\(--${token}\\);`))
  })

  /** hue は oklch の色相角。 */
  const hue = (name: string, scope: ':root' | '.dark') =>
    Number(/oklch\(\s*[\d.]+\s+[\d.]+\s+([\d.]+)\s*\)/.exec(cssToken(name, scope))![1])

  // ライトとダークを別々に見る。片方だけ色相がずれる事故を拾うため
  it.each([':root', '.dark'] as const)('%s でタリーは赤、警告は琥珀', (scope) => {
    // 赤は 0〜40 度、琥珀は 40〜90 度。
    expect(hue('tally', scope)).toBeGreaterThanOrEqual(0)
    expect(hue('tally', scope)).toBeLessThan(40)
    expect(hue('warning', scope)).toBeGreaterThanOrEqual(40)
    expect(hue('warning', scope)).toBeLessThan(90)
  })
})
