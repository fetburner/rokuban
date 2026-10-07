// Issue #1232: search controls follow their conditions, Enter routes to the active form,
// and EmptyState stays visually distinct from the scanlined loading skeleton.
//
// Run with:
//   E2E_URL=http://127.0.0.1:4173 node e2e/issue-1232-search.mjs
// Screenshots are written to E2E_SHOT_DIR (default: /private/tmp/rokuban-issue-1232-shots).
import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { finish, installApiStubs, launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://127.0.0.1:4173'
const SHOT_DIR = process.env.E2E_SHOT_DIR ?? '/private/tmp/rokuban-issue-1232-shots'
const SITE = 'default'
const ng = []
const searchRequests = []
const rulePosts = []
let holdNextSearchResponse = false
let releaseSearchResponse

const services = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
]

async function apiHandler({ path: requestPath, json, route }) {
  const request = route.request()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: false, cmDetect: false })
  if (requestPath === `/api/sites/${SITE}/services`) return json(services)
  if (requestPath === '/api/reservations') return json([])
  if (requestPath === '/api/programs/search' && request.method() === 'POST') {
    searchRequests.push(request.postDataJSON())
    if (holdNextSearchResponse) {
      holdNextSearchResponse = false
      await new Promise((resolve) => {
        releaseSearchResponse = resolve
      })
    }
    return json([])
  }
  if (requestPath === '/api/rules' && request.method() === 'POST') {
    rulePosts.push(request.postDataJSON())
    return json({ id: 42 })
  }
  if (requestPath.startsWith('/api/sites/') && requestPath.endsWith('/programs')) return json([])
  if (requestPath.startsWith('/api/sites/') && requestPath.includes('/programs/')) return json({})
  return json([])
}

mkdirSync(SHOT_DIR, { recursive: true })
const browser = await launchBrowser('chromium')
await verifyBundleMatchesOrExit(URL_BASE, ng, browser)
const context = await browser.newContext({ viewport: { width: 390, height: 844 } })
const page = await context.newPage()
await installApiStubs(page, apiHandler)
await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })

const input = page.locator('input[aria-label="テキスト条件 1 の値"]')
const detailsButton = page.getByRole('button', { name: '詳細条件を表示', exact: true })
await Promise.all([
  input.waitFor({ timeout: 15000 }),
  detailsButton.waitFor({ timeout: 15000 }),
  page.getByText('サイト一覧を取得中…').waitFor({ state: 'hidden', timeout: 15000 }),
])

// The keyboard hint belongs only to the value field on SearchPage.
const enterKeyHint = await input.getAttribute('enterkeyhint')
if (enterKeyHint !== 'search') {
  ng.push(`検索条件の値に enterKeyHint="search" が無い（${enterKeyHint ?? '属性なし'}）`)
}

// Controls must follow the complete condition UI in document flow and on the 390px layout.
const order = await page.evaluate(() => {
  const form = document.querySelector('form[aria-label="検索条件"]')
  const details = form?.querySelector('section[aria-label="詳細条件"] button')
  const buttons = [...(form?.querySelectorAll('button') ?? [])]
  const search = buttons.find((button) => button.textContent?.trim() === '検索')
  const clear = buttons.find((button) => button.textContent?.trim() === '条件をクリア')
  if (!details || !search || !clear) return null
  const follows = (first, next) =>
    Boolean(first.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING)
  return {
    searchFollows: follows(details, search),
    clearFollows: follows(details, clear),
    detailsBottom: details.getBoundingClientRect().bottom,
    searchTop: search.getBoundingClientRect().top,
    clearTop: clear.getBoundingClientRect().top,
  }
})
if (order === null) {
  ng.push('検索フォームの条件と主操作の位置を測れない')
} else {
  if (!order.searchFollows || !order.clearFollows) {
    ng.push('検索・条件をクリアの両ボタンが詳細条件の後ろにない')
  }
  if (order.searchTop < order.detailsBottom || order.clearTop < order.detailsBottom) {
    ng.push('390px で主操作が条件より上に描画されている')
  }
}

