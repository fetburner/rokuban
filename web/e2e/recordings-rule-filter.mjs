// 録画一覧の絞り込みパネルの「ルール」節の実ブラウザ判定。
//
// jsdom はレイアウトを計算しないので、節が 1 つ増えたときにパネルの高さ予算
// （`max-h-[min(34rem,80vh)]`）を超えないか・スクロールして末尾まで届くかは
// `pnpm test` が全部通っても何の保証にもならない。
//
// 選択肢に無い `ruleId` を渡したときの `<select>` の挙動も、HTML の
// ask-for-a-reset に従うかどうかは実装依存なのでここで測る（jsdom では
// selectedIndex 0 = 先頭の「問わない」になる）。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:recordings-rule-filter
import { ListRecordingsResponseItem, ListRulesResponseItem } from '../src/api/zod.ts'
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

// `max-h-[min(34rem,80vh)]` の 34rem 側（root font-size 16px）。どちらの
// ビューポートでも 80vh より小さいので、これがパネルの高さ予算になる。
const PANEL_MAX_HEIGHT = 34 * 16

const rules = [
  {
    id: 8,
    name: '平日夜のニュースを録る',
    enabled: true,
    priority: 20,
    keepOriginal: 'always',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
  {
    id: 3,
    name: '朝の連続ドラマを録る',
    enabled: true,
    priority: 10,
    keepOriginal: 'always',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
]

const recording = {
  id: 1,
  site: 'default',
  source: 'rule',
  ruleId: 8,
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'ルール由来の録画',
  startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always',
  createdAt: '2026-01-02T12:30:00Z',
}

// ruleList を差し替えることで「ルールが 0 件」の判定へ同じ stub を使い回す。
let ruleList = rules

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/rules') return json(ruleList)
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(path)) return json([])
  if (/^\/api\/recordings\/\d+\/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') return json([recording])
  return json([])
}

/** panelBox はポップオーバー本体の実測ボックスを返す。 */
function panelBox(page) {
  return page.getByRole('dialog', { name: '絞り込み' }).evaluate((el) => {
    const r = el.getBoundingClientRect()
    return { top: r.top, bottom: r.bottom, height: r.height, scrollHeight: el.scrollHeight }
  })
}

/**
 * reachable は要素をパネル内でスクロールして出したうえで、その中心の
 * ヒットテストが本当にその要素に当たるかを返す。可視判定だけでは固定ヘッダ・
 * 隣の節に覆われている状態を検出できない。
 */
async function reachable(locator) {
  await locator.scrollIntoViewIfNeeded()
  return locator.evaluate((el) => {
    const r = el.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) return false
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)
    return hit !== null && (hit === el || el.contains(hit) || hit.contains(el))
  })
}

/** checkPanelBudget は 1 つのビューポートでパネルの高さ予算と到達性を判定する。 */
async function checkPanelBudget(page, label, viewportHeight) {
  await page.getByRole('button', { name: /絞り込み/ }).click()
  const panel = page.getByRole('dialog', { name: '絞り込み' })
  await panel.waitFor({ timeout: 15000 })

  const box = await panelBox(page)
  if (box.height > PANEL_MAX_HEIGHT + 1) {
    ng.push(`${label} パネルが高さ予算を超えた（${box.height}px > ${PANEL_MAX_HEIGHT}px）`)
  }
  if (box.top < 0 || box.bottom > viewportHeight + 1) {
    ng.push(`${label} パネルがビューポートから溢れた（top=${box.top} bottom=${box.bottom} / ${viewportHeight}）`)
  }

  const ruleSelect = panel.getByRole('combobox', { name: 'ルール' })
  if ((await ruleSelect.count()) !== 1) {
    ng.push(`${label} ルール節の select が無い`)
  } else if (!(await reachable(ruleSelect))) {
    ng.push(`${label} ルール節の select にスクロールで到達できない`)
  }

  // 節が 1 つ増えたことで末尾の節が届かなくなっていないかを見る。ルール節は
  // 末尾から 2 番目で、最後は「種別」（`recording-filters.tsx` の section の並びは
  // チャンネル / サイト / ジャンル / 期間 / 状態 / ルール / 種別）。
  const lastSection = panel.getByRole('group', { name: '種別' })
  if (!(await reachable(lastSection))) {
    ng.push(`${label} 末尾の「種別」節にスクロールで到達できない`)
  }

  const overflow = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
  }))
  if (overflow.scrollWidth > overflow.clientWidth) {
    ng.push(`${label} ページが横に溢れた（${overflow.scrollWidth} > ${overflow.clientWidth}）`)
  }

  await page.keyboard.press('Escape')
  await panel.waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
    ng.push(`${label} Escape でパネルが閉じない`)
  })

  return box
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['recording', ListRecordingsResponseItem, recording],
    ...rules.map((rule, i) => [`rules[${i}]`, ListRulesResponseItem, rule]),
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 390, height: 844 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)

