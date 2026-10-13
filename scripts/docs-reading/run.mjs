#!/usr/bin/env node

// docs の読解テスト: 節の「見出しと冒頭」だけを読んだ読み手が、その節の結論に
// 答えられるかを測る。段落の字数（textlint の gate）は代わりの指標にすぎず、段落を
// 切れば下がるが読めるようにはならない。こちらは「読み手が必要な節だけを 1 回で読める」
// を直接測る。
//
// 問い集: questions/*.json。対象 doc ごとに 1 ファイルに分け、本文タスクが並行して
// 問いを足しても衝突しないようにする。1 問は対象節の見出し・問い・正答の要点（1〜3 個）。
// 要点に識別子（列名・関数名・設定キー）を入れない。冒頭から識別子を抜いた正しい
// 書き直しが FAIL になる。
//
// 冒頭の範囲: 節の最初の段落（空行までのブロック）だけ。最大 LEAD_CHARS 字で、超える
// ときは最後の「。」で切る。
// - 2 段落目以降を足さないのは、1 文だけの段落を前置きにして結論を後ろへ送る書き方を
//   通さないため。段落を細かく切るほど通りやすくなる尺度では、字数の gate と同じ抜け道が残る
// - 上限は冒頭段落の目標（200 字以内）に余裕を持たせた値。長さで落とすのはこのテストの
//   役目ではない。リストや表で始まる節は最初の LEAD_CHARS 字だけが残るので、結論が
//   先頭項目に無ければ落ちる
// - 見出しは祖先の見出しも渡す（読み手は索引から見出しを辿って来る）。祖先の節の本文は渡さない
//
// 問いを置く節: 自身の本文（子の節・<details>・コードブロックを除く）が LEAD_CHARS 字を
// 超える節すべて。これ以下の節は冒頭がほぼ全文になり、このテストでは何も測れない。
// `--missing <doc...>` が、この基準を満たすのに問いが無い節を列挙する。本文タスクは
// その一覧に問いを足す。
//
// 回答器と採点器: `claude -p`。モデルは MODEL に固定し、ツールは無し、cwd は空の一時
// ディレクトリにして、設定と MCP も読ませない。回答器がリポジトリを grep できると、
// 冒頭が結論になっているかではなく探索力を測ってしまう。回答器には抜粋だけを根拠に
// 答えさせ、書かれていなければ「書かれていない」と答えさせる。採点器には抜粋を見せず、
// 回答と要点だけを突き合わせる。判定の根拠を回答に限るためである。
//
// 揺れ: 同じ問いを `--runs` 回（既定 3）回し、多数決で判定する（同数は FAIL）。2026-10-13 の
// main で種の問い 17 問を 3 回ずつ 2 巡したところ、3 回の結果が割れたのは 34 組中 1 組
// だった。多数決後の判定は 2 巡で全問一致した。要点を結論の文に絞っても割れは 0 に
// ならなかったので、要点の書き方だけでは抑えず多数決にする。要点に括弧書きで条件を
// 足すと、結論は読めているのにその条件だけで毎回落ちた（3 回とも FAIL なので多数決では
// 消えない）。要点は結論だけを書く。
//
// CI には入れない。`claude` の認証が要り、実行ごとに費用がかかり、結果が非決定的なので、
// 赤くなっても原因がコードの変更とは限らない。本文の書き直しの前後に手で回す。
//
// 使い方:
//   npm run docs:reading                          全問
//   npm run docs:reading -- scripts/docs-reading/questions/foo.json   指定した問い集だけ
//   npm run docs:reading -- --root <dir>          <dir>/docs/... を読む（一時コピーの検算用）
//   npm run docs:reading -- --runs 1              1 問あたりの実行回数
//   npm run docs:reading -- --missing docs/a.md   問いの足りない節を列挙（claude を呼ばない）

import { spawn } from 'node:child_process'
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

const MODEL = 'claude-sonnet-5-5'
const LEAD_CHARS = 300
const CONCURRENCY = 6

const scriptDir = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(scriptDir, '../..')
const questionsDir = join(scriptDir, 'questions')

