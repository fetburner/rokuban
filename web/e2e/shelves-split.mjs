// シリーズ一覧の実サーバー判定: 「/rules で分類ルールを 1 本作ると、/series の棚が
// 再評価の後に割れる」。
//
// **`/api/**` を差し替えない。** 他の e2e は page.route で API を丸ごと偽物にするが、
// この判定が確かめるのは API → 全件再評価ジョブ（worker）→ SSE / 再取得 → シリーズの
// 再描画という**非同期の連鎖の実物**なので、実バイナリ（api + worker）と実 DB を使う。
// 分類ルールを作った直後の応答は古い棚を返す（ジョブが走る前）ので、割れるまで
// ポーリングする。読み込み前に通る空虚な成功を避けるため、作成前に「1 棚 12 件」を
// 見てから始め、作成後に「棚が 2 つに割れた」ことを観測側のページで見る。
//
// 併せて、値に空白を含めると棚キーが最初の空白で切れて**割れない**こと
// （`NHK高校講座 化学` は `NHK高校講座` のままになる）をフォームの注記で見る。
//
//   cd web && corepack pnpm build
//   cd .. && go build -o /tmp/rokuban ./cmd/rokuban   # go:embed なので web を変えたら作り直す
//   /tmp/rokuban migrate up --config e2e.yml
//   /tmp/rokuban server --roles api,worker,notifier --config e2e.yml
//   cd web && E2E_URL=http://localhost:40799 \
//     E2E_DATABASE_URL='postgres://localhost:5432/<サーバーと同じ DB>?sslmode=disable' \
//     corepack pnpm e2e:shelves-split
//
// **E2E_DATABASE_URL の DB は判定が TRUNCATE する。** 開発用 DB を指さないこと。
import { execFileSync } from 'node:child_process'

import { finish, launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const DATABASE_URL = process.env.E2E_DATABASE_URL
if (DATABASE_URL === undefined) {
  console.error('E2E_DATABASE_URL が未設定（サーバーと同じ DB。判定が TRUNCATE する）')
  process.exit(2)
}
const ng = []

/** シリーズ一覧の母集団（数学 6 件 + 化学 6 件）を作る。自動キーは全件 NHK高校講座。 */
function seed() {
  const sql = `
TRUNCATE recordings, label_rules RESTART IDENTITY CASCADE;
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
  title, program_start_at, program_duration_ms, status)
SELECT 'manual', 'default', 32736, 1024, i, 'NHK Eテレ', 'GR', '13',
  CASE WHEN i <= 6 THEN 'NHK高校講座　数学I　第' || i || '回'
       ELSE 'NHK高校講座　化学　第' || (i - 6) || '回' END,
  now() - (i || ' hours')::interval, 1800000, 'finished'
FROM generate_series(1, 12) i;
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
SELECT id, 'original', 'sites/default/e2e/' || id, 1, 'active' FROM recordings;`
  execFileSync('psql', [DATABASE_URL, '-v', 'ON_ERROR_STOP=1', '-c', sql], { stdio: 'pipe' })
}

/** shelfState はシリーズ一覧の値と総件数を全部返す。 */
async function shelfState(page) {
  return page.locator('[data-testid="series-shelf"]').evaluateAll((rows) =>
    rows.map((row) => ({
      value: row.getAttribute('data-series-value'),
      count: Number(row.getAttribute('data-count')),
    })),
  )
}

async function pollUntil(label, fn, timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs
  let last
  while (Date.now() < deadline) {
    last = await fn()
    if (last.ok) return last
    await new Promise((r) => setTimeout(r, 250))
  }
  ng.push(`${label}: ${timeoutMs}ms 待っても成立しなかった（最後の値: ${JSON.stringify(last?.value)}）`)
  return last
}

seed()
await verifyBundleMatchesOrExit(URL_BASE, ng)
const browser = await launchBrowser()
const rulesPage = await browser.newPage({ viewport: { width: 1280, height: 900 } })
const seriesPage = await browser.newPage({ viewport: { width: 1280, height: 900 } })

await rulesPage.goto(`${URL_BASE}/rules`)
await seriesPage.goto(`${URL_BASE}/series`)

const before = await pollUntil('作成前のシリーズ', async () => {
  const rows = await shelfState(seriesPage)
  return {
    ok: rows.length === 1 && rows[0].value === 'NHK高校講座' && rows[0].count === 12,
    value: rows,
  }
})
log('  作成前のシリーズ:', JSON.stringify(before?.value))

await rulesPage.getByRole('button', { name: '分類ルールを作成' }).click()
const dialog = rulesPage.getByRole('dialog')
await dialog.getByLabel('キーワード').fill('化学')

// 値に空白を入れると、棚キーは最初の空白で切れる（= 割れない）。
await dialog.getByLabel('棚のキー').fill('NHK高校講座 化学')
const note = await pollUntil('食い違いの注記', async () => {
  const text = await dialog.getByRole('status').allTextContents()
  return {
    ok: text.some((t) => t.includes('この値は棚キー NHK高校講座 として扱われます')),
    value: text,
  }
})
log('  空白入りの値の注記:', JSON.stringify(note?.value))

await dialog.getByLabel('棚のキー').fill('化学')
await pollUntil('注記の消滅', async () => {
  const count = await dialog.getByRole('status').count()
  return { ok: count === 0, value: count }
})
await dialog.getByRole('button', { name: '作成' }).click()

const isSplit = async () => {
  const rows = await shelfState(seriesPage)
  const sorted = [...rows].sort((a, b) => a.value.localeCompare(b.value))
  return {
    ok:
      sorted.length === 2 &&
      sorted[0].value === 'NHK高校講座' &&
      sorted[0].count === 6 &&
      sorted[1].value === '化学' &&
      sorted[1].count === 6,
    value: rows,
  }
}
const after = await pollUntil('シリーズが割れる', isSplit)
log('  作成後のシリーズ:', JSON.stringify(after?.value))

await finish(ng, browser)
