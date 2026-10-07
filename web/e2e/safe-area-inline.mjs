// H-10: 横向き端末の左右セーフエリアを、非ゼロの inset を与えて寸法で確認する。
// CDP の Emulation.setSafeAreaInsetsOverride で env(safe-area-inset-*) に値を入れ、
// getBoundingClientRect の寸法だけで判定する（CSS の文字列一致は見ない）。
//   - 1280×800 / 390×844 inset 0: inset が 0 のときレイアウトが変わらないこと
//   - 844×390 inset 左右 59・下 21（ノッチ付き iPhone 横向き。md 以上なのでサイドバー配置）
//   - 700×390 inset 左右 47（md 未満の横向き。ボトムタブ配置）
// override が効いていない場合は ng を積む（CDP メソッド欠落や将来の Chromium の変更を黙って通さない）。
// 未検証: 実機の Safari が返す inset の実値、回転時の再計算。実機確認は別 issue で扱う。
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

const cdp = await context.newCDPSession(page)

/** env(safe-area-inset-left) が実際に何 px に解決されるかを、使い捨て要素の padding で測る。 */
async function resolvedInsetLeft() {
  return page.evaluate(() => {
    const probe = document.createElement('div')
    probe.style.cssText = 'position:fixed;visibility:hidden;padding-left:env(safe-area-inset-left)'
    document.body.append(probe)
    const px = parseFloat(getComputedStyle(probe).paddingLeft)
    probe.remove()
    return px
  })
}

async function setInsets(width, height, left, right, bottom, label) {
  await page.setViewportSize({ width, height })
  try {
    await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets: { left, right, top: 0, bottom } })
  } catch (error) {
    ng.push(`${label}: Emulation.setSafeAreaInsetsOverride が失敗した: ${error.message}`)
    return false
  }
  const got = await resolvedInsetLeft()
  if (got !== left) {
    ng.push(`${label}: safe-area override が効いていない（env(safe-area-inset-left) expected ${left}, got ${got}）`)
    return false
  }
  return true
}

function measure() {
  return page.evaluate(() => {
    const rect = (element) => {
      if (!element) return null
      const { left, right, width } = element.getBoundingClientRect()
      return { left, right, width }
    }
    const main = document.querySelector('main#main')
    const header = document.querySelector('header')
    const sidebar = [...document.querySelectorAll('nav[aria-label="主ナビゲーション"]')]
      .find((element) => element.getAttribute('data-testid') !== 'bottom-nav')
    const bottomNav = document.querySelector('[data-testid="bottom-nav"]')
    return {
      main: rect(main),
      header: rect(header),
      headerTitleLeft: header?.querySelector('h1')?.getBoundingClientRect().left ?? null,
      sidebar: sidebar ? { ...rect(sidebar), display: getComputedStyle(sidebar).display } : null,
      bottomNav: bottomNav ? { ...rect(bottomNav), display: getComputedStyle(bottomNav).display } : null,
      bottomNavList: rect(bottomNav?.querySelector('ul')),
    }
  })
}

function same(actual, expected, label) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    ng.push(`${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`)
  }
}

const cases = [
  {
    name: '1280×800 / inset 0', width: 1280, height: 800, insets: [0, 0, 0],
    expected: {
      main: { left: 192, right: 1280, width: 1088 },
      header: { left: 192, right: 1280, width: 1088 },
      headerTitleLeft: 208,
      sidebar: { left: 0, right: 192, width: 192, display: 'flex' },
      bottomNav: { left: 0, right: 0, width: 0, display: 'none' },
    },
  },
  {
    name: '390×844 / inset 0', width: 390, height: 844, insets: [0, 0, 0],
    expected: {
      main: { left: 0, right: 390, width: 390 },
      header: { left: 0, right: 390, width: 390 },
      headerTitleLeft: 16,
      bottomNav: { left: 0, right: 390, width: 390, display: 'block' },
      bottomNavList: { left: 0, right: 390, width: 390 },
    },
  },
  {
    // iPhone 横向き。md 以上なのでサイドバー配置。サイドバー左端が inset 分だけ内側に入る。
    name: '844×390 / inset 59,59,bottom 21', width: 844, height: 390, insets: [59, 59, 21],
    expected: {
      main: { left: 251, right: 785, width: 534 },
      header: { left: 251, right: 785, width: 534 },
      headerTitleLeft: 267,
      sidebar: { left: 59, right: 251, width: 192, display: 'flex' },
      bottomNav: { left: 0, right: 0, width: 0, display: 'none' },
    },
  },
  {
    // md 未満の横向き。サイドバーは無く、ボトムタブの中身が inset の内側に入る。
    name: '700×390 / inset 47,47', width: 700, height: 390, insets: [47, 47, 0],
    expected: {
      main: { left: 47, right: 653, width: 606 },
      header: { left: 47, right: 653, width: 606 },
      headerTitleLeft: 63,
      sidebar: null,
      bottomNavList: { left: 47, right: 653, width: 606 },
    },
  },
]

for (const c of cases) {
  const [left, bottom] = [c.insets[0], c.insets[2]]
  if (!await setInsets(c.width, c.height, left, c.insets[1], bottom, c.name)) continue
  const m = await measure()
  log(`\n=== ${c.name} ===`)
  log(JSON.stringify(m))
  for (const key of Object.keys(c.expected)) {
    if (key === 'sidebar' && c.expected.sidebar === null) {
      // md 未満では sidebar は display:none。非表示（幅 0）であること。
      if (m.sidebar && m.sidebar.display !== 'none') ng.push(`${c.name} sidebar: expected hidden, got ${JSON.stringify(m.sidebar)}`)
      continue
    }
    same(m[key], c.expected[key], `${c.name} ${key}`)
  }
}

await context.close()
await finish(ng, browser)
