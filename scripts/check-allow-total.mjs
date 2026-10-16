// scripts/*.allow.json の許容が、ベースより増えていないことを検査する。
//
// 許容は既存違反の猶予で、減らすだけのものである。許容ファイルごとに次を見る。
// - base の木に実在するファイルのキーは、キーごとに増加を禁じる。
// - base に無いファイルのキー（doc を割って生まれたファイル）は、合計で見る。予算は、base の木に
//   実在するファイルのキーごとの減少分の和である。減少分は base の件数から HEAD の件数（HEAD に
//   ファイルが無ければ 0）を引いたもので、負なら 0 とする。元のパスに索引を残して割っても、
//   元のパスを消して割っても、割った分が移るだけなら通る。
// - HEAD にファイルが無いキーは、何も許さない行なので落とす（規則の新設も同じ）。doc を消したら
//   キーも消す。
// 許容ファイルが base に無く、対応する scripts/textlint-rules/<名前>.js も base に無い場合は、規則の
// 新設として対象外にする。ただし base の scripts/textlint-rules/*.js か scripts/*.allow.json が HEAD
// で 1 つでも消えている PR では、改名と区別できないので対象外にせず、base を 0 件として比べる。
//
// 残る穴（レビューで見る）:
// - 規則の純粋な新設に持たせた許容は通る。
// - base に無いファイルへ移した許容は、出どころの doc を見ないので、別の doc を直したり消したり
//   した分を新しい doc の違反に回せる。
// - ci.yml の step を弱める差分は捕まらない。
// CI は base 側にこのスクリプトがあれば base 側のものを実行するので、同じ PR でこのスクリプトを
// 弱めても捕まる。そのため検査を緩める修正は 2 段階になる。先にスクリプトの修正をマージし、
// 緩めた検査を使う変更は次の PR に出す。
//
// 使い方: node scripts/check-allow-total.mjs [base-ref]
// base-ref の既定は `git merge-base HEAD origin/main`。CI の PR では base ブランチの先端を渡す。
import { execFileSync } from 'node:child_process'
import { existsSync, readdirSync, readFileSync } from 'node:fs'

const git = (...args) => execFileSync('git', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
const existsAt = (base, path) => {
  try {
    git('cat-file', '-e', `${base}:${path}`)
    return true
  } catch {
    return false
  }
}
const sum = (o) => Object.values(o).reduce((a, b) => a + b, 0)

const baseRef = process.argv[2] ?? git('merge-base', 'HEAD', 'origin/main').trim()
let base
try {
  base = git('rev-parse', '--verify', `${baseRef}^{commit}`).trim()
} catch {
  console.error(`base ${baseRef} を読めない。比べられないので落とす`)
  process.exit(2)
}

const lsBase = (dir) => git('ls-tree', '--name-only', base, `${dir}/`).split('\n').filter(Boolean)
const renamed = [...lsBase('scripts'), ...lsBase('scripts/textlint-rules')]
  .filter((p) => p.endsWith('.allow.json') || (p.startsWith('scripts/textlint-rules/') && p.endsWith('.js')))
  .some((p) => !existsSync(p))

const errors = []
const missing = []
const names = readdirSync('scripts').filter((n) => n.endsWith('.allow.json'))
for (const name of names) {
  const path = `scripts/${name}`
  const rule = `scripts/textlint-rules/${name.replace(/\.allow\.json$/, '.js')}`
  const after = JSON.parse(readFileSync(path, 'utf8'))
  for (const key of Object.keys(after)) if (!existsSync(key)) missing.push(`${path}: "${key}" はファイルが無い。行ごと消す`)
  let before = {}
  if (existsAt(base, path)) {
    before = JSON.parse(git('show', `${base}:${path}`))
  } else if (!renamed && !existsAt(base, rule) && existsSync(rule)) {
    continue // 規則の新設
  }
  let budget = 0
  for (const [key, n] of Object.entries(before)) {
    if (existsAt(base, key)) budget += Math.max(0, n - (existsSync(key) ? (after[key] ?? 0) : 0))
  }
  const added = {}
  for (const [key, n] of Object.entries(after)) {
    if (!existsSync(key)) continue
    if (existsAt(base, key)) {
      if (n > (before[key] ?? 0)) errors.push(`${path}: "${key}" が ${before[key] ?? 0} から ${n} に増えた`)
    } else added[key] = n
  }
  if (sum(added) > budget) {
    errors.push(`${path}: base に無いファイルの許容 ${sum(added)} 件が、base のファイルで減った ${budget} 件を超えた`)
  }
}
for (const e of errors) console.error(`${e}。許容は減らすだけ。直すか、doc なら割る`)
for (const e of missing) console.error(e)
console.log(`base ${base.slice(0, 12)} と比べた、${names.length} ファイル`)
process.exit(errors.length || missing.length ? 1 : 0)
