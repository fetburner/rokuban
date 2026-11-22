// やること:
//   ① 主要画面 × ライト / ダーク × デスクトップ / モバイルを撮って
//      e2e/screenshots/design-screenshots/ に置く（人が見るための成果物。追跡しない）

import path from 'node:path'
import { FIXED_NOW, expectedHomeWarningKinds, fixturePairs } from './design-fixtures.mjs'
import {
  URL_BASE,
  checkMissingStrings,
  createOpen,
  homeDesktop,
  homeWatchScreen,
  mobile,
  mobileWide,
  prepareScreenshotDir,
  screens,
  themes,
  viewports,
} from './design-support.mjs'
import {
  finish,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const ng = []

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(fixturePairs, ng)

const OUT_DIR = prepareScreenshotDir('design-screenshots')

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const open = createOpen(browser, ng)

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

/** checkHomeWarnings はホームの警告種別とモード切替バッジの件数を照合する。 */
async function checkHomeWarnings(page, mode, theme, viewport) {
  const label = `${mode}/${theme}/${viewport.width}px`
  const toggle = page.getByTestId('home-mode-toggle')
  if ((await toggle.count()) === 0) {
    ng.push(`[${label}] ホームのモード切替が見つからない`)
  }

  const badge = page.getByTestId('home-warning-count')
  const badgeReady = await badge.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  if (!badgeReady) {
    ng.push(`[${label}] 警告件数バッジが見つからない`)
  }

  let warningKinds = expectedHomeWarningKinds
  if (mode === 'ops') {
    const warningSection = page.locator('section[aria-labelledby="home-action-required"]')
    const warningSectionReady = await warningSection.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!warningSectionReady) {
      ng.push(`[${label}] 要対応の警告一覧が出ない`)
    }
    const warningRows = warningSection.locator('li[data-warning-kind]')
    warningKinds = await warningRows.evaluateAll((elements) => elements.map((element) => element.dataset.warningKind))
    if (warningKinds.join(',') !== expectedHomeWarningKinds.join(',')) {
      ng.push(
        `[${label}] 警告種別の順序が ${expectedHomeWarningKinds.join('→')} ` +
          `でない（${warningKinds.join(',')}）`,
      )
    }
  }
  if (badgeReady) {
    const badgeText = (await badge.innerText()).trim()
    if (badgeText !== String(warningKinds.length)) {
      ng.push(`[${label}] 警告件数 ${badgeText} が警告行 ${warningKinds.length} 件と一致しない`)
    }
  }
}

// 警告の red / green と変異確認では、レイアウト判定を含む全シナリオを起動せず
// この契約だけを実ブラウザで実行する。
if (process.env.E2E_HOME_WARNINGS_ONLY === '1') {
  for (const mode of ['watch', 'ops']) {
    for (const theme of themes) {
      for (const viewport of [homeDesktop, mobile, mobileWide]) {
        const screen = { name: `home-${mode}`, path: `/?mode=${mode}` }
        const { context, page } = await open(viewport, theme, screen, { homeModeFixture: true })
        await checkHomeWarnings(page, mode, theme, viewport)
        await context.close()
      }
    }
  }
  await finish(ng, browser)
}

