// 展開した番組行の「予約」ボタンが、隣の折りたたみ行と同じ右余白に揃うことを
// 実ブラウザで測る（issue #1235 / H-14）。
//
// jsdom はレイアウトを計算しないため、左右端の距離とボタンの実寸は測れない。
// 1280px / 390px の両方で、展開ボタンの右端と行の右端の距離を、隣の行の
// トグルボタンが持つ右 padding と比較する。行トグルの幅（境界線 1 + 列 80 +
// padding 16 だけ縮む）、行上部 64px、予約ボタン 80×44px 以上、展開中の
// scrollWidth が viewport 幅を超えないことも確認する。1280px（fine pointer）では
// 折りたたみ行をホバーして開いた列の右余白も同じ 16px であることを測る。
//
// mirakc・実チューナー・DB は不要。API は `page.route` で差し替える。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:program-row-right-padding
import { ListProgramsResponseItem, ListServicesResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const FIXED_NOW = new Date('2026-08-13T00:00:00.000Z')
const ng = []

const service = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

const programs = [
  {
    programId: 123501,
    networkId: service.networkId,
    serviceId: service.serviceId,
    eventId: 1,
    startAt: '2026-08-13T01:00:00.000Z',
    endAt: '2026-08-13T02:00:00.000Z',
    durationMs: 3_600_000,
    name: 'ニュース速報と話題の特集',
    description: '',
    genres: [0],
    isFree: true,
  },
  {
    programId: 123502,
    networkId: service.networkId,
    serviceId: service.serviceId,
    eventId: 2,
    startAt: '2026-08-13T02:00:00.000Z',
    endAt: '2026-08-13T03:00:00.000Z',
    durationMs: 3_600_000,
    name: '折りたたんだままの番組',
    description: '',
    genres: [0],
    isFree: true,
  },
]

async function apiHandler({ path, json }) {
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: false })
  if (path === '/api/reservations') return json([])
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/capacity/overages') return json([])
  if (path === `/api/sites/${SITE}/services`) return json([service])
  if (path === `/api/sites/${SITE}/programs`) return json(programs)
  if (/\/overlaps$/.test(path)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(path)) return json({ extended: {}, audios: [] })
  return json([])
}

