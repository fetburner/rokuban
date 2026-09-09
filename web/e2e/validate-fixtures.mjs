import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// 契約検証を持つスクリプトはここで列挙する。各スクリプトを個別の Node プロセスで
// 実行するので、1 本のフィクスチャが壊れていても残りのスクリプトの検証を省略しない。
// 新しいスクリプトに validateFixturesOrExit を足したら、この一覧にも追加する。
const SCRIPTS = [
  'badge-links.mjs',
  'grid-reserved.mjs',
  'personalization.mjs',
  'design.mjs',
  'programs-empty-window.mjs',
  'reservations-capacity-error.mjs',
  'reservations-mobile.mjs',
  'multi-site.mjs',
  'subtitles.mjs',
  'recordings-selection.mjs',
  'sse-refresh.mjs',
]

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url))

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
