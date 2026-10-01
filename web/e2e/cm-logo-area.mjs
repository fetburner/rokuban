// CM 検出のロゴ画面を実ブラウザで判定する。
//
// 旧 URL から局別 URL へ移動できること、EPG の duration の中央から始まる
// スライダー、SAR 4:3 の 1440x1080 コマ、数値入力で解析を依頼し、候補の
// running / failed / ready、候補比較、採用 body を見る。jsdom では実際の表示枠と
// 画像の SAR を測れないため、座標と候補画像の最後の判定は実ブラウザで行う。
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

// コマ用の 1x1 PNG。コマの表示比はヘッダの SAR と coded size で決まる。
const FRAME_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

// ロゴのプレビュー用。幅 100px 超の実寸（今のロゴ 240x120、候補 200x100）にして、
// 狭い列での縮み（SAR が掛からない）と見切れを実ブラウザで測れるようにする。
const CURRENT_LOGO_PNG = 'iVBORw0KGgoAAAANSUhEUgAAAPAAAAB4CAIAAABD1OhwAAABW0lEQVR4nO3SQQkAMAzAwMqpfxWTNRODQTg4AXlkzi5kzPcCeMjQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUm5FI4TNhwFSAEAAAAASUVORK5CYII='
const CANDIDATE_LOGO_PNG = 'iVBORw0KGgoAAAANSUhEUgAAAMgAAABkCAIAAABM5OhcAAABG0lEQVR4nO3SUQkAIBTAQOO8/imMZQmHIAcXYB9bMxuuW88L+JKxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLxAHyZHf8ZP6KHgAAAABJRU5ErkJggg=='
const SAR = 4 / 3

let savedArea
let areaUpdatedAt
let candidate
let adopted = false
let adoptBody
const frameRequests = []