// --- ① スクリーンショット ---
log('\n=== ① スクリーンショット ===')
for (const viewport of viewports) {
  for (const theme of themes) {
    for (const screen of screens) {
      const screenOptions = screen.name === 'home' ? { homeModeFixture: true } : {}
      const { context, page } = await open(viewport, theme, screen, screenOptions)
      const file = path.join(OUT_DIR, `${screen.name}-${theme}-${viewport.name}.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(ng, page, `${screen.name}/${theme}/${viewport.name}`)
      await context.close()
    }
  }
}

// --- ホーム M8-25: モードと「次に見る 1 本」の実寸 -------------------------
// jsdom では測れないサムネイル比・固定ナビとの重なり・バッジのはみ出しを、
// 実ブラウザで 1280 / 360 / 390px において測る。各モードの desktop/phone は
// ライト・ダーク両方を撮り、issue のモックと並べて確認する。
for (const mode of ['watch', 'ops']) {
  for (const theme of themes) {
    for (const viewport of [homeDesktop, mobile, mobileWide]) {
      const screen = { name: `home-${mode}`, path: `/?mode=${mode}` }
      const { context, page } = await open(viewport, theme, screen, { homeModeFixture: true })
      await page.locator('main > header').waitFor({ timeout: 5000 }).catch(() => {})

      await checkHomeWarnings(page, mode, theme, viewport)

      const geometry = await page.evaluate(() => {
        const rect = (selector) => {
          const element = document.querySelector(selector)
          if (!element) return null
          const { x, y, width, height, bottom, right } = element.getBoundingClientRect()
          return { x, y, width, height, bottom, right }
        }
        const badgeElement = document.querySelector('[data-testid="home-warning-count"]')
        const badgeStyle = badgeElement ? getComputedStyle(badgeElement) : null
        return {
          content: rect('[data-testid="page-content"], [data-testid="bounded-page-content"]'),
          thumbnail: rect('[data-testid="home-next-watch-thumbnail"]'),
          primary: rect('[data-testid="home-primary-action"]'),
          header: rect('main > header'),
          nav: rect('[data-testid="bottom-nav"]'),
          toggle: rect('[data-testid="home-mode-toggle"]'),
          badge: rect('[data-testid="home-warning-count"]'),
          badgeWhiteSpace: badgeStyle?.whiteSpace ?? null,
          badgeScrollWidth: badgeElement?.scrollWidth ?? null,
          badgeClientWidth: badgeElement?.clientWidth ?? null,
        }
      })

      if (geometry.toggle !== null && geometry.badge !== null) {
        const { toggle: frame, badge: count } = geometry
        if (
          count.x < frame.x ||
          count.y < frame.y ||
          count.right > frame.right ||
          count.bottom > frame.bottom
        ) {
          ng.push(`[${mode}/${theme}/${viewport.width}px] 管理バッジがトグル枠からはみ出す`)
        }
        if (
          geometry.badgeWhiteSpace !== 'nowrap' ||
          geometry.badgeScrollWidth > geometry.badgeClientWidth
        ) {
          ng.push(`[${mode}/${theme}/${viewport.width}px] 管理バッジが折り返す`)
        }
      }

      if (mode === 'watch' && viewport.width === homeDesktop.width) {
        if (geometry.content === null || geometry.thumbnail === null) {
          ng.push(`[watch/${theme}/1280px] 「次に見る 1 本」か本文の幅を測れない`)
        } else {
          const ratio = geometry.thumbnail.width / geometry.content.width
          log(`  [watch/${theme}/1280px] 主役サムネイル / 本文幅=${ratio.toFixed(3)}`)
          if (ratio < 0.55) {
            ng.push(`[watch/${theme}/1280px] 主役サムネイルが本文幅の55%未満（${ratio.toFixed(3)}）`)
          }
        }
      }

      if (mode === 'watch' && (viewport.width === mobile.width || viewport.width === mobileWide.width)) {
        if (geometry.primary === null || geometry.header === null || geometry.nav === null) {
          ng.push(`[watch/${theme}/${viewport.width}px] 主ボタン・固定ヘッダー・ボトムナビを測れない`)
        } else if (
          geometry.primary.y < geometry.header.bottom ||
          geometry.primary.bottom > viewport.height ||
          geometry.primary.bottom > geometry.nav.y
        ) {
          ng.push(`[watch/${theme}/${viewport.width}px] 主ボタンが初期画面から隠れる`)
        }
      }

      if (viewport.width === homeDesktop.width || viewport.width === mobile.width) {
        const file = path.join(OUT_DIR, `home-${mode}-${theme}-${viewport.name}.png`)
        await page.screenshot({ path: file })
        log(`  ${path.basename(file)}`)
        await checkMissingStrings(ng, page, `home-${mode}/${theme}/${viewport.name}`)
      }
      await context.close()
    }
  }
}

// --- issue #1276: 主役の絵は主ボタンと同じ遷移、キーボード順には入れない ---
{
  const { context, page } = await open(homeDesktop, 'light', homeWatchScreen, { homeModeFixture: true })
  const thumbnail = page.getByTestId('home-next-watch-thumbnail')
  const primaryAction = page.getByTestId('home-primary-action')
  const ready = await thumbnail.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  if (!ready) {
    ng.push('home/watch: 主役のサムネイルが表示されない')
    await context.close()
  } else {
    const expectedHref = await primaryAction.getAttribute('href')
    const thumbnailState = await thumbnail.evaluate((element) => ({
      tagName: element.tagName,
      href: element.getAttribute('href'),
      tabIndex: element.tabIndex,
      ariaHidden: element.getAttribute('aria-hidden'),
    }))
    if (thumbnailState.tagName !== 'A' || thumbnailState.href !== expectedHref) {
      ng.push('home/watch: 主役の絵が主ボタンと同じ録画詳細リンクではない')
    }
    if (thumbnailState.tabIndex !== -1 || thumbnailState.ariaHidden !== 'true') {
      ng.push('home/watch: 主役の絵のリンクが Tab 順または読み上げから外れていない')
    }

    await page.evaluate(() => {
      if (document.activeElement instanceof HTMLElement) document.activeElement.blur()
    })
    let reachedPrimaryAction = false
    let thumbnailReceivedFocus = false
    for (let i = 0; i < 50; i++) {
      await page.keyboard.press('Tab')
      const focusedTestId = await page.evaluate(() => document.activeElement?.getAttribute('data-testid'))
      if (focusedTestId === 'home-next-watch-thumbnail') thumbnailReceivedFocus = true
      if (focusedTestId === 'home-primary-action') {
        reachedPrimaryAction = true
        break
      }
    }
    if (!reachedPrimaryAction) ng.push('home/watch: Tab 走査で主ボタンに到達しない')
    if (thumbnailReceivedFocus) ng.push('home/watch: Tab 走査が主役の絵のリンクで止まる')

    if (expectedHref !== null) {
      await thumbnail.click()
      const navigated = await page
        .waitForURL((url) => `${url.pathname}${url.hash}` === expectedHref, { timeout: 5000 })
        .then(() => true)
        .catch(() => false)
      if (!navigated) ng.push('home/watch: 主役の絵を押した遷移先が主ボタンと一致しない')
    } else {
      ng.push('home/watch: 主ボタンの遷移先を取得できない')
    }
    await context.close()
  }
}

// ホーム「見る」: 2560x1440 でも主役と新着 1 行目が初期 viewport に入り、
// 新着件数は 176px カードで入る列数に追従する。低い desktop viewport でも主役を保つ。
{
  const wideViewport = { name: 'home-wide', width: 2560, height: 1440 }
  const shortViewport = { name: 'home-short', width: 1366, height: 400 }
  const arrivalCounts = new Map()
  for (const viewport of [homeDesktop, wideViewport, shortViewport]) {
    const { context, page } = await open(
      viewport,
      'light',
      { name: 'home-watch', path: '/?mode=watch' },
      { homeModeFixture: true, homeWideFixture: true },
    )
    const grid = page.getByTestId('home-new-arrivals-grid')
    const ready = await grid.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
    if (!ready) {
      ng.push(`home/watch/${viewport.width}: 新着カードが表示されない`)
      await context.close()
      continue
    }
    const metrics = await page.evaluate(() => {
      const rect = (element) => {
        if (!(element instanceof HTMLElement)) return null
        const { x, y, right, bottom, width, height } = element.getBoundingClientRect()
        return { x, y, right, bottom, width, height }
      }
      const gridElement = document.querySelector('[data-testid="home-new-arrivals-grid"]')
      const hero = document.querySelector('[data-testid="home-next-watch-thumbnail"]')
      const firstCard = gridElement?.querySelector('li')
      return {
        columns: Number(gridElement?.getAttribute('data-column-count') ?? 0),
        cards: gridElement?.children.length ?? 0,
        cardRows: gridElement
          ? [...gridElement.children].map((card) => Math.round(card.getBoundingClientRect().y))
          : [],
        hero: rect(hero),
        firstCard: rect(firstCard),
        header: rect(document.querySelector('main > header')),
      }
    })
    arrivalCounts.set(viewport.width, metrics.cards)
    log(`  home/watch/${viewport.width}: columns=${metrics.columns}, cards=${metrics.cards}`)
    if (metrics.cards !== metrics.columns) {
      ng.push(`home/watch/${viewport.width}: 新着 ${metrics.cards} 件が 1 行の ${metrics.columns} 列と一致しない`)
    }
    if (new Set(metrics.cardRows).size > 1) {
      ng.push(`home/watch/${viewport.width}: 新着カードが 2 行以上に折り返されている`)
    }
    if (viewport.name === shortViewport.name) {
      if (metrics.hero === null || metrics.hero.width < 320 || metrics.hero.height < 180) {
        ng.push(`home/watch/1366x400: 主役サムネイルが表示可能な大きさでない（${metrics.hero?.width ?? 0}×${metrics.hero?.height ?? 0}px）`)
      }
      await page.screenshot({ path: path.join(OUT_DIR, 'home-watch-light-short.png') })
    }
    if (viewport.width === wideViewport.width) {
      if (metrics.cards <= 6) ng.push('home/watch/2560: 新着が 6 件以下のまま')
      if (metrics.hero === null || metrics.firstCard === null || metrics.header === null) {
        ng.push('home/watch/2560: 主役または先頭カードの位置を測れない')
      } else {
        if (metrics.hero.width > 1024.5 || metrics.hero.height > 576.5) {
          ng.push(`home/watch/2560: 主役サムネイルの上限を超えている（${metrics.hero.width}×${metrics.hero.height}px）`)
        }
        if (metrics.hero.bottom > viewport.height || metrics.firstCard.bottom > viewport.height) {
          ng.push('home/watch/2560: 主役または新着 1 枚目が初期画面に入らない')
        }
      }
      const file = path.join(OUT_DIR, 'home-watch-light-desktop-wide.png')
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
    }
    await context.close()
  }
  const narrowCount = arrivalCounts.get(homeDesktop.width)
  const wideCount = arrivalCounts.get(2560)
  if (narrowCount !== undefined && wideCount !== undefined && narrowCount >= wideCount) {
    ng.push(`home/watch: 1280px の新着件数 ${narrowCount} が 2560px の ${wideCount} 件より減っていない`)
  }
}

await finish(ng, browser)
