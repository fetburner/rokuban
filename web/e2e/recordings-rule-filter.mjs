// 録画一覧の絞り込みパネルとジャンル選択の実ブラウザ判定。
//
// jsdom はレイアウトを計算しないので、パネルの高さ予算を超えないか・スクロールなしで
// 全体を見せられるかは `pnpm test` が全部通っても何の保証にも
// ならない。予算は md 以上のポップオーバーが `max-h-[min(34rem,80vh)]`、md 未満の
// 下からのシートが `max-h-[85dvh]`（`components/toolbar-panel.tsx`）。
//
// 選択肢に無い `ruleId` を渡したときの `<select>` の挙動も、HTML の
// ask-for-a-reset に従うかどうかは実装依存なのでここで測る（jsdom では
// selectedIndex 0 = 先頭の「問わない」になる）。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:recordings-rule-filter
import {
  GetEncodeQueueResponse,
  GetStorageResponseItem,
  ListRecordingsResponseItem,
  ListRulesResponseItem,
} from '../src/api/zod.ts'
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

// ポップオーバー（1280px）は `max-h-[min(34rem,80vh)]` の 34rem 側（root font-size 16px。
// 800px の 80vh より小さい）。シート（390px）は `max-h-[85dvh]`。
const POPOVER_MAX_HEIGHT = 34 * 16
const SHEET_MAX_RATIO = 0.85

