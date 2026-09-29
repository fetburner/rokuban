// シリーズ棚の実サーバー判定: 「棚の画面で分類ルールを 1 本作ると、再評価の後に棚が割れる」。
//
// **`/api/**` を差し替えない。** 他の e2e は page.route で API を丸ごと偽物にするが、
// この判定が確かめるのは API → 全件再評価ジョブ（worker）→ SSE / 再取得 → 棚の再描画
// という**非同期の連鎖の実物**なので、実バイナリ（api + worker）と実 DB を使う。
// 分類ルールを作った直後の応答は古い棚を返す（ジョブが走る前）ので、割れるまで
// ポーリングする。読み込み前に通る空虚な成功を避けるため、作成前に「1 棚 12 件」を
// 見てから始め、作成後に「棚が 2 つに割れた」ことを両側で見る。
//
// 併せて、値に空白を含むと棚キーが最初の空白で切れて**割れない**こと（`NHK高校講座
// 化学` は `NHK高校講座` のままになる）をフォームの注記で見る。
//
//   cd web && corepack pnpm build
//   cd .. && go build -o /tmp/rokuban ./cmd/rokuban   # go:embed なので web を変えたら作り直す
//   /tmp/rokuban migrate up --config e2e.yml
//   /tmp/rokuban server --roles api,worker,notifier --config e2e.yml
//     # SSE（棚の再取得の契機）は notifier ロールが配る。worker.queues: [cleanup] で足りる
//     # 起動時に media_assets の rel_path 名前空間を検査するので、先に seed が要る（下の seed の形）
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

/** シリーズ棚の母集団（再生できる録画）を作る。数学 6 件 + 化学 6 件、どれも自動キーは NHK高校講座。 */
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

/** shelfLines は棚の副見出し行（`{値} · {件数} 件`）を全部返す。 */
async function shelfLines(page) {
  return (await page.locator('ul > li span.text-xs').allTextContents()).filter((t) => / · \d+ 件$/.test(t))
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
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })

await page.goto(`${URL_BASE}/shelves`)

// 作成前: 1 棚 12 件（読み込み完了を待つ。空の画面に対する否定で通らないようにする）。
const before = await pollUntil('作成前の棚', async () => {
  const lines = await shelfLines(page)
  return { ok: lines.length === 1 && lines[0] === 'NHK高校講座 · 12 件', value: lines }
})
log('  作成前の棚:', JSON.stringify(before?.value))

// 2 枚目のページ（同じ /shelves。以後は一切操作しない）。作成したタブ自身は作成成功時の
// invalidate で更新されるので、SSE を経由した保証にならない。操作しないタブが割れるのは
// recordings トピックの SSE（notifier）か 60 秒周期の取得だけで、待ち時間は 60 秒より短い。
const page2 = await browser.newPage({ viewport: { width: 1280, height: 900 } })
await page2.goto(`${URL_BASE}/shelves`)
await pollUntil('2 枚目の作成前の棚', async () => {
  const lines = await shelfLines(page2)
  return { ok: lines.length === 1 && lines[0] === 'NHK高校講座 · 12 件', value: lines }
})

await page.getByRole('button', { name: '分類ルールを作成' }).click()
const dialog = page.getByRole('dialog')
await dialog.getByLabel('キーワード').fill('化学')

// 値に空白を入れると、棚キーは最初の空白で切れる（= 割れない）。実サーバーの series_key が答える。
await dialog.getByLabel('棚のキー').fill('NHK高校講座 化学')
const note = await pollUntil('食い違いの注記', async () => {
  const text = await dialog.getByRole('status').allTextContents()
  return { ok: text.some((t) => t.includes('この値は棚キー NHK高校講座 として扱われます')), value: text }
})
log('  空白入りの値の注記:', JSON.stringify(note?.value))

// 空白の無い値へ直して作成する。注記は消える。
await dialog.getByLabel('棚のキー').fill('化学')
await pollUntil('注記の消滅', async () => {
  const count = await dialog.getByRole('status').count()
  return { ok: count === 0, value: count }
})
await dialog.getByRole('button', { name: '作成' }).click()

// 再評価（worker）の後に棚が割れる。作成の直後は古い棚のまま。
// 2 枚目（操作していない）も同時に待つ。直列に待つと、1 枚目の待ちの間に 60 秒周期の
// 定期取得が届いて、SSE を経由しなくても 2 枚目が通ってしまう。
const isSplit = async (pg) => {
  const lines = await shelfLines(pg)
  const sorted = [...lines].sort()
  return {
    ok: sorted.length === 2 && sorted[0] === 'NHK高校講座 · 6 件' && sorted[1] === '化学 · 6 件',
    value: lines,
  }
}
const [after, after2] = await Promise.all([
  pollUntil('棚が割れる', () => isSplit(page)),
  pollUntil('2 枚目（SSE 経由）の棚が割れる', () => isSplit(page2)),
])
log('  作成後の棚:', JSON.stringify(after?.value))
log('  2 枚目の棚:', JSON.stringify(after2?.value))

await finish(ng, browser)
