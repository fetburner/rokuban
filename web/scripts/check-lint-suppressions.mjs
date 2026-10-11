// `web/src` の Oxlint 抑制コメント（eslint-* 互換 alias を含む）を規則・ファイルごとに数え、台帳と照合する。
// テストも対象に含める。テスト内の抑制もアサーションの意図を弱める可能性があり、
// 実装と同じくレビューに見える形で管理する。
//
//   pnpm check:lint-suppressions
//
// 1 コメントで複数規則を抑制する場合は規則ごとに数える。文字列リテラルを誤検出
// しないよう、TypeScript AST の leading / trailing comment ranges だけを読む。
import { readFileSync, readdirSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import ts from 'typescript'

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const srcDir = path.join(webDir, 'src')
const ledgerPath = path.join(webDir, 'lint-suppressions.json')
const sourceFileRe = /\.(?:[cm]?js|jsx|[cm]?ts|tsx)$/

// react/purity のファイル別の上限（cap）。台帳の書き換えだけでは別ファイルへ移せない。
// 減らすときはここを触らなくてよい。各抑制の理由はソースのコメントにある。新しいファイルで
// 許す場合は、下の REACT_PURITY_ALLOWLIST に理由を足し、台帳も同じ変更で更新する。
const REACT_PURITY_BASELINE = new Map([
  ['src/pages/home.tsx', 1],
  ['src/pages/programs.tsx', 1],
])

/** react/purity を新しいファイルで使ってよい場合、そのファイルと理由を登録する。 */
const REACT_PURITY_ALLOWLIST = new Map([])

function walk(dir) {
  const out = []
  for (const entry of readdirSync(dir).sort()) {
    const full = path.join(dir, entry)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else if (sourceFileRe.test(entry)) out.push(full)
  }
  return out
}

function commentTokens(file, source) {
  const extension = path.extname(file).toLowerCase()
  const scriptKind = extension === '.tsx'
    ? ts.ScriptKind.TSX
    : extension === '.jsx'
      ? ts.ScriptKind.JSX
      : /\.tsx?$/.test(extension)
        ? ts.ScriptKind.TS
        : ts.ScriptKind.JS
  const sourceFile = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, scriptKind)
  const ranges = new Map()
  const jsxTextRanges = []

  function visit(node) {
    if (node.kind === ts.SyntaxKind.JsxText) jsxTextRanges.push([node.pos, node.end])
    for (const group of [
      ts.getLeadingCommentRanges(source, node.pos),
      ts.getTrailingCommentRanges(source, node.end),
    ]) {
      for (const range of group ?? []) ranges.set(range.pos, range)
    }
    // getChildren includes punctuation tokens, so comments inside otherwise empty blocks
    // and JSX comment containers still have a token boundary to attach to.
    for (const child of node.getChildren(sourceFile)) visit(child)
  }
  visit(sourceFile)

  return [...ranges.values()]
    // getTrailingCommentRanges can interpret JSX text beginning with `//` as a comment.
    // Such text is not trivia, so discard any range that starts inside a JSX text node.
    .filter((range) => !jsxTextRanges.some(([start, end]) => range.pos >= start && range.pos < end))
    .sort((left, right) => left.pos - right.pos)
    .map(({ pos, end }) => source.slice(pos, end))
}

