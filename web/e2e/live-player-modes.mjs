// LivePlayer の live で離脱時のヒント（pagehide / visibilitychange(hidden) は navigator.sendBeacon で
// 実際に送り、visible では送らない）が実ブラウザで届くこと、live と録画 HLS の 503 の文言が方式に合うことを測る。
// jsdom には sendBeacon が無く fetch に落ちるので、離脱ヒントの送信経路は実ブラウザでしか測れない。
// 録画 HLS は離脱ヒントを持たない（セッションを作らない）。各判定は browser-e2e で実行する。
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readdirSync, readFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { ListRecordingsResponseItem } from '../src/api/zod.ts'
import { finish, launchBrowser, log, validateFixturesOrExit, verifyBundleMatchesOrExit, recordingMedia } from './lib.mjs'

const BASE_URL = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = 'default'
const RECORDING_ID = 14701
const ng = []

const programStartMs = Date.now() - 25 * 60_000
const recordingStartMs = programStartMs + 20 * 60_000
const programStartAt = new Date(programStartMs).toISOString()
const recordingStartedAt = new Date(recordingStartMs).toISOString()
const recordingMasterUrl = `/api/media/stores/media/recordings/${RECORDING_ID}/hls/v1/hd/master.m3u8`

// 録画 HLS の 503 を見る録画詳細の fixture。
const hlsRecording = {
  id: RECORDING_ID,
  site: SITE,
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 1,
  serviceId: 9001,
  eventId: 1,
  title: 'LivePlayer recording HLS fixture',
  description: '録画 HLS の 503 文言を測る fixture。',
  startAt: programStartAt,
  durationMs: 60 * 60_000,
  status: 'recording',
  keepOriginal: 'always',
  media: recordingMedia(RECORDING_ID, []), hlsPlayback: [{ profile: 'hd', masterUrl: recordingMasterUrl }],
  cmDetection: { state: 'disabled' },
  startedAt: recordingStartedAt,
  createdAt: '2026-01-01T12:00:00Z',
  encodeProfiles: ['vod-h264'],
  sizeBytes: 1_000_000,
  encodedAssets: [],
}

const program = {
  programId: 201,
  networkId: 1,
  serviceId: 9001,
  eventId: 1,
  startAt: programStartAt,
  endAt: new Date(programStartMs + 60 * 60_000).toISOString(),
  durationMs: 60 * 60_000,
  name: 'テスト番組',
  description: '',
  genres: [],
  isFree: true,
}

const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-live-player-modes')
const segmentsDir = path.join(fixtureDir, 'segments')
const variantPath = path.join(fixtureDir, 'variant.m3u8')

function ensureFixture() {
  if (existsSync(variantPath) && readdirSync(segmentsDir).some((name) => name.endsWith('.ts'))) return

  mkdirSync(segmentsDir, { recursive: true })
  execFileSync('ffmpeg', [
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc2=size=320x180:rate=25:duration=20',
    '-f', 'lavfi', '-i', 'sine=frequency=440:duration=20',
    '-map', '0:v:0', '-map', '1:a:0',
    '-c:v', 'libx264', '-profile:v', 'baseline', '-level', '3.0', '-pix_fmt', 'yuv420p',
    '-preset', 'ultrafast', '-g', '25', '-sc_threshold', '0',
    '-c:a', 'aac', '-b:a', '64k',
    '-f', 'hls', '-hls_time', '1', '-hls_list_size', '0', '-hls_flags', 'independent_segments',
    '-hls_segment_filename', path.join(segmentsDir, 'segment_%03d.ts'),
    variantPath,
  ], { stdio: 'ignore' })
}

await validateFixturesOrExit([
  ['recording HLS recording', ListRecordingsResponseItem, hlsRecording],
], ng)
await verifyBundleMatchesOrExit(BASE_URL, ng)

try {
  execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  ensureFixture()
} catch (error) {
  ng.push(`FFmpeg fixture を生成できない: ${error.message}`)
  await finish(ng)
}

const variant = readFileSync(variantPath)
const master = Buffer.from([
  '#EXTM3U',
  '#EXT-X-VERSION:3',
  '#EXT-X-STREAM-INF:BANDWIDTH=450000,RESOLUTION=320x180',
  'variant.m3u8',
  '',
].join('\n'))

