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

async function apiHandler({ path, url, json, route }) {
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

await finish(ng, browser)
