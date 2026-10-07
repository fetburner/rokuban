// H-10: 横向き端末の左右セーフエリアと、0px のときの既存レイアウトを確認する。
// Chromium は non-zero の safe-area-inset を与えられないため、CSS が左右の
// env() を使う契約と、0px 時の実測寸法を分けて確認する。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:safe-area-inline
import { finish, installApiStubs, launchBrowser, log, sseKeepAlive, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []

log(`URL: ${URL_BASE}`)
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 390, height: 844 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, async ({ path, json, route }) => {
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/breakers' || path === '/api/reservations' || path === '/api/capacity/overages') {
    return json([])
  }
  if (path === '/api/sites/default/services' || path === '/api/sites/default/programs') return json([])
  return json([])
})
await page.goto(`${URL_BASE}/programs`, { waitUntil: 'domcontentloaded' })
await page.locator('main#main').waitFor()
await page.locator('header').waitFor()
await page.locator('[data-testid="bottom-nav"] li').first().waitFor()

async function measure() {
  return page.evaluate(() => {
    const rect = (element) => {
      const { left, right, width } = element.getBoundingClientRect()
      return { left, right, width }
    }
    const main = document.querySelector('main#main')
    const header = document.querySelector('header')
    const headerContent = header?.firstElementChild
    const headerTitle = headerContent?.querySelector('h1')
    const shell = main?.parentElement?.parentElement
    const sidebar = [...document.querySelectorAll('nav[aria-label="主ナビゲーション"]')]
      .find((element) => element.getAttribute('data-testid') !== 'bottom-nav')
    const bottomNav = document.querySelector('[data-testid="bottom-nav"]')
    const bottomNavList = bottomNav?.querySelector('ul')
    const style = (element) => element ? getComputedStyle(element) : undefined
    return {
      viewportWidth: window.innerWidth,
      bodyPadding: { left: style(document.body)?.paddingLeft, right: style(document.body)?.paddingRight },
      shell: shell ? rect(shell) : null,
      shellPadding: shell ? { left: style(shell).paddingLeft, right: style(shell).paddingRight } : null,
      main: main ? rect(main) : null,
      header: header ? rect(header) : null,
      headerContent: headerContent ? rect(headerContent) : null,
      headerContentPadding: headerContent ? {
        left: style(headerContent).paddingLeft,
        right: style(headerContent).paddingRight,
      } : null,
      headerTitleLeft: headerTitle ? headerTitle.getBoundingClientRect().left : null,
      sidebar: sidebar ? { ...rect(sidebar), display: style(sidebar).display } : null,
      bottomNav: bottomNav ? { ...rect(bottomNav), display: style(bottomNav).display,
        paddingLeft: style(bottomNav).paddingLeft, paddingRight: style(bottomNav).paddingRight } : null,
      bottomNavList: bottomNavList ? rect(bottomNavList) : null,
      safeAreaRules: (() => {
        const declarations = []
        const visit = (rules) => {
          for (const rule of rules) {
            if (rule.selectorText?.split(',').some((selector) => selector.trim() === '.safe-area-inline')) {
              declarations.push({
                left: rule.style.getPropertyValue('padding-left'),
                right: rule.style.getPropertyValue('padding-right'),
                top: rule.style.getPropertyValue('padding-top'),
                bottom: rule.style.getPropertyValue('padding-bottom'),
              })
            }
            if (rule.cssRules) visit(rule.cssRules)
          }
        }
        for (const sheet of document.styleSheets) {
          try { visit(sheet.cssRules) } catch { /* Cross-origin styles are not our app CSS. */ }
        }
        return declarations
      })(),
      topSafeAreaRuleCount: (() => {
        let count = 0
        const visit = (rules) => {
          for (const rule of rules) {
            if (rule.style?.cssText.includes('env(safe-area-inset-top)')) count++
            if (rule.cssRules) visit(rule.cssRules)
          }
        }
        for (const sheet of document.styleSheets) {
          try { visit(sheet.cssRules) } catch { /* Cross-origin styles are not our app CSS. */ }
        }
        return count
      })(),
    }
  })
}

function same(actual, expected, label) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    ng.push(`${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`)
  }
}

for (const width of [1280, 390]) {
  const height = width === 390 ? 844 : 800
  await page.setViewportSize({ width, height })
  const m = await measure()
  log(`\n=== ${width}px / safe-area 0 の寸法 ===`)
  log(JSON.stringify(m))

  const expected = width === 390
    ? {
        bodyPadding: { left: '0px', right: '0px' },
        shell: { left: 0, right: 390, width: 390 },
        shellPadding: { left: '0px', right: '0px' },
        main: { left: 0, right: 390, width: 390 },
        header: { left: 0, right: 390, width: 390 },
        headerContent: { left: 0, right: 390, width: 390 },
        headerContentPadding: { left: '16px', right: '16px' },
        headerTitleLeft: 16,
        bottomNav: { left: 0, right: 390, width: 390, display: 'block', paddingLeft: '0px', paddingRight: '0px' },
        bottomNavList: { left: 0, right: 390, width: 390 },
      }
    : {
        bodyPadding: { left: '0px', right: '0px' },
        shell: { left: 0, right: 1280, width: 1280 },
        shellPadding: { left: '0px', right: '0px' },
        main: { left: 192, right: 1280, width: 1088 },
        header: { left: 192, right: 1280, width: 1088 },
        headerContent: { left: 192, right: 1280, width: 1088 },
        headerContentPadding: { left: '16px', right: '16px' },
        headerTitleLeft: 208,
        sidebar: { left: 0, right: 192, width: 192, display: 'flex' },
        bottomNav: { left: 0, right: 0, width: 0, display: 'none', paddingLeft: '0px', paddingRight: '0px' },
      }

  for (const key of Object.keys(expected)) same(m[key], expected[key], `${width}px ${key}`)

  if (
    !m.safeAreaRules.some((rule) =>
      rule.left === 'env(safe-area-inset-left)' &&
      rule.right === 'env(safe-area-inset-right)',
    ) ||
    m.safeAreaRules.some((rule) => rule.top !== '' || rule.bottom !== '') ||
    m.topSafeAreaRuleCount !== 0
  ) {
    ng.push(`${width}px: .safe-area-inline needs left/right env() declarations and no top/bottom inset`)
  }
  if (!m.shell || !await page.evaluate(() => {
    const main = document.querySelector('main#main')
    const shell = main?.parentElement?.parentElement
    const nav = document.querySelector('[data-testid="bottom-nav"]')
    return shell?.classList.contains('safe-area-inline') && nav?.classList.contains('safe-area-inline')
  })) {
    ng.push(`${width}px: body/header shell and bottom tabs must use .safe-area-inline`)
  }
}

await context.close()
await finish(ng, browser)
