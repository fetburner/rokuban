// 発動バナーから置き場の範囲のブレーカーを指定して再開できることを実ブラウザで確認する。
// API はブラウザ側で stub するが、表示・確認ダイアログ・実際に送る URL はビルド済み bundle で測る。
import { ListCircuitBreakersResponseItem } from '../src/api/zod.ts'
import {
  finish,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const STORE = 's3main'
const NAME = 'delete_reconcile'
const expectedPath = `/api/stores/${STORE}/breakers/${NAME}/resume`
const ng = []
const makeBreaker = (store) => ({
  site: '',
  store,
  name: NAME,
  trippedAt: new Date(Date.now() - 3_600_000).toISOString(),
  pending: 3,
  threshold: 20,
  detail: { total: 3, programs: [] },
})
// FS と S3 が同名で同時に発動している状態。再開先は s3main の行だけ。
const breakers = [makeBreaker('test-store'), makeBreaker(STORE)]

await validateFixturesOrExit(
  breakers.map((b) => [`store breaker ${b.store}`, ListCircuitBreakersResponseItem, b]),
  ng,
)
await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } })
page.on('pageerror', (error) => {
  log(`NG  page error: ${error.message}`)
  ng.push('page error')
})

let tripped = true
const resumePosts = []
await page.route('**/api/**', async (route) => {
  const url = new URL(route.request().url())
  if (url.pathname === '/api/events') return sseKeepAlive(route)
  if (url.pathname === '/api/breakers') {
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(tripped ? breakers : []),
    })
  }
  if (url.pathname.endsWith('/resume') && route.request().method() === 'POST') {
    resumePosts.push(url.pathname)
    tripped = false
    return route.fulfill({ status: 204, body: '' })
  }
  if (url.pathname === '/api/capabilities') {
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{"encode":false,"live":false,"storage":false}' })
  }
  if (url.pathname === '/api/version') {
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{"version":"e2e"}' })
  }
  if (url.pathname === '/api/sites') {
    return route.fulfill({ status: 200, contentType: 'application/json', body: '["tokyo"]' })
  }
  return route.fulfill({ status: 200, contentType: 'application/json', body: '[]' })
})

try {
  await page.goto(BASE + '/recordings', { waitUntil: 'domcontentloaded' })
  await page.getByText('置き場: test-store', { exact: true }).waitFor({ state: 'visible', timeout: 10_000 })
  await page.getByText(`置き場: ${STORE}`, { exact: true }).waitFor({ state: 'visible', timeout: 10_000 })

  const row = page.getByRole('listitem').filter({ hasText: `置き場: ${STORE}` })
  await row.getByRole('button', { name: '再開' }).click()
  await page.getByRole('button', { name: '再開する' }).click()
  await page.getByText(/ごみ箱・エンコード原本・孤児ファイルの物理削除を再開しました/).waitFor({ state: 'visible', timeout: 10_000 })

  if (resumePosts.length !== 1 || resumePosts[0] !== expectedPath) {
    log(`NG  resume POST: ${JSON.stringify(resumePosts)}（期待 [${expectedPath}] のみ）`)
    ng.push('store-specific resume URL')
  } else {
    log(`OK  resume POST: ${resumePosts[0]}`)
  }
} catch (error) {
  log(`NG  S3 store breaker resume: ${error.message}`)
  ng.push('S3 store breaker resume')
} finally {
  await finish(ng, browser)
}
