// scripts/*.allow.json の件数の合計が、ベースより増えていないことを検査する。
//
// 許容は既存違反の猶予で、減らすだけのものである。キーごとではなく合計で見るのは、doc を割ったとき
// 許容がキーを移っても合計は増えないからである。ベースに無い許容ファイル（新しい規則の導入）は対象外。
//
// 使い方: node scripts/check-allow-total.mjs [base-ref]
// base-ref の既定は `git merge-base HEAD origin/main`。CI の PR では base ブランチの先端を渡す。
import { execFileSync } from 'node:child_process'
import { readdirSync, readFileSync } from 'node:fs'

const git = (...args) => execFileSync('git', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
const base = process.argv[2] ?? git('merge-base', 'HEAD', 'origin/main').trim()
const total = (json) => Object.values(JSON.parse(json)).reduce((a, b) => a + b, 0)

let bad = 0
for (const name of readdirSync('scripts').filter((n) => n.endsWith('.allow.json'))) {
  const path = `scripts/${name}`
  let before
  try {
    before = total(git('show', `${base}:${path}`))
  } catch {
    continue // ベースに無い（新しい規則）
  }
  const after = total(readFileSync(path, 'utf8'))
  if (after > before) {
    console.error(`${path}: 許容の合計が ${before} から ${after} に増えた。許容は減らすだけ。直すか、doc なら割る`)
    bad++
  }
}
process.exit(bad ? 1 : 0)
