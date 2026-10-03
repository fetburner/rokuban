// 予約一覧の時間順ビューにある日付見出し・出自リンク・ルール絞り込みを実ブラウザで測る。
// jsdom では sticky、ポップアップの画面内配置、リンクの前面 hit-test を測れない。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/reservation-rule-shots \
//     pnpm e2e:reservations-rule-filter

import { mkdirSync } from 'node:fs'
import path from 'node:path'

import {
  ListCapacityOveragesResponseItem,
  ListReservationsResponseItem,
  ListRulesResponseItem,
} from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const EVIDENCE_DIR = process.env.E2E_SHOT_DIR
if (EVIDENCE_DIR) mkdirSync(EVIDENCE_DIR, { recursive: true })
const ng = []
const FIXED_NOW = new Date('2026-10-02T12:00:00+09:00')
const STAMP = FIXED_NOW.toISOString()

function reservation({ id, day, hour, title, state = 'active', source = 'manual', ruleId }) {
  const start = new Date(`2026-10-${String(day).padStart(2, '0')}T${String(hour).padStart(2, '0')}:00:00+09:00`)
  return {
    site: 'default',
    programId: 9000 + id,
    source,
    ...(ruleId === undefined ? {} : { ruleId }),
    state,
    title,
    serviceName: 'ＮＨＫ総合１・東京',
    channelType: 'GR',
    startAt: start.toISOString(),
    durationMs: 60 * 60_000,
    createdAt: STAMP,
    updatedAt: STAMP,
    series: null,
    skip: false,
  }
}

const reservations = [
  reservation({ id: 1, day: 2, hour: 18, title: 'ニュース７', state: 'orphaned', source: 'manual' }),
  reservation({ id: 2, day: 2, hour: 20, title: '深夜アニメ 第一話', source: 'rule', ruleId: 8 }),
  reservation({ id: 3, day: 2, hour: 22, title: '深夜アニメ 第二話', source: 'rule', ruleId: 8 }),
  reservation({ id: 4, day: 3, hour: 7, title: '朝の連続ドラマ', source: 'rule', ruleId: 3 }),
  reservation({ id: 5, day: 3, hour: 8, title: '手動予約', source: 'manual' }),
  reservation({ id: 6, day: 3, hour: 9, title: '未解決ルールの予約', source: 'rule', ruleId: 55 }),
  reservation({ id: 7, day: 4, hour: 18, title: '週末の映画１', source: 'manual' }),
  reservation({ id: 8, day: 4, hour: 19, title: '週末の映画２', source: 'manual' }),
  reservation({ id: 9, day: 4, hour: 20, title: '週末の映画３', source: 'manual' }),
  reservation({ id: 10, day: 4, hour: 21, title: '週末の映画４', source: 'manual' }),
  // スクロールで行がヘッダー・日付見出しの下を通る距離を作る。
  ...Array.from({ length: 12 }, (_, i) =>
    reservation({
      id: 11 + i,
      day: 5 + Math.floor(i / 6),
      hour: 6 + (i % 6) * 3,
      title: `深夜の再放送${i + 1}`,
      source: 'manual',
    }),
  ),
]

const overages = [reservations[3], reservations[1]].map((item) => ({
  site: item.site,
  startAt: item.startAt,
  endAt: new Date(Date.parse(item.startAt) + item.durationMs).toISOString(),
  shortfall: 1,
  jammedTypes: ['BS'],
}))

const rules = [
  {
    id: 8,
    name: '深夜アニメ',
    enabled: true,
    priority: 20,
    keepOriginal: 'always',
    createdAt: STAMP,
    updatedAt: STAMP,
  },
  {
    id: 9,
    name: '深夜アニメ',
    enabled: true,
    priority: 10,
    keepOriginal: 'always',
    createdAt: STAMP,
    updatedAt: STAMP,
  },
  {
    id: 3,
    name: '朝ドラ',
    enabled: true,
    priority: 15,
    keepOriginal: 'always',
    createdAt: STAMP,
    updatedAt: STAMP,
  },
]