// A neutral empty state must exist before search, with the existing dark/light screenshots
// showing the actual page treatment rather than a synthetic component story.
const idleMessage = page.getByText('条件を指定して検索してください', { exact: true })
await idleMessage.waitFor({ timeout: 15000 })
const idleClassName = await idleMessage.evaluate((element) => element.closest('div')?.className ?? '')
if (idleClassName.split(/\s+/).includes('scanlines')) {
  ng.push('未検索の EmptyState に scanlines が残っている')
}
await idleMessage.evaluate((element) => element.scrollIntoView({ block: 'center' }))
await page.screenshot({ path: path.join(SHOT_DIR, 'issue-1232-search-empty-light.png'), fullPage: true })
await page.evaluate(() => document.documentElement.classList.add('dark'))
await page.screenshot({ path: path.join(SHOT_DIR, 'issue-1232-search-empty-dark.png'), fullPage: true })
await page.evaluate(() => document.documentElement.classList.remove('dark'))

// Enter in the condition field submits Search exactly once and never saves a rule.
const searchCountBeforeEnter = searchRequests.length
const searchResponse = page.waitForResponse(
  (response) =>
    new URL(response.url()).pathname === '/api/programs/search' &&
    response.request().method() === 'POST',
  { timeout: 15000 },
)
await input.fill('ニュース')
await input.press('Enter')
await searchResponse.catch(() => null)
if (searchRequests.length !== searchCountBeforeEnter + 1) {
  ng.push(`検索条件で Enter を押したとき検索 API が 1 回でない（${searchRequests.length - searchCountBeforeEnter} 回）`)
}
if (rulePosts.length !== 0) ng.push('検索条件で Enter を押すとルール作成 API も呼ばれる')
else log('  条件欄の Enter は検索 API だけを送信')

// Hold a second search response so this browser gate also confirms that the loading
// Skeleton keeps its scanlines while EmptyState has none.
holdNextSearchResponse = true
const secondSearchCount = searchRequests.length
const pendingSearchResponse = page.waitForResponse(
  (response) =>
    new URL(response.url()).pathname === '/api/programs/search' &&
    response.request().method() === 'POST',
  { timeout: 15000 },
)
await input.fill('ニュース 続き')
await input.press('Enter')
const loadingSkeleton = page.locator('.animate-pulse.scanlines').first()
await loadingSkeleton.waitFor({ timeout: 15000 }).catch(() => {
  ng.push('検索中の Skeleton に scanlines が出ない')
})
if (typeof releaseSearchResponse === 'function') releaseSearchResponse()
await pendingSearchResponse.catch(() => null)
if (searchRequests.length !== secondSearchCount + 1) {
  ng.push(`2 回目の Enter で検索 API が 1 回でない（${searchRequests.length - secondSearchCount} 回）`)
}
else log('  検索中の Skeleton は scanlines を維持')

// Rule creation is a separate form. Enter in its Name field creates a rule without
// resubmitting the search form.
const openCreateRule = page.getByRole('button', { name: 'この条件でルールを作成', exact: true })
await openCreateRule.click()
const createRuleForm = page.getByRole('form', { name: 'この条件でルールを作成' })
await createRuleForm.waitFor({ timeout: 15000 })
const ruleName = createRuleForm.getByRole('textbox', { name: '名前', exact: true })
await ruleName.fill('Enter キーの E2E ルール')
const searchCountBeforeRuleEnter = searchRequests.length
const rulePostResponse = page.waitForResponse(
  (response) =>
    new URL(response.url()).pathname === '/api/rules' &&
    response.request().method() === 'POST',
  { timeout: 15000 },
)
await ruleName.press('Enter')
await rulePostResponse.catch(() => null)
if (rulePosts.length !== 1) ng.push(`名前欄で Enter を押したときルール API が 1 回でない（${rulePosts.length} 回）`)
if (searchRequests.length !== searchCountBeforeRuleEnter) {
  ng.push('ルール名で Enter を押すと検索 API も再実行される')
}
else log('  ルール名欄の Enter はルール作成だけを送信')

await context.close()
await finish(ng, browser)
