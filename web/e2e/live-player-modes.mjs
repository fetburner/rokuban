// LivePlayer の live / chase / original-vod 各モードで、離脱時のヒントと
// 503 の表示が実ブラウザで方式に合うこと、追っかけの表示時刻が録画先頭 + media time に
// 対応することを測る。各判定は browser-e2e で実行する。
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readdirSync, readFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { ListRecordingsResponseItem } from '../src/api/zod.ts'
import { finish, launchBrowser, log, validateFixturesOrExit, verifyBundleMatchesOrExit } from './lib.mjs'

const BASE_URL = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = 'default'
const CHASE_ID = 14701
const ORIGINAL_ID = 14702
const ng = []

const programStartMs = Date.now() - 25 * 60_000
const recordingStartMs = programStartMs + 20 * 60_000
const programStartAt = new Date(programStartMs).toISOString()
const recordingStartedAt = new Date(recordingStartMs).toISOString()
const originalEndedAt = new Date(recordingStartMs + 20_000).toISOString()

const chaseRecording = {
  id: CHASE_ID,
  site: SITE,
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 1,
  serviceId: 9001,
  eventId: 1,
  title: 'LivePlayer chase mode fixture',
  description: '実メディア上の追っかけ時間軸と離脱ヒントの fixture。',
  startAt: programStartAt,
  durationMs: 60 * 60_000,
  status: 'recording',
  keepOriginal: 'always',
  hlsPlayback: [], cmDetection: { state: 'disabled' },
  startedAt: recordingStartedAt,
  createdAt: '2026-01-01T12:00:00Z',
  encodeProfiles: ['vod-h264'],
  sizeBytes: 1_000_000,
  encodedAssets: [],
}

const originalRecording = {
  ...chaseRecording,
  id: ORIGINAL_ID,
  title: 'LivePlayer original-vod mode fixture',
  description: '原本 VOD offset の離脱ヒントを収集する fixture。',
  status: 'finished',
  startAt: recordingStartedAt,
  durationMs: 20_000,
  startedAt: recordingStartedAt,
  endedAt: originalEndedAt,
  resumePositionMs: 12_000,
}