const ANSWER_SYSTEM = `あなたは技術文書の読み手です。渡されるのは、ある設計文書の 1 節の見出しと冒頭の抜粋だけです。
問いに、抜粋に書かれていることだけを根拠に日本語で簡潔に答えてください。
抜粋に書かれていない事柄は、推測や一般知識で補わず「書かれていない」と答えてください。
問いの一部だけが書かれているときは、書かれている部分だけを答え、残りは「書かれていない」としてください。`

const GRADE_SYSTEM = `あなたは採点者です。問い・正答の要点（番号付き）・回答が渡されます。
要点ごとに、回答がその要点の内容を述べているかを判定してください。
言い回しや識別子の有無は問わず、意味が一致していれば満たすとします。
回答が「書かれていない」としている要点、要点と矛盾する内容、どの要点か特定できない曖昧な内容は満たさないとします。
すべての要点を満たすときだけ verdict を PASS、1 つでも欠ければ FAIL にし、missing に満たさない要点の番号を入れてください。`

const GRADE_SCHEMA = {
  type: 'object',
  properties: {
    verdict: { enum: ['PASS', 'FAIL'] },
    missing: { type: 'array', items: { type: 'integer' } },
  },
  required: ['verdict', 'missing'],
}

// parseSections は Markdown を見出しで節に割る。body は子の節を含まない自身の本文。
function parseSections(markdown) {
  const sections = []
  const stack = []
  let inFence = false
  for (const line of markdown.split('\n')) {
    if (/^\s*(```|~~~)/.test(line)) inFence = !inFence
    const m = !inFence && /^(#{1,6})\s+(.*?)\s*$/.exec(line)
    if (!m) {
      stack.at(-1)?.body.push(line)
      continue
    }
    const level = m[1].length
    while (stack.length && stack.at(-1).level >= level) stack.pop()
    const section = { level, title: m[2], path: [...stack.map((s) => s.title), m[2]], body: [] }
    stack.push(section)
    sections.push(section)
  }
  return sections
}

function lead(section) {
  const first = section.body.join('\n').trim().split(/\n\s*\n/)[0] ?? ''
  if (first.length <= LEAD_CHARS) return first
  const cut = first.slice(0, LEAD_CHARS)
  const end = cut.lastIndexOf('。')
  return `${end > 0 ? cut.slice(0, end + 1) : cut}（以下略）`
}

// proseLength は <details> とコードブロックを除いた本文の字数を返す。
function proseLength(section) {
  let inFence = false
  let inDetails = false
  let n = 0
  for (const line of section.body) {
    if (/^\s*(```|~~~)/.test(line)) {
      inFence = !inFence
      continue
    }
    if (/^\s*<details>/.test(line)) inDetails = true
    if (!inFence && !inDetails) n += line.trim().length
    if (/<\/details>\s*$/.test(line)) inDetails = false
  }
  return n
}

function findSection(sections, heading, file) {
  const hits = sections.filter((s) => s.title === heading)
  if (hits.length !== 1) throw new Error(`${file}: 見出し「${heading}」が ${hits.length} 件ある（1 件であること）`)
  return hits[0]
}

function claude(cwd, system, prompt, schema) {
  const args = [
    '-p',
    '--model', MODEL,
    '--tools', '',
    '--system-prompt', system,
    '--setting-sources', '',
    '--strict-mcp-config',
    '--no-session-persistence',
    '--output-format', 'json',
  ]
  if (schema) args.push('--json-schema', JSON.stringify(schema))
  return new Promise((resolvePromise, reject) => {
    const child = spawn('claude', args, { cwd, stdio: ['pipe', 'pipe', 'inherit'] })
    let out = ''
    child.stdout.on('data', (d) => (out += d))
    child.on('error', reject)
    child.on('close', (code) => {
      let res
      try {
        res = JSON.parse(out)
      } catch {
        return reject(new Error(`claude が JSON を返さなかった（終了コード ${code}）: ${out.slice(0, 500)}`))
      }
      if (code !== 0 || res.is_error) return reject(new Error(`claude が失敗した（終了コード ${code}）: ${res.result}`))
      if (schema && !res.structured_output) return reject(new Error(`claude が構造化出力を返さなかった: ${res.result}`))
      resolvePromise(schema ? res.structured_output : res.result)
    })
    child.stdin.end(prompt)
  })
}

