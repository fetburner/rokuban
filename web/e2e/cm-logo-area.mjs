// CM 検出のロゴ画面を実ブラウザで判定する。
//
// 旧 URL から局別 URL へ移動できること、EPG の duration の中央から始まる
// スライダー、SAR 4:3 の 1440x1080 コマ、数値入力で記録上の枠を保存することを
// 見る。jsdom では実際の表示枠と画像の SAR を測れないため、座標の最後の判定は
// 実ブラウザで行う。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:cm-logo-area

import { ListCMLogosResponseItem, ListRecordingsResponseItem } from '../src/api/zod.ts'
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
  detectedCount: 1,
  redetectableCount: 2,
  lastFailureStage: 'logo',
  frameRecordingId: 7,
}

const recording = {
  id: 7,
  site: 'default',
  source: 'manual',
  serviceName: 'e2e CM ロゴ局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'CM ロゴの座標確認',
  startAt: '2026-01-01T12:00:00.000Z',
  durationMs: 600_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'failed', stage: 'logo' },
  sizeBytes: 500_000_000,
  createdAt: '2026-01-02T12:30:00Z',
}

// 画像として認識できればよい 1x1 PNG。コマの表示比はヘッダの SAR と coded size で決まる。
const FRAME_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

let savedArea
const frameRequests = []

async function apiHandler({ path, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/cm-logos' && method === 'GET') {
    return json([{ ...logo, ...(savedArea === undefined ? {} : { logoArea: savedArea }) }])
  }
  if (path === '/api/recordings' && method === 'GET') return json([recording])
  if (path === '/api/media/recordings/7/frame' && method === 'GET') {
    frameRequests.push(new URL(route.request().url()).searchParams.get('at'))
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

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit(
  [
    ['CM ロゴ状態', ListCMLogosResponseItem, logo],
    ['影響する録画', ListRecordingsResponseItem, recording],
  ],
  ng,
)

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

log('\n=== ① 旧 URL から局別画面へ移動する ===')
await page.goto(`${URL_BASE}/cm-logos?network=32678&service=5168&recording=7`, {
  waitUntil: 'domcontentloaded',
})
await page.waitForURL(/\/cm-logos\/32678\/5168\?recording=7$/, { timeout: 15000 })
if ((await page.locator('[data-testid="cm-logo-tiles"]').count()) !== 0) {
  ng.push('① 旧シークタイルが局画面に残っている')
}
if (!(await page.getByText('ロゴを見つけられず、CM を検出できませんでした。').count())) {
  ng.push('① 局の失敗理由が表示されない')
}

log('\n=== ② duration の中央から SAR 付きコマを表示する ===')
const frame = page.getByTestId('cm-logo-frame')
await frame.waitFor({ timeout: 15000 })
await page.getByTestId('cm-logo-frame-image').waitFor({ timeout: 15000 })
if (!frameRequests.includes('300000')) ng.push(`② 初期コマが duration の中央ではない（${frameRequests.join(', ')}）`)
const frameGeometry = await frame.evaluate((element) => {
  const box = element.getBoundingClientRect()
  const image = element.querySelector('img')?.getBoundingClientRect()
  return {
    frame: { left: box.left, top: box.top, width: element.clientWidth, height: element.clientHeight },
    image: { width: image?.width ?? 0, height: image?.height ?? 0 },
  }
})
if (frameGeometry.image.width <= 0 || frameGeometry.image.height <= 0) {
  ng.push('② SAR 付きコマの実寸が取れない')
} else if (Math.abs(frameGeometry.image.width / frameGeometry.image.height - 16 / 9) > 0.02) {
  ng.push(`② coded 1440x1080 + SAR 4:3 の表示比が違う（${frameGeometry.image.width}x${frameGeometry.image.height}）`)
}

const slider = page.getByTestId('cm-logo-time-slider')
await slider.fill('100000')
if (frameRequests.includes('100000')) ng.push('② スライダーを動かしただけでコマを取り直している')
await slider.dispatchEvent('pointerup')
await page.waitForTimeout(100)
if (!frameRequests.includes('100000')) ng.push('② スライダー確定後にコマを取り直さない')

log('\n=== ③ 数値入力の枠を記録上の座標で保存する ===')
for (const [field, value] of [['x', '400'], ['y', '300'], ['w', '400'], ['h', '300']]) {
await page.getByTestId(`cm-logo-field-${field}`).fill(value)
}
await page.getByRole('button', { name: '枠に寄る' }).click()
await page.getByRole('button', { name: '枠を保存' }).click()
await page.getByTestId('cm-logo-save-message').waitFor({ state: 'visible', timeout: 5000 })
const expected = { x: 400, y: 300, w: 400, h: 300, codedWidth: 1440, codedHeight: 1080 }
if (savedArea === undefined || JSON.stringify(savedArea) !== JSON.stringify(expected)) {
  ng.push(`③ 保存された枠が違う（実際 ${JSON.stringify(savedArea)} / 期待 ${JSON.stringify(expected)}）`)
} else {
  log(`  記録上の枠: ${JSON.stringify(savedArea)}`)
}
if (!(await page.getByTestId('cm-logo-save-message').textContent()).includes('数分〜数十分かかります')) {
  ng.push('③ 保存後の検出待ちメッセージが表示されない')
}

await finish(ng, browser)
