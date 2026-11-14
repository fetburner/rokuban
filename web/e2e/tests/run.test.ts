import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  discoverScripts,
  partitionScriptsBySourceBytes,
  selectCIScripts,
  summaryHeading,
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

      expect(selectCIScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])
      expect(discoverScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'shebang-needs.mjs',
        'webkit.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])

      const candidate = path.join(directory, 'with-finish.mjs')
      writeFileSync(candidate, '// e2e-needs: db-state\n' + "import { helper } from './shared.mjs'\nawait finish([])\n")
      expect(selectCIScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'without-finish.mjs',
      ])

      writeFileSync(candidate, "import { helper } from './shared.mjs'\nawait finish([])\n")
      expect(selectCIScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])
      expect(discoverScripts(directory).map(({ name }) => name)).toEqual([
        'body-declaration.mjs',
        'shebang-needs.mjs',
        'webkit.mjs',
        'with-finish.mjs',
        'without-finish.mjs',
      ])
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