const rules = [
  {
    id: 8,
    name: '平日夜のニュースを録る',
    enabled: true,
    priority: 20,
    keepOriginal: 'always', cmDetection: { state: 'disabled' },
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
  {
    id: 3,
    name: '朝の連続ドラマを録る',
    enabled: true,
    priority: 10,
    keepOriginal: 'always', cmDetection: { state: 'disabled' },
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
]

const recording = {
  id: 1,
  site: 'tokyo',
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
  encodeProfiles: ['cut'],
  encodeStatus: [{ profile: 'cut', state: 'awaiting_review' }],
  keepOriginal: 'always', hlsPlayback: [], cmDetection: { state: 'disabled' },
  createdAt: '2026-01-02T12:30:00Z',
}
const regularRecording = {
  ...recording,
  id: 2,
  title: '通常の録画',
  encodeProfiles: undefined,
  encodeStatus: undefined,
}
const encodeQueue = { queued: 0, running: 0, awaitingReview: 1 }
const encodeStateRequests = []
const storageRoot = {
  root: 'media',
  path: '/media',
  totalBytes: 1_000_000_000_000,
  usedBytes: 300_000_000_000,
  availableBytes: 700_000_000_000,
  observedAt: new Date().toISOString(),
}

// ruleList を差し替えることで「ルールが 0 件」の判定へ同じ stub を使い回す。
let ruleList = rules

async function apiHandler({ path, url, json, route }) {
  if (path === '/api/sites') return json(['takamatsu', 'tokyo'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/encode-queue') return json(encodeQueue)
  if (path === '/api/storage') return json([storageRoot])
  if (path === '/api/rules') return json(ruleList)
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(path)) return json([])
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') {
    const encodeState = url.searchParams.get('encodeState')
    encodeStateRequests.push(encodeState)
    return json(encodeState === 'awaiting_review' ? [recording] : [recording, regularRecording])
  }
  return json([])
}

/** panelBox はパネル本体（ポップオーバー / シート）の実測ボックスを返す。 */
function panelBox(page) {
  return page.getByRole('dialog', { name: '絞り込み' }).evaluate((el) => {
    const r = el.getBoundingClientRect()
    return {
      top: r.top,
      bottom: r.bottom,
      height: r.height,
      clientHeight: el.clientHeight,
      scrollHeight: el.scrollHeight,
    }
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

/**
 * checkPanelBudget は 1 つのビューポートでパネルの高さ予算と到達性を判定する。
 * `sheet` なら画面の下端に付いたシート、そうでなければ高さ 34rem 以内のポップオーバー。
 */
async function checkPanelBudget(page, label, viewportHeight, sheet, noInternalScroll = false) {
  await page.getByRole('button', { name: /絞り込み/ }).click()
  const panel = page.getByRole('dialog', { name: '絞り込み' })
  await panel.waitFor({ timeout: 15000 })

  const box = await panelBox(page)
  const budget = sheet ? viewportHeight * SHEET_MAX_RATIO : POPOVER_MAX_HEIGHT
  if (box.height > budget + 1) {
    ng.push(`${label} パネルが高さ予算を超えた（${box.height}px > ${budget}px）`)
  }
  if (noInternalScroll && box.scrollHeight > box.clientHeight + 1) {
    ng.push(`${label} パネルの中にスクロールが必要（scrollHeight=${box.scrollHeight} / clientHeight=${box.clientHeight}）`)
  }
  if (box.top < 0 || box.bottom > viewportHeight + 1) {
    ng.push(`${label} パネルがビューポートから溢れた（top=${box.top} bottom=${box.bottom} / ${viewportHeight}）`)
  }
  if (sheet && Math.abs(box.bottom - viewportHeight) > 1) {
    ng.push(`${label} シートが画面の下端に付いていない（bottom=${box.bottom} / ${viewportHeight}）`)
  }
  if (!sheet && box.bottom > viewportHeight - 1 && box.top > viewportHeight / 2) {
    ng.push(`${label} ポップオーバーでなく下からのシートで開いた（top=${box.top}）`)
  }

  const ruleSelect = panel.getByRole('combobox', { name: 'ルール' })
  if ((await ruleSelect.count()) !== 1) {
    ng.push(`${label} ルール節の select が無い`)
  } else if (!(await reachable(ruleSelect))) {
    ng.push(`${label} ルール節の select にスクロールで到達できない`)
  }

  // 節が 1 つ増えたことで末尾の節が届かなくなっていないかを見る。ルール節は
  // 末尾から 2 番目で、最後は「種別」（`recording-filters.tsx` の section の並びは
  // チャンネル / サイト / ジャンル / 状態 / ルール / 種別。期間はツールバーの別の操作）。
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
    ['regular recording', ListRecordingsResponseItem, regularRecording],
    ['encode queue', GetEncodeQueueResponse, encodeQueue],
    ['media storage root', GetStorageResponseItem, storageRoot],
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

log('\n=== ① 390px: シートで開き、ルール節を足しても高さ予算（85%）に収まり、末尾まで届く ===')
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
const mobileBox = await checkPanelBudget(page, '①', 844, true)
log(`  シート: height=${mobileBox.height}px scrollHeight=${mobileBox.scrollHeight}px（予算 ${844 * SHEET_MAX_RATIO}px）`)

log('\n=== ①´ 390px: ジャンル選択は絞り込みシート内の画面遷移で開き、同じシートへ戻る ===')
await page.getByRole('button', { name: /絞り込み/ }).click()
const mobileFilter = page.getByRole('dialog', { name: '絞り込み' })
await mobileFilter.waitFor({ timeout: 15000 })
const mobileFilterHandle = await mobileFilter.elementHandle()
await mobileFilter.getByRole('button', { name: 'ジャンル: 0 件' }).click()
const mobileGenre = page.getByRole('dialog', { name: 'ジャンル' })
await mobileGenre.waitFor({ timeout: 15000 })
const sameSheet = mobileFilterHandle !== null &&
  await mobileFilterHandle.evaluate((el) => el === document.querySelector('[role="dialog"]'))
if ((await page.getByRole('dialog').count()) !== 1 || !sameSheet) {
  ng.push('①´ ジャンル選択が絞り込みシートとは別のダイアログになった')
}
await mobileGenre.getByRole('button', { name: '絞り込みに戻る' }).click()
await page.getByRole('dialog', { name: '絞り込み' }).waitFor({ timeout: 15000 })
await page.keyboard.press('Escape')
await mobileFilter.waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
  ng.push('①´ 戻った絞り込みシートを閉じられない')
})

log('\n=== ② 1280px: ポップオーバーで同じ判定 ===')
await page.setViewportSize({ width: 1280, height: 800 })
const desktopBox = await checkPanelBudget(page, '②', 800, false, true)
log(`  パネル: height=${desktopBox.height}px scrollHeight=${desktopBox.scrollHeight}px（予算 ${POPOVER_MAX_HEIGHT}px、スクロールなし）`)

log('\n=== ②´ 1280px: 未選択の 12・13・拡張を候補に出さない ===')
await page.getByRole('button', { name: /絞り込み/ }).click()
const desktopFilter = page.getByRole('dialog', { name: '絞り込み' })
await desktopFilter.waitFor({ timeout: 15000 })
await desktopFilter.getByRole('button', { name: 'ジャンル: 0 件' }).click()
const desktopGenre = page.getByRole('dialog', { name: 'ジャンル' })
await desktopGenre.waitFor({ timeout: 15000 })
const genreOptions = desktopGenre.getByRole('group', { name: 'ジャンルの候補' })
// 通常候補だけ（12・13・拡張なし）のメニューは、中でスクロールせず全体が見える（HIG: popover を大きくしすぎない）。
const genreBox = await desktopGenre.evaluate((el) => ({ scrollHeight: el.scrollHeight, clientHeight: el.clientHeight }))
if (genreBox.scrollHeight > genreBox.clientHeight + 1) {
  ng.push(`②´ ジャンルメニューの中にスクロールが必要（scrollHeight=${genreBox.scrollHeight} / clientHeight=${genreBox.clientHeight}）`)
}
// トリガーはチャンネル選択と同じ構造: 節見出し + 値だけ、シェブロンは値の直後。
const genreTrigger = desktopFilter.getByRole('button', { name: 'ジャンル: 0 件' })
if ((await desktopFilter.getByRole('heading', { name: 'ジャンル' }).count()) !== 1) {
  ng.push('②´ ジャンルに節見出し「ジャンル」が無い（チャンネル選択と構造が違う）')
}
const triggerGap = await genreTrigger.evaluate((el) => {
  const value = el.querySelector('[aria-hidden="true"]:not(svg)')
  const chevron = el.querySelector('svg')
  if (!value || !chevron) return null
  const range = document.createRange()
  range.selectNodeContents(value)
  return chevron.getBoundingClientRect().left - range.getBoundingClientRect().right
})
if (triggerGap === null || triggerGap > 16) {
  ng.push(`②´ ジャンルのシェブロンが値の直後にない（間隔 ${triggerGap}px）`)
}
for (const reserved of ['ジャンル 12', 'ジャンル 13', '拡張']) {
  if ((await genreOptions.getByRole('checkbox', { name: reserved }).count()) !== 0) {
    ng.push(`②´ 未選択の「${reserved}」がジャンル候補に出る`)
  }
}
await page.keyboard.press('Escape')
await desktopGenre.waitFor({ state: 'detached', timeout: 15000 })
await page.keyboard.press('Escape')
await desktopFilter.waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
  ng.push('②´ Escape で絞り込みパネルが閉じない')
})

log('\n=== ③ URL の ?genre=12 はチップとメニューの選択状態に同じ値を出す ===')
await page.goto(URL_BASE + '/recordings?genre=12', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
if ((await page.getByRole('button', { name: 'ジャンル: ジャンル 12' }).count()) !== 1) {
  ng.push('③ URL のジャンル 12 が「ジャンル: ジャンル 12」のチップにならない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const selectedFilter = page.getByRole('dialog', { name: '絞り込み' })
await selectedFilter.waitFor({ timeout: 15000 })
await selectedFilter.getByRole('button', { name: 'ジャンル: 1 件' }).click()
const selectedGenre = page.getByRole('dialog', { name: 'ジャンル' })
await selectedGenre.waitFor({ timeout: 15000 })
const selectedOption = selectedGenre.getByRole('checkbox', { name: 'ジャンル 12' })
if ((await selectedOption.getAttribute('aria-checked')) !== 'true') {
  ng.push('③ URL のジャンル 12 がメニューで選択状態にならない')
}
if ((await selectedGenre.getByRole('checkbox', { name: 'ジャンル 13' }).count()) !== 0 ||
    (await selectedGenre.getByRole('checkbox', { name: '拡張' }).count()) !== 0) {
  ng.push('③ 選択されていない 13 / 拡張がジャンル候補に残る')
}
await page.keyboard.press('Escape')
await selectedGenre.waitFor({ state: 'detached', timeout: 15000 })
await page.keyboard.press('Escape')
await selectedFilter.waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
  ng.push('③ Escape でジャンルメニューと絞り込みパネルが閉じない')
})

log('\n=== ④ 一覧に無い ruleId でも select が「問わない」に落ちない ===')
await page.goto(URL_BASE + '/recordings?ruleId=99', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
// チップは適用中の条件を `ルール #99` として出す（名前に解決できないため）。
if ((await page.getByRole('button', { name: 'ルール #99' }).count()) !== 1) {
  ng.push('④ 一覧に無い ruleId のチップが「ルール #99」で出ない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const unresolvedPanel = page.getByRole('dialog', { name: '絞り込み' })
await unresolvedPanel.waitFor({ timeout: 15000 })
const unresolved = await unresolvedPanel
  .getByRole('combobox', { name: 'ルール' })
  .evaluate((el) => ({ value: el.value, selectedText: el.options[el.selectedIndex]?.text ?? null }))
if (unresolved.value !== '99') {
  ng.push(`④ select が ruleId=99 を選択状態にしない（value=${JSON.stringify(unresolved.value)} / 表示=${JSON.stringify(unresolved.selectedText)}）`)
}
if (unresolved.selectedText !== 'ルール #99') {
  ng.push(`④ select の表示が「ルール #99」でない（${JSON.stringify(unresolved.selectedText)}）`)
}
await page.keyboard.press('Escape')

log('\n=== ⑤ 一覧にある ruleId は名前で選択され、チップも名前で出る ===')
await page.goto(URL_BASE + '/recordings?ruleId=8', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
if ((await page.getByRole('button', { name: 'ルール: 平日夜のニュースを録る' }).count()) !== 1) {
  ng.push('⑤ チップが「ルール: <名前>」で出ない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const resolvedPanel = page.getByRole('dialog', { name: '絞り込み' })
await resolvedPanel.waitFor({ timeout: 15000 })
const resolved = await resolvedPanel
  .getByRole('combobox', { name: 'ルール' })
  .evaluate((el) => ({ value: el.value, selectedText: el.options[el.selectedIndex]?.text ?? null }))
if (resolved.value !== '8' || resolved.selectedText !== '平日夜のニュースを録る') {
  ng.push(`⑤ select が名前の option を選択しない（${JSON.stringify(resolved)}）`)
}
await page.keyboard.press('Escape')

log('\n=== ⑥ 同名のルールは option とチップで id を添えて押し分ける ===')
ruleList = [
  { ...rules[0], name: '同名ルール' },
  { ...rules[1], name: '同名ルール' },
]
await page.goto(URL_BASE + '/recordings?ruleId=8', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
if ((await page.getByRole('button', { name: 'ルール: 同名ルール (#8)' }).count()) !== 1) {
  ng.push('⑥ 同名ルールのチップが「ルール: 同名ルール (#8)」で出ない')
}
await page.getByRole('button', { name: /絞り込み/ }).click()
const duplicatePanel = page.getByRole('dialog', { name: '絞り込み' })
await duplicatePanel.waitFor({ timeout: 15000 })
const duplicateOptions = await duplicatePanel
  .getByRole('combobox', { name: 'ルール' })
  .locator('option')
  .allTextContents()
if (!duplicateOptions.includes('同名ルール (#8)') || !duplicateOptions.includes('同名ルール (#3)')) {
  ng.push(`⑥ 同名ルールの option が id 付きで出ない（${JSON.stringify(duplicateOptions)}）`)
}
await page.keyboard.press('Escape')

log('\n=== ⑦ ルールが 0 件なら節ごと出さない（機能しないコントロールは置かない） ===')
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
    ng.push('⑦ ルールが 0 件でもルール節が残る')
  })

log('\n=== ⑧ 確認待ちチップは録画一覧を絞り、再度押すと解除する ===')
await page.setViewportSize({ width: 390, height: 844 })
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
await page.getByText('通常の録画').waitFor({ timeout: 15000 })
const awaitingReviewChip = page.getByRole('button', { name: '確認待ち 1件', exact: true })
await awaitingReviewChip.waitFor({ timeout: 15000 })
await page.waitForFunction(() => {
  const summary = document.querySelector(
    '[data-testid="recordings-management-summary"] details > summary',
  )
  return summary?.textContent?.includes('空き') === true
}, undefined, { timeout: 15000 })
const storageSummary = page
  .getByTestId('recordings-management-summary')
  .locator('details > summary')
const visibleStorageSummary = await storageSummary.innerText()
if (!visibleStorageSummary.includes('空き')) {
  ng.push(`⑧ 通常の容量要約が見えない（${JSON.stringify(visibleStorageSummary)}）`)
}
for (const width of [360, 390]) {
  await page.setViewportSize({ width, height: 844 })
  await page.evaluate(
    () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
  )
  const managementSummaryBox = await page
    .getByTestId('recordings-management-summary')
    .boundingBox()
  log(`  ${width}px 管理情報行: ${managementSummaryBox?.height ?? '—'}px（上限 45px）`)
  if (managementSummaryBox === null || managementSummaryBox.height > 45) {
    ng.push(
      `⑧ ${width}px で確認待ちチップを含む管理情報が 1 行に収まらない（${managementSummaryBox?.height ?? '—'}px）`,
    )
  }
  const chipText = await Promise.all([
    page.getByRole('button', { name: '待機中 0件', exact: true }).innerText(),
    page.getByRole('button', { name: '実行中 0件', exact: true }).innerText(),
    awaitingReviewChip.innerText(),
  ])
  if (chipText[0] !== '待機 0件' || chipText[1] !== '実行 0件' || chipText[2] !== '確認待ち 1件') {
    ng.push(`⑧ ${width}px のチップ表示名が崩れている（${JSON.stringify(chipText)}）`)
  }
  const storageLayout = await storageSummary.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
  }))
  if (storageLayout.scrollWidth > storageLayout.clientWidth + 1) {
    ng.push(
      `⑧ ${width}px で容量要約が横にはみ出す（${storageLayout.scrollWidth}px > ${storageLayout.clientWidth}px）`,
    )
  }
}
await page.setViewportSize({ width: 390, height: 844 })
const awaitingReviewRequest = page.waitForRequest((request) => {
  const requestUrl = new URL(request.url())
  return requestUrl.pathname === '/api/recordings' &&
    requestUrl.searchParams.get('encodeState') === 'awaiting_review'
})
await awaitingReviewChip.click()
await awaitingReviewRequest
await page.waitForFunction(() => {
  const main = document.querySelector('main')
  return new URL(location.href).searchParams.get('encodeState') === 'awaiting_review' &&
    main?.textContent?.includes('ルール由来の録画') === true &&
    main.textContent.includes('通常の録画') === false
}, undefined, { timeout: 15000 }).catch(() => {
  ng.push('⑧ 確認待ちに絞っても通常の録画が残るか、確認待ちの録画が消える')
})
await awaitingReviewChip.click()
await page.waitForFunction(() => {
  const main = document.querySelector('main')
  return new URL(location.href).searchParams.get('encodeState') === null &&
    main?.textContent?.includes('ルール由来の録画') === true &&
    main.textContent.includes('通常の録画') === true
}, undefined, { timeout: 15000 }).catch(() => {
  ng.push('⑧ 確認待ちを再度押して解除しても通常の録画が戻らない')
})
if (!encodeStateRequests.includes('awaiting_review')) {
  ng.push(`⑧ encodeState リクエストが往復しない（${JSON.stringify(encodeStateRequests)}）`)
}

await finish(ng, browser)
