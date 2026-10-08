// ホームの主役が保存位置のフレームを見せるかを実ブラウザで判定する。
//
// フレーム番号ごとに位置が違う白い目印を付けた encoded MP4 を使い、見えている
// ピクセルからシーク先を判定する。カット版も同じ MP4 から keepRanges に沿って
// 作り、原本時間の再開位置を asset 固有の区間で写した場合だけ正しい目印が出る。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:home-resume-frame
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:home-resume-frame
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, statSync } from 'node:fs'
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

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const BROWSER = process.env.E2E_BROWSER ?? 'chromium'
const RECORDING_ID = 1277
const PROFILE = 'h264'
const CUT_PROFILE = 'h264-cut'
const UNAVAILABLE_PROFILE = 'unavailable'
const MARKER_FRAME = 492
const MARKER_SLOT = 6
const CUT_KEEP_RANGES = [
  { startMs: 0, endMs: 5_400 },
  { startMs: 10_800, endMs: 22_000 },
]
const MARKER_PIXELS = [
  [16, 9], [32, 9], [48, 9],
  [16, 18], [32, 18], [48, 18],
  [16, 27], [32, 27], [48, 27],
]
const FALLBACK_IMAGE = Buffer.from(
  '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="9"><rect width="16" height="9" fill="#31506b"/></svg>',
)
const ng = []
const now = Date.now()
const startedAt = new Date(now - 30_000).toISOString()
const baseRecording = {
  id: RECORDING_ID,
  site: 'default',
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'ホームの再開位置フレーム確認',
  description: '保存位置のフレームを表示する e2e fixture。',
  startAt: startedAt,
  durationMs: 22_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  startedAt,
  endedAt: new Date(now - 8_000).toISOString(),
  createdAt: new Date(now - 60_000).toISOString(),
  resumePositionMs: 16_416,
  sizeBytes: 2_000_000,
  encodedAssets: [{ profile: PROFILE, sizeBytes: 1_000_000 }],
}

function runFFmpeg(args) {
  execFileSync('ffmpeg', args, { stdio: 'pipe' })
}

function makeFixtures() {
  const fixtureDir = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-home-resume-'))
  execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  const repoRoot = path.resolve(process.cwd(), '..')
  execFileSync('go', ['test', './internal/worker', '-run', '^TestWritePlaybackTimelineFixture$', '-count=1'], {
    cwd: repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR: fixtureDir },
  })

  const manifest = JSON.parse(readFileSync(path.join(fixtureDir, 'manifest.json'), 'utf8'))
  const encodedPath = path.join(fixtureDir, manifest.encoded)
  const encodedProbe = JSON.parse(execFileSync('ffprobe', [
    '-v', 'error', '-select_streams', 'v:0', '-show_streams', '-show_frames',
    '-show_entries', 'stream=start_time:frame=pts_time,best_effort_timestamp_time', '-of', 'json', encodedPath,
  ], { encoding: 'utf8' }))
  const encodedTimes = encodedProbe.frames.map((frame) => Number(frame.best_effort_timestamp_time ?? frame.pts_time))
  const markerIndex = manifest.markerFrames.indexOf(MARKER_FRAME)
  const videoStart = Number(encodedProbe.streams[0]?.start_time)
  const targetSeconds = encodedTimes[MARKER_FRAME] - videoStart
  if (markerIndex !== MARKER_SLOT || !Number.isFinite(targetSeconds)) {
    throw new Error(`目印 frame ${MARKER_FRAME} の encoded 時刻を得られない`)
  }

  // The production encoded fixture has an intentional 700 ms audio lead. Normalize
  // the video track to zero so fixture keepRanges and browser currentTime share an axis.
  const resumeEncodedPath = path.join(fixtureDir, 'encoded-resume.mp4')
  runFFmpeg([
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y', '-i', encodedPath,
    '-map', '0:v:0', '-an', '-vf', 'setpts=PTS-STARTPTS', '-c:v', 'libx264',
    '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', resumeEncodedPath,
  ])

  const cutPath = path.join(fixtureDir, 'encoded-cut.mp4')
  runFFmpeg([
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y', '-i', resumeEncodedPath,
    '-filter_complex',
    '[0:v]trim=start=0:end=5.4,setpts=PTS-STARTPTS[first];[0:v]trim=start=10.8:end=22,setpts=PTS-STARTPTS[second];[first][second]concat=n=2:v=1:a=0[outv]',
    '-map', '[outv]', '-an', '-c:v', 'libx264', '-preset', 'ultrafast',
    '-pix_fmt', 'yuv420p', '-movflags', '+faststart', cutPath,
  ])

  const markerPixels = MARKER_PIXELS
  return {
    fixtureDir,
    manifest,
    encodedPath: resumeEncodedPath,
    encodedBytes: readFileSync(resumeEncodedPath),
    cutBytes: readFileSync(cutPath),
    // Place the saved point in the middle of frame 492's presentation interval, not on its lower edge.
    targetPositionMs: Math.round(targetSeconds * 1000) + 16,
    markerPixels,
  }
}

