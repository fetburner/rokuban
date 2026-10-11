import { spawnSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { collectSuppressions, validateSuppressions } from './check-lint-suppressions.mjs'

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

describe('lint suppression checker', () => {
  it('counts comma-separated directives and ignores string literals', () => {
    const root = mkdtempSync(path.join(os.tmpdir(), 'rokuban-suppression-scan-'))
    const srcDir = path.join(root, 'src')
    mkdirSync(srcDir)
    writeFileSync(path.join(srcDir, 'sample.tsx'), [
      'const example = "// oxlint-disable-next-line react/refs"',
      'const template = `/* oxlint-disable react/purity */`',
      '// oxlint-disable-next-line react/refs, react/purity -- test case',
      'function Example() { return null }',
      '/* oxlint-disable-line react/refs -- block comment */',
    ].join('\n'))

    try {
      expect(collectSuppressions(srcDir, root)).toEqual({
        'react/purity': { 'src/sample.tsx': 1 },
        'react/refs': { 'src/sample.tsx': 2 },
      })
    } finally {
      rmSync(root, { recursive: true, force: true })
    }
  })

  it('fails when a suppression is added without changing the ledger', () => {
    const ledger = { 'react/purity': { 'src/pages/programs.tsx': 1 } }
    const actual = { 'react/purity': { 'src/pages/programs.tsx': 2 } }

    expect(validateSuppressions(actual, ledger)).toContain(
      '  react/purity src/pages/programs.tsx: 台帳 1 件 / 実際 2 件',
    )
  })

  it('fails when a suppression is removed without changing the ledger', () => {
    const ledger = { 'react/set-state-in-effect': { 'src/pages/search.tsx': 3 } }
    const actual = { 'react/set-state-in-effect': { 'src/pages/search.tsx': 2 } }

    expect(validateSuppressions(actual, ledger)).toContain(
      '  react/set-state-in-effect src/pages/search.tsx: 台帳 3 件 / 実際 2 件',
    )
  })

  it('rejects a directive that disables every rule', () => {
    const allRules = { '*': { 'src/pages/example.tsx': 1 } }

    expect(validateSuppressions(allRules, allRules)).toContain(
      '  * src/pages/example.tsx: 台帳 1 件 / 実際 1 件。全規則の抑制は規則別に監査できないため、個別の規則名を明示してください',
    )
  })

  it('rejects a new react/purity file even when the ledger is updated', () => {
    const newFile = 'src/pages/new-page.tsx'
    const updated = { 'react/purity': { [newFile]: 1 } }
    const errors = validateSuppressions(updated, updated)

    expect(errors).toContain(
      `  react/purity ${newFile}: 台帳 1 件 / 実際 1 件 / 既存上限 0 件。新しいファイルで使う場合は REACT_PURITY_ALLOWLIST に理由を追加してください`,
    )
    const existingFile = 'src/pages/programs.tsx'
    const increasedExisting = { 'react/purity': { [existingFile]: 2 } }
    expect(validateSuppressions(increasedExisting, increasedExisting)).toContain(
      `  react/purity ${existingFile}: 台帳 2 件 / 実際 2 件 / 既存上限 1 件。新しいファイルで使う場合は REACT_PURITY_ALLOWLIST に理由を追加してください`,
    )
    expect(validateSuppressions(updated, updated, {
      purityBaseline: new Map(),
      purityAllowlist: new Map([[newFile, 'fixture reason']]),
    })).toEqual([])
  })

  it('makes pnpm lint fail on an unused disable directive', () => {
    const packageJSON = JSON.parse(readFileSync(path.join(webDir, 'package.json'), 'utf8'))
    const [binary, ...lintArgs] = packageJSON.scripts.lint.split(' && ')[0].trim().split(/\s+/)
    const fixtureDir = mkdtempSync(path.join(webDir, '.tmp-lint-suppressions-'))
    const fixturePath = path.join(fixtureDir, 'unused.tsx')
    writeFileSync(fixturePath, [
      '// oxlint-disable-next-line react/purity -- verifies unused directives fail lint',
      'export function Fixture() { return <div /> }',
    ].join('\n'))

    try {
      const result = spawnSync(path.join(webDir, 'node_modules', '.bin', binary), [...lintArgs, fixturePath], {
        cwd: webDir,
        encoding: 'utf8',
      })
      expect(result.error).toBeUndefined()
      expect(result.status).not.toBe(0)
      expect(`${result.stdout}\n${result.stderr}`).toMatch(/unused|disable directive/i)
    } finally {
      rmSync(fixtureDir, { recursive: true, force: true })
    }
  })
})
