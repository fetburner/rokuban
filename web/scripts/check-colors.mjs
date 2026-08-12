// トークン外の生の色値を検出する（docs/frontend/design.md「トークン外の生の色値を
// 書かない」）。合格なら exit 0、1 つでも見つかれば該当行を出して exit 1。
//
//   pnpm check:colors
//
// 検出するのは 2 種類:
//   1. Tailwind の標準パレット（`bg-amber-700` / `text-red-600` など）
//   2. CSS の生の色リテラル（`#rrggbb` / `rgb(...)` / `hsl(...)` / `oklch(...)`）
//
// 色の定義は src/index.css だけに置き、コンポーネントは意味を持つトークン
// （`bg-background` / `text-warning` / `bg-tally`）を使う。例外は下の ALLOW に
// 理由付きで並べる --- 「ここだけ」を無言で増やさないため、除外はファイル単位で
// 明示する。
import { readdirSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const srcDir = path.join(webDir, 'src')

/** ALLOW は「生の色値を書いてよい」ファイルと、その理由。 */
const ALLOW = new Map([
  ['src/index.css', 'トークンの定義そのもの。ここが唯一の色の出どころ'],
  [
    'src/lib/genre.ts',
    'ジャンルの淡色は Tailwind 標準パレットの 50 / 950 に限定するという既存の規律' +
      '（docs/frontend/programs.md）。16 ジャンルぶんの色相をトークンにはしない',
  ],
  // 以下 3 つの `black` は「テーマの色」ではない。映像が来ていない領域と、
  // 背後を沈める幕であって、ライト/ダークで変わってはいけない。
  ['src/components/recording-player.tsx', '<video> のレターボックス（映像の無い領域）は黒で固定する'],
  ['src/components/live-player.tsx', '<video> のレターボックス（映像の無い領域）は黒で固定する'],
  ['src/components/ui/alert-dialog.tsx', 'ダイアログの幕（shadcn 生成物）。背後を沈める黒で、地の色ではない'],
])

/** Tailwind 標準パレットの色名（無彩・有彩とも。トークン側の名前と衝突しないもの）。 */
const PALETTE =
  'red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|neutral|stone'
/**
 * 色を取るユーティリティの接頭辞。`border-l-sky-400` のような方向付きも拾えるよう、
 * 長い方（`border-l`）を先に並べる。
 */
const PREFIX =
  'bg|text|border(?:-[xytrbles])?|ring|fill|stroke|from|via|to|decoration|outline|shadow|divide|accent|caret|placeholder'

const paletteRe = new RegExp(`\\b(?:${PREFIX})-(?:${PALETTE})-\\d{2,3}\\b`, 'g')
// `white` / `black` は段階を持たないので上の正規表現に掛からない。
// 不透明度修飾（`bg-black/10`）まで含めて別に拾う。
const monoRe = new RegExp(`\\b(?:${PREFIX})-(?:white|black)(?:/\\d{1,3})?\\b`, 'g')
// 16 進は 6 桁 / 8 桁だけを見る。3 桁も CSS としては色だが、doc コメント中の
// issue 番号（`#137`）と区別できないので拾わない --- 誤検出で無効化される
// チェックより、6 桁だけを確実に止めるチェックの方が生き延びる。
const literalRe = /#[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?\b|\b(?:rgba?|hsla?|oklch|oklab|lab|lch)\(/g

function walk(dir) {
  const out = []
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else if (/\.(tsx?|css)$/.test(entry)) out.push(full)
  }
  return out
}

/**
 * 見ないもの（この検査で止められない書き方）。**明示しておく** ---
 * 「check:colors が通った」を「色をトークン外に書いていない」と読み替えられると、
 * 検査の外側が静かに広がる。
 *
 * - 動的合成（`` `bg-${tone}-500` `` / `cn('bg-' + name)`）。Tailwind 自身が
 *   クラス名を文字列として走査するので実装側の規律でも禁止されている
 * - CSS の名前付き色（`bg-[red]` / `color: 'rebeccapurple'`）
 * - 3 桁の 16 進（doc コメント中の issue 番号と区別が付かない。下の literalRe 参照）
 * - `public/` の資産（`favicon.svg` は scripts/gen-favicon.mjs の生成物で、
 *   色を持つのが仕事。生成元は docs/frontend/branding.md が権威）
 */
const OUT_OF_SCOPE = 'dynamic-class / named-color / 3-digit-hex / public/'

/** 走査対象。`src` に加えて index.html（テーマ色や inline style を書ける口）。 */
const targets = [...walk(srcDir), path.join(webDir, 'index.html')]

const hits = []
for (const file of targets) {
  const rel = path.relative(webDir, file)
  if (ALLOW.has(rel)) continue
  // テストは「壊れたら落ちる」ことを確かめるために色名を文字列で持つことがある。
  // 実装ではないので対象外にする。
  if (/\.test\.tsx?$/.test(rel)) continue
  const lines = readFileSync(file, 'utf8').split('\n')
  lines.forEach((line, i) => {
    for (const re of [paletteRe, monoRe, literalRe]) {
      re.lastIndex = 0
      let m
      while ((m = re.exec(line)) !== null) {
        hits.push({ rel, line: i + 1, text: m[0], source: line.trim() })
      }
    }
  })
}

if (hits.length === 0) {
  console.log(
    `トークン外の色値なし（${targets.length} ファイルを走査、${ALLOW.size} ファイルを除外。` +
      `理由は scripts/check-colors.mjs）`,
  )
  console.log(`  見ていない書き方: ${OUT_OF_SCOPE}`)
  process.exit(0)
}

console.log('トークン外の色値が見つかりました（docs/frontend/design.md）:')
for (const h of hits) {
  console.log(`  ${h.rel}:${h.line}: ${h.text}`)
  console.log(`    ${h.source}`)
}
process.exit(1)
