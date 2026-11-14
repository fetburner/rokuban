#!/usr/bin/env node
// e2e-needs がない判定を既定で回す。CI の対象は各ファイルの要件宣言から導出する。
// `finish(` の有無では判別しない。呼ばない実行判定があり、実行判定ではない fixture builder などの
// 部品もあるため、他の .mjs から相対 import されているファイルを部品として除く。run.mjs 自身も除く。
// 2026-11-14 の CI 相当実測は 5 shard で 236 / 147 / 113 / 180 / 112 秒（最長は design.mjs 単独）。
// 準備は約 110 秒で、最長 shard と合わせた経路は約 346 秒。
// main の旧 browser-e2e は 375 秒、Go test は matrix 27 秒 + 最長 shard 280 秒だった。最長 shard は
// design.mjs 単独で、各 shard の判定時間は 280 秒を下回る。n=5 なら旧 browser-e2e より短く、
// これ以上 shard を増やしても design.mjs 自体の時間は縮まらない。
// 2026-11-13 の測定では、ソース byte 数と実行時間の順位相関は Spearman 0.58 で、50 秒超の判定は
// byte 数上位 5 本にすべて含まれた。固定の所要時間表を増やさずに重い判定を分散するため、
// .mjs の byte 数を降順に並べ、現在の合計 byte 数が最小の shard へ貪欲に割り当てる。
// Go test のように直前の main の実測を cache から読む方式は timing artifact の保存・復元運用も要るため、
// ここでは毎回同じ割当てになる byte 数を代理指標にする。
import { appendFileSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { performance } from 'node:perf_hooks'

const SELF = fileURLToPath(import.meta.url)
const E2E_DIR = path.dirname(SELF)
const WEB_DIR = path.dirname(E2E_DIR)
const NEEDS = new Set(['webkit', 'db-state', 's3', 'manual'])

function compareNames(left, right) {
  return left < right ? -1 : left > right ? 1 : 0
}

function leadingNeeds(source) {
  const lines = source.split(/\r?\n/)
  let index = lines[0]?.startsWith('#!') ? 1 : 0
  const leadingComments = []
  for (; index < lines.length; index++) {
    const line = lines[index]
    if (line.trim() === '') continue
    if (!/^\s*\/\//.test(line)) break
    leadingComments.push(line)
  }
  const declaration = leadingComments
    .map((line) => line.match(/^\s*\/\/\s*e2e-needs:\s*(.*)$/))
    .find(Boolean)
  return declaration ? declaration[1].trim().split(/\s+/).filter(Boolean) : []
}

/** discoverScripts は e2e/ 直下の実行スクリプトをファイル内容から選ぶ。 */
export function discoverScripts(directory = E2E_DIR) {
  const files = readdirSync(directory).filter((name) => name.endsWith('.mjs')).sort()
  const source = new Map(files.map((name) => [name, readFileSync(path.join(directory, name), 'utf8')]))
  const imported = new Set()
  const importPattern = /(?:\bfrom\s*|\bimport\s*\()\s*['"]\.\/([^'"]+\.mjs)['"]/g
  const sideEffectImportPattern = /\bimport\s*['"]\.\/([^'"]+\.mjs)['"]/g
  for (const text of source.values()) {
    for (const pattern of [importPattern, sideEffectImportPattern]) {
      for (const match of text.matchAll(pattern)) imported.add(match[1])
    }
  }

  return files
    .filter((name) => name !== 'run.mjs' && name !== 'validate-fixtures.mjs')
    .filter((name) => !imported.has(name))
    .map((name) => {
      const needs = leadingNeeds(source.get(name))
      const unknown = needs.filter((need) => !NEEDS.has(need))
      if (unknown.length > 0) {
        throw new Error(`${name}: 未知の e2e-needs: ${unknown.join(', ')}`)
      }
      return { name, needs }
    })
}

/** selectCIScripts は追加要件を宣言していない判定だけを CI の既定対象にする。 */
export function selectCIScripts(directory = E2E_DIR) {
  return discoverScripts(directory).filter(({ needs }) => needs.length === 0)
}

/** partitionScriptsBySourceBytes は対象をソース byte 数の LPT 順で決定的に shard へ割る。 */
export function partitionScriptsBySourceBytes(scripts, shardCount, directory = E2E_DIR) {
  if (!Number.isSafeInteger(shardCount) || shardCount < 1) {
    throw new RangeError(`shard 数が不正: ${shardCount}`)
  }
  const shards = Array.from({ length: shardCount }, () => ({ bytes: 0, scripts: [] }))
  const sized = scripts
    .map((script) => ({ ...script, bytes: statSync(path.join(directory, script.name)).size }))
    .sort((left, right) => right.bytes - left.bytes || compareNames(left.name, right.name))

  for (const script of sized) {
    let smallest = 0
    for (let index = 1; index < shards.length; index++) {
      if (shards[index].bytes < shards[smallest].bytes) smallest = index
    }
    shards[smallest].scripts.push({ name: script.name, needs: script.needs })
    shards[smallest].bytes += script.bytes
  }
  return shards.map(({ scripts: shard }) => shard.sort((left, right) => compareNames(left.name, right.name)))
}

/** summaryHeading は step summary で shard の判定表を識別できる見出しを返す。 */
export function summaryHeading(shard) {
  return shard === null
    ? 'Browser E2E'
    : `Browser E2E (shard ${shard.number}/${shard.count})`
}

function usage() {
  console.error('使い方: node e2e/run.mjs [スクリプト名 | --needs 語彙 | --shard i/n]')
  console.error('例: node e2e/run.mjs chapters')
  console.error('例: node e2e/run.mjs --shard 1/5')
}

function parseShard(value) {
  const match = value?.match(/^([1-9]\d*)\/([1-9]\d*)$/)
  if (!match) return null
  const number = Number(match[1])
  const count = Number(match[2])
  if (!Number.isSafeInteger(number) || !Number.isSafeInteger(count) || number > count) return null
  return { number, count }
}

function run(script) {
  return new Promise((resolve) => {
    const env = { ...process.env }
    // 契約検証だけで exit 0 になる環境漏れを防ぎ、常に実判定を走らせる。
    delete env.E2E_VALIDATE_FIXTURES_ONLY
    const started = performance.now()
    const child = spawn(process.execPath, [path.join(E2E_DIR, script.name)], {
      cwd: WEB_DIR,
      env,
      stdio: 'inherit',
    })
    let settled = false
    const settle = (code, signal, error) => {
      if (settled) return
      settled = true
      if (error) console.error(`${script.name}: 起動に失敗しました: ${error.message}`)
      resolve({ name: script.name, code: code ?? 1, signal, seconds: (performance.now() - started) / 1000 })
    }
    child.on('error', (error) => settle(1, null, error))
    child.on('close', (code, signal) => settle(code, signal))
  })
}

function markdown(results) {
  const rows = results.map(({ name, code, signal, seconds }) =>
    `| ${name} | ${code === 0 ? 'PASS' : `FAIL (${code ?? signal})`} | ${seconds.toFixed(1)} s |`,
  )
  return ['| Script | Result | Duration |', '|---|---:|---:|', ...rows].join('\n')
}

async function main() {
  const args = process.argv.slice(2)
  if (
    args.length > 2 ||
    (['--needs', '--shard'].includes(args[0]) && args.length !== 2) ||
    (!['--needs', '--shard'].includes(args[0]) && args.length > 1)
  ) {
    usage()
    process.exitCode = 2
    return
  }

  let scripts
  let summaryShard = null
  try {
    scripts = discoverScripts()
  } catch (error) {
    console.error(error.message)
    process.exitCode = 2
    return
  }

  if (args[0] === '--needs') {
    const need = args[1]
    if (!NEEDS.has(need)) {
      console.error(`未知の e2e-needs: ${need}`)
      usage()
      process.exitCode = 2
      return
    }
    scripts = scripts.filter(({ needs }) => needs.includes(need))
  } else if (args[0] === '--shard') {
    const shard = parseShard(args[1])
    if (!shard) {
      console.error(`shard 指定が不正: ${args[1]}`)
      usage()
      process.exitCode = 2
      return
    }
    const targets = selectCIScripts()
    scripts = partitionScriptsBySourceBytes(targets, shard.count)[shard.number - 1]
    summaryShard = shard
  } else if (args[0]) {
    const requested = args[0].replace(/\.mjs$/, '')
    const found = scripts.find(({ name }) => name.replace(/\.mjs$/, '') === requested)
    if (!found) {
      console.error(`判定スクリプトが見つからない: ${args[0]}`)
      usage()
      process.exitCode = 2
      return
    }
    scripts = [found]
  } else {
    scripts = selectCIScripts()
  }

  const results = []
  for (const script of scripts) {
    console.log(`\n=== E2E: ${script.name} ===`)
    results.push(await run(script))
  }

  const table = markdown(results)
  console.log(`\n${table}`)
  if (process.env.GITHUB_STEP_SUMMARY) {
    appendFileSync(process.env.GITHUB_STEP_SUMMARY, `\n\n## ${summaryHeading(summaryShard)}\n\n${table}\n`)
  }
  if (results.some(({ code }) => code !== 0)) process.exitCode = 1
}

if (process.argv[1] && path.resolve(process.argv[1]) === SELF) await main()