async function apiHandler({ path: requestPath, json }) {
  if (requestPath === '/api/sites') return json(['default'])
  if (requestPath === '/api/capabilities') return json({ live: true })
  if (requestPath === '/api/breakers') return json([])
  if (requestPath === '/api/reservations') return json(reservations)
  if (requestPath === '/api/rules') return json(rules)
  if (requestPath === '/api/capacity/overages') return json(overages)
  return json([])
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit(
  [
    ...reservations.map((item, index) => [`reservations[${index}]`, ListReservationsResponseItem, item]),
    ...overages.map((item, index) => [`overages[${index}]`, ListCapacityOveragesResponseItem, item]),
    ...rules.map((item, index) => [`rules[${index}]`, ListRulesResponseItem, item]),
  ],
  ng,
)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

async function openPage(width, theme = 'light') {
  const context = await browser.newContext({
    viewport: { width, height: 800 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: theme,
    isMobile: width <= 390,
    hasTouch: width <= 390,
  })
  const page = await context.newPage()
  await page.clock.install({ time: FIXED_NOW })
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + '/reservations', { waitUntil: 'domcontentloaded' })
  await page.getByText('ニュース７', { exact: true }).waitFor({ timeout: 15000 }).catch(() => {
    ng.push(`${width}px/${theme}: 予約行が表示されない`)
  })
  return { context, page }
}

async function screenshot(page, name, fullPage = true) {
  if (EVIDENCE_DIR) {
    await page.screenshot({
      path: path.join(EVIDENCE_DIR, name),
      fullPage,
      animations: 'disabled',
    })
  }
}

async function checkLayout(width, theme) {
  const { context, page } = await openPage(width, theme)
  await page.getByRole('button', { name: '要確認（3）' }).waitFor({ timeout: 5_000 }).catch(() => {
    ng.push(`${width}px/${theme}: モックと同じ要確認チップ（3件）が表示されない`)
  })
  const headings = page.getByTestId('reservation-date-heading')
  const count = await headings.count()
  log(`\n=== ${width}px / ${theme}: date groups=${count} ===`)
  if (count < 3) ng.push(`${width}px/${theme}: 日付見出しが 3 日分表示されない`)

  if (count > 0) {
    const first = headings.first()
    await page.evaluate(() => window.scrollTo(0, 220))
    await page.waitForTimeout(50)
    const metrics = await page.evaluate(({ heading, pageHeader }) => {
      const p = document.querySelector(pageHeader)?.getBoundingClientRect()
      // 画面上端に張り付いている見出し（ヘッダー直下に最も近いもの）を測る。
      const el = [...document.querySelectorAll(heading)].sort(
        (a, b) =>
          Math.abs(a.getBoundingClientRect().top - (p?.bottom ?? 0)) -
          Math.abs(b.getBoundingClientRect().top - (p?.bottom ?? 0)),
      )[0]
      const h = el?.getBoundingClientRect()
      return {
        headingTop: h?.top ?? -1,
        pageHeaderBottom: p?.bottom ?? -1,
        position: el ? getComputedStyle(el).position : 'missing',
        cssTop: el ? getComputedStyle(el).top : 'missing',
      }
    }, { heading: '[data-testid="reservation-date-heading"]', pageHeader: 'header' })
    log(`  sticky: ${JSON.stringify(metrics)}`)
    if (metrics.position !== 'sticky') ng.push(`${width}px/${theme}: 日付見出しが sticky でない`)
    if (metrics.headingTop + 1 < metrics.pageHeaderBottom) {
      ng.push(`${width}px/${theme}: 日付見出しがページ見出しの下に収まらない`)
    }
    await page.evaluate(() => window.scrollTo(0, 0))
    await first.waitFor()
  } else {
    ng.push(`${width}px/${theme}: sticky の測定対象が無い`)
  }

  const scroll = await page.evaluate(() => ({
    width: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
  }))
  log(`  document width: ${JSON.stringify(scroll)}`)
  if (width === 360 && scroll.width > scroll.client) {
    ng.push(`360px/${theme}: 横スクロール (${scroll.width}px > ${scroll.client}px)`)
  }

  const originLink = page.getByRole('link', { name: /ルール「深夜アニメ/ }).first()
  const originCount = await page.getByRole('link', { name: /ルール「深夜アニメ/ }).count()
  if (originCount === 0) {
    ng.push(`${width}px/${theme}: ルール出自リンクが無い`)
  } else {
    const box = await originLink.boundingBox()
    const hit = box && await originLink.evaluate((el) => {
      const r = el.getBoundingClientRect()
      const target = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)
      return target !== null && (target === el || el.contains(target))
    })
    if (!box || box.width < 24 || box.height < 24) {
      ng.push(`${width}px/${theme}: ルール名リンクが 24x24px 未満 (${JSON.stringify(box)})`)
    }
    log(`  origin link hit target: ${JSON.stringify(box)}`)
    if (!hit) ng.push(`${width}px/${theme}: 行リンクが出自リンクの hit-test を奪っている`)
    const href = await originLink.getAttribute('href')
    if (href !== '/search?ruleId=8') ng.push(`${width}px/${theme}: 出自リンクの宛先が不正 (${href})`)
  }

  // 行の中身（時刻欄・出自リンク）が sticky の日付見出しとヘッダーのチップの上に
  // 描かれないこと。スクロール位置を細かく変えて elementFromPoint で測る。
  const occluded = await page.evaluate(async () => {
    const bad = []
    const max = document.documentElement.scrollHeight - innerHeight
    const own = (el, x, y) => {
      const t = document.elementFromPoint(x, y)
      return t !== null && (t === el || el.contains(t))
    }
    for (let y = 0; y <= max; y += 7) {
      window.scrollTo(0, y)
      await new Promise((resolve) => requestAnimationFrame(resolve))
      const headerBottom = document.querySelector('header')?.getBoundingClientRect().bottom ?? 0
      const targets = [
        ...document.querySelectorAll('[role="group"][aria-label="予約の絞り込み"] button'),
        ...[...document.querySelectorAll('[data-testid="reservation-date-heading"]')].filter((el) => {
          const r = el.getBoundingClientRect()
          // ヘッダー直下の帯だけ（画面下端はタブバーが正当に覆う）。
          return r.top >= headerBottom - 1 && r.top <= headerBottom + 80
        }),
      ]
      for (const el of targets) {
        const r = el.getBoundingClientRect()
        if (!own(el, r.left + r.width / 2, r.top + r.height / 2)) {
          const t = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)
          bad.push(`scroll ${y}: ${el.textContent?.slice(0, 20)} <- ${t?.tagName}.${String(t?.className).slice(0, 40)} ${t?.textContent?.slice(0, 20)}`)
        }
      }
    }
    window.scrollTo(0, 0)
    return bad
  })
  log(`  occluded samples: ${occluded.length}`)
  if (occluded.length > 0) {
    ng.push(`${width}px/${theme}: 行の中身がヘッダーのチップか日付見出しを遮る (${occluded.length} 点: ${occluded.slice(0, 3).join(' / ')})`)
  }

  // 時刻欄を押しても行リンクが受けて詳細へ遷移する。
  {
    const timeCell = page.getByText('18:00', { exact: true }).first()
    // 実クリックの座標で測る（locator.click は行リンクが受けると待ち続けるため）。
    await timeCell.scrollIntoViewIfNeeded()
    const box = await timeCell.boundingBox()
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2)
    const navigated = await page
      .waitForURL(/\/reservations\/default\/9001/, { timeout: 3000 })
      .then(() => true, () => false)
    if (!navigated) ng.push(`${width}px/${theme}: 時刻欄のクリックで予約詳細へ遷移しない`)
    else {
      await page.goBack()
      await page.getByText('ニュース７', { exact: true }).waitFor()
    }
  }

  await screenshot(page, `${width}-time-${theme}.png`, false)
  await page.evaluate(() => window.scrollTo(0, 330))
  await page.waitForTimeout(50)
  await screenshot(page, `${width}-scrolled-${theme}.png`, false)
  await page.evaluate(() => window.scrollTo(0, 0))

  if (originCount > 0) {
    await originLink.click()
    await page.waitForURL(/\/search\?ruleId=8/).catch(() => {
      ng.push(`${width}px/${theme}: 出自リンクのクリックがルール編集画面へ届かない`)
    })
  }

  await context.close()
}

