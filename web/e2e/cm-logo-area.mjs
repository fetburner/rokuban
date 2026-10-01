// CM 検出のロゴ位置を教える画面の実ブラウザ判定。
//
// jsdom では表示比・ポインタ座標・カーソルを測れないため、CM ロゴ画面の契約を
// 実ブラウザで判定する。API はブラウザ側で差し替えるので、mirakc・DB・原本は要らない。
// 判定側は画面の実装を import せず、coded size と実測した表示寸法から期待値を計算する。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:cm-logo-area

import { GetRecordingResponse, ListCMLogosResponseItem, ListRecordingsResponseItem } from '../src/api/zod.ts'
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

// 録画 7 の詳細。スライダーの範囲（尺）はここから決まる。
const recording7 = {
  id: 7,
  site: 'tokyo',
  source: 'rule',
  serviceName: logo.serviceName,
  channelType: 'GR',
  channel: '27',
  networkId: logo.networkId,
  serviceId: logo.serviceId,
  eventId: 1,
  title: 'e2e 番組',
  startAt: '2026-01-01T00:00:00Z',
  durationMs: 1800000,
  status: 'finished',
  keepOriginal: 'always',
  sizeBytes: 123456789,
  // L-4 後は生ログが録画の cmDetection.error に載る。現行の zod では未知フィールドとして無視される。
  cmDetection: { state: 'failed', error: longError },
  createdAt: '2026-01-01T00:00:00Z',
}

