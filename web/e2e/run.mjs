#!/usr/bin/env node
// e2e-needs がない判定を既定で回す。CI の対象は各ファイルの要件宣言から導出する。
// `finish(` の有無では判別しない。呼ばない実行判定があり、実行判定ではない fixture builder などの
// 部品もあるため、他の .mjs から相対 import されているファイルを部品として除く。run.mjs 自身も除く。
// 2026-11-14 の CI 相当実測は 5 shard で 236 / 147 / 113 / 180 / 112 秒。
// 最長 shard は分割前の design.mjs 単独だった。準備は約 110 秒で、最長 shard と合わせた経路は約 346 秒。
// main の旧 browser-e2e は 375 秒。Go test は matrix 27 秒 + 最長 shard 280 秒だった。
// この実測では各 shard の判定時間は 280 秒を下回り、n=5 で旧 browser-e2e より短かった。
// 分割前は design.mjs 1 本の所要時間が最長 shard の下限だった。この下限を縮めるために分割した。
// 2026-11-22 のローカル実測（macOS、pnpm preview）は分割前の design.mjs が 236.9 秒。
// 分割後は design-screenshots 36.0 / design-home 34.8 / design-layout 60.2 /
// design-appearance 45.6 / design-interactions 60.8 秒で、すべて PASS。
// 分割後の CI 所要時間は未計測。
// 2026-11-13 の測定では、ソース byte 数と実行時間の順位相関は Spearman 0.58 で、50 秒超の判定は
// byte 数上位 5 本にすべて含まれた。固定の所要時間表を増やさずに重い判定を分散するため、
// .mjs の byte 数を降順に並べ、現在の合計 byte 数が最小の shard へ貪欲に割り当てる。
// Go test のように直前の main の実測を cache から読む方式は timing artifact の保存・復元運用も要るため、
// ここでは毎回同じ割当てになる byte 数を代理指標にする。
import { appendFileSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { spawn, spawnSync } from 'node:child_process'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { performance } from 'node:perf_hooks'

const SELF = fileURLToPath(import.meta.url)
const E2E_DIR = path.dirname(SELF)
const WEB_DIR = path.dirname(E2E_DIR)
const require = createRequire(path.join(WEB_DIR, 'package.json'))
const NEEDS = new Set(['webkit', 'db-state', 's3', 'manual'])

/** resolveViteCLI は web の依存としてインストールされた Vite CLI のパスを返す。 */
export function resolveViteCLI() {
  const packagePath = require.resolve('vite/package.json')
  const vitePackage = JSON.parse(readFileSync(packagePath, 'utf8'))
  return path.resolve(path.dirname(packagePath), vitePackage.bin.vite)
}

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

function leadingGoDeps(source) {
  const lines = source.split(/\r?\n/)
  let index = lines[0]?.startsWith('#!') ? 1 : 0
  const comments = []
  for (; index < lines.length; index++) {
    const line = lines[index]
    if (line.trim() === '') continue
    if (!/^\s*\/\//.test(line)) break
    comments.push(line)
  }
  const declarations = comments
    .map((line) => line.match(/^\s*\/\/\s*e2e-go-deps:\s*(.*)$/))
    .filter(Boolean)
  const dependencies = declarations.flatMap(([, value]) => value.trim().split(/\s+/).filter(Boolean))
  if (declarations.length > 0 && dependencies.length === 0) {
    throw new Error('e2e-go-deps: Go パッケージ指定がありません')
  }
  const invalid = dependencies.filter((dependency) => dependency.startsWith('-') || dependency.includes('...'))
  if (invalid.length > 0) {
    throw new Error(`e2e-go-deps: 不正な Go パッケージ指定: ${invalid.join(', ')}`)
  }
  return [...new Set(dependencies)]
}

/** discoverScripts は e2e/ 直下の実行スクリプトをファイル内容から選ぶ。 */
export function discoverScripts(directory = E2E_DIR) {
  const files = readdirSync(directory).filter((name) => name.endsWith('.mjs')).sort()
  const source = new Map(files.map((name) => [name, readFileSync(path.join(directory, name), 'utf8')]))
  const imported = new Set()
  for (const text of source.values()) {
    for (const specifier of relativeE2EImports(text)) imported.add(specifier.slice(2))
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
      return { name, needs, goDeps: leadingGoDeps(source.get(name)) }
    })
}

/** selectCIScripts は追加要件を宣言していない判定だけを CI の既定対象にする。 */
export function selectCIScripts(directory = E2E_DIR) {
  return discoverScripts(directory).filter(({ needs }) => needs.length === 0)
}

function relativeE2EImports(source) {
  const imports = new Set()
  const importPattern = /(?:\bfrom\s*|\bimport\s*\()\s*['"](\.\/[^'"]+\.mjs)['"]/g
  const sideEffectImportPattern = /\bimport\s*['"](\.\/[^'"]+\.mjs)['"]/g
  for (const pattern of [importPattern, sideEffectImportPattern]) {
    for (const match of source.matchAll(pattern)) imports.add(match[1])
  }
  return [...imports]
}

function sourceCallsGo(source) {
  return /\b(?:execFileSync|runOutput)\s*\(\s*(['"])go\1\s*,/.test(source)
}

/** validateGoDepsDeclarations は CI 判定の import 先まで調べ、Go 起動に宣言があることを確かめる。 */
export function validateGoDepsDeclarations(directory = E2E_DIR) {
  for (const script of selectCIScripts(directory)) {
    const pending = [path.join(directory, script.name)]
    const visited = new Set()
    let goSource
    while (pending.length > 0) {
      const filename = pending.pop()
      if (visited.has(filename)) continue
      visited.add(filename)
      const source = readFileSync(filename, 'utf8')
      if (sourceCallsGo(source)) goSource ??= filename
      for (const specifier of relativeE2EImports(source)) {
        const imported = path.resolve(path.dirname(filename), specifier)
        const relative = path.relative(directory, imported)
        if (relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)) {
          pending.push(imported)
        }
      }
    }
    if (goSource && script.goDeps.length === 0) {
      throw new Error(`${script.name}: Go を起動する ${path.relative(directory, goSource)} に e2e-go-deps 宣言がありません`)
    }
  }
}

/** selectScriptsByName は指定された判定だけを、対象集合の元の順序で返す。 */
export function selectScriptsByName(scripts, names) {
  const requested = new Set(names.map((name) => name.replace(/\.mjs$/, '')))
  if (requested.size !== names.length) throw new Error('判定スクリプト名が重複しています')
  const selected = scripts.filter(({ name }) => requested.delete(name.replace(/\.mjs$/, '')))
  if (requested.size > 0) throw new Error(`判定スクリプトが見つからない: ${[...requested].join(', ')}`)
  return selected
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
    shards[smallest].scripts.push({ name: script.name, needs: script.needs, goDeps: script.goDeps })
    shards[smallest].bytes += script.bytes
  }
  return shards.map(({ scripts: shard }) => shard.sort((left, right) => compareNames(left.name, right.name)))
}

/** createShardMatrix は対象を byte 数で shard に割り、空 shard を除いて CI matrix を作る。 */
export function createShardMatrix(scripts, shardCount, directory = E2E_DIR) {
  const shards = partitionScriptsBySourceBytes(scripts, shardCount, directory)
  return shards.flatMap((shard, index) => shard.length > 0
    ? [{
      shard: String(index + 1),
      count: String(shardCount),
      needs_go: shard.some(({ goDeps }) => goDeps.length > 0),
    }]
    : [])
}

function defaultCommand(command, args, input) {
  const result = spawnSync(command, args, { cwd: path.dirname(WEB_DIR), input, encoding: 'utf8' })
  if (result.error) throw result.error
  if (result.status !== 0) throw new Error(`${command} ${args.join(' ')} failed: ${result.stderr.trim()}`)
  return result.stdout
}

/** createCIPlan は CI の変更ファイルから対象判定と shard matrix を作り、GitHub output に書く。 */
export function createCIPlan({ full, baseSha, outputFile, runCommand = defaultCommand, scripts = selectCIScripts(), shardCount = 5, directory = E2E_DIR, distDirectory = path.join(WEB_DIR, 'dist') }) {
  let targets = scripts
  if (!full) {
    if (!baseSha) throw new Error('部分選定には PR base SHA が必要です')
    mkdirSync(distDirectory, { recursive: true })
    writeFileSync(path.join(distDirectory, '.keep'), '', { flag: 'a' })
    const changed = runCommand('git', ['diff', '--name-only', '-z', '--no-renames', baseSha, 'HEAD'])
    const packages = [...new Set(scripts.flatMap(({ goDeps }) => goDeps))]
    const affected = new Set()
    if (packages.length > 0) {
      const result = runCommand('go', ['run', './scripts/citest', ...packages], changed)
      for (const name of result.split(/\r?\n/).filter(Boolean)) affected.add(name)
    }
    const modulePath = readFileSync(path.join(path.dirname(WEB_DIR), 'go.mod'), 'utf8').match(/^module\s+(\S+)/m)?.[1]
    if (!modulePath) throw new Error('go.mod に module 宣言がありません')
    targets = scripts.filter(({ goDeps }) => goDeps.some((dependency) => {
      const pattern = dependency.startsWith('./') ? `${modulePath}/${dependency.slice(2)}` : dependency
      return affected.has(pattern)
    }))
  }

  const names = targets.map(({ name }) => name.replace(/\.mjs$/, '')).join(',')
  const matrix = { include: createShardMatrix(targets, shardCount, directory) }
  const output = [
    `matrix=${JSON.stringify(matrix)}`,
    `scripts=${names}`,
    `planned_skip=${targets.length === 0}`,
  ].join('\n') + '\n'
  if (outputFile) appendFileSync(outputFile, output)
  return { matrix, scripts: names, planned_skip: targets.length === 0, output }
}

/** summaryHeading は step summary で shard の判定表を識別できる見出しを返す。 */
export function summaryHeading(shard) {
  return shard === null
    ? 'Browser E2E'
    : `Browser E2E (shard ${shard.number}/${shard.count})`
}

function usage() {
  console.error('使い方: node e2e/run.mjs [スクリプト名 | --needs 語彙 | --shard i/n | --scripts 名前,... | --ci-plan]')
  console.error('例: node e2e/run.mjs chapters')
  console.error('例: node e2e/run.mjs --shard 1/5')
  console.error('例: node e2e/run.mjs --scripts recording-segmented-vod --shard 1/5')
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
  if (args.length === 1 && args[0] === '--ci-plan') {
    try {
      createCIPlan({
        full: process.env.E2E_CI_FULL === 'true',
        baseSha: process.env.E2E_CI_BASE_SHA,
        outputFile: process.env.GITHUB_OUTPUT,
      })
    } catch (error) {
      console.error(error.message)
      process.exitCode = 2
    }
    return
  }

  let scriptsOption
  let shardOption
  let preview = false
  const positional = []
  for (let index = 0; index < args.length; index++) {
    if (args[index] === '--scripts' && index + 1 < args.length && scriptsOption === undefined) {
      scriptsOption = args[++index]
    } else if (args[index] === '--shard' && index + 1 < args.length && shardOption === undefined) {
      shardOption = args[++index]
    } else if (args[index] === '--preview' && !preview) {
      preview = true
    } else {
      positional.push(args[index])
    }
  }
  if (
    positional.length > 2 ||
    (positional[0] === '--needs' && positional.length !== 2) ||
    (positional.length > 1 && positional[0] !== '--needs') ||
    (scriptsOption !== undefined && (scriptsOption.length === 0 || positional.length > 0)) ||
    (shardOption !== undefined && positional.length > 0)
  ) {
    usage()
    process.exitCode = 2
    return
  }

  if (preview && shardOption === undefined) {
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

  if (positional[0] === '--needs') {
    const need = positional[1]
    if (!NEEDS.has(need)) {
      console.error(`未知の e2e-needs: ${need}`)
      usage()
      process.exitCode = 2
      return
    }
    scripts = scripts.filter(({ needs }) => needs.includes(need))
  } else if (shardOption !== undefined) {
    const shard = parseShard(shardOption)
    if (!shard) {
      console.error(`shard 指定が不正: ${shardOption}`)
      usage()
      process.exitCode = 2
      return
    }
    const targets = scriptsOption === undefined
      ? selectCIScripts()
      : selectScriptsByName(selectCIScripts(), scriptsOption.split(',').filter(Boolean))
    scripts = partitionScriptsBySourceBytes(targets, shard.count)[shard.number - 1]
    summaryShard = shard
  } else if (scriptsOption !== undefined) {
    scripts = selectScriptsByName(discoverScripts(), scriptsOption.split(',').filter(Boolean))
  } else if (positional[0]) {
    const requested = positional[0].replace(/\.mjs$/, '')
    const found = scripts.find(({ name }) => name.replace(/\.mjs$/, '') === requested)
    if (!found) {
      console.error(`判定スクリプトが見つからない: ${positional[0]}`)
      usage()
      process.exitCode = 2
      return
    }
    scripts = [found]
  } else {
    scripts = selectCIScripts()
  }

  let previewProcess
  let previewLog = ''
  if (preview) {
    const server = new URL(process.env.E2E_URL ?? 'http://localhost:40773')
    const child = spawn(process.execPath, [resolveViteCLI(), 'preview', '--host', '0.0.0.0', '--port', server.port || '80', '--strictPort'], {
      cwd: WEB_DIR,
      detached: process.platform !== 'win32',
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    previewProcess = child
    child.stdout.setEncoding('utf8').on('data', (chunk) => { previewLog += chunk })
    child.stderr.setEncoding('utf8').on('data', (chunk) => { previewLog += chunk })
    let spawnError
    child.once('error', (error) => { spawnError = error })
    try {
      await waitForPreview(child, process.env.E2E_URL ?? 'http://localhost:40773', () => spawnError)
    } catch (error) {
      console.error(error.message)
      await stopPreview(child)
      console.error(previewLog)
      process.exitCode = 1
      return
    }
  }

  try {
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
    if (results.some(({ code }) => code !== 0)) {
      process.exitCode = 1
      if (previewLog) console.error(previewLog)
    }
  } finally {
    if (previewProcess) await stopPreview(previewProcess)
  }
}

/** waitForPreview は preview の応答を待ち、起動失敗を検出する。 */
export async function waitForPreview(child, url, getSpawnError, fetchImpl = fetch) {
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    if (getSpawnError()) throw new Error(`Could not start static SPA server: ${getSpawnError().message}`)
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(`Static SPA server exited with ${child.exitCode ?? child.signalCode}`)
    }
    try {
      const response = await fetchImpl(url, { signal: AbortSignal.timeout(1000) })
      if (response.ok) return
    } catch { /* Preview is not ready yet. */ }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  throw new Error(`Static SPA server did not become ready at ${url}`)
}

async function stopPreview(child) {
  if (!child.pid || child.exitCode !== null || child.signalCode !== null) return
  try {
    if (process.platform === 'win32') child.kill('SIGTERM')
    else process.kill(-child.pid, 'SIGTERM')
  } catch (error) {
    if (error.code !== 'ESRCH') throw error
  }
  await new Promise((resolve) => {
    const timer = setTimeout(resolve, 5000)
    child.once('close', () => { clearTimeout(timer); resolve() })
  })
  if (child.exitCode === null && child.signalCode === null) {
    try {
      if (process.platform === 'win32') child.kill('SIGKILL')
      else process.kill(-child.pid, 'SIGKILL')
    } catch (error) {
      if (error.code !== 'ESRCH') throw error
    }
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === SELF) await main()
