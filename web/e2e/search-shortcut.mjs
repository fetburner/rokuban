// `/` は検索欄へ移り、入力中は文字として入力できることの実ブラウザ判定。
// `/search` の最初のテキスト条件は詳細条件の外にあり、折りたたみ状態のまま使える。
// 入力イベントの委譲と IME の composing 判定は実ブラウザで測る。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 40773 --strictPort &
//   E2E_URL=http://localhost:40773 corepack pnpm e2e:search-shortcut

import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  sseKeepAlive,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []

const targets = [
  { path: '/recordings', label: '録画一覧', selector: 'input[aria-label="番組名・説明で検索"]' },
  { path: '/series', label: 'シリーズ一覧', selector: 'input[aria-label="番組名・説明で検索"]' },
  { path: '/search', label: '番組検索', selector: 'input[aria-label="テキスト条件 1 の値"]' },
]

async function focusFromSlash(page, target) {
  const input = page.locator(target.selector)
  await input.waitFor({ state: 'attached', timeout: 10_000 })
  await page.keyboard.press('/')
  await page.waitForFunction(
    (selector) => {
      const element = document.querySelector(selector)
      return element !== null && document.activeElement === element && element.closest('[hidden]') === null
    },
    target.selector,
    { timeout: 1_500 },
  ).catch(() => {})

  const focused = await input.evaluate((element) => document.activeElement === element)
  if (!focused) ng.push(`${target.label}: / で検索欄にフォーカスしない`)
}

async function checkSlashIsText(page, target) {
  const input = page.locator(target.selector)
  await input.focus()
  await input.fill('')
  await input.press('/')

  const value = await input.inputValue()
  const focused = await input.evaluate((element) => document.activeElement === element)
  if (value !== '/' || !focused) {
    ng.push(`${target.label}: 入力欄内の / が文字として残らない（value=${JSON.stringify(value)}, focused=${focused}）`)
  }
}

log(`URL: ${URL_BASE}`)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, async ({ path, json, route }) => {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/events') return sseKeepAlive(route)
  return json([])
})

for (const target of targets) {
  log(`\n=== ${target.label}: / で検索欄へ移り、入力中は文字として残る ===`)
  await page.goto(`${URL_BASE}${target.path}`, { waitUntil: 'domcontentloaded' })
  await focusFromSlash(page, target)
  await checkSlashIsText(page, target)
  if (target.path === '/search') {
    const details = page.getByRole('button', { name: /詳細条件を表示/ })
    if ((await details.getAttribute('aria-expanded')) !== 'false') {
      ng.push('番組検索: / で詳細条件が開く')
    }
  }
}

log('\n=== textarea と contenteditable では / を文字として入力できる ===')
await page.goto(`${URL_BASE}/recordings`, { waitUntil: 'domcontentloaded' })
await page.locator('input[aria-label="番組名・説明で検索"]').waitFor({ state: 'attached', timeout: 10_000 })
await page.evaluate(() => {
  const textarea = document.createElement('textarea')
  textarea.setAttribute('aria-label', 'shortcut test textarea')
  const editable = document.createElement('div')
  editable.setAttribute('aria-label', 'shortcut test contenteditable')
  editable.setAttribute('contenteditable', 'true')
  document.body.append(textarea, editable)
})

const textarea = page.getByLabel('shortcut test textarea')
await textarea.focus()
await textarea.press('/')
if ((await textarea.inputValue()) !== '/') ng.push('textarea: / が文字として入力されない')

const editable = page.locator('[contenteditable="true"][aria-label="shortcut test contenteditable"]')
await editable.focus()
await editable.press('/')
if ((await editable.textContent()) !== '/') ng.push('contenteditable: / が文字として入力されない')

log('\n=== IME 変換中の / は横取りしない ===')
const ime = await page.evaluate(() => {
  const body = document.body
  body.tabIndex = -1
  body.focus()
  const event = new KeyboardEvent('keydown', {
    key: '/',
    bubbles: true,
    cancelable: true,
    isComposing: true,
  })
  body.dispatchEvent(event)
  return {
    prevented: event.defaultPrevented,
    bodyFocused: document.activeElement === body,
  }
})
if (ime.prevented || !ime.bodyFocused) {
  ng.push(`IME 変換中の / を横取りした（prevented=${ime.prevented}, bodyFocused=${ime.bodyFocused}）`)
}

log('\n=== 番組表では / を横取りしない ===')
await page.goto(`${URL_BASE}/programs`, { waitUntil: 'domcontentloaded' })
await page.evaluate(() => {
  document.body.tabIndex = -1
  document.body.focus()
  window.__searchShortcutDefaultPrevented = undefined
  window.addEventListener('keydown', (event) => {
    if (event.key === '/') window.__searchShortcutDefaultPrevented = event.defaultPrevented
  }, { once: true })
})
await page.keyboard.press('/')
const programGuide = await page.evaluate(() => ({
  prevented: window.__searchShortcutDefaultPrevented,
  bodyFocused: document.activeElement === document.body,
}))
if (programGuide.prevented || !programGuide.bodyFocused) {
  ng.push(`番組表で / を横取りした（prevented=${programGuide.prevented}, bodyFocused=${programGuide.bodyFocused}）`)
}

await context.close()
await finish(ng, browser)
