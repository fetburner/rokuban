// CM 検出のロゴ位置を教える画面の実ブラウザ判定。
//
// jsdom では表示枠の実寸とポインタ座標を測れないため、拡大表示中のドラッグが
// 記録上の解像度の座標で保存されることはここで確認する。API はブラウザ側で
// 差し替えるので、mirakc・DB・原本は要らない。
//
// 見るもの:
//   ① 局を開いてタイルを押すと、原寸コマが記録上の大きさ付きで表示される
//   ② 右上の拡大表示のまま描いた枠が、CSS px ではなく 1920x1080 の座標で PUT される
//   ③ 直近の失敗理由が局の行に残る
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:cm-logo-area

import { ListCMLogosResponseItem } from '../src/api/zod.ts'
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

const logo = {
  networkId: 32678,
  serviceId: 5168,
  serviceName: 'e2e CM ロゴ局',
  site: 'default',
  state: 'failed',
  recordingCount: 2,
  failedCount: 1,
  pendingCount: 0,
  detectedCount: 0,
  redetectableCount: 0,
  lastFailureStage: 'logo',
  frameRecordingId: 7,
}

// ブラウザが画像として認識できればよい 1x1 PNG。枠の座標は応答ヘッダの
// X-Coded-Width / X-Coded-Height から決めるため、画像の画素数とは分けている。
const FRAME_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

let savedArea

async function apiHandler({ path, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/cm-logos' && method === 'GET') {
    return json([{ ...logo, ...(savedArea === undefined ? {} : { logoArea: savedArea }) }])
  }
  if (path === '/api/media/recordings/7/frame' && method === 'GET') {
    return route.fulfill({
      status: 200,
      contentType: 'image/png',
      headers: { 'X-Coded-Width': '1920', 'X-Coded-Height': '1080' },
      body: FRAME_PNG,
    })
  }
  if (path === '/api/media/recordings/7/seek-tiles' && method === 'GET') {
    return route.fulfill({ status: 200, contentType: 'image/png', body: FRAME_PNG })
  }
  if (path === '/api/cm-logos/32678/5168/area' && method === 'PUT') {
    savedArea = JSON.parse(route.request().postData() ?? '{}')
    return route.fulfill({ status: 204 })
  }
  if (path === '/api/cm-logos/32678/5168/area' && method === 'DELETE') {
    savedArea = undefined
    return route.fulfill({ status: 204 })
  }
  return json([])
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit([['CM ロゴ状態', ListCMLogosResponseItem, logo]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)
await page.goto(`${URL_BASE}/cm-logos?network=32678&service=5168&recording=7`, {
  waitUntil: 'domcontentloaded',
})

log('\n=== ① 局を開き、タイルから原寸コマを表示する ===')
const row = page.getByTestId('cm-logo-row')
await row.waitFor({ timeout: 15000 })
if (!(await row.getByTestId('cm-logo-warning').textContent()).includes('ロゴを見つけられず、CM を検出できませんでした。')) {
  ng.push('③ 直近の失敗理由が局の行に表示されない')
}
await row.getByRole('button', { name: 'e2e CM ロゴ局' }).click()
const frame = page.getByTestId('cm-logo-frame')
await frame.waitFor({ timeout: 15000 })
const tiles = page.getByTestId('cm-logo-tiles').locator('img')
const tilesBox = await tiles.boundingBox()
if (!tilesBox || tilesBox.width <= 0 || tilesBox.height <= 0) {
  ng.push('① シークタイルの実寸が取れない')
} else {
  // 左端のタイルを選び、初期状態（frame=null）からコマの取得が始まることも見る。
  await page.waitForFunction(
    () => (document.querySelector('[data-testid="cm-logo-tiles"] img')?.naturalWidth ?? 0) > 0,
    undefined,
    { timeout: 15000 },
  )
  await tiles.click({ position: { x: 2, y: tilesBox.height / 2 } })
}
const frameImage = page.getByTestId('cm-logo-frame-image')
try {
  await frameImage.waitFor({ timeout: 15000 })
  await page.waitForFunction(
    () => (document.querySelector('[data-testid="cm-logo-frame-image"]')?.naturalWidth ?? 0) > 0,
    undefined,
    { timeout: 15000 },
  )
} catch {
  ng.push('① タイルを押しても原寸コマが表示されない')
}
await frame.scrollIntoViewIfNeeded()
log('\n=== ② 拡大表示中のドラッグを記録上の座標で保存する ===')
const zoomButton = page.getByRole('button', { name: '全体表示' })
if ((await zoomButton.count()) === 0) ng.push('② 初期表示が右上の拡大表示ではない')

const geometry = await frame.evaluate((element) => {
  const rect = element.getBoundingClientRect()
  return { left: rect.left, top: rect.top, width: element.clientWidth, height: element.clientHeight }
})
if (geometry.width <= 0 || geometry.height <= 0) {
  ng.push('② コマの表示枠に実寸が無い')
} else {
  // 画面上の 160x120px の枠は、拡大率 2.5 なら記録上は 192x144px になる。
  // 1920x1080 / 右上合わせ / 2.5 倍を判定側にリテラルで置き、実装の変換関数を
  // import しない（同じ関数を比較すると、実装を変えても同じ値を返すだけになる）。
  const scale = Math.min(geometry.width / 1920, geometry.height / 1080) * 2.5
  const start = { x: geometry.left + geometry.width - 220, y: geometry.top + 60 }
  const end = { x: geometry.left + geometry.width - 60, y: geometry.top + 180 }
  const coded = (point) => ({
    x: 1920 - (geometry.width - (point.x - geometry.left)) / scale,
    y: (point.y - geometry.top) / scale,
  })
  const a = coded(start)
  const b = coded(end)
  const expected = {
    x: Math.round(Math.min(a.x, b.x)),
    y: Math.round(Math.min(a.y, b.y)),
    w: Math.round(Math.abs(a.x - b.x)),
    h: Math.round(Math.abs(a.y - b.y)),
    codedWidth: 1920,
    codedHeight: 1080,
  }

  await page.mouse.move(start.x, start.y)
  await page.mouse.down()
  await page.mouse.move(end.x, end.y)
  await page.mouse.up()
  const save = page.getByRole('button', { name: '枠を保存' })
  await save.waitFor({ state: 'visible', timeout: 5000 })
  await page.waitForFunction(
    () =>
      Array.from(document.querySelectorAll('[aria-label="ロゴの枠"] button')).some(
        (button) => button.textContent?.includes('枠を保存') && !button.hasAttribute('disabled'),
      ),
    undefined,
    { timeout: 5000 },
  )
  await save.click()
  await page.waitForTimeout(300)

  if (savedArea === undefined || JSON.stringify(savedArea) !== JSON.stringify(expected)) {
    ng.push(`② 保存された枠が違う（実際 ${JSON.stringify(savedArea)} / 期待 ${JSON.stringify(expected)}）`)
  } else {
    log(`  記録上の枠: ${JSON.stringify(savedArea)}`)
  }
}

await finish(ng, browser)
