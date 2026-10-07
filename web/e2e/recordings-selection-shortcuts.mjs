// 録画選択モードの実ブラウザ操作を判定する（issue #1227）。
// Keyboard focus, native select-all behavior, and Shift-click text selection depend on
// browser behavior and cannot be established by jsdom alone.
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/recordings-selection-shortcuts.mjs
import { ListRecordingsResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []
const recordings = Array.from({ length: 4 }, (_, i) => {
  const id = i + 1
  return {
    id,
    site: 'default',
    source: 'manual',
    serviceName: 'ＯＨＫ',
    channelType: 'GR',
    channel: '27',
    networkId: 32678,
    serviceId: 5168,
    eventId: id,
    title: ['一つ目の録画', '二つ目の録画', '三つ目の録画', '四つ目の録画'][i],
    startAt: new Date(Date.parse('2026-01-01T12:00:00Z') + id * 60_000).toISOString(),
    durationMs: 1_800_000,
    status: 'finished',
    keepOriginal: 'always',
    cmDetection: { state: 'disabled' },
    createdAt: '2026-01-02T12:30:00Z',
  }
})

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/breakers') return json([])
  if (path === '/api/encode-profiles' || path === '/api/rules') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(path)) return json([])
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(path)) {
    return route.fulfill({ status: 404 })
  }
  if (path === '/api/recordings' && route.request().method() === 'GET') return json(recordings)
  return json([])
}

function selectedIds(options) {
  return options.evaluateAll((rows) =>
    rows
      .map((row, index) => (row.getAttribute('aria-selected') === 'true' ? String(index + 1) : null))
      .filter((id) => id !== null)
      .join(','),
  )
}