const browser = await launchBrowser('chromium')
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
await context.addInitScript(() => {
  class E2EEventSource extends EventTarget {
    static CONNECTING = 0
    static OPEN = 1
    static CLOSED = 2
    readyState = E2EEventSource.OPEN
    withCredentials = false
    constructor(url) {
      super()
      this.url = String(url)
      queueMicrotask(() => this.dispatchEvent(new Event('open')))
    }
    close() { this.readyState = E2EEventSource.CLOSED }
  }
  window.EventSource = E2EEventSource
})

async function installRoutes(page, playlistErrorMode) {
  const leaveRequests = []
  const mediaRequests = []
  page.on('request', (request) => {
    const requestURL = new URL(request.url())
    if (request.method() === 'POST' && requestURL.pathname.endsWith('/leave')) {
      leaveRequests.push(requestURL.pathname)
    }
    if (/\.(?:m3u8|ts)$/.test(requestURL.pathname)) mediaRequests.push(requestURL.pathname)
  })

  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const requestURL = new URL(request.url())
    const requestPath = requestURL.pathname
    const method = request.method()
    const json = (body) => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(body),
    })

    if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
    if (requestPath === '/api/live-profiles') return json([{ name: 'hd', height: 720 }])
    if (requestPath === '/api/sites') return json([SITE])
    if (requestPath === `/api/sites/${SITE}/services`) {
      return json([{
        id: 109001,
        networkId: 1,
        serviceId: 9001,
        name: '放送局 A',
        channelType: 'GR',
        channel: '1',
        remoteControlKeyId: 1,
        hasLogoData: false,
        hasPrograms: true,
      }])
    }
    if (requestPath === `/api/sites/${SITE}/programs`) return json([program])
    if (requestPath === '/api/reservations' || requestPath === '/api/breakers' ||
      requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
    if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0, awaitingReview: 0 })
    if (requestPath === '/api/recordings') return json([hlsRecording])
    if (requestPath === `/api/recordings/${RECORDING_ID}`) return json(hlsRecording)
    if (/^\/api\/recordings\/\d+\/chapters$/.test(requestPath)) {
      return json({ version: 'chapters-v1', detectionPending: false, source: 'auto', spans: [] })
    }
    if (method !== 'GET' && !requestPath.endsWith('/leave')) return route.fulfill({ status: 204 })

    if (requestPath.endsWith('/leave') && method === 'POST') return route.fulfill({ status: 204 })
    const isLivePlaylist = /\/live\/playlist\.m3u8$/.test(requestPath)
    const isRecordingMaster = requestPath === recordingMasterUrl
    if (isLivePlaylist || isRecordingMaster) {
      const requestMode = isRecordingMaster ? 'recording-hls' : 'live'
      if (requestMode === playlistErrorMode) {
        return route.fulfill({
          status: 503,
          contentType: 'text/plain',
          body: 'too many concurrent live sessions on this process',
        })
      }
      return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: master })
    }
    if (/\/(?:live\/variant\.m3u8|hls\/v\d+\/[^/]+\/variant\.m3u8)$/.test(requestPath)) {
      return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: variant })
    }
    if (requestPath.endsWith('.ts')) {
      const filename = path.basename(requestPath)
      const filePath = path.join(segmentsDir, filename)
      if (!existsSync(filePath)) return route.fulfill({ status: 404 })
      return route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(filePath) })
    }
    return json([])
  })
  return { leaveRequests, mediaRequests }
}

async function waitForRealPlayback(page, label) {
  const metadata = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video !== null && video.readyState >= HTMLMediaElement.HAVE_METADATA
  }, undefined, { timeout: 10_000 }).then(() => true).catch(() => false)
  if (metadata) {
    await page.locator('video').evaluate(async (video) => {
      video.muted = true
      await video.play().catch(() => {})
    })
  }
  const started = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video !== null && video.readyState >= HTMLMediaElement.HAVE_METADATA &&
      !video.paused && video.currentTime > 1
  }, undefined, { timeout: 10_000 }).then(() => true).catch(() => false)
  if (!started) {
    const state = await page.evaluate(() => {
      const video = document.querySelector('video')
      return {
        video: video ? {
          src: video.getAttribute('src'),
          currentSrc: video.currentSrc,
          currentTime: video.currentTime,
          paused: video.paused,
          readyState: video.readyState,
          networkState: video.networkState,
          error: video.error?.message,
        } : null,
        text: document.body.innerText.slice(0, 240),
      }
    })
    ng.push(`${label}: 実メディアの再生が進まない（${JSON.stringify(state)}）`)
  }
  return started
}

