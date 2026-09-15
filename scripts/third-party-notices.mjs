#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { createRequire } from 'node:module'
import {
  existsSync,
  readFileSync,
  readdirSync,
  realpathSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const rootDir = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const webDir = join(rootDir, 'web')
const outputPaths = [
  join(rootDir, 'THIRD_PARTY_NOTICES'),
  join(webDir, 'public', 'THIRD_PARTY_NOTICES'),
]
const goModulePath = readGoModulePath()
// 公式 OCI イメージは linux/amd64 と linux/arm64 を配布する。ホスト OS の違いで
// notice が揺れないよう、両ターゲットの依存を union して棚卸しする。
const goTargets = [
  { GOOS: 'linux', GOARCH: 'amd64' },
  { GOOS: 'linux', GOARCH: 'arm64' },
]

const noticeFilePattern = /^(?:license|copying|notice|patents?)(?:[._-].*)?$/i
const licenseFilePattern = /^(?:license|copying)(?:[._-].*)?$/i

function readGoModulePath() {
  const goMod = readFileSync(join(rootDir, 'go.mod'), 'utf8')
  const match = /^module\s+(\S+)/m.exec(goMod)
  if (!match) throw new Error('go.mod に module 宣言がありません')
  return match[1]
}

function readJSON(path) {
  return JSON.parse(readFileSync(path, 'utf8'))
}

function run(command, args, cwd = rootDir, env = {}) {
  try {
    return execFileSync(command, args, {
      cwd,
      encoding: 'utf8',
      env: { ...process.env, ...env },
      maxBuffer: 64 * 1024 * 1024,
    })
  } catch (error) {
    const detail = error.stderr?.toString().trim() || error.message
    throw new Error(`${command} ${args.join(' ')} に失敗しました:\n${detail}`)
  }
}

function parseJSONStream(input) {
  const values = []
  let cursor = 0

  while (cursor < input.length) {
    while (/\s/.test(input[cursor] ?? '')) cursor += 1
    if (cursor === input.length) break
    if (input[cursor] !== '{') {
      throw new Error(`go list の JSON を解釈できません (offset ${cursor})`)
    }

    const start = cursor
    let depth = 0
    let inString = false
    let escaped = false

    for (; cursor < input.length; cursor += 1) {
      const character = input[cursor]
      if (inString) {
        if (escaped) escaped = false
        else if (character === '\\') escaped = true
        else if (character === '"') inString = false
        continue
      }
      if (character === '"') {
        inString = true
        continue
      }
      if (character === '{') depth += 1
      if (character === '}') {
        depth -= 1
        if (depth === 0) {
          cursor += 1
          values.push(JSON.parse(input.slice(start, cursor)))
          break
        }
      }
    }

    if (depth !== 0) throw new Error('go list の JSON が途中で終わっています')
  }

  return values
}

function runGo(args, target = goTargets[0]) {
  return run('go', args, rootDir, { CGO_ENABLED: '0', ...target })
}

function findNoticeFiles(directory) {
  return readdirSync(directory)
    .filter((name) => noticeFilePattern.test(name))
    .map((name) => join(directory, name))
    .filter((path) => statSync(path).isFile())
    .sort()
}

function findFileUpward(directory, name) {
  let current = directory
  for (let i = 0; i < 8; i += 1) {
    const candidate = join(current, name)
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate
    const parent = dirname(current)
    if (parent === current) break
    current = parent
  }
  return undefined
}

function parseGoPackages(target) {
  return parseJSONStream(runGo(['list', '-deps', '-json', './cmd/rokuban'], target))
}

function collectGoComponents() {
  const modules = new Map()
  for (const target of goTargets) {
    for (const packageInfo of parseGoPackages(target)) {
      const module = packageInfo.Module
      if (!module || module.Path === goModulePath) continue
      if (!module.Version || !module.Dir) {
        throw new Error(`Go モジュールのバージョンまたはソースディレクトリがありません: ${module.Path}`)
      }
      modules.set(`${module.Path}@${module.Version}`, module)
    }
  }

  const goEnvironment = runGo(['env', 'GOVERSION', 'GOROOT']).trim().split(/\r?\n/)
  const [goVersion, goRoot] = goEnvironment
  if (!goVersion || !goRoot) throw new Error('Go のバージョンまたは GOROOT を取得できません')

  const standardLicense = findFileUpward(goRoot, 'LICENSE')
  const standardPatents = findFileUpward(goRoot, 'PATENTS')
  if (!standardLicense) {
    throw new Error(`Go 標準ライブラリの LICENSE が見つかりません: ${goRoot}`)
  }

  const standardFiles = [standardLicense, standardPatents].filter(Boolean)
  return [
    {
      kind: 'go',
      name: 'Go standard library',
      version: goVersion,
      role: 'CGO_ENABLED=0 でビルドする公式 Go バイナリ',
      source: `https://go.dev/dl/${goVersion}.src.tar.gz`,
      files: standardFiles,
    },
    ...[...modules.values()]
      .sort((a, b) => a.Path.localeCompare(b.Path))
      .map((module) => ({
        kind: 'go',
        name: module.Path,
        version: module.Version,
        role: 'CGO_ENABLED=0 でビルドする公式 Go バイナリ',
        source: goModuleSourceURL(module.Path, module.Version),
        files: findNoticeFiles(module.Dir),
      })),
  ]
}

function resolvePackageJSON(name, fromDirectory) {
  const requireFromPackage = createRequire(join(fromDirectory, 'package.json'))
  try {
    return requireFromPackage.resolve(`${name}/package.json`)
  } catch {
    // Some packages (for example tw-animate-css) expose only a style export and
    // therefore do not allow package.json through the exports map. Walk the
    // node_modules links to find the package root in that case.
    let current = fromDirectory
    for (let i = 0; i < 12; i += 1) {
      const candidate = join(current, 'node_modules', name)
      if (existsSync(candidate)) {
        const packageDirectory = realpathSync(candidate)
        const packageJSON = join(packageDirectory, 'package.json')
        if (existsSync(packageJSON)) return packageJSON
      }
      const parent = dirname(current)
      if (parent === current) break
      current = parent
    }
  }
  throw new Error(`npm パッケージを解決できません: ${name} (from ${relative(rootDir, fromDirectory)})`)
}

function collectWebComponents() {
  const packageJSONPath = join(webDir, 'package.json')
  const rootPackage = readJSON(packageJSONPath)
  const components = new Map()

  function visit(name, fromDirectory) {
    const packagePath = resolvePackageJSON(name, fromDirectory)
    const packageDirectory = dirname(packagePath)
    const packageInfo = readJSON(packagePath)
    if (!packageInfo.name || !packageInfo.version) {
      throw new Error(`npm パッケージの name または version がありません: ${packagePath}`)
    }

    const key = `${packageInfo.name}@${packageInfo.version}`
    if (components.has(key)) return

    const files = findNoticeFiles(packageDirectory)
    if (files.length === 0) {
      throw new Error(`npm パッケージにライセンス情報ファイルがありません: ${key}`)
    }

    const component = {
      kind: 'npm',
      name: packageInfo.name,
      version: packageInfo.version,
      role: 'web/dist に組み込む本番 Web 依存',
      source: npmPackageSourceURL(packageInfo.name, packageInfo.version),
      upstream: repositoryURL(packageInfo.repository) || packageInfo.homepage,
      licenseMetadata: packageInfo.license || packageInfo.licenses,
      author: packageInfo.author,
      files,
    }
    components.set(key, component)

    const dependencies = {
      ...packageInfo.dependencies,
      ...packageInfo.optionalDependencies,
    }
    for (const dependencyName of Object.keys(dependencies).sort()) {
      const optional = Boolean(packageInfo.optionalDependencies?.[dependencyName])
      try {
        visit(dependencyName, packageDirectory)
      } catch (error) {
        if (optional && error.message.includes('解決できません')) continue
        throw error
      }
    }
  }

  for (const name of Object.keys(rootPackage.dependencies || {}).sort()) visit(name, webDir)
  return [...components.values()].sort((a, b) => a.name.localeCompare(b.name))
}

function repositoryURL(repository) {
  if (!repository) return undefined
  const value = typeof repository === 'string' ? repository : repository.url
  if (!value) return undefined
  if (/^[\w.-]+\/[\w.-]+$/.test(value)) return `https://github.com/${value}`
  return value.replace(/^git\+/, '').replace(/\.git$/, '')
}

function npmPackageSourceURL(name, version) {
  const packageName = name.startsWith('@') ? name.split('/')[1] : name
  return `https://registry.npmjs.org/${name}/-/${packageName}-${encodeURIComponent(version)}.tgz`
}

function goModuleSourceURL(path, version) {
  const encodedPath = path
    .split('/')
    .map((segment) => segment.replace(/[A-Z]/g, (character) => `!${character.toLowerCase()}`))
    .join('/')
  return `https://proxy.golang.org/${encodedPath}/@v/${encodeURIComponent(version)}.zip`
}

function normalizeText(text) {
  return text.replace(/\r\n/g, '\n').replace(/\r/g, '\n').trimEnd()
}

function detectLicenseIdentifier(text) {
  if (/mozilla public license version 2\.0/i.test(text)) return 'MPL-2.0'
  if (/apache license[\s\S]{0,100}version 2\.0/i.test(text)) return 'Apache-2.0'
  if (/sil open font license[\s\S]{0,100}version 1\.1/i.test(text)) return 'OFL-1.1'
  if (/isc license/i.test(text)) return 'ISC'
  if (/unlicense/i.test(text)) return 'Unlicense'
  if (/\b(?:the )?mit license\b/i.test(text)) return 'MIT'
  if (
    /permission is hereby granted[\s\S]{0,700}(?:the )?["']?software/i.test(text) &&
    /the software is provided/i.test(text)
  ) {
    return 'MIT'
  }
  if (
    /redistribution and use in source and binary forms/i.test(text) &&
    /neither the name/i.test(text)
  ) {
    return 'BSD-3-Clause'
  }
  if (/redistribution and use in source and binary forms/i.test(text)) return 'BSD-2-Clause'
  return undefined
}

function licenseFiles(component) {
  return component.files.map((path) => ({
    name: path.split('/').pop(),
    text: normalizeText(readFileSync(path, 'utf8')),
  }))
}

function licenseIdentifier(component, files) {
  if (component.kind === 'npm') {
    const metadata = component.licenseMetadata
    if (typeof metadata === 'string' && metadata.trim()) return metadata.trim()
    if (metadata && typeof metadata.type === 'string') return metadata.type
    if (Array.isArray(metadata)) {
      const types = metadata.map((item) => (typeof item === 'string' ? item : item.type)).filter(Boolean)
      if (types.length) return types.join(' AND ')
    }
  }

  const identifiers = new Set(
    files
      .filter((file) => licenseFilePattern.test(file.name))
      .map((file) => detectLicenseIdentifier(file.text))
      .filter(Boolean),
  )
  if (identifiers.size === 0) {
    throw new Error(`ライセンス種別を判定できません: ${component.name}@${component.version}`)
  }
  return [...identifiers].sort().join(' AND ')
}

function copyrightSummary(component, files) {
  const lines = files
    .flatMap((file) => file.text.split('\n'))
    .map((line) => line.trim())
    .filter((line) => {
      const isCopyrightLine =
        /^(?:copyright\b|original work copyright\b|modified work copyright\b)/i.test(line) ||
        /^[^:]{1,100}\bcopyright\s+\(c\)/i.test(line)
      const isTemplateOrDefinition =
        /\[(?:yyyy|name of copyright owner)\]/i.test(line) ||
        /^copyright (?:owner|license|notice|holder|able|statement)/i.test(line)
      return isCopyrightLine && !isTemplateOrDefinition
    })

  const uniqueLines = [...new Set(lines)]
  if (uniqueLines.length) return uniqueLines.join(' / ')

  const firstLine = files.find((file) => licenseFilePattern.test(file.name))?.text.split('\n')[0].trim()
  if (firstLine && !/license|mozilla|apache|sil open font/i.test(firstLine) && firstLine.length <= 100) {
    return firstLine
  }
  if (component.author) {
    const author = typeof component.author === 'string' ? component.author : component.author.name
    if (author) return `${author} (package metadata)`
  }
  return 'Included license/notice files below'
}

function renderFile(file) {
  let kind = 'license text'
  if (/^notice(?:[._-]|$)/i.test(file.name)) kind = 'additional notice'
  if (/^patents?(?:[._-]|$)/i.test(file.name)) kind = 'additional patent grant'
  return [`----- ${file.name} (${kind}) -----`, file.text, ''].join('\n')
}

function renderComponent(component) {
  const files = licenseFiles(component)
  const lines = [
    `Component: ${component.name}`,
    `Version: ${component.version}`,
    `Included in: ${component.role}`,
    `Copyright: ${copyrightSummary(component, files)}`,
    `License: ${licenseIdentifier(component, files)}`,
    `Source: ${component.source}`,
  ]
  if (component.upstream) lines.push(`Upstream: ${component.upstream}`)
  lines.push('', ...files.map(renderFile).join('').split('\n'), '')
  return lines.join('\n')
}

function buildNotice() {
  const header = [
    'THIRD-PARTY NOTICES',
    '====================',
    '',
    'This file is generated by `npm run third-party-notices`.',
    'It covers the Go standard library and external modules used by the',
    'production Go binary, plus the production dependency closure used to build',
    'web/dist. The complete license and notice files from each component are',
    'included below. Source archives for the exact versions are listed per entry.',
    '',
    "Rokuban's own source code is covered by LICENSE.",
    'ffmpeg and Debian base-image packages are outside this file; their own',
    'distribution notices remain in the image supplied by their distributors.',
    '',
  ].join('\n')

  const components = [...collectGoComponents(), ...collectWebComponents()]
  return `${header}${components.map(renderComponent).join('\n')}`.replace(/\n+$/, '\n')
}

function main() {
  const args = process.argv.slice(2)
  const check = args.length === 1 && args[0] === '--check'
  if (args.length > 1 || (args.length === 1 && !check)) {
    throw new Error('使い方: npm run third-party-notices [-- --check]')
  }

  const expected = buildNotice()
  const stale = outputPaths.filter((path) => !existsSync(path) || readFileSync(path, 'utf8') !== expected)
  if (check) {
    if (stale.length) {
      for (const path of stale) console.error(`${relative(rootDir, path)} が古いか存在しません`)
      console.error('npm run third-party-notices を実行して生成物を更新してください')
      process.exitCode = 1
    }
    return
  }

  for (const path of outputPaths) writeFileSync(path, expected)
}

try {
  main()
} catch (error) {
  console.error(error instanceof Error ? error.message : error)
  process.exitCode = 1
}
