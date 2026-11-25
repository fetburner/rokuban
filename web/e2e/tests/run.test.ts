import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import os from 'node:os'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  createShardMatrix,
  createCIPlan,
  discoverScripts,
  partitionScriptsBySourceBytes,
  resolveViteCLI,
  selectCIScripts,
  selectScriptsByName,
  summaryHeading,
  validateGoDepsDeclarations,
  waitForPreview,
} from '../run.mjs'

describe('e2e/run.mjs の CI 対象選別', () => {
  it('要件宣言の追加で外れ、削除で戻る。finish の有無は対象を決めない', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-select-'))
    try {
      writeFileSync(path.join(directory, 'run.mjs'), '')
      writeFileSync(path.join(directory, 'validate-fixtures.mjs'), '')
      writeFileSync(path.join(directory, 'shared.mjs'), 'export const helper = true\n')
      writeFileSync(path.join(directory, 'with-finish.mjs'), "import { helper } from './shared.mjs'\nawait finish([])\n")
      writeFileSync(path.join(directory, 'without-finish.mjs'), '// no finish call\n')
      writeFileSync(path.join(directory, 'webkit.mjs'), '// e2e-needs: webkit\n')
      writeFileSync(path.join(directory, 'body-declaration.mjs'), "const value = true\n// e2e-needs: manual\n")
      writeFileSync(path.join(directory, 'shebang-needs.mjs'), '#!/usr/bin/env node\n// e2e-needs: db-state\n')
      writeFileSync(path.join(directory, 'go-check.mjs'), '// e2e-go-deps: ./internal/streamer\n// browser check\n')
      writeFileSync(path.join(directory, 'two-go-deps.mjs'), '// e2e-go-deps: ./internal/streamer ./internal/mediaexec\n')

      expect(selectCIScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'go-check.mjs',
        'two-go-deps.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])
      expect(discoverScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'go-check.mjs',
        'shebang-needs.mjs',
        'two-go-deps.mjs',
        'webkit.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])

      const candidate = path.join(directory, 'with-finish.mjs')
      writeFileSync(candidate, '// e2e-needs: db-state\n' + "import { helper } from './shared.mjs'\nawait finish([])\n")
      expect(selectCIScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'go-check.mjs',
        'two-go-deps.mjs',
        'without-finish.mjs',
      ])

      writeFileSync(candidate, "import { helper } from './shared.mjs'\nawait finish([])\n")
      expect(selectCIScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'go-check.mjs',
        'two-go-deps.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])
      expect(discoverScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'go-check.mjs',
        'shebang-needs.mjs',
        'two-go-deps.mjs',
        'webkit.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('Go 依存宣言を読み、指定された CI 判定だけを選ぶ', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-go-deps-'))
    try {
      writeFileSync(path.join(directory, 'run.mjs'), '')
      writeFileSync(path.join(directory, 'validate-fixtures.mjs'), '')
      writeFileSync(path.join(directory, 'one.mjs'), '// e2e-go-deps: ./internal/streamer\n')
      writeFileSync(path.join(directory, 'two.mjs'), '// e2e-go-deps: ./internal/mediaexec ./internal/streamer\n')
      writeFileSync(path.join(directory, 'plain.mjs'), '// no Go dependency\n')

      const scripts = discoverScripts(directory)
      expect(scripts.map(({ name, goDeps }) => [name, goDeps])).toEqual([
        ['one.mjs', ['./internal/streamer']],
        ['plain.mjs', []],
        ['two.mjs', ['./internal/mediaexec', './internal/streamer']],
      ])
      expect(selectScriptsByName(selectCIScripts(directory), ['two', 'one.mjs']).map(({ name }) => name)).toEqual([
        'one.mjs',
        'two.mjs',
      ])
      expect(() => selectScriptsByName(selectCIScripts(directory), ['missing'])).toThrow('判定スクリプトが見つからない')

      expect(() => validateGoDepsDeclarations()).not.toThrow()
      const productionGoChecks = discoverScripts().filter(({ goDeps }) => goDeps.length > 0)
      expect(productionGoChecks.map(({ name, goDeps }) => [name, goDeps])).toEqual([
        ['home-resume-frame.mjs', ['./internal/worker', './internal/streamer']],
        ['recording-playback-timeline.mjs', ['./internal/worker', './internal/streamer']],
        ['recording-segmented-vod.mjs', ['./internal/streamer']],
      ])
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('CI 判定が相対 import 先で Go を起動する場合も宣言を要求する', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-go-check-'))
    try {
      writeFileSync(path.join(directory, 'run.mjs'), '')
      writeFileSync(path.join(directory, 'validate-fixtures.mjs'), '')
      writeFileSync(path.join(directory, 'check.mjs'), "import './fixture.mjs'\n")
      const fixture = path.join(directory, 'fixture.mjs')
      writeFileSync(fixture, "execFileSync('go', ['test', './internal/worker'])\n")

      expect(() => validateGoDepsDeclarations(directory)).toThrow('check.mjs: Go を起動する fixture.mjs に e2e-go-deps 宣言がありません')

      writeFileSync(fixture, "runOutput('go', ['test', './internal/streamer'])\n")
      expect(() => validateGoDepsDeclarations(directory)).toThrow('check.mjs: Go を起動する fixture.mjs に e2e-go-deps 宣言がありません')

      writeFileSync(path.join(directory, 'check.mjs'), "// e2e-go-deps: ./internal/worker\nimport './fixture.mjs'\n")
      expect(() => validateGoDepsDeclarations(directory)).not.toThrow()

      writeFileSync(path.join(directory, 'check.mjs'), "// e2e-go-deps: ./internal/worker/...\nimport './fixture.mjs'\n")
      expect(() => validateGoDepsDeclarations(directory)).toThrow('e2e-go-deps: 不正な Go パッケージ指定: ./internal/worker/...')
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('byte 数で作った shard は決定的で、対象全体を重複なく覆う', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-shards-'))
    try {
      for (const [name, bytes] of [
        ['short.mjs', 20],
        ['medium.mjs', 80],
        ['large.mjs', 180],
        ['other.mjs', 55],
        ['last.mjs', 120],
        ['also-short.mjs', 35],
      ]) {
        writeFileSync(path.join(directory, name), `// ${'x'.repeat(bytes)}\n`)
      }
      const targets = selectCIScripts(directory)
      const shards = partitionScriptsBySourceBytes(targets, 3, directory)
      const repeated = partitionScriptsBySourceBytes(targets, 3, directory)
      const names = shards.flat().map(({ name }) => name)

      expect(shards).toEqual(repeated)
      expect(shards.every((shard) => shard.length > 0)).toBe(true)
      expect(new Set(names).size).toBe(names.length)
      expect([...names].sort()).toEqual(targets.map(({ name }) => name).sort())
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })
})

describe('e2e/run.mjs の CLI', () => {
  it('--matrix の削除後も手元の選択入口は維持し、旧 matrix 入口を受け付けない', () => {
    const runner = path.resolve(process.cwd(), 'e2e/run.mjs')
    const result = spawnSync(process.execPath, [runner, '--matrix', '5'], { encoding: 'utf8' })
    expect(result.status).toBe(2)
    expect(result.stderr).toContain('使い方:')
  })
})

describe('e2e/run.mjs の step summary 見出し', () => {
  it('5 shard の結果表を重複しない見出しで識別する', () => {
    const headings = Array.from({ length: 5 }, (_, index) =>
      summaryHeading({ number: index + 1, count: 5 }),
    )

    expect(headings).toEqual([
      'Browser E2E (shard 1/5)',
      'Browser E2E (shard 2/5)',
      'Browser E2E (shard 3/5)',
      'Browser E2E (shard 4/5)',
      'Browser E2E (shard 5/5)',
    ])
    expect(new Set(headings).size).toBe(5)
    expect(summaryHeading(null)).toBe('Browser E2E')
  })
})

describe('e2e/run.mjs の CI shard matrix', () => {
  it('Go 依存の有無、空 shard、番号を反映し、選択対象も同じ規則で分割する', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-matrix-'))
    try {
      writeFileSync(path.join(directory, 'run.mjs'), '')
      writeFileSync(path.join(directory, 'validate-fixtures.mjs'), '')
      const writeSizedScript = (name: string, byteLength: number, goPackage?: string) => {
        const declaration = goPackage ? `// e2e-go-deps: ${goPackage}\n` : ''
        const prefix = `${declaration}// `
        writeFileSync(path.join(directory, name), `${prefix}${'x'.repeat(byteLength - prefix.length - 1)}\n`)
      }
      writeSizedScript('go-large.mjs', 300, './internal/worker')
      writeSizedScript('plain-medium.mjs', 200)
      writeSizedScript('go-small.mjs', 100, './internal/streamer')

      const scripts = selectCIScripts(directory)
      expect(createShardMatrix(scripts, 5, directory)).toEqual([
        { shard: '1', count: '5', needs_go: true },
        { shard: '2', count: '5', needs_go: false },
        { shard: '3', count: '5', needs_go: true },
      ])

      const selected = selectScriptsByName(scripts, ['plain-medium', 'go-small.mjs'])
      expect(createShardMatrix(selected, 4, directory)).toEqual([
        { shard: '1', count: '4', needs_go: false },
        { shard: '2', count: '4', needs_go: true },
      ])
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })
})

describe('e2e/run.mjs の CI 計画', () => {
  it('全件では全判定を shard に割り、GitHub output に書き出す', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-plan-full-'))
    const outputFile = path.join(directory, 'output')
    try {
      writeFileSync(path.join(directory, 'a.mjs'), 'a'.repeat(20))
      writeFileSync(path.join(directory, 'b.mjs'), 'b'.repeat(30))
      const plan = createCIPlan({
        full: true,
        outputFile,
        directory,
        shardCount: 5,
        scripts: [
          { name: 'a.mjs', needs: [], goDeps: [] },
          { name: 'b.mjs', needs: [], goDeps: ['./internal/worker'] },
        ],
        runCommand: () => { throw new Error('全件計画では子プロセスを呼ばない') },
      })

      expect(plan.matrix).toEqual({ include: [
        { shard: '1', count: '5', needs_go: true },
        { shard: '2', count: '5', needs_go: false },
      ] })
      expect(plan.scripts).toBe('a,b')
      expect(plan.planned_skip).toBe(false)
      expect(readFileSync(outputFile, 'utf8')).toBe([
        'matrix={"include":[{"shard":"1","count":"5","needs_go":true},{"shard":"2","count":"5","needs_go":false}]}',
        'scripts=a,b',
        'planned_skip=false',
        '',
      ].join('\n'))
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('変更ファイルを citest に一度渡し、出力された依存を宣言した判定だけを割る', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-plan-partial-'))
    const distDirectory = path.join(directory, 'web', 'dist')
    try {
      for (const name of ['worker.mjs', 'streamer.mjs', 'plain.mjs']) writeFileSync(path.join(directory, name), name.repeat(10))
      const commands: Array<{ command: string; args: string[]; input?: string }> = []
      const plan = createCIPlan({
        full: false,
        baseSha: 'base-sha',
        directory,
        distDirectory,
        shardCount: 5,
        scripts: [
          { name: 'worker.mjs', needs: [], goDeps: ['./internal/worker'] },
          { name: 'streamer.mjs', needs: [], goDeps: ['./internal/streamer'] },
          { name: 'plain.mjs', needs: [], goDeps: [] },
        ],
        runCommand: (command, args, input) => {
          commands.push({ command, args, input })
          return command === 'git' ? 'internal/worker/file.go\0' : 'github.com/fetburner/rokuban/internal/worker\n'
        },
      })

      expect(commands).toEqual([
        { command: 'git', args: ['diff', '--name-only', '-z', '--no-renames', 'base-sha', 'HEAD'], input: undefined },
        { command: 'go', args: ['run', './scripts/citest', '-body-only', './internal/worker', './internal/streamer'], input: 'internal/worker/file.go\0' },
      ])
      expect(plan.scripts).toBe('worker')
      expect(plan.matrix).toEqual({ include: [{ shard: '1', count: '5', needs_go: true }] })
      expect(plan.planned_skip).toBe(false)
      expect(readFileSync(path.join(distDirectory, '.keep'), 'utf8')).toBe('')
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('影響する Go 依存が無い場合は空の計画を skip として書く', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-plan-empty-'))
    const distDirectory = path.join(directory, 'web', 'dist')
    const outputFile = path.join(directory, 'output')
    try {
      writeFileSync(path.join(directory, 'go.mjs'), 'go'.repeat(20))
      const plan = createCIPlan({
        full: false,
        baseSha: 'base-sha',
        outputFile,
        directory,
        distDirectory,
        scripts: [{ name: 'go.mjs', needs: [], goDeps: ['./internal/worker'] }],
        runCommand: (command) => command === 'git' ? 'docs/readme.md\0' : '',
      })

      expect(plan.matrix).toEqual({ include: [] })
      expect(plan.scripts).toBe('')
      expect(plan.planned_skip).toBe(true)
      expect(readFileSync(outputFile, 'utf8')).toBe('matrix={"include":[]}\nscripts=\nplanned_skip=true\n')
      expect(readFileSync(path.join(distDirectory, '.keep'), 'utf8')).toBe('')
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('Go 依存は import path の完全一致だけで選ぶ', () => {
    const directory = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-plan-exact-'))
    try {
      writeFileSync(path.join(directory, 'wildcard.mjs'), 'wildcard'.repeat(10))
      const plan = createCIPlan({
        full: false,
        baseSha: 'base-sha',
        directory,
        distDirectory: path.join(directory, 'dist'),
        scripts: [{ name: 'wildcard.mjs', needs: [], goDeps: ['./internal/worker/...'] }],
        runCommand: (command) => command === 'git'
          ? 'internal/worker/service/worker.go\0'
          : 'github.com/fetburner/rokuban/internal/worker/service\n',
      })

      expect(plan.matrix).toEqual({ include: [] })
      expect(plan.scripts).toBe('')
      expect(plan.planned_skip).toBe(true)
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })
})

describe('e2e/run.mjs の preview readiness', () => {
  it('pnpm を介さず Node から Vite CLI を起動できる', () => {
    const result = spawnSync(process.execPath, [resolveViteCLI(), '--version'], {
      cwd: process.cwd(),
      encoding: 'utf8',
    })
    expect(result.status).toBe(0)
    expect(result.stdout).toMatch(/^vite\/\d/)
  })

  it('preview が応答したら判定を始められる', async () => {
    const child = { exitCode: null, signalCode: null }
    const fetchImpl = async () => ({ ok: true })
    await expect(waitForPreview(child, 'http://localhost:40773/', () => undefined, fetchImpl)).resolves.toBeUndefined()
  })

  it('preview が起動直後に終了したら判定を始めない', async () => {
    const child = { exitCode: 1, signalCode: null }
    const fetchImpl = async () => { throw new Error('応答前に終了したため fetch しない') }
    await expect(waitForPreview(child, 'http://localhost:40773/', () => undefined, fetchImpl))
      .rejects.toThrow('Static SPA server exited with 1')
  })
})