async function expectSelected(options, expected, label) {
  const actual = await selectedIds(options)
  if (actual !== expected) ng.push(`${label}: aria-selected=${actual || '(none)'}, expected ${expected || '(none)'}`)
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  recordings.map((recording, i) => [`recordings[${i}]`, ListRecordingsResponseItem, recording]),
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('四つ目の録画').waitFor({ timeout: 15000 })

log('\n=== ① 選択モードの roving tabindex と Shift+クリック範囲 ===')
await page.getByRole('button', { name: '選択' }).click()
const listbox = page.getByRole('listbox', { name: '録画を選択' })
const options = listbox.getByRole('option')
if ((await options.count()) !== 4) ng.push('① 読み込み済みの 4 行が option として現れない')
const tabStops = await options.evaluateAll((rows) => rows.filter((row) => row.tabIndex === 0).length)
if (tabStops !== 1) ng.push(`① option の Tab stop が 1 件ではない（${tabStops}）`)
const checkboxTabStops = await page.getByRole('checkbox').evaluateAll((boxes) =>
  boxes.filter((box) => box.tabIndex !== -1).length,
)
if (checkboxTabStops !== 0) ng.push(`① 行 checkbox が Tab 順に残っている（${checkboxTabStops}）`)
if (!(await options.nth(0).evaluate((row) => row === document.activeElement))) {
  ng.push('① 最上部で「選択」を押しても先頭行にフォーカスが乗らない')
}

const first = options.nth(0)
const third = options.nth(2)
await first.click()
await third.click({ modifiers: ['Shift'] })
await expectSelected(options, '1,2,3', '① Shift+クリックで anchor からの範囲を選ぶ')
const selectedText = await page.evaluate(() => window.getSelection()?.toString() ?? '')
if (selectedText !== '') ng.push(`① Shift+クリック後にブラウザの文字選択が残る（${selectedText}）`)

log('\n=== ② ↑/↓・Space・Shift+↑/↓ ===')
await page.keyboard.press('ArrowDown')
if (!(await options.nth(3).evaluate((row) => row === document.activeElement))) {
  ng.push('② ↓ で次の行へフォーカスが移らない')
}
await page.keyboard.press('Shift+ArrowUp')
if (!(await options.nth(2).evaluate((row) => row === document.activeElement))) {
  ng.push('② Shift+↑ で前の行へフォーカスが移らない')
}
await page.keyboard.press('Space')
await expectSelected(options, '1,2', '② Space が現在行を切り替える')
await page.keyboard.press('Space')
await expectSelected(options, '1,2,3', '② Space でもう一度現在行を選べる')
await page.keyboard.press('Shift+Meta+a')
await first.focus()
await page.keyboard.press('Space')
await page.keyboard.press('ArrowDown')
await page.keyboard.press('Shift+ArrowDown')
await expectSelected(options, '1,2,3', '② Shift+↓ が anchor から範囲を広げる')

log('\n=== ③ Cmd/Ctrl+A と Shift+Cmd/Ctrl+A は読み込み済みだけ ===')
await page.keyboard.press('Meta+a')
await expectSelected(options, '1,2,3,4', '③ Cmd+A で全選択する')
await page.keyboard.press('Shift+Meta+a')
await expectSelected(options, '', '③ Shift+Cmd+A で全解除する')
await page.keyboard.press('Control+a')
await expectSelected(options, '1,2,3,4', '③ Ctrl+A で全選択する')
await page.keyboard.press('Shift+Control+a')
await expectSelected(options, '', '③ Shift+Ctrl+A で全解除する')

log('\n=== ④ Tab は一覧から固定バーへ移り、Esc は選択モードを抜ける ===')
await first.focus()
await page.keyboard.press('Tab')
const firstToolbarButton = page
  .getByRole('toolbar', { name: '選択した録画の操作' })
  .getByRole('button')
  .first()
if (!(await firstToolbarButton.evaluate((button) => button === document.activeElement))) {
  ng.push('④ Tab 1 回で固定選択バーへ移らない')
}
await page.keyboard.press('Escape')
if ((await page.getByRole('listbox').count()) !== 0) ng.push('④ Esc で選択モードを抜けない')
if ((await page.getByRole('checkbox').count()) !== 0) ng.push('④ 選択モード終了後も checkbox が残る')

log('\n=== ⑤ 編集欄と選択モード外の Cmd/Ctrl+A はブラウザ既定 ===')
const search = page.getByRole('searchbox', { name: '番組名・説明で検索' })
const platformShortcut = await page.evaluate(() =>
  navigator.platform.toLowerCase().includes('mac') ? 'Meta+a' : 'Control+a',
)

await page.getByRole('button', { name: '選択' }).click()
await search.fill('native selection')
await search.press(platformShortcut)
const inSelectionMode = await search.evaluate((input) => ({
  start: input.selectionStart,
  end: input.selectionEnd,
  length: input.value.length,
}))
if (inSelectionMode.start !== 0 || inSelectionMode.end !== inSelectionMode.length) {
  ng.push('⑤ 選択モード中の入力欄で Cmd/Ctrl+A が文字列全体を選択しない')
}
await expectSelected(options, '', '⑤ 入力欄の Cmd/Ctrl+A が録画を選択しない')
await page.keyboard.press('Escape')

await search.fill('native outside')
await search.press(platformShortcut)
const outsideSelection = await search.evaluate((input) => ({
  start: input.selectionStart,
  end: input.selectionEnd,
  length: input.value.length,
}))
if (outsideSelection.start !== 0 || outsideSelection.end !== outsideSelection.length) {
  ng.push('⑤ 選択モード外で Cmd/Ctrl+A が入力欄の文字列全体を選択しない')
}

log('\n=== ⑥ スクロールした位置で「選択」を押すと、見えている行から始まる ===')
// 40 件の別ページで測る。先頭行が sticky ヘッダーの下に数 px 隠れるだけの配置だと、
// 先頭行を active にする実装でも判定が偶然通ったり落ちたりする。
const manyRecordings = Array.from({ length: 40 }, (_, i) => ({
  ...recordings[0],
  id: 100 + i,
  eventId: 100 + i,
  title: `スクロール録画${i + 1}`,
  startAt: new Date(Date.parse('2026-01-01T12:00:00Z') - i * 60_000).toISOString(),
}))

// el が viewport 内にあり、sticky ヘッダーにも固定選択バー・下部ナビにも隠れていないか。
// 下側の固定要素は中心の hit test で見る（カードは checkbox で伸びて下へ押し出される）。
const isUnobscured = (el) => {
  const header = Array.from(document.querySelectorAll('header')).find(
    (h) => h.querySelector('h1') && getComputedStyle(h).position === 'sticky',
  )
  const headerBottom = header?.getBoundingClientRect().bottom ?? 0
  const box = el.getBoundingClientRect()
  const hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
  return box.top >= headerBottom - 1 && box.bottom <= window.innerHeight + 1 && el.contains(hit)
}
const activeOption = `(() => { const el = document.activeElement; return el?.getAttribute('role') === 'option'
  ? { index: [...document.querySelectorAll('[role=option]')].indexOf(el), height: el.offsetHeight, visible: (${isUnobscured})(el) }
  : null })()`

async function checkStartRow(view, viewport, position) {
  const label = `⑥ ${view}×${viewport.width} ${position}`
  log(`  -- ${label}`)
  const scrollContext = await browser.newContext({
    viewport,
    hasTouch: viewport.width < 500,
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  await scrollContext.addInitScript((v) => localStorage.setItem('rokuban:recordings:view', v), view)
  const scrollPage = await scrollContext.newPage()
  await installApiStubs(scrollPage, async (args) => {
    if (args.path === '/api/recordings' && args.route.request().method() === 'GET') {
      return args.json(manyRecordings)
    }
    return apiHandler(args)
  })
  await scrollPage.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
  await scrollPage.getByText('スクロール録画40', { exact: true }).waitFor({ timeout: 15000 })
  await scrollPage.evaluate(
    (p) => window.scrollTo(0, p === 'middle' ? document.body.scrollHeight / 2 : document.body.scrollHeight),
    position,
  )
  await scrollPage.waitForTimeout(200)

  const scrollBefore = await scrollPage.evaluate(() => window.scrollY)
  const firstRowHidden = await scrollPage
    .getByText('スクロール録画1', { exact: true })
    .evaluate((el) => {
      const header = Array.from(document.querySelectorAll('header')).find(
        (h) => h.querySelector('h1') && getComputedStyle(h).position === 'sticky',
      )
      return el.getBoundingClientRect().bottom <= (header?.getBoundingClientRect().bottom ?? 0)
    })
  if (scrollBefore === 0 || !firstRowHidden) {
    ng.push(`${label} 前提: 先頭行がヘッダーの下に隠れていない（scrollY=${scrollBefore}）`)
  }
  await scrollPage.getByRole('button', { name: '選択', exact: true }).click()
  await scrollPage.waitForTimeout(200)
  const scrollAfter = await scrollPage.evaluate(() => window.scrollY)
  if (scrollAfter !== scrollBefore) ng.push(`${label} 「選択」で scrollY が ${scrollBefore} から ${scrollAfter} に変わる`)
  const activeAfterBegin = await scrollPage.evaluate(activeOption)
  log(`     「選択」直後: scrollY=${scrollBefore}→${scrollAfter} active=${JSON.stringify(activeAfterBegin)}`)
  if (!activeAfterBegin?.visible) {
    ng.push(`${label} 「選択」直後のフォーカス行が見えていない（${JSON.stringify(activeAfterBegin)}）`)
  }

  // ↓ より先に押す。↓ で先頭へ飛んだ後だと、見えない行を選ぶ壊れ方を見逃す。
  await scrollPage.keyboard.press('Space')
  const spaceSelected = await scrollPage.evaluate(
    `[...document.querySelectorAll('[role=option][aria-selected=true]')].map((el) => (${isUnobscured})(el))`,
  )
  if (spaceSelected.length !== 1 || !spaceSelected[0]) {
    ng.push(`${label} Space で選ばれた行が見えている 1 行ではない（visible=${JSON.stringify(spaceSelected)}）`)
  }

  await scrollPage.keyboard.press('ArrowDown')
  await scrollPage.waitForTimeout(200)
  const scrollAfterDown = await scrollPage.evaluate(() => window.scrollY)
  const activeAfterDown = await scrollPage.evaluate(activeOption)
  log(`     ↓ の後: scrollY=${scrollAfterDown} active=${JSON.stringify(activeAfterDown)}`)
  // 次の行へ移るための 1 行分までのスクロールは許す。
  if (!activeAfterDown || Math.abs(scrollAfterDown - scrollAfter) > activeAfterDown.height) {
    ng.push(`${label} ↓ 1 回で scrollY が ${scrollAfter} から ${scrollAfterDown} に飛ぶ`)
  }
  if (!activeAfterDown?.visible || activeAfterDown.index !== (activeAfterBegin?.index ?? -2) + 1) {
    ng.push(`${label} ↓ で見えている次の行へ移らない（${JSON.stringify(activeAfterBegin)} → ${JSON.stringify(activeAfterDown)}）`)
  }
  await scrollContext.close()
}

await checkStartRow('list', { width: 1280, height: 800 }, 'bottom')
await checkStartRow('list', { width: 390, height: 844 }, 'bottom')
// カードは選択モードの checkbox で伸び、下側に固定選択バーと下部ナビが重なる。
await checkStartRow('card', { width: 390, height: 844 }, 'middle')
await checkStartRow('card', { width: 390, height: 844 }, 'bottom')

await finish(ng, browser)