function directivesInComment(comment) {
  const text = comment
    .replace(/^\/\//, '')
    .replace(/^\/\*/, '')
    .replace(/\*\/$/, '')

  const rules = []
  for (const line of text.split(/\r?\n/)) {
    const content = line.replace(/^\s*\*\s?/, '').trim()
    const match = content.match(/^(?:oxlint|eslint)-disable(?:-next-line|-line)?(?=\s|$)(?:\s+(.*))?$/)
    if (!match) continue

    let ruleText = (match[1] ?? '').trim()
    if (ruleText.startsWith('--')) ruleText = ''
    else ruleText = ruleText.replace(/\s+--\s+[\s\S]*$/, '').trim()

    // 規則名なしは全規則の抑制として記録する。特に `*` は react/purity も
    // 抑制するが、別の検査で必ずエラーにする。
    const names = ruleText
      ? [...new Set(ruleText.split(',').map((name) => name.trim()).filter(Boolean))]
      : ['*']
    rules.push(...names)
  }
  return rules
}

/** collectSuppressions は src 配下の抑制コメントを規則・ファイルごとに数える。 */
export function collectSuppressions(root = srcDir, rootForRelativePaths = path.dirname(root)) {
  const counts = new Map()
  for (const file of walk(root)) {
    const rel = path.relative(rootForRelativePaths, file).split(path.sep).join('/')
    const source = readFileSync(file, 'utf8')
    for (const comment of commentTokens(file, source)) {
      for (const rule of directivesInComment(comment)) {
        if (!counts.has(rule)) counts.set(rule, new Map())
        const byFile = counts.get(rule)
        byFile.set(rel, (byFile.get(rel) ?? 0) + 1)
      }
    }
  }

  return Object.fromEntries(
    [...counts]
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([rule, byFile]) => [
        rule,
        Object.fromEntries([...byFile].sort(([left], [right]) => left.localeCompare(right))),
      ]),
  )
}

/** validateSuppressions は実測件数と台帳の差、および react/purity の許可範囲を返す。 */
export function validateSuppressions(actual, ledger, {
  purityBaseline = REACT_PURITY_BASELINE,
  purityAllowlist = REACT_PURITY_ALLOWLIST,
} = {}) {
  const errors = []
  for (const [file, reason] of purityAllowlist) {
    if (typeof reason !== 'string' || reason.trim() === '') {
      errors.push(`  react/purity ${file}: REACT_PURITY_ALLOWLIST に理由がありません`)
    }
  }

  for (const [file, count] of Object.entries(actual['*'] ?? {})) {
    const ledgerCount = ledger['*']?.[file] ?? 0
    errors.push(
      `  * ${file}: 台帳 ${ledgerCount} 件 / 実際 ${count} 件。` +
        '全規則の抑制は規則別に監査できないため、個別の規則名を明示してください',
    )
  }

  const rules = new Set([...Object.keys(actual), ...Object.keys(ledger)])
  for (const rule of [...rules].sort()) {
    const files = new Set([...Object.keys(actual[rule] ?? {}), ...Object.keys(ledger[rule] ?? {})])
    for (const file of [...files].sort()) {
      const actualCount = actual[rule]?.[file] ?? 0
      const ledgerCount = ledger[rule]?.[file] ?? 0
      if (actualCount !== ledgerCount) {
        errors.push(`  ${rule} ${file}: 台帳 ${ledgerCount} 件 / 実際 ${actualCount} 件`)
      }
    }
  }

  for (const [file, count] of Object.entries(actual['react/purity'] ?? {}).sort()) {
    const baseline = purityBaseline.get(file) ?? 0
    if (count > baseline && !purityAllowlist.has(file)) {
      const ledgerCount = ledger['react/purity']?.[file] ?? 0
      errors.push(
        `  react/purity ${file}: 台帳 ${ledgerCount} 件 / 実際 ${count} 件 / 既存上限 ${baseline} 件。` +
          '新しいファイルで使う場合は REACT_PURITY_ALLOWLIST に理由を追加してください',
      )
    }
  }
  return errors
}

function main() {
  const actual = collectSuppressions()
  const ledger = JSON.parse(readFileSync(ledgerPath, 'utf8'))
  const errors = validateSuppressions(actual, ledger)
  if (errors.length > 0) {
    console.error('oxlint 抑制コメントと台帳が一致しません:')
    console.error(errors.join('\n'))
    process.exitCode = 1
    return
  }

  const total = Object.values(actual).reduce(
    (sum, byFile) => sum + Object.values(byFile).reduce((fileSum, count) => fileSum + count, 0),
    0,
  )
  console.log(`oxlint 抑制コメントは台帳と一致しています（${total} 件、${Object.keys(actual).length} 規則）`)
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) main()