log('\n=== ① 390px: ルール節を足してもパネルの高さ予算に収まり、末尾まで届く ===')
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
const mobileBox = await checkPanelBudget(page, '①', 844)
log(`  パネル: height=${mobileBox.height}px scrollHeight=${mobileBox.scrollHeight}px（予算 ${PANEL_MAX_HEIGHT}px）`)

log('\n=== ② 1280px: 同じ判定 ===')
await page.setViewportSize({ width: 1280, height: 800 })
const desktopBox = await checkPanelBudget(page, '②', 800)
log(`  パネル: height=${desktopBox.height}px scrollHeight=${desktopBox.scrollHeight}px（予算 ${PANEL_MAX_HEIGHT}px）`)

log('\n=== ③ 一覧に無い ruleId でも select が「問わない」に落ちない ===')
await page.goto(URL_BASE + '/recordings?ruleId=99', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
// チップは適用中の条件を `ルール #99` として出す（名前に解決できないため）。
if ((await page.getByRole('button', { name: 'ルール #99' }).count()) !== 1) {
  ng.push('③ 一覧に無い ruleId のチップが「ルール #99」で出ない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const unresolvedPanel = page.getByRole('dialog', { name: '絞り込み' })
await unresolvedPanel.waitFor({ timeout: 15000 })
const unresolved = await unresolvedPanel
  .getByRole('combobox', { name: 'ルール' })
  .evaluate((el) => ({ value: el.value, selectedText: el.options[el.selectedIndex]?.text ?? null }))
if (unresolved.value !== '99') {
  ng.push(`③ select が ruleId=99 を選択状態にしない（value=${JSON.stringify(unresolved.value)} / 表示=${JSON.stringify(unresolved.selectedText)}）`)
}
if (unresolved.selectedText !== 'ルール #99') {
  ng.push(`③ select の表示が「ルール #99」でない（${JSON.stringify(unresolved.selectedText)}）`)
}
await page.keyboard.press('Escape')

log('\n=== ④ 一覧にある ruleId は名前で選択され、チップも名前で出る ===')
await page.goto(URL_BASE + '/recordings?ruleId=8', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
if ((await page.getByRole('button', { name: 'ルール: 平日夜のニュースを録る' }).count()) !== 1) {
  ng.push('④ チップが「ルール: <名前>」で出ない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const resolvedPanel = page.getByRole('dialog', { name: '絞り込み' })
await resolvedPanel.waitFor({ timeout: 15000 })
const resolved = await resolvedPanel
  .getByRole('combobox', { name: 'ルール' })
  .evaluate((el) => ({ value: el.value, selectedText: el.options[el.selectedIndex]?.text ?? null }))
if (resolved.value !== '8' || resolved.selectedText !== '平日夜のニュースを録る') {
  ng.push(`④ select が名前の option を選択しない（${JSON.stringify(resolved)}）`)
}
await page.keyboard.press('Escape')

log('\n=== ⑤ 同名のルールは option とチップで id を添えて押し分ける ===')
ruleList = [
  { ...rules[0], name: '同名ルール' },
  { ...rules[1], name: '同名ルール' },
]
await page.goto(URL_BASE + '/recordings?ruleId=8', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
if ((await page.getByRole('button', { name: 'ルール: 同名ルール (#8)' }).count()) !== 1) {
  ng.push('⑤ 同名ルールのチップが「ルール: 同名ルール (#8)」で出ない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const duplicatePanel = page.getByRole('dialog', { name: '絞り込み' })
await duplicatePanel.waitFor({ timeout: 15000 })
const duplicateOptions = await duplicatePanel
  .getByRole('combobox', { name: 'ルール' })
  .locator('option')
  .allTextContents()
if (!duplicateOptions.includes('同名ルール (#8)') || !duplicateOptions.includes('同名ルール (#3)')) {
  ng.push(`⑤ 同名ルールの option が id 付きで出ない（${JSON.stringify(duplicateOptions)}）`)
}
await page.keyboard.press('Escape')

log('\n=== ⑥ ルールが 0 件なら節ごと出さない（機能しないコントロールは置かない） ===')
ruleList = []
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
await page.getByRole('button', { name: /絞り込み/ }).click()
const emptyPanel = page.getByRole('dialog', { name: '絞り込み' })
await emptyPanel.waitFor({ timeout: 15000 })
// 「種別」節は必ずあるので、それが出てからルール節の不在を見る（取得完了を
// 待たずに不在を確認する空虚な成功を避ける）。
await emptyPanel.getByRole('group', { name: '種別' }).waitFor({ timeout: 15000 })
await emptyPanel
  .getByRole('heading', { name: 'ルール' })
  .waitFor({ state: 'detached', timeout: 15000 })
  .catch(() => {
    ng.push('⑤ ルールが 0 件でもルール節が残る')
  })

await finish(ng, browser)
