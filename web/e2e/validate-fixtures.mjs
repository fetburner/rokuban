import { spawn } from 'node:child_process'
import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url))

// 契約検証を持つスクリプトは手書きで一覧しない --- 手書きだと、並行して増えた
// スクリプトの契約検証が一覧への追加漏れで静かに検査対象から外れる（実際に
// programs-reservation-error.mjs で起きた）。各ファイルの中身に
// validateFixturesOrExit の呼び出し（`await` に続く形）があるものを導出する。
// `lib.mjs` は定義（`export async function` に続く形）なので当たらず自動的に
// 外れる。このスクリプト自身が誤って一覧に入らないよう、判定に使う文字列は
// 連結して組み立てている --- 1 つのリテラルとして書くとこのファイル自身の
// ソースにその文字列が現れ、自分を一覧に入れてしまう（検証時に必ず確かめる）。
// 各スクリプトを個別の Node プロセスで実行するので、1 本のフィクスチャが
// 壊れていても残りのスクリプトの検証を省略しない。
const CALL_MARKER = ['await', 'validateFixturesOrExit('].join(' ')
const SCRIPTS = readdirSync(E2E_DIR)
  .filter((f) => f.endsWith('.mjs'))
  .filter((f) => readFileSync(path.join(E2E_DIR, f), 'utf8').includes(CALL_MARKER))
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
