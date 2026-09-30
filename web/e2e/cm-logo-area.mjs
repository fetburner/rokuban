// CM 検出のロゴ位置を教える画面の実ブラウザ判定。
//
// jsdom では表示比・ポインタ座標・カーソルを測れないため、CM ロゴ画面の契約を
// 実ブラウザで判定する。API はブラウザ側で差し替えるので、mirakc・DB・原本は要らない。
// 判定側は画面の実装を import せず、coded size と実測した表示寸法から期待値を計算する。
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
const longError = `raw-log-${'x'.repeat(4096)}`

const logo = {
  networkId: 32678,
  serviceId: 5168,
  serviceName: 'e2e CM ロゴ局',
  site: 'tokyo',
  state: 'failed',
  recordingCount: 2,
  failedCount: 1,
  pendingCount: 0,
  lastFailureStage: 'logo',
  detectedCount: 0,
  redetectableCount: 0,
  // L-4 前の zod 契約でも検証できるように残す。L-4 後は未知フィールドとして無視される。
  lastError: longError,
  frameRecordingId: 7,
}

// ブラウザが画像として認識できればよい 1x1 PNG。画像の画素数は coded size と分ける。
const FRAME_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

let savedArea
const frameRequests = []

async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['tokyo'])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/cm-logos' && method === 'GET') {
    return json([{ ...logo, ...(savedArea === undefined ? {} : { logoArea: savedArea }) }])
  }
  if (path === '/api/recordings' && method === 'GET') return json([])
  if (path === '/api/media/recordings/7/frame' && method === 'GET') {
    frameRequests.push(Number(url.searchParams.get('at')))
    return route.fulfill({
      status: 200,
      contentType: 'image/png',
      headers: {
        'X-Coded-Width': '1440',
        'X-Coded-Height': '1080',
        'X-Sample-Aspect-Ratio': '4:3',
      },
      body: FRAME_PNG,
    })
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

async function check(label, fn) {
  try {
    await fn()
  } catch (error) {
    ng.push(`${label}: ${error instanceof Error ? error.message : String(error)}`)
  }
}

function numberInput(page, label) {
  return page.locator(`input[type="number"][aria-label="${label}"]`)
}

async function computedCursor(locator) {
  return locator.evaluate((element) => getComputedStyle(element).cursor)
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
await page.goto(`${URL_BASE}/cm-logos/32678/5168?recording=7`, {
  waitUntil: 'domcontentloaded',
})

log('\n=== ① SAR を掛けた表示比 ===')
await check('①', async () => {
  const image = page.getByTestId('cm-logo-frame-image')
  await image.waitFor({ state: 'visible', timeout: 15000 })
  await page.waitForFunction(
    () => (document.querySelector('[data-testid="cm-logo-frame-image"]')?.naturalWidth ?? 0) > 0,
    undefined,
    { timeout: 15000 },
  )
  const box = await image.boundingBox()
  if (!box || box.width <= 0 || box.height <= 0) throw new Error('コマの描画寸法が取れない')
  const ratio = box.width / box.height
  if (Math.abs(ratio / (16 / 9) - 1) > 0.01) {
    throw new Error(`表示比 ${ratio.toFixed(4)}（期待 16:9）`)
  }
})

log('\n=== ② スライダーの時刻を /frame?at= に渡す ===')
await check('②', async () => {
  const slider = page.getByTestId('cm-logo-time')
  await slider.waitFor({ state: 'visible', timeout: 5000 })
  const min = Number(await slider.getAttribute('min') ?? 0)
  const max = Number(await slider.getAttribute('max') ?? 0)
  const target = Math.round(min + (max - min) * 0.37)
  await slider.fill(String(target))
  await page.waitForTimeout(200)
  if (!frameRequests.includes(target)) {
    throw new Error(`/frame?at=${target} が呼ばれない`)
  }
})

log('\n=== ③ 4 隅の変形を coded size へ変換する ===')
await check('③', async () => {
  const frame = page.getByTestId('cm-logo-frame')
  const frameBox = await frame.boundingBox()
  if (!frameBox || frameBox.width <= 0 || frameBox.height <= 0) throw new Error('表示枠の寸法が取れない')

  const start = { x: frameBox.x + frameBox.width * 0.2, y: frameBox.y + frameBox.height * 0.2 }
  const end = { x: frameBox.x + frameBox.width * 0.55, y: frameBox.y + frameBox.height * 0.5 }
  await page.mouse.move(start.x, start.y)
  await page.mouse.down()
  await page.mouse.move(end.x, end.y)
  await page.mouse.up()

  const rect = page.getByTestId('cm-logo-rect')
  await rect.waitFor({ state: 'visible', timeout: 5000 })
  const before = await rect.boundingBox()
  const handle = page.getByTestId('cm-logo-handle-se')
  const handleBox = await handle.boundingBox()
  if (!before || !handleBox) throw new Error('枠または右下ハンドルが表示されない')

  const dx = 23
  const dy = 17
  const finalRight = before.x + before.width + dx
  const finalBottom = before.y + before.height + dy
  const expected = {
    w: Math.round((finalRight - before.x) * 1440 / frameBox.width),
    h: Math.round((finalBottom - before.y) * 1080 / frameBox.height),
  }
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + handleBox.height / 2)
  await page.mouse.down()
  await page.mouse.move(handleBox.x + handleBox.width / 2 + dx, handleBox.y + handleBox.height / 2 + dy)
  await page.mouse.up()

  const save = page.getByRole('button', { name: '枠を保存' })
  await save.click()
  await page.waitForTimeout(150)
  if (savedArea?.w !== expected.w || savedArea?.h !== expected.h) {
    throw new Error(`保存 w/h が ${savedArea?.w}×${savedArea?.h}（期待 ${expected.w}×${expected.h}）`)
  }
})

log('\n=== ④ 数値入力との往復 ===')
await check('④', async () => {
  const rect = page.getByTestId('cm-logo-rect')
  const before = await rect.boundingBox()
  const x = numberInput(page, 'X')
  await x.fill('120')
  await x.press('Tab')
  await page.waitForTimeout(50)
  const after = await rect.boundingBox()
  if (!before || !after || before.x === after.x) throw new Error('X 入力で枠の位置が変わらない')

  const currentX = await x.inputValue()
  const rectBox = await rect.boundingBox()
  if (!rectBox) throw new Error('枠の寸法が取れない')
  await page.mouse.move(rectBox.x + rectBox.width / 2, rectBox.y + rectBox.height / 2)
  await page.mouse.down()
  await page.mouse.move(rectBox.x + rectBox.width / 2 + 12, rectBox.y + rectBox.height / 2)
  await page.mouse.up()
  if ((await x.inputValue()) === currentX) throw new Error('枠のドラッグで X 入力が変わらない')
})

log('\n=== ⑤ カーソル ===')
await check('⑤', async () => {
  const sliderCursor = await computedCursor(page.getByTestId('cm-logo-time'))
  const frameCursor = await computedCursor(page.getByTestId('cm-logo-frame-image'))
  const rectCursor = await computedCursor(page.getByTestId('cm-logo-rect'))
  const nwCursor = await computedCursor(page.getByTestId('cm-logo-handle-nw'))
  const neCursor = await computedCursor(page.getByTestId('cm-logo-handle-ne'))
  const swCursor = await computedCursor(page.getByTestId('cm-logo-handle-sw'))
  const seCursor = await computedCursor(page.getByTestId('cm-logo-handle-se'))
  const got = [sliderCursor, frameCursor, rectCursor, nwCursor, neCursor, swCursor, seCursor]
  const want = ['pointer', 'crosshair', 'move', 'nwse-resize', 'nesw-resize', 'nesw-resize', 'nwse-resize']
  if (got.some((value, index) => value !== want[index])) {
    throw new Error(`cursor=${JSON.stringify(got)}（期待 ${JSON.stringify(want)}）`)
  }
})

log('\n=== ⑥ 局の画面に生ログを出さない ===')
await check('⑥', async () => {
  const text = await page.locator('body').innerText()
  if (text.includes(longError)) throw new Error('4KB の生ログが DOM に現れる')
})

await finish(ng, browser)
