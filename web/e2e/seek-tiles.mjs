// シークバーのプレビュー（タイル画像、issue #873）の実ブラウザ判定。
//
// **jsdom では原理的に測れないものだけを見る。** プレビューの位置は
// 「ポインタの x → 動画内の割合 → 再生位置 → タイルの格子位置」で決まるが、
// jsdom の `getBoundingClientRect()` は常に 0 を返すので、この経路は
// 単体テストでは 1 歩も進まない（`recording-player.test.tsx` が見るのは
// 「問い合わせが始まること」だけ）。CLAUDE.md §テスト規律のとおり、
// 実装より先にここで判定手段を作る。
//
// 見るのは 4 点:
//   ① ホバー位置に対応するタイルが出る（列の折り返しと行送りを別々の位置で固定）
//   ② プレビューが動画の矩形の中に収まる（はみ出さない）
//   ③ ポインタが離れると消える（両方向）
//   ④ タイルが無い録画（404）ではプレビューが出ず、再生面は従来のまま
//
// フィクスチャは ffmpeg で作る（動画の長さが判定に要る）。無い環境では
// この判定だけを skip として終了する。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:seek-tiles
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, statSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { ListRecordingsResponseItem } from '../src/api/zod.ts'
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

// タイルの形。**web/src/lib/seek-tiles.ts と同じ値をここにも書く**のは意図的で、
// 判定側が実装から値を import すると「実装がどう変わっても同じ値で比較する」
// 循環になり何も主張しなくなる。
const TILE_INTERVAL_SECONDS = 10
const TILE_DISPLAY_WIDTH = 320
const TILE_DISPLAY_HEIGHT = 180
const TILE_COLUMNS = 10

const recording = {
  id: 1,
  site: 'default',
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'プレビュー確認用',
  startAt: '2026-01-01T12:00:00.000Z',
  durationMs: 120_000,
  status: 'finished',
  keepOriginal: 'always',
  sizeBytes: 500_000_000,
  encodedAssets: [{ profile: 'h264', sizeBytes: 400_000_000 }],
  createdAt: '2026-01-02T12:30:00Z',
}

/** serveTiles が false の間はタイル配信だけ 404 を返す（④の判定用）。 */
let serveTiles = false

/**
 * タイル画像のフィクスチャ（1x1 の PNG）。**中身は判定に効かない** ---
 * 見ているのは `background-position` が指す格子の位置である。必要なのは
 * 「ブラウザが画像として実際に読み込めること」だけで、`<img>` の
 * `naturalWidth > 0` がその証拠になる。
 */
const TILE_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

/**
 * 判定に使う動画を一度だけ作る。長さ（duration）が要るので実在の動画を配る。
 * **VP8/WebM を使う** --- Playwright の Chromium は H.264 を持たない構成があり、
 * コーデックの有無で落ちると「実装が壊れている」と区別できない。
 */
function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-seek-tiles')
  const videoPath = path.join(fixtureDir, 'clip.webm')
  if (existsSync(videoPath) && statSync(videoPath).size > 0) return videoPath

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(fixtureDir, { recursive: true })
  log(`判定用の動画フィクスチャを生成中... (${videoPath})`)
  execFileSync(
    'ffmpeg',
    [
      '-y',
      '-f',
      'lavfi',
      '-i',
      'testsrc=size=160x90:rate=2',
      '-t',
      '120',
      '-c:v',
      'libvpx',
      '-b:v',
      '30k',
      '-pix_fmt',
      'yuv420p',
      videoPath,
    ],
    { stdio: 'ignore' },
  )
  return existsSync(videoPath) ? videoPath : undefined
}

async function apiHandler({ path: apiPath, url, json, route }) {
  const method = route.request().method()
  if (apiPath === '/api/sites') return json(['default'])
  if (apiPath === '/api/capabilities') return json({ live: false })
  if (apiPath === '/api/breakers') return json([])
  if (apiPath === '/api/encode-profiles' || apiPath === '/api/rules') return json([])
  if (apiPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (apiPath === '/api/capacity/overages') return json([])
  if (apiPath === '/api/events') return sseKeepAlive(route)
  if (apiPath === '/api/recordings' && method === 'GET') {
    return json(url.searchParams.get('trash') === 'true' ? [] : [recording])
  }
  if (/^\/api\/recordings\/1$/.test(apiPath) && method === 'GET') return json(recording)
  if (/^\/api\/media\/recordings\/1\/file$/.test(apiPath)) {
    return route.fulfill({ status: 200, contentType: 'video/webm', body: videoBytes })
  }
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(apiPath)) {
    return route.fulfill({ status: 404 })
  }
  if (/^\/api\/media\/recordings\/1\/seek-tiles$/.test(apiPath)) {
    if (!serveTiles) return route.fulfill({ status: 404 })
    return route.fulfill({ status: 200, contentType: 'image/png', body: TILE_PNG })
  }
  return json([])
}

/** moveToTile は「対応するタイルが出るはずの位置」へポインタを動かす。 */
async function moveToSeconds(page, videoBox, duration, seconds) {
  await page.mouse.move(
    videoBox.x + videoBox.width * (seconds / duration),
    videoBox.y + videoBox.height / 2,
  )
}

/** expectedTile は位置から期待する格子オフセットを独立に計算する。 */
function expectedTile(seconds) {
  const index = Math.floor(seconds / TILE_INTERVAL_SECONDS)
  return {
    index,
    x: -(index % TILE_COLUMNS) * TILE_DISPLAY_WIDTH,
    y: -Math.floor(index / TILE_COLUMNS) * TILE_DISPLAY_HEIGHT,
  }
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const videoPath = ensureFixture()
if (videoPath === undefined) {
  log('  ffmpeg が無いため、シークプレビューの実ブラウザ判定は測れない（skip）')
  await finish(ng)
}
const videoBytes = readFileSync(videoPath)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)