async function pool(tasks, n) {
  const results = []
  let next = 0
  await Promise.all(
    Array.from({ length: n }, async () => {
      while (next < tasks.length) {
        const i = next++
        results[i] = await tasks[i]()
      }
    }),
  )
  return results
}

async function runOnce(cwd, q) {
  const excerpt = `見出し: ${q.section.path.join(' > ')}\n\n${lead(q.section)}`
  const answer = await claude(cwd, ANSWER_SYSTEM, `# 抜粋\n\n${excerpt}\n\n# 問い\n\n${q.question}`)
  const points = q.points.map((p, i) => `${i + 1}. ${p}`).join('\n')
  const grade = await claude(cwd, GRADE_SYSTEM, `# 問い\n\n${q.question}\n\n# 正答の要点\n\n${points}\n\n# 回答\n\n${answer}`, GRADE_SCHEMA)
  return { ...grade, answer }
}

function loadQuestions(files, root) {
  return files.flatMap((path) => {
    const set = JSON.parse(readFileSync(path, 'utf8'))
    const sections = parseSections(readFileSync(join(root, set.file), 'utf8'))
    return set.questions.map((q) => ({ ...q, file: set.file, section: findSection(sections, q.heading, set.file) }))
  })
}

function listMissing(docs) {
  const asked = new Set(
    readdirSync(questionsDir).flatMap((name) => {
      const set = JSON.parse(readFileSync(join(questionsDir, name), 'utf8'))
      return set.questions.map((q) => `${set.file}\0${q.heading}`)
    }),
  )
  let count = 0
  for (const doc of docs) {
    for (const s of parseSections(readFileSync(join(repoRoot, doc), 'utf8'))) {
      if (proseLength(s) > LEAD_CHARS && !asked.has(`${doc}\0${s.title}`)) {
        console.log(`${doc}\t${s.path.join(' > ')}\t${proseLength(s)} 字`)
        count++
      }
    }
  }
  console.log(`問いの無い節: ${count}`)
  return count === 0 ? 0 : 1
}

async function main() {
  const { values, positionals } = parseArgs({
    allowPositionals: true,
    options: {
      root: { type: 'string', default: repoRoot },
      runs: { type: 'string', default: '3' },
      missing: { type: 'boolean', default: false },
    },
  })
  if (values.missing) return listMissing(positionals)

  const runs = Number(values.runs)
  const files = positionals.length
    ? positionals.map((p) => resolve(p))
    : readdirSync(questionsDir).map((name) => join(questionsDir, name))
  const questions = loadQuestions(files, resolve(values.root))

  const cwd = mkdtempSync(join(tmpdir(), 'docs-reading-'))
  let results
  try {
    const tasks = questions.flatMap((q) => Array.from({ length: runs }, () => () => runOnce(cwd, q)))
    results = await pool(tasks, CONCURRENCY)
  } finally {
    rmSync(cwd, { recursive: true, force: true })
  }

  let pass = 0
  let split = 0
  questions.forEach((q, i) => {
    const rs = results.slice(i * runs, (i + 1) * runs)
    const passes = rs.filter((r) => r.verdict === 'PASS').length
    const ok = passes * 2 > runs
    if (ok) pass++
    if (passes !== 0 && passes !== runs) split++
    const marks = rs.map((r) => (r.verdict === 'PASS' ? 'P' : 'F')).join('')
    console.log(`${ok ? 'PASS' : 'FAIL'} [${marks}] ${q.file} § ${q.heading}`)
    const missing = [...new Set(rs.flatMap((r) => r.missing))].sort((a, b) => a - b)
    for (const m of missing) console.log(`    欠けた要点 ${m}: ${q.points[m - 1]}`)
    const failed = rs.find((r) => r.verdict === 'FAIL')
    if (failed) console.log(`    回答例: ${failed.answer.replace(/\s*\n\s*/g, ' ')}`)
  })
  console.log(`\nPASS ${pass} / ${questions.length}、実行ごとに割れた問い ${split} / ${questions.length}（${runs} 回実行）`)
  return pass === questions.length ? 0 : 1
}

process.exitCode = await main()