const program = {
  programId: 201,
  networkId: 1,
  serviceId: 9001,
  eventId: 1,
  recordingId: CHASE_ID,
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
  ['chase recording', ListRecordingsResponseItem, chaseRecording],
  ['original-vod recording', ListRecordingsResponseItem, originalRecording],
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
    if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
    if (requestPath === '/api/recordings') return json([chaseRecording, originalRecording])
    if (requestPath === `/api/recordings/${CHASE_ID}`) return json(chaseRecording)
    if (requestPath === `/api/recordings/${ORIGINAL_ID}`) return json(originalRecording)
    if (/^\/api\/recordings\/\d+\/chapters$/.test(requestPath)) {
      return json({ version: 'chapters-v1', detectionPending: false, source: 'auto', spans: [] })
    }
    if (method !== 'GET' && !requestPath.endsWith('/leave')) return route.fulfill({ status: 204 })

    if (requestPath.endsWith('/leave') && method === 'POST') return route.fulfill({ status: 204 })
    if (/\/live\/playlist\.m3u8$/.test(requestPath) ||
      /\/(?:chase|original-vod)(?:\/offset\/\d+)?\/playlist\.m3u8$/.test(requestPath)) {
      let requestMode = 'live'
      if (requestPath.includes('/original-vod/')) requestMode = 'original-vod'
      else if (requestPath.includes('/chase/')) requestMode = 'chase'
      if (requestMode === playlistErrorMode) {
        return route.fulfill({
          status: 503,
          contentType: 'text/plain',
          body: 'too many concurrent live sessions on this process',
        })
      }
      return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: master })
    }
    if (/\/(?:live|chase|original-vod)(?:\/offset\/\d+)?\/variant\.m3u8$/.test(requestPath)) {
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

async function openMode(mode) {
  const page = await context.newPage()
  const { leaveRequests, mediaRequests } = await installRoutes(page)
  if (mode === 'live') {
    await page.goto(`${BASE_URL}/live?site=${SITE}&service=109001`, { waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: /再生/ }).click()
  } else if (mode === 'chase') {
    await page.goto(`${BASE_URL}/recordings/${CHASE_ID}#chase`, { waitUntil: 'domcontentloaded' })
  } else {
    await page.goto(`${BASE_URL}/recordings/${ORIGINAL_ID}`, { waitUntil: 'domcontentloaded' })
    await page.getByTestId('recording-playback-start').click()
  }
  await waitForRealPlayback(page, mode)
  await page.waitForTimeout(250)
  return { page, leaveRequests, mediaRequests }
}

async function check503Mode(mode) {
  const page = await context.newPage()
  await installRoutes(page, mode)
  if (mode === 'live') {
    await page.goto(`${BASE_URL}/live?site=${SITE}&service=109001`, { waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: /再生/ }).click()
  } else if (mode === 'chase') {
    await page.goto(`${BASE_URL}/recordings/${CHASE_ID}#chase`, { waitUntil: 'domcontentloaded' })
  } else {
    await page.goto(`${BASE_URL}/recordings/${ORIGINAL_ID}`, { waitUntil: 'domcontentloaded' })
    await page.getByTestId('recording-playback-start').click()
  }

  const expectedByMode = {
    live: {
      title: 'いま視聴できません（チューナー不足または同時視聴数の上限）。',
      guidance: /チャンネルを切り替えた直後は/,
    },
    chase: {
      title: 'いま追っかけ再生を始められません。',
      guidance: 'しばらく待ってから再読み込みしてください。',
    },
    'original-vod': {
      title: 'いま再生を始められません（同時再生数の上限、または streamer の再起動中）。',
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
for (const scenario of [
  {
    mode: 'live',
    expectedPath: `/api/sites/${SITE}/networks/1/services/9001/live/leave`,
  },
  {
    mode: 'chase',
    expectedPath: `/api/sites/${SITE}/recordings/${CHASE_ID}/chase/leave`,
  },
  {
    mode: 'original-vod',
    expectedPath: `/api/sites/${SITE}/recordings/${ORIGINAL_ID}/original-vod/offset/12/leave`,
  },
]) {
  log(`\n=== ${scenario.mode}: hidden / pagehide ===`)
  const { page, leaveRequests, mediaRequests } = await openMode(scenario.mode)

  if (scenario.mode === 'chase') {
    await page.waitForFunction(() => (document.querySelector('video')?.currentTime ?? 0) > 5.5, undefined, {
      timeout: 10_000,
    }).catch(() => ng.push('chase: 実メディアの currentTime が 5.5 秒に達しない'))
    const originCheck = await page.evaluate(() => {
      const video = document.querySelector('video')
      const output = document.querySelector('[data-testid="playback-time"]')
      const match = /^\s*(\d+):(\d{2})/.exec(output?.textContent ?? '')
      return {
        mediaSeconds: video?.currentTime ?? Number.NaN,
        displayedSeconds: match ? Number(match[1]) * 60 + Number(match[2]) : Number.NaN,
      }
    })
    const headSeconds = (Date.parse(recordingStartedAt) - Date.parse(programStartAt)) / 1000
    const expectedSeconds = Math.floor(headSeconds + originCheck.mediaSeconds)
    if (!Number.isFinite(originCheck.displayedSeconds) ||
      Math.abs(originCheck.displayedSeconds - expectedSeconds) > 2 || originCheck.mediaSeconds < 1) {
      ng.push(
        `chase: 録画先頭 ${headSeconds}s + 実 media time ${originCheck.mediaSeconds.toFixed(2)}s と表示時刻が対応しない ` +
          `(表示=${originCheck.displayedSeconds}, 期待=${expectedSeconds})`,
      )
    }
  }

  if (scenario.mode === 'original-vod' &&
    !mediaRequests.some((pathname) => pathname.includes(`/original-vod/offset/12/playlist.m3u8`))) {
    const requested = mediaRequests.filter((pathname) => pathname.includes('/original-vod/'))
    ng.push(`original-vod: 続き位置 12 秒の offset playlist を要求しない（${JSON.stringify(requested)}）`)
  }
  await checkNoLeaveOnVisible(page, leaveRequests, scenario.expectedPath, scenario.mode)
  await triggerAndCheck(page, leaveRequests, scenario.expectedPath, 'visibilitychange', scenario.mode)
  await triggerAndCheck(page, leaveRequests, scenario.expectedPath, 'pagehide', scenario.mode)
  await page.close()
}

for (const mode of ['live', 'chase', 'original-vod']) {
  log(`\n=== ${mode}: 503 error copy ===`)
  await check503Mode(mode)
}

await context.close()
await finish(ng, browser)