for (const width of [360, 390, 1280]) await checkLayout(width, 'light')
for (const width of [360, 1280]) await checkLayout(width, 'dark')

async function checkRuleMenu(width, theme) {
  const { context, page } = await openPage(width, theme)
  const trigger = page.getByRole('button', { name: /ルール/ })
  if (await trigger.count() === 0) {
    ng.push(`${width}px/${theme}: ルールメニューのトリガーが無い`)
    await context.close()
    return
  }
  await trigger.click()
  const menu = page.getByRole('menu')
  await menu.waitFor({ timeout: 3000 }).catch(() => ng.push(`${width}px/${theme}: ルールメニューが開かない`))
  if (await menu.count() === 0) {
    await context.close()
    return
  }

  const items = menu.getByRole('menuitem')
  const names = await items.allTextContents()
  log(`\n=== ${width}px / ${theme}: rule menu ${JSON.stringify(names)} ===`)
  if (names.length !== 3) ng.push(`${width}px/${theme}: 予約のあるルールだけをメニューに出していない (${names.length} 件)`)
  if (!names[0]?.includes('深夜アニメ') || !names[0]?.includes('2')) {
    ng.push(`${width}px/${theme}: ルール件数順の先頭が不正 (${names[0] ?? 'なし'})`)
  }
  if (!names[1]?.includes('#55') || !names[2]?.includes('朝ドラ')) {
    ng.push(`${width}px/${theme}: 同数件数の名前順が不正 (${names.slice(1).join(' / ')})`)
  }
  if (!names.some((name) => name.includes('朝ドラ') && name.includes('1')))
    ng.push(`${width}px/${theme}: 朝ドラの件数が表示されない`)
  if (!names.some((name) => name.includes('#55') && name.includes('1')))
    ng.push(`${width}px/${theme}: 未解決ルール #55 の項目が無い`)

  const metrics = await menu.evaluate((el) => {
    const rect = el.getBoundingClientRect()
    const items = Array.from(el.querySelectorAll('[role="menuitem"]')).map((item) => {
      const r = item.getBoundingClientRect()
      const target = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)
      return {
        width: r.width,
        height: r.height,
        hit: target !== null && (target === item || item.contains(target)),
      }
    })
    return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, items }
  })
  log(`  menu bounds: ${JSON.stringify(metrics)}`)
  const viewport = await page.evaluate(() => ({ width: innerWidth, height: innerHeight }))
  if (metrics.left < 0 || metrics.right > viewport.width || metrics.top < 0 || metrics.bottom > viewport.height) {
    ng.push(`${width}px/${theme}: ルールメニューが viewport からはみ出す`)
  }
  if (metrics.items.some((item) => item.width < 24 || item.height < 24 || !item.hit)) {
    ng.push(`${width}px/${theme}: メニュー項目の hit 領域が 24x24px 未満または前面にない`)
  }

  await screenshot(page, `${width}-rule-menu-${theme}.png`, false)

  const ruleItem = menu.getByRole('menuitem').filter({ hasText: '深夜アニメ' }).first()
  await ruleItem.click()
  await page.waitForURL(/\/reservations\?ruleId=8/)
  if (await page.getByRole('button', { name: /ルール「深夜アニメ.*」の絞り込みを解除/ }).count() !== 1) {
    ng.push(`${width}px/${theme}: 選択したルールの chip が表示されない`)
  }
  const editLink = page.getByRole('link', { name: 'ルールの条件を直す' })
  if (await editLink.getAttribute('href') !== '/search?ruleId=8') {
    ng.push(`${width}px/${theme}: 条件編集リンクの宛先が不正`)
  }
  if (await page.getByText('深夜アニメ 第一話', { exact: true }).count() !== 1 ||
      await page.getByText('深夜アニメ 第二話', { exact: true }).count() !== 1 ||
      await page.getByText('ニュース７', { exact: true }).count() !== 0) {
    ng.push(`${width}px/${theme}: ルール絞り込みの結果が不正`)
  }
  await screenshot(page, `${width}-rule-filter-${theme}.png`, false)

  await page.getByRole('button', { name: /ルール「.*」の絞り込みを解除/ }).click()
  await page.waitForURL((url) => {
    const search = new URL(url).searchParams
    return !search.has('ruleId') && !search.has('only')
  })
  await page.getByRole('button', { name: '要確認（3）' }).click()
  await page.waitForURL(/only=attention/)
  await page.getByRole('button', { name: 'ルールで絞り込む' }).click()
  await page.getByRole('menu').waitFor({ state: 'visible', timeout: 3_000 })
  const attentionMenuNames = await page.getByRole('menuitem').allTextContents()
  if (attentionMenuNames.join('|') !== names.join('|')) {
    ng.push(`${width}px/${theme}: only=attention でルールメニューの全予約件数が変わる (${JSON.stringify(attentionMenuNames)})`)
  }
  await page.getByRole('menuitem', { name: /深夜アニメ/ }).first().click()
  await page.waitForURL((url) => {
    const search = new URL(url).searchParams
    return search.get('ruleId') === '8' && search.get('only') === 'attention'
  })
  if (await page.getByRole('button', { name: '要確認（1）' }).count() !== 1) {
    ng.push(`${width}px/${theme}: only=attention と ruleId の組み合わせを維持しない`)
  }
  if (await page.getByText('深夜アニメ 第一話', { exact: true }).count() !== 1 ||
      await page.getByText('深夜アニメ 第二話', { exact: true }).count() !== 0) {
    ng.push(`${width}px/${theme}: only=attention と ruleId の組み合わせ結果が不正`)
  }
  await context.close()
}

await checkRuleMenu(360, 'light')
await checkRuleMenu(390, 'light')
await checkRuleMenu(1280, 'light')
await checkRuleMenu(1280, 'dark')
await checkRuleMenu(360, 'dark')

await finish(ng, browser)