const openPlayer = async () => {
  await page.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
  const video = page.locator('video')
  await video.waitFor({ timeout: 15000 })
  // duration が確定するまで待つ（未確定だと実装は位置を出さない）。
  await page.waitForFunction(
    () => {
      const el = document.querySelector('video')
      return Number.isFinite(el?.duration) && el.duration > 0
    },
    undefined,
    { timeout: 15000 },
  )
  return video
}

log('\n=== ④ タイルが無い録画ではプレビューを出さず、再生面は従来のまま ===')
const noTilesVideo = await openPlayer()
const noTilesBox = await noTilesVideo.boundingBox()
// タイルは 404 なので読み込まれない。duration は分かっているので、実装が
// 「タイルの有無を見ずに出す」ならここで出てしまう。
await moveToSeconds(page, noTilesBox, 120, 35)
await page.waitForTimeout(500)
if ((await page.locator('[data-testid="seek-tile-preview"]').count()) !== 0) {
  ng.push('④ タイルが 404 なのにプレビューが出ている')
}
const srcWithoutTiles = await noTilesVideo.getAttribute('src')
if (srcWithoutTiles !== '/api/media/recordings/1/file?profile=h264') {
  ng.push(`④ タイルが無いだけで再生面が変わっている（src=${srcWithoutTiles}）`)
}
if ((await noTilesVideo.evaluate((v) => v.duration)) <= 0) {
  ng.push('④ タイルが無いと再生面が壊れている（duration が取れない）')
}

log('\n=== ①② タイルがあるときのホバー位置 → タイル ===')
serveTiles = true
const video = await openPlayer()
const duration = await video.evaluate((v) => v.duration)
if (Math.abs(duration - 120) > 5) {
  ng.push(`フィクスチャの長さが想定と違う（duration=${duration}）--- 判定の前提が崩れている`)
}
const videoBox = await video.boundingBox()
if (!videoBox || videoBox.width <= 0) {
  ng.push('動画の矩形が取れない（レイアウトが想定と違う）')
  await finish(ng, browser)
}

/** hoverTile は 1 枚ぶんの位置へポインタを送り、プレビューが出るまで待つ。 */
async function hoverTile(seconds) {
  const want = expectedTile(seconds)
  // タイルの中ほどを狙う（端に寄せると duration の丸めで隣のタイルになる）。
  const target = want.index * TILE_INTERVAL_SECONDS + 5
  await moveToSeconds(page, videoBox, duration, target)
  // 1 度目はタイルの取得が始まるだけなので、届くまで待ってからもう一度動かす。
  await page
    .waitForFunction(() => (document.querySelector('img[src*="/seek-tiles"]')?.naturalWidth ?? 0) > 0, undefined, {
      timeout: 15000,
    })
    .catch(() => {})
  await moveToSeconds(page, videoBox, duration, target)
  return want
}

// 列の折り返し（3 → 9）と行送り（10）を別々の位置で押さえる。
for (const seconds of [35, 95, 105]) {
  const want = await hoverTile(seconds)
  const preview = page.locator('[data-testid="seek-tile-preview"]')
  try {
    await preview.waitFor({ timeout: 5000 })
  } catch {
    ng.push(`① ${seconds}s 相当（タイル #${want.index}）の位置でプレビューが出ない`)
    continue
  }

  const { position, image } = await preview.locator('> div').evaluate((el) => {
    const style = getComputedStyle(el)
    return { position: style.backgroundPosition, image: style.backgroundImage }
  })
  const wantPosition = `${want.x}px ${want.y}px`
  if (position !== wantPosition) {
    ng.push(`① タイル #${want.index} の background-position が ${position}（期待 ${wantPosition}）`)
  }
  if (!image.includes('/api/media/recordings/1/seek-tiles')) {
    ng.push(`① プレビューの背景がタイル配信を指していない（${image}）`)
  }

  // ② 動画の矩形の中に収まる（はみ出さない）。
  const previewBox = await preview.boundingBox()
  if (!previewBox) {
    ng.push(`② タイル #${want.index} のプレビューに実寸が無い`)
    continue
  }
  const videoRight = videoBox.x + videoBox.width
  const videoBottom = videoBox.y + videoBox.height
  if (previewBox.x < videoBox.x - 1 || previewBox.x + previewBox.width > videoRight + 1) {
    ng.push(
      `② タイル #${want.index} のプレビューが横にはみ出している` +
        `（preview ${previewBox.x}..${previewBox.x + previewBox.width} / video ${videoBox.x}..${videoRight}）`,
    )
  }
  if (previewBox.y < videoBox.y - 1 || previewBox.y + previewBox.height > videoBottom + 1) {
    ng.push(`② タイル #${want.index} のプレビューが縦にはみ出している`)
  }
}

log('\n=== ③ ポインタが離れると消える ===')
await hoverTile(35)
const backOnVideo = await page
  .locator('[data-testid="seek-tile-preview"]')
  .waitFor({ timeout: 5000 })
  .then(() => true)
  .catch(() => false)
if (!backOnVideo) {
  ng.push('③ 動画の上に戻してもプレビューが出ない（①②の待ちが失敗している疑い）')
}
// 動画の外（上のヘッダー側）へ逃がす。
await page.mouse.move(videoBox.x + videoBox.width / 2, Math.max(2, videoBox.y - 60))
await page.waitForTimeout(300)
if ((await page.locator('[data-testid="seek-tile-preview"]').count()) !== 0) {
  ng.push('③ ポインタが動画から離れてもプレビューが残っている')
}

await finish(ng, browser)