async function apiHandler({ path, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/cm-logos' && method === 'GET') {
    return json([{
      ...logo,
      pendingCount: adopted ? 2 : logo.pendingCount,
      ...(savedArea === undefined ? {} : { logoArea: { ...savedArea, updatedAt: areaUpdatedAt } }),
      ...(candidate === undefined ? {} : { candidate }),
      ...(candidate?.state === 'ready' || adopted
        ? {
            previewPng: CURRENT_LOGO_PNG,
            codedWidth: 1440,
            codedHeight: 1080,
            learnedAt: adopted ? '2026-01-03T00:00:00Z' : '2026-01-01T00:00:00Z',
          }
        : {}),
    }])
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
    // 実バックエンドの PutCMLogoArea は旧候補を消して job を積むだけで、running 行は
    // worker が job を拾ってから作る。ここでも candidate は作らない。
    savedArea = JSON.parse(route.request().postData() ?? '{}')
    areaUpdatedAt = '2026-01-02T00:00:00Z'
    candidate = undefined
    return route.fulfill({ status: 204 })
  }
  if (path === '/api/cm-logos/32678/5168/area' && method === 'DELETE') {
    savedArea = undefined
    candidate = undefined
    return route.fulfill({ status: 204 })
  }
  if (path === '/api/cm-logos/32678/5168/candidate/adopt' && method === 'POST') {
    adoptBody = JSON.parse(route.request().postData() ?? '{}')
    adopted = true
    candidate = undefined
    return route.fulfill({ status: 204 })
  }
  if (path === '/api/cm-logos/32678/5168/candidate' && method === 'DELETE') {
    candidate = undefined
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
await page.getByRole('button', { name: 'ロゴを解析' }).click()
await page.getByTestId('cm-logo-candidate-running').waitFor({ state: 'visible', timeout: 5000 })
const expected = { recordingId: 7, x: 400, y: 300, w: 400, h: 300, codedWidth: 1440, codedHeight: 1080 }
if (savedArea === undefined || JSON.stringify(savedArea) !== JSON.stringify(expected)) {
  ng.push(`③ 保存された枠が違う（実際 ${JSON.stringify(savedArea)} / 期待 ${JSON.stringify(expected)}）`)
} else {
  log(`  記録上の枠: ${JSON.stringify(savedArea)}`)
}
if ('atMs' in (savedArea ?? {})) {
  ng.push('③ 全編解析なのに atMs を送っている')
}

log('\n=== ④ 画面を離れても解析待ちが残り、候補はポーリングで届く ===')
// 枠を保存した直後は candidate の行が無い（worker が job を拾うまで）。それでも解析中を出す。
await page.goto(`${URL_BASE}/cm-logos`, { waitUntil: 'domcontentloaded' })
await page.goto(`${URL_BASE}/cm-logos/32678/5168?recording=7`, { waitUntil: 'domcontentloaded' })
await page.getByTestId('cm-logo-candidate-running').waitFor({ timeout: 5000 })
// 開き直したページ（メモリ state なし）が、candidate が無い間もポーリングして failed を拾う。
candidate = {
  state: 'failed',
  stage: 'logo',
  x: 400,
  y: 300,
  w: 400,
  h: 300,
  codedWidth: 1440,
  codedHeight: 1080,
  recordingId: 7,
  attemptedAt: '2026-01-02T00:00:00Z',
}
await page.getByTestId('cm-logo-candidate-failed').waitFor({ timeout: 8000 }).catch(() => {
  ng.push('④ candidate が無い解析待ちの間にポーリングしていない（開き直した後に failed が届かない）')
})

log('\n=== ⑤ 候補の failed / ready と原寸 SAR 表示 ===')
candidate = {
  state: 'failed',
  stage: 'area',
  error: 'raw error must not be shown',
  x: 400,
  y: 300,
  w: 400,
  h: 300,
  codedWidth: 1440,
  codedHeight: 1080,
  recordingId: 7,
  attemptedAt: '2026-01-02T00:00:00Z',
}
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByTestId('cm-logo-candidate-failed').waitFor({ timeout: 5000 })
if (!(await page.getByTestId('cm-logo-candidate-failure-message').textContent()).includes('教えた枠が録画の解像度と合わない')) {
  ng.push('⑤ 候補 failed の工程文が表示されない')
}

candidate = {
  state: 'ready',
  previewPng: CANDIDATE_LOGO_PNG,
  x: 400,
  y: 300,
  w: 400,
  h: 300,
  codedWidth: 1440,
  codedHeight: 1080,
  recordingId: 7,
  attemptedAt: '2026-01-02T00:00:00Z',
}
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByTestId('cm-logo-candidate-ready').waitFor({ timeout: 5000 })
// 今のロゴと候補の両方で「描画幅 = naturalWidth × SAR、描画高さ = naturalHeight」かつ
// 各 img が自分の欄に収まっている（スクロールしないと見えない状態でない）ことを測る。
async function measurePreviews(label) {
  for (const [testId, name, width, height] of [
    ['cm-logo-current-preview', '今のロゴ', 240, 120],
    ['cm-logo-candidate-preview', '候補', 200, 100],
  ]) {
    const box = page.getByTestId(testId)
    const m = await box.evaluate((element) => {
      const image = element.querySelector('img')
      const outer = element.getBoundingClientRect()
      const rect = image?.getBoundingClientRect()
      return {
        natural: [image?.naturalWidth ?? 0, image?.naturalHeight ?? 0],
        width: rect?.width ?? 0,
        height: rect?.height ?? 0,
        left: (rect?.left ?? 0) - outer.left,
        right: outer.right - (rect?.right ?? 0),
        scrolls: element.scrollWidth > element.clientWidth,
      }
    })
    log(`  ${label} ${name}: ${JSON.stringify(m)}`)
    if (m.natural[0] !== width || m.natural[1] !== height) {
      ng.push(`⑤ ${label} ${name}の画像が読めていない（natural ${m.natural}）`)
      continue
    }
    if (Math.abs(m.width - width * SAR) > 1 || Math.abs(m.height - height) > 1) {
      ng.push(`⑤ ${label} ${name}が原寸×SAR でない（${m.width}x${m.height} / 期待 ${width * SAR}x${height}）`)
    }
    if (m.left < 0 || m.right < 0 || m.scrolls) {
      ng.push(`⑤ ${label} ${name}が欄から見切れている（左 ${m.left} / 右 ${m.right} / scroll ${m.scrolls}）`)
    }
  }
}
await measurePreviews('lg 1280')
await page.setViewportSize({ width: 1024, height: 900 })
await measurePreviews('lg 1024')
await page.setViewportSize({ width: 1280, height: 900 })

log('\n=== ⑥ ready 候補を redetect=false で採用する ===')
await page.getByTestId('cm-logo-candidate-redetect').uncheck()
await page.getByTestId('cm-logo-candidate-adopt').click()
await page.getByTestId('cm-logo-candidate-adopted').waitFor({ timeout: 5000 })
if (JSON.stringify(adoptBody) !== JSON.stringify({ redetect: false })) {
  ng.push(`⑥ 採用 body が違う（実際 ${JSON.stringify(adoptBody)}）`)
}
if (!(await page.getByTestId('cm-logo-candidate-adopted').textContent()).includes('検出待ち 2 件')) {
  ng.push('⑥ 採用後の検出待ちメッセージが表示されない')
}

await finish(ng, browser)
