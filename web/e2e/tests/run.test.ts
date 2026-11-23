import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  createShardMatrix,
  discoverScripts,
  partitionScriptsBySourceBytes,
  selectCIScripts,
  selectScriptsByName,
  summaryHeading,
  validateGoDepsDeclarations,
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
        ['home-resume-frame.mjs', ['./internal/worker']],
        ['recording-playback-timeline.mjs', ['./internal/worker']],
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