log(`URL: ${BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ...programs.map((program, index) => [
      `program${index + 1}`,
      ListProgramsResponseItem,
      program,
    ]),
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()

for (const width of [1280, 390]) {
  log(`\n=== ${width}px: 展開行の「予約」右余白 ===`)
  const coarsePointerExpected = width === 390
  const context = await browser.newContext({
    viewport: { width, height: 844 },
    hasTouch: coarsePointerExpected,
    isMobile: coarsePointerExpected,
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, apiHandler)
  await page.goto(`${BASE}/programs`, { waitUntil: 'domcontentloaded' })

  const pointer = await page.evaluate(() => ({
    coarse: matchMedia('(pointer: coarse)').matches,
    fine: matchMedia('(pointer: fine)').matches,
  }))
  log(`  pointer: coarse=${pointer.coarse}, fine=${pointer.fine}`)
  if (pointer.coarse !== coarsePointerExpected || pointer.fine === coarsePointerExpected) {
    ng.push(
      `${width}px: pointer 種別が想定外（coarse=${pointer.coarse}, fine=${pointer.fine}）`,
    )
  }

  const expandedRow = page.locator(`li[data-program-id="${programs[0].programId}"]`)
  const referenceRow = page.locator(`li[data-program-id="${programs[1].programId}"]`)
  await expandedRow.waitFor({ timeout: 15000 })
  await referenceRow.waitFor({ timeout: 15000 })

  const expandedHeader = expandedRow.locator('[data-testid="program-row"] > .group')
  const referenceToggle = referenceRow.locator('button[aria-expanded]')
  const headerHeightBefore = await expandedHeader.evaluate((el) => el.getBoundingClientRect().height)
  const referenceToggleWidth = await referenceToggle.evaluate((el) => el.getBoundingClientRect().width)
  const referencePadding = await referenceToggle.evaluate(
    (el) => Number.parseFloat(getComputedStyle(el).paddingRight),
  )

  if (!coarsePointerExpected) {
    // fine pointer: 折りたたみ行をホバーして開いた列の右余白も参照 padding と揃う。
    await referenceToggle.hover()
    await page.waitForTimeout(300)
    const hoverGap = await referenceRow.evaluate((row) => {
      const button = row.querySelector('[data-program-action="reserve"] button')
      return button ? row.getBoundingClientRect().right - button.getBoundingClientRect().right : null
    })
    log(`  ホバーした折りたたみ行の予約ボタン右余白: ${hoverGap}px`)
    if (hoverGap === null || Math.abs(hoverGap - referencePadding) > 0.5) {
      ng.push(
        `${width}px: ホバーした折りたたみ行の予約ボタン右余白 ${hoverGap}px が、参照 padding ${referencePadding}px と一致しない`,
      )
    }
    await page.mouse.move(0, 0)
    await page.waitForTimeout(300)
  }

  // 展開のクリック直前から約 400ms、幅が伸びる間の scrollWidth の最大値を記録する。
  await page.evaluate(() => {
    window.__maxScrollWidth = 0
    const start = performance.now()
    const tick = () => {
      window.__maxScrollWidth = Math.max(window.__maxScrollWidth, document.documentElement.scrollWidth)
      if (performance.now() - start < 400) requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })
  await expandedRow.locator('button[aria-expanded]').click()
  await page.waitForFunction(
    (id) => document.querySelector(`li[data-program-id="${id}"] button[aria-expanded]`)?.getAttribute('aria-expanded') === 'true',
    programs[0].programId,
  )
  await expandedRow.locator('[id^="program-row-detail-"]').waitFor({ timeout: 10000 })
  // 列幅の 150ms transition が終わった実レイアウトを測る。
  await page.waitForTimeout(200)

  const maxScrollWidth = await page.evaluate(() => window.__maxScrollWidth)
  const innerWidth = await page.evaluate(() => window.innerWidth)
  const reserve = expandedRow
    .getByTestId('program-row-reserve')
    .locator('[data-program-action="reserve"] button')
  await reserve.waitFor({ state: 'visible', timeout: 10000 })
  const metrics = await page.evaluate(
    ({ rowId, reserveButton }) => {
      const row = document.querySelector(`li[data-program-id="${rowId}"]`)
      const button = row?.querySelector(reserveButton)
      const header = row?.querySelector('[data-testid="program-row"] > .group')
      if (!row || !button || !header) return null
      const rowRect = row.getBoundingClientRect()
      const buttonRect = button.getBoundingClientRect()
      const toggle = header.querySelector('button[aria-expanded]')
      const toggleRect = toggle?.getBoundingClientRect()
      return {
        rowRight: rowRect.right,
        buttonRight: buttonRect.right,
        rightGap: rowRect.right - buttonRect.right,
        viewportRightGap: window.innerWidth - buttonRect.right,
        buttonWidth: buttonRect.width,
        buttonHeight: buttonRect.height,
        headerHeight: header.getBoundingClientRect().height,
        toggleWidth: toggleRect?.width,
      }
    },
    {
      rowId: programs[0].programId,
      reserveButton: '[data-program-action="reserve"] button',
    },
  )

  log(`  参照行の右 padding: ${referencePadding}px`)
  log(
    `  展開行: row.right=${metrics?.rowRight}px, button.right=${metrics?.buttonRight}px, ` +
      `行端からの余白=${metrics?.rightGap}px, viewport 端から=${metrics?.viewportRightGap}px, ` +
      `予約ボタン=${metrics?.buttonWidth}×${metrics?.buttonHeight}px, ` +
      `行上部の高さ=${metrics?.headerHeight}px（展開前=${headerHeightBefore}px）, ` +
      `行トグル幅=${metrics?.toggleWidth}px（参照行=${referenceToggleWidth}px）, ` +
      `展開中 scrollWidth 最大=${maxScrollWidth}px（innerWidth=${innerWidth}px）`,
  )

  if (!metrics) {
    ng.push(`${width}px: 展開行の「予約」ボタンの矩形を取得できない`)
  } else {
    if (Math.abs(metrics.rightGap - referencePadding) > 0.5) {
      ng.push(
        `${width}px: 展開行の予約ボタン右余白 ${metrics.rightGap}px が、折りたたみ行の右 padding ${referencePadding}px と一致しない`,
      )
    }
    if (Math.abs(metrics.buttonWidth - 80) > 0.5 || metrics.buttonHeight < 44) {
      ng.push(
        `${width}px: 予約ボタンの標的が 80×44px 以上でない（${metrics.buttonWidth}×${metrics.buttonHeight}px）`,
      )
    }
    if (Math.abs(headerHeightBefore - 64) > 0.5) {
      ng.push(
        `${width}px: 展開前の行上部が期待値 64px でない（${headerHeightBefore}px）`,
      )
    }
    if (Math.abs(metrics.headerHeight - 64) > 0.5) {
      ng.push(
        `${width}px: 展開後の行上部が期待値 64px でない（${metrics.headerHeight}px）`,
      )
    }
    if (Math.abs(metrics.toggleWidth - (referenceToggleWidth - 97)) > 0.5) {
      ng.push(
        `${width}px: 展開で行トグル幅が参照行から境界線 1 + 列 80 + padding 16 の 97px だけ縮んでいない（${referenceToggleWidth}px → ${metrics.toggleWidth}px）`,
      )
    }
    if (maxScrollWidth > innerWidth) {
      ng.push(
        `${width}px: 展開中に横スクロールが出ている（scrollWidth 最大 ${maxScrollWidth}px > innerWidth ${innerWidth}px）`,
      )
    }
  }

  await context.close()
}

await finish(ng, browser)