async function openLive() {
  const page = await context.newPage()
  const { leaveRequests, mediaRequests } = await installRoutes(page)
  await page.goto(`${BASE_URL}/live?site=${SITE}&service=109001`, { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: /再生/ }).click()
  await waitForRealPlayback(page, 'live')
  await page.waitForTimeout(250)
  return { page, leaveRequests, mediaRequests }
}

async function check503Mode(mode) {
  const page = await context.newPage()
  await installRoutes(page, mode)
  if (mode === 'live') {
    await page.goto(`${BASE_URL}/live?site=${SITE}&service=109001`, { waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: /再生/ }).click()
  } else {
    await page.goto(`${BASE_URL}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
    await page.getByTestId('recording-playback-start').click()
  }

  const expectedByMode = {
    live: {
      title: 'いま視聴できません（チューナー不足または同時視聴数の上限）。',
      guidance: /チャンネルを切り替えた直後は/,
    },
    'recording-hls': {
      title: 'いま録画を再生できません。',
      guidance: 'しばらく待ってから再読み込みしてください。',
    },
  }
  const expected = expectedByMode[mode]
  const titleShown = await page.getByText(expected.title, { exact: true })
    .waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  const bodyShown = await page.getByText('too many concurrent live sessions on this process', { exact: true })
    .waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  const guidanceShown = await page.getByText(expected.guidance)
    .waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  if (!titleShown || !bodyShown || !guidanceShown) {
    ng.push(`${mode}: 503 の見出し・本文・案内を表示しない`)
  }
  if (mode !== 'live') {
    const text = await page.locator('body').innerText()
    if (/チューナー|チャンネルを切り替えた/.test(text)) {
      ng.push(`${mode}: 503 にライブのチューナー案内が表示された`)
    }
  }
  await page.close()
}

// visible への切替は離脱ではない。hidden の判定が無い実装（visible でも送る）を落とす。
async function checkNoLeaveOnVisible(page, leaveRequests, expectedPath, label) {
  const before = leaveRequests.length
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' })
    document.dispatchEvent(new Event('visibilitychange'))
  })
  await page.waitForTimeout(500)
  if (leaveRequests.length !== before) {
    ng.push(`${label}: visibilityState=visible の visibilitychange で leave を送った（${before} → ${leaveRequests.length}、${expectedPath}）`)
  }
}

async function triggerAndCheck(page, leaveRequests, expectedPath, eventType, label) {
  const before = leaveRequests.filter((pathname) => pathname === expectedPath).length
  const responseWaiter = page.waitForResponse((response) => {
    const request = response.request()
    return request.method() === 'POST' && new URL(request.url()).pathname === expectedPath
  }, { timeout: 5000 }).catch(() => null)
  await page.evaluate((type) => {
    if (type === 'visibilitychange') {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' })
      document.dispatchEvent(new Event('visibilitychange'))
    } else {
      window.dispatchEvent(new Event('pagehide'))
    }
  }, eventType)
  const response = await responseWaiter
  const after = leaveRequests.filter((pathname) => pathname === expectedPath).length
  if (!response || response.status() !== 204 || after !== before + 1) {
    ng.push(`${label}: ${eventType} で POST ${expectedPath} が 1 件増えない（${before} → ${after}）`)
  }
}

log(`browser: Chromium ${browser.version()}`)
log('\n=== live: hidden / pagehide ===')
{
  const expectedPath = `/api/sites/${SITE}/networks/1/services/9001/live/leave`
  const { page, leaveRequests } = await openLive()
  await checkNoLeaveOnVisible(page, leaveRequests, expectedPath, 'live')
  await triggerAndCheck(page, leaveRequests, expectedPath, 'visibilitychange', 'live')
  await triggerAndCheck(page, leaveRequests, expectedPath, 'pagehide', 'live')
  await page.close()
}

for (const mode of ['live', 'recording-hls']) {
  log(`\n=== ${mode}: 503 error copy ===`)
  await check503Mode(mode)
}

await context.close()
await finish(ng, browser)
