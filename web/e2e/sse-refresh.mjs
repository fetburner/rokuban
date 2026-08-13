// SSE が 1 通も届かないときの定期 REST 再取得の受け入れ判定。
//
// jsdom の単体テスト（src/lib/events.test.tsx）はフック単体しか通らないので、
// 「画面が実際に使っているクエリキー」を外している取りこぼしを検出できない
// （`epg` トピックが番組リストに一度も届いていなかった実例は docs/api/sse.md
// 「経緯と失敗事例」）。ここでは実ブラウザに**ビルド済みの bundle** を読ませ、
// `/api/**` をスタブして**リクエスト数を数える**。
//
// 合格なら exit 0、1 つでも NG なら exit 1。
//
// 使い方（Go サーバーも Postgres も要らない。API は全部スタブする）:
//
//   cd web && pnpm build && pnpm exec vite preview --port 4173 --strictPort &
//   node e2e/sse-refresh.mjs
//
// SSE の再接続時 invalidate はここでは見ない（実ブラウザで切断を決定的に
// 起こす手段が無い）。単体テスト「再接続したら切断中の変更を全グループ取り直す」の担当。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
/** 運用状態の周期（src/lib/events.ts の operationalRefreshIntervalMs と同じ値をリテラルで書く）。 */
const operationalMs = 60_000
/** EPG の周期（同 epgRefreshIntervalMs）。 */
const epgMs = 600_000

/** 現在測っているページのリクエスト数。ページを変えるたびに差し替える。 */
let counts = new Map()
const ng = []
const log = (...a) => console.log(...a)
const count = (path) => counts.get(path) ?? 0
/** check は期待値と実測を突き合わせる。落ちても続行して全部の NG を出す。 */
const check = (label, actual, expected) => {
  if (actual === expected) log(`OK  ${label}: ${actual}`)
  else {
    log(`NG  ${label}: ${actual}（期待 ${expected}）`)
    ng.push(label)
  }
}

// ⓪ 配っている bundle が自分の dist かを先に確かめる（badge-links.mjs と同じ理由。
// 別 worktree の preview が同じポートに居座っていると、無関係な古いビルドを
// 測ったまま判定が進む）。
const rootHtml = await fetch(BASE + '/').then((r) => r.text())
const served = /assets\/(index-[^"]+\.js)/.exec(rootHtml)?.[1]
let local
try {
  local = readdirSync(path.join(process.cwd(), 'dist', 'assets')).find((f) =>
    /^index-.*\.js$/.test(f),
  )
} catch {
  local = undefined
}
if (served === undefined || served !== local) {
  log(`NG  ⓪ 配っている bundle（${served ?? '不明'}）が dist/assets/（${local ?? '不明'}）と違う`)
  log('    別プロセス・古いビルドを測っている。以降の判定に意味が無いので打ち切る')
  process.exit(1)
}
log(`OK  ⓪ 配っている bundle は自分の dist（${served}）`)

const reservation = {
  id: 111,
  site: 'tokyo',
  programId: 300000,
  source: 'manual',
  state: 'active',
  title: 'e2e 予約',
  startAt: '2026-08-14T00:00:00.000Z',
  durationMs: 1_800_000,
  createdAt: '2026-08-13T00:00:00.000Z',
  updatedAt: '2026-08-13T00:00:00.000Z',
  skip: false,
}

const browser = await chromium.launch()

/**
 * openStubbed は `/api/**` を丸ごと差し替えたページを開く。カウンタ（`counts`）は
 * ページごとに新しくするので、増分はそのページの回復経路だけを表す。
 */
async function openStubbed(pathname, label) {
  counts = new Map()
  const p = await browser.newPage({ viewport: { width: 1280, height: 800 } })
  p.on('pageerror', (e) => {
    log(`NG  ページ例外（${label}）:`, e.message)
    ng.push(`pageerror（${label}）`)
  })
  await p.route('**/api/**', async (route) => {
    const requested = new URL(route.request().url()).pathname
    counts.set(requested, count(requested) + 1)
    if (requested === '/api/events') {
      // 接続は張るが通知は 1 通も送らない（notifier がバッファ満杯で捨てた状態）。
      // retry を 1 日にして、時計を進めても再接続 invalidate が混ざらないようにする
      await route.fulfill({
        status: 200,
        headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
        body: 'retry: 86400000\n\n: ping\n\n',
      })
      return
    }
    const body =
      requested === '/api/capabilities'
        ? '{"encode":false,"live":false,"storage":false}'
        : requested === '/api/version'
          ? '{"version":"e2e"}'
          : requested === '/api/sites'
            ? '["tokyo"]'
            : /\/reservation$/.test(requested)
              ? JSON.stringify(reservation)
              : /\/overlaps$/.test(requested)
                ? '{"count":0,"reservations":[]}'
                : '[]'
    await route.fulfill({ status: 200, headers: { 'content-type': 'application/json' }, body })
  })
  // 時計を握ってから開く。10 分を実時間で待たない
  await p.clock.install()
  await p.goto(BASE + pathname, { waitUntil: 'networkidle' })
  await p.waitForTimeout(500)
  return p
}

// 番組リスト（EPG）と予約・ブレーカー（運用状態）が同じ画面から出る
const page = await openStubbed('/programs', '番組表')

const programs = '/api/sites/tokyo/programs'
const services = '/api/sites/tokyo/services'
log('初回ロード後:', Object.fromEntries([...counts.entries()].sort()))
check('初回: 予約', count('/api/reservations'), 1)
check('初回: 番組リスト', count(programs), 1)
if (ng.length > 0) {
  log('初回ロードで既に期待とずれている。以降の増分は判定できない')
  await browser.close()
  process.exit(1)
}

// 運用状態は 60 秒で取り直す。EPG はまだ動かない
await page.clock.runFor(operationalMs)
await page.waitForTimeout(500)
check('60 秒後: 予約', count('/api/reservations'), 2)
check('60 秒後: ブレーカー', count('/api/breakers'), 2)
check('60 秒後: 番組リスト（まだ増えない）', count(programs), 1)

// 10 分でちょうど EPG の 1 周。運用状態はこの間に 10 回
await page.clock.runFor(epgMs - operationalMs)
await page.waitForTimeout(500)
check('10 分後: 番組リスト', count(programs), 2)
check('10 分後: サービス一覧', count(services), 2)
check('10 分後: 予約', count('/api/reservations'), 1 + epgMs / operationalMs)

// 予約詳細（別ページ）。URL は /api/sites/... なので、生成キーのままだと
// EPG グループ（10 分）に落ちる --- ここで見るのは「60 秒側で取り直すこと」。
// キーの先頭要素が所属を決めるという規律（docs/api/sse.md）を、画面が実際に
// 使っているキーの上で確かめる唯一の判定。
log('\n=== 予約詳細（/reservations/$site/$programId）===')
const detailPage = await openStubbed('/reservations/tokyo/300000', '予約詳細')

const detail = '/api/sites/tokyo/programs/300000/reservation'
log('初回ロード後:', Object.fromEntries([...counts.entries()].sort()))
check('初回: 予約詳細', count(detail), 1)
await detailPage.clock.runFor(operationalMs)
await detailPage.waitForTimeout(500)
check('60 秒後: 予約詳細（運用状態グループ）', count(detail), 2)

await browser.close()
if (ng.length > 0) {
  log(`\nNG ${ng.length} 件: ${ng.join(' / ')}`)
  process.exit(1)
}
log('\nすべて OK')
