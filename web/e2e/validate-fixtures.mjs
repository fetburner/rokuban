import { spawn } from 'node:child_process'
import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const SELF = fileURLToPath(import.meta.url)
const E2E_DIR = path.dirname(SELF)

// 契約検証を持つスクリプトは手書きで一覧しない --- 手書きだと、並行して増えた
// スクリプトの契約検証が一覧への追加漏れで静かに検査対象から外れる（実際に
// programs-reservation-error.mjs で起きた）。`await validateFixturesOrExit(`
// を含むファイルを導出する。`lib.mjs` は定義（`export async function` に続く形）
// なので当たらず自動的に外れる。**このファイル自身は名前で除く** --- 自分を
// 一覧に入れると自分を spawn し続ける（除外を外して実測: 通常の完走が約 5 秒
// なのに対し 25 秒で 5 段目に入りまだ増えていた。CI では job のタイムアウトまで
// ハングする）。コメントに判定文字列を書いても自己参照しない形にしておく。
// 各スクリプトを個別の Node プロセスで実行するので、1 本のフィクスチャが
// 壊れていても残りのスクリプトの検証を省略しない。
const SCRIPTS = readdirSync(E2E_DIR)
  .filter((f) => f.endsWith('.mjs') && path.join(E2E_DIR, f) !== SELF)
  .filter((f) => readFileSync(path.join(E2E_DIR, f), 'utf8').includes('await validateFixturesOrExit('))
  .sort()

function run(script) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [path.join(E2E_DIR, script)], {
      env: { ...process.env, E2E_VALIDATE_FIXTURES_ONLY: '1' },
      stdio: 'inherit',
    })
    let settled = false
    const settle = (result) => {
      if (settled) return
      settled = true
      resolve(result)
    }
    child.on('error', (error) => {
      console.error(`${script}: 起動に失敗しました: ${error.message}`)
      settle({ script, code: 1, signal: null })
    })
    child.on('close', (code, signal) => settle({ script, code, signal }))
  })
}

const results = []
for (const script of SCRIPTS) {
  console.log(`\n=== 契約検証: ${script} ===`)
  results.push(await run(script))
}

const failed = results.filter(({ code }) => code !== 0)
if (failed.length > 0) {
  console.error('\n契約検証に失敗したスクリプト:')
  for (const { script, code, signal } of failed) {
    console.error(`  ${script}（終了コード ${code ?? `signal ${signal}`}）`)
  }
  process.exitCode = 1
}
