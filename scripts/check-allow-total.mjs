// scripts/*.allow.json の許容が、ベースより増えていないことを検査する。
//
// 許容は既存違反の猶予で、減らすだけのものである。ファイルごとに次の 2 つを見る。
// - base の木に実在するファイルのキーは、キーごとに増加を禁じる。
// - base に無いファイルのキー（doc を割って生まれたファイル）は、合計で見る。予算は、HEAD の木から
//   消えたファイルのキーが base で持っていた件数の合計である。割った分が移るだけなら通る。
// 許容ファイルが base に無くても、対応する scripts/textlint-rules/<名前>.js が base に無く HEAD にある
// （規則そのものの新設）場合だけは対象外にする。規則が base にあるのに許容ファイルを新設・改名した
// 場合は、base を 0 件として比べるので落ちる。
//
// 残る穴: 規則そのものを新設しつつ許容を持たせる変更は通る。ci.yml の step を弱める差分は、この検査
// では捕まらない。レビューで見る。CI は base 側にこのスクリプトがあれば base 側のものを実行する
// ので、同じ PR でこのスクリプトを弱めても捕まる。
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

const errors = []
const names = readdirSync('scripts').filter((n) => n.endsWith('.allow.json'))
for (const name of names) {
  const path = `scripts/${name}`
  const rule = `scripts/textlint-rules/${name.replace(/\.allow\.json$/, '.js')}`
  const after = JSON.parse(readFileSync(path, 'utf8'))
  let before = {}
  if (existsAt(base, path)) {
    before = JSON.parse(git('show', `${base}:${path}`))
  } else if (!existsAt(base, rule) && existsSync(rule)) {
    continue // 規則そのものの新設
  }
  let budget = 0
  for (const [key, n] of Object.entries(before)) if (!existsSync(key)) budget += n
  const added = {}
  for (const [key, n] of Object.entries(after)) {
    if (existsAt(base, key)) {
      if (n > (before[key] ?? 0)) errors.push(`${path}: "${key}" が ${before[key] ?? 0} から ${n} に増えた`)
    } else added[key] = n
  }
  if (sum(added) > budget) {
    errors.push(`${path}: base に無いファイルの許容 ${sum(added)} 件が、消えたファイルから移せる ${budget} 件を超えた`)
  }
}
for (const e of errors) console.error(`${e}。許容は減らすだけ。直すか、doc なら割る`)
console.log(`base ${base.slice(0, 12)} と比べた、${names.length} ファイル`)
process.exit(errors.length ? 1 : 0)