const cutRecording = {
  ...baseRecording,
  encodedAssets: [{
    profile: CUT_PROFILE,
    sizeBytes: 1_000_000,
    cut: true,
    keepRanges: CUT_KEEP_RANGES,
  }],
}
await validateFixturesOrExit([
  ['non-cut continue-watching recording', ListRecordingsResponseItem, baseRecording],
  ['cut continue-watching recording', ListRecordingsResponseItem, cutRecording],
], ng)

const fixture = makeFixtures()
baseRecording.resumePositionMs = fixture.targetPositionMs
cutRecording.resumePositionMs = fixture.targetPositionMs
baseRecording.encodedAssets[0].sizeBytes = statSync(fixture.encodedPath).size
cutRecording.encodedAssets[0].sizeBytes = statSync(path.join(fixture.fixtureDir, 'encoded-cut.mp4')).size

log(`URL: ${URL_BASE}`)
await verifyBundleMatchesOrExit(URL_BASE, ng)
const browser = await launchBrowser(BROWSER)
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
await context.addInitScript((markerPixels) => {
  window.__homeResumeMarker = () => {
    const video = document.querySelector('[data-testid="home-hero-resume-video"]')
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    if (!video || !ctx || video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return { slot: -2 }
    try {
      ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
      const slot = markerPixels.findIndex(([x, y]) => {
        const pixel = ctx.getImageData(x, y, 1, 1).data
        return pixel[0] > 210 && pixel[1] > 210 && pixel[2] > 210
      })
      return { slot, currentTime: video.currentTime, paused: video.paused }
    } catch (error) {
      return { slot: -3, error: String(error) }
    }
  }
}, fixture.markerPixels)

let activeRecording = baseRecording
const page = await context.newPage()
const rangeRequests = []
const delayedProfiles = new Set()
await installApiStubs(page, async ({ path: apiPath, url, json, route }) => {
  const method = route.request().method()
  if (apiPath === '/api/events') return sseKeepAlive(route)
  if (apiPath === '/api/recordings/continue-watching' && method === 'GET') return json([activeRecording])
  if (apiPath === '/api/recordings' && method === 'GET') {
    const status = url.searchParams.get('status')
    return json(status === 'recording' ? [] : [])
  }
  if (apiPath === '/api/reservations' || apiPath === '/api/breakers' || apiPath === '/api/capacity/overages') return json([])
  if (apiPath === '/api/storage') return json([])
  if (apiPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (apiPath === `/api/media/recordings/${RECORDING_ID}/thumbnail`) {
    return route.fulfill({ status: 200, contentType: 'image/svg+xml', body: FALLBACK_IMAGE })
  }
  if (apiPath === `/api/media/recordings/${RECORDING_ID}/file`) {
    if (url.searchParams.get('profile') === UNAVAILABLE_PROFILE) return route.fulfill({ status: 404 })
    const profile = url.searchParams.get('profile')
    // Hold the first response briefly so the browser can observe the fallback while the seek is pending.
    if (!delayedProfiles.has(profile)) {
      delayedProfiles.add(profile)
      await new Promise((resolve) => setTimeout(resolve, 700))
    }
    const bytes = profile === CUT_PROFILE ? fixture.cutBytes : fixture.encodedBytes
    const range = /^bytes=(\d+)-(\d*)$/.exec(route.request().headers().range ?? '')
    if (!range) {
      return route.fulfill({
        status: 200,
        contentType: 'video/mp4',
        body: bytes,
        headers: { 'Accept-Ranges': 'bytes', 'Content-Length': String(bytes.length) },
      })
    }
    const start = Number(range[1])
    const end = Math.min(range[2] ? Number(range[2]) : bytes.length - 1, bytes.length - 1)
    if (start >= bytes.length || end < start) {
      return route.fulfill({ status: 416, headers: { 'Content-Range': `bytes */${bytes.length}` } })
    }
    rangeRequests.push({ profile: url.searchParams.get('profile'), start, end })
    return route.fulfill({
      status: 206,
      contentType: 'video/mp4',
      body: bytes.subarray(start, end + 1),
      headers: {
        'Accept-Ranges': 'bytes',
        'Content-Range': `bytes ${start}-${end}/${bytes.length}`,
        'Content-Length': String(end - start + 1),
      },
    })
  }
  if (apiPath === '/api/sites') return json(['default'])
  if (apiPath === '/api/capabilities') return json({ live: false })
  return json([])
})

async function checkHero(label, recording, profile) {
  activeRecording = recording
  rangeRequests.length = 0
  await page.goto(`${URL_BASE}/?mode=watch`, { waitUntil: 'domcontentloaded' })
  const thumbnail = page.getByTestId('home-next-watch-thumbnail')
  await thumbnail.waitFor({ timeout: 15_000 })
  const video = page.getByTestId('home-hero-resume-video')
  const present = await video.count()
  if (present === 0) {
    ng.push(`${label}: ホームの主役に encoded video を置かない`)
    return
  }
  await page.waitForFunction(() => {
    const image = document.querySelector('[data-testid="home-next-watch-thumbnail"] img')
    return image?.complete && image.naturalWidth > 0
  }, undefined, { timeout: 10_000 }).catch(() => {})
  const fallback = await thumbnail.locator('img').evaluate((image) => ({
    complete: image.complete,
    naturalWidth: image.naturalWidth,
    display: getComputedStyle(image).display,
    visibility: getComputedStyle(image).visibility,
    opacity: getComputedStyle(image).opacity,
  }))
  if (
    !fallback.complete || fallback.naturalWidth === 0 || fallback.display === 'none' ||
    fallback.visibility === 'hidden' || fallback.opacity === '0'
  ) {
    ng.push(`${label}: video の代わりに見せる固定サムネイルが表示されない (${JSON.stringify(fallback)})`)
  }

  const media = await video.evaluate((element) => {
    const style = getComputedStyle(element)
    return {
      controls: element.controls,
      muted: element.muted,
      playsInline: element.playsInline,
      preload: element.preload,
      paused: element.paused,
      pointerEvents: style.pointerEvents,
      src: element.getAttribute('src'),
      opacity: style.opacity,
      resumeState: element.getAttribute('data-resume-state'),
      requestVideoFrameCallback: typeof element.requestVideoFrameCallback,
      readyState: element.readyState,
      duration: element.duration,
      error: element.error ? { code: element.error.code, message: element.error.message } : null,
    }
  })
  if (media.resumeState !== 'waiting') ng.push(`${label}: fixture の取得中に waiting 状態を観測できない (${media.resumeState})`)
  if (media.opacity !== '0') ng.push(`${label}: 目印フレームが出る前に video が見える (${media.opacity})`)
  if (media.controls) ng.push(`${label}: resume video に controls が付いている`)
  if (!media.muted) ng.push(`${label}: resume video が muted でない`)
  if (!media.playsInline) ng.push(`${label}: resume video が playsInline でない`)
  if (media.preload !== 'metadata') ng.push(`${label}: resume video の preload が metadata ではない (${media.preload})`)
  if (media.pointerEvents !== 'none') ng.push(`${label}: resume video が pointer-events-none ではない (${media.pointerEvents})`)
  if (!media.paused) ng.push(`${label}: home 表示中に video が再生を始めた`)
  if (!media.src?.includes(`/api/media/recordings/${RECORDING_ID}/file?profile=${profile}`)) {
    ng.push(`${label}: encoded file の profile が選ばれていない (${media.src})`)
  }

  try {
    await page.waitForFunction((slot) => window.__homeResumeMarker()?.slot === slot, MARKER_SLOT, { timeout: 10_000 })
    await page.waitForFunction(() => {
      const video = document.querySelector('[data-testid="home-hero-resume-video"]')
      return video !== null && Number(getComputedStyle(video).opacity) > 0.9
    }, undefined, { timeout: 3000 })
    const marker = await page.evaluate(() => window.__homeResumeMarker())
    if (marker.slot !== MARKER_SLOT) ng.push(`${label}: 表示中の目印が違う (${JSON.stringify(marker)})`)
    if (!marker.paused) ng.push(`${label}: 目印フレーム表示後に video が再生中 (${JSON.stringify(marker)})`)
    log(`  ${label}: marker slot=${marker.slot}, t=${marker.currentTime?.toFixed(3)}s, paused=${marker.paused}`)
  } catch (error) {
    const state = await video.evaluate((element) => ({
      currentTime: element.currentTime,
      duration: element.duration,
      readyState: element.readyState,
      seeking: element.seeking,
      paused: element.paused,
      resumeState: element.getAttribute('data-resume-state'),
      requestVideoFrameCallback: typeof element.requestVideoFrameCallback,
      error: element.error ? { code: element.error.code, message: element.error.message } : null,
      marker: window.__homeResumeMarker(),
    }))
    ng.push(`${label}: 保存位置の目印 frame ${MARKER_FRAME} が見えない (${error.message}; ${JSON.stringify(state)})`)
  }
  if (rangeRequests.length === 0) ng.push(`${label}: encoded MP4 への Range 要求がない`)
  try {
    const navigation = page.waitForURL((url) => url.pathname === `/recordings/${RECORDING_ID}`, { timeout: 5_000 })
    await thumbnail.click()
    await navigation
    log(`  ${label}: サムネイルを押すと /recordings/${RECORDING_ID} へ移動`)
  } catch (error) {
    ng.push(`${label}: サムネイルを押しても録画詳細へ移らない (${error.message})`)
  }
}

async function checkVideoErrorFallback() {
  activeRecording = {
    ...baseRecording,
    encodedAssets: [{ profile: UNAVAILABLE_PROFILE, sizeBytes: 1_000 }],
  }
  await page.goto(`${URL_BASE}/?mode=watch`, { waitUntil: 'domcontentloaded' })
  const thumbnail = page.getByTestId('home-next-watch-thumbnail')
  await thumbnail.waitFor({ timeout: 15_000 })
  const video = page.getByTestId('home-hero-resume-video')
  try {
    await page.waitForFunction(() => (
      document.querySelector('[data-testid="home-hero-resume-video"]')?.getAttribute('data-resume-state') === 'failed'
    ), undefined, { timeout: 10_000 })
  } catch (error) {
    ng.push(`動画の取得失敗後に固定サムネイルへ戻らない (${error.message})`)
    return
  }
  const fallback = await thumbnail.locator('img').evaluate((image) => ({
    complete: image.complete,
    width: image.naturalWidth,
    display: getComputedStyle(image).display,
    visibility: getComputedStyle(image).visibility,
    opacity: getComputedStyle(image).opacity,
  }))
  const videoState = await video.evaluate((element) => ({
    src: element.getAttribute('src'),
    opacity: getComputedStyle(element).opacity,
  }))
  if (!fallback.complete || fallback.width === 0 || fallback.display === 'none' || fallback.visibility === 'hidden' || fallback.opacity === '0') {
    ng.push(`動画失敗時に固定サムネイルが見えない (${JSON.stringify(fallback)})`)
  }
  if (videoState.src !== null || Number(videoState.opacity) !== 0) {
    ng.push(`動画失敗時に resume video が固定画像の上へ残る (${JSON.stringify(videoState)})`)
  }
  log(`  動画失敗時: fallback image ${fallback.width}x${fallback.complete ? 'loaded' : 'loading'}`)
}

log(`\n=== 実ブラウザ: ${BROWSER} ===`)
await checkHero('非カット版', baseRecording, PROFILE)
await checkHero('カット版', cutRecording, CUT_PROFILE)
await checkVideoErrorFallback()
await finish(ng, browser)