async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['tokyo'])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/cm-logos' && method === 'GET') {
    return json([{ ...logo, ...(savedArea === undefined ? {} : { logoArea: savedArea }) }])
  }
  if (path === '/api/recordings' && method === 'GET') return json([recording7])
  if (path === '/api/recordings/7' && method === 'GET') return json(recording7)
  if (path === '/api/media/recordings/7/frame' && method === 'GET') {
    frameRequests.push(url.searchParams.get('at'))
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

// 点 (x, y) の最前面の要素の computed cursor。透明オーバーレイが覆っていれば、それが見える。
async function cursorAt(page, point) {
  // 利用者に見えるカーソルはポインタがそこにあるときの値なので、実際に動かしてから読む。
  await page.mouse.move(point.x, point.y)
  return page.evaluate(
    ({ x, y }) => {
      const element = document.elementFromPoint(x, y)
      return element ? getComputedStyle(element).cursor : '(要素なし)'
    },
    point,
  )
}

async function centerOf(locator, name) {
  await locator.scrollIntoViewIfNeeded()
  const box = await locator.boundingBox()
  if (!box) throw new Error(`${name} の寸法が取れない`)
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 }
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit(
  [
    ['CM ロゴ状態', ListCMLogosResponseItem, logo],
    ['録画 7', GetRecordingResponse, recording7],
    ['録画 7（一覧）', ListRecordingsResponseItem, recording7],
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
page.setDefaultTimeout(5000)
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
  const min = Number(await slider.getAttribute('min'))
  const max = Number(await slider.getAttribute('max'))
  if (!(max > min)) throw new Error(`スライダーの範囲が空（min=${min} max=${max}）`)
  const initial = await slider.inputValue()
  // 利用者の操作で動かす（fill は input/change だけで、離したら確定する実装を落とす）。
  const box = await slider.boundingBox()
  if (!box) throw new Error('スライダーの寸法が取れない')
  frameRequests.length = 0
  await page.mouse.click(box.x + box.width * 0.37, box.y + box.height / 2)
  const target = await slider.inputValue()
  if (target === initial) throw new Error(`トラックをクリックしても値が動かない（${initial}）`)
  for (let i = 0; i < 30 && !frameRequests.includes(target); i++) await page.waitForTimeout(100)
  if (!frameRequests.includes(target)) {
    throw new Error(`/frame?at=${target} が呼ばれない（実際 ${JSON.stringify(frameRequests)}）`)
  }
})

log('\n=== ③ 4 隅の変形を coded size へ変換する ===')
await check('③', async () => {
  // ① と同じ要素の描画寸法を分母にする。
  const frameBox = await page.getByTestId('cm-logo-frame-image').boundingBox()
  if (!frameBox || frameBox.width <= 0 || frameBox.height <= 0) throw new Error('コマの描画寸法が取れない')

  const start = { x: frameBox.x + frameBox.width * 0.2, y: frameBox.y + frameBox.height * 0.2 }
  const end = { x: frameBox.x + frameBox.width * 0.55, y: frameBox.y + frameBox.height * 0.5 }
  await page.mouse.move(start.x, start.y)
  await page.mouse.down()
  await page.mouse.move(end.x, end.y)
  await page.mouse.up()

  const rect = page.getByTestId('cm-logo-rect')
  await rect.waitFor({ state: 'visible', timeout: 5000 })
  const before = await rect.boundingBox()
  const handleCenter = await centerOf(page.getByTestId('cm-logo-handle-se'), '右下ハンドル')
  if (!before) throw new Error('枠が表示されない')

  const dx = 23
  const dy = 17
  const expected = {
    w: Math.round(((before.width + dx) * 1440) / frameBox.width),
    h: Math.round(((before.height + dy) * 1080) / frameBox.height),
  }
  await page.mouse.move(handleCenter.x, handleCenter.y)
  await page.mouse.down()
  await page.mouse.move(handleCenter.x + dx, handleCenter.y + dy)
  await page.mouse.up()

  await page.getByRole('button', { name: '枠を保存' }).click()
  for (let i = 0; i < 30 && savedArea === undefined; i++) await page.waitForTimeout(100)
  if (savedArea === undefined) throw new Error('PUT /area が届かない')
  const { w, h } = savedArea
  if (!Number.isFinite(w) || !Number.isFinite(h)) throw new Error(`PUT body の w/h が有限数でない（${JSON.stringify(savedArea)}）`)
  if (Math.abs(w - expected.w) > 1 || Math.abs(h - expected.h) > 1) {
    throw new Error(`保存 w/h が ${w}×${h}（期待 ${expected.w}×${expected.h} ±1）`)
  }
})

log('\n=== ④ 数値入力との往復 ===')
await check('④', async () => {
  const image = await page.getByTestId('cm-logo-frame-image').boundingBox()
  const rect = page.getByTestId('cm-logo-rect')
  if (!image) throw new Error('コマの描画寸法が取れない')
  const fill = async (label, value) => {
    const input = numberInput(page, label)
    await input.fill(String(value))
    await input.press('Tab')
    await page.waitForTimeout(50)
  }
  await fill('X', 120)
  await fill('幅', 200)
  const box = await rect.boundingBox()
  if (!box) throw new Error('枠の寸法が取れない')
  const want = { x: image.x + (120 * image.width) / 1440, w: (200 * image.width) / 1440 }
  if (Math.abs(box.x - want.x) > 1 || Math.abs(box.width - want.w) > 1) {
    throw new Error(
      `X=120 幅=200 の枠が x=${box.x.toFixed(1)} w=${box.width.toFixed(1)}（期待 x=${want.x.toFixed(1)} w=${want.w.toFixed(1)} ±1）`,
    )
  }

  const x = numberInput(page, 'X')
  const beforeX = Number(await x.inputValue())
  const center = await centerOf(rect, '枠')
  const move = 12
  await page.mouse.move(center.x, center.y)
  await page.mouse.down()
  await page.mouse.move(center.x + move, center.y)
  await page.mouse.up()
  const afterX = Number(await x.inputValue())
  const wantX = beforeX + (move * 1440) / image.width
  if (Math.abs(afterX - wantX) > 1) {
    throw new Error(`${move}px ドラッグ後の X が ${afterX}（期待 ${wantX.toFixed(1)} ±1）`)
  }
})

log('\n=== ⑤ カーソル（各点の最前面の要素で見る）===')
await check('⑤', async () => {
  const image = page.getByTestId('cm-logo-frame-image')
  await image.scrollIntoViewIfNeeded()
  const box = await image.boundingBox()
  if (!box) throw new Error('コマの描画寸法が取れない')
  const points = [
    ['スライダー', await centerOf(page.getByTestId('cm-logo-time'), 'スライダー'), 'pointer'],
    ['枠の外のコマ', { x: box.x + box.width * 0.95, y: box.y + box.height * 0.95 }, 'crosshair'],
  ]
  // 上のスクロールでコマの座標が動かないよう、コマはスライダーの後で測り直す。
  const box2 = await image.boundingBox()
  if (box2) points[1][1] = { x: box2.x + box2.width * 0.95, y: box2.y + box2.height * 0.95 }
  points.push(['枠の中心', await centerOf(page.getByTestId('cm-logo-rect'), '枠'), 'move'])
  for (const [corner, cursor] of [['nw', 'nwse-resize'], ['ne', 'nesw-resize'], ['sw', 'nesw-resize'], ['se', 'nwse-resize']]) {
    points.push([`ハンドル ${corner}`, await centerOf(page.getByTestId(`cm-logo-handle-${corner}`), corner), cursor])
  }
  const bad = []
  for (const [name, point, want] of points) {
    const got = await cursorAt(page, point)
    if (got !== want) bad.push(`${name}=${got}（期待 ${want}）`)
  }
  if (bad.length > 0) throw new Error(bad.join(' / '))
})

log('\n=== ⑥ 局の画面に生ログを出さない ===')
await check('⑥', async () => {
  // 局の画面が描かれていなければ「出ない」は空虚に通るので、先に局名を待つ。
  await page.getByText(logo.serviceName).first().waitFor({ state: 'attached', timeout: 10000 })
  // 閉じた <details> の中の生ログも「画面に出さない」に反するので textContent で見る。
  const text = await page.locator('body').evaluate((element) => element.textContent ?? '')
  if (text.includes(longError)) throw new Error('4KB の生ログが DOM に現れる')
})

log('\n=== ⑦ 旧形式の URL は局のルートへ飛ぶ ===')
await check('⑦', async () => {
  await page.goto(`${URL_BASE}/cm-logos?network=32678&service=5168&recording=7`, {
    waitUntil: 'domcontentloaded',
  })
  await page.waitForURL((url) => url.pathname === '/cm-logos/32678/5168', { timeout: 5000 })
  const search = new URL(page.url()).searchParams
  if (search.get('recording') !== '7') throw new Error(`recording が引き継がれない（${page.url()}）`)
})

await finish(ng, browser)
