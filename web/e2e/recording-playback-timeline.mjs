// 原本 HLS と非カット MP4 が同じ原本時間軸を使うか、表示中のフレームで測る。
// 期待値はチャプターの軸である非カット MP4（encoded.mp4）の各目印フレームの PTS を
// ffprobe で読んで求める。原本 TS の「目印 PTS - 最早 start_time」は参考値としてログにだけ出す。
// fixture は Go のテストが製品の BuildOriginalVODFFmpegArgs / BuildFFmpegArgs で作る。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-playback-timeline
//
// 変異確認: E2E_TIMELINE_EXPECTED_SHIFT_FRAMES=1 を付けると、期待時刻を 1 フレーム
// ずらした判定が落ちる。
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, statSync } from 'node:fs'
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
const SITE = 'default'
const RECORDING_ID = 1067
const LIVE_PROFILE = 'hd'
const ENCODE_PROFILE = 'h264'
const SOURCE_FRAME_RATE = 30_000 / 1_001
const HALF_FRAME_SECONDS = 1_001 / 60_000
const INITIAL_HLS_SEGMENTS = 5
const EXPECTED_SHIFT_FRAMES = Number.parseInt(process.env.E2E_TIMELINE_EXPECTED_SHIFT_FRAMES ?? '0', 10) || 0
const ng = []
const now = Date.now()
const recordingSeconds = 22
const startedAt = new Date(now - 30_000).toISOString()
const endedAt = new Date(now - 8_000).toISOString()
const recording = {
  id: RECORDING_ID,
  site: SITE,
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '原本 HLS の再生時刻測定',
  description: '製品の引数ビルダーが作った fixture のフレーム時刻を測る。',
  startAt: startedAt,
  durationMs: recordingSeconds * 1000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  startedAt,
  endedAt,
  createdAt: '2026-10-03T00:00:00Z',
  encodeProfiles: [ENCODE_PROFILE],
  sizeBytes: 2_000_000,
  encodedAssets: [],
}
const encodedRecording = {
  ...recording,
  encodedAssets: [{ profile: ENCODE_PROFILE, sizeBytes: 1_000_000 }],
}

await validateFixturesOrExit([
  ['original-only recording', ListRecordingsResponseItem, recording],
  ['encoded recording', ListRecordingsResponseItem, encodedRecording],
], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const fixtureDir = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-playback-timeline-'))
const repoRoot = path.resolve(process.cwd(), '..')
log(`\n=== 製品ビルダーで時間軸 fixture を生成 (${fixtureDir}) ===`)
try {
  execFileSync('ffprobe', ['-version'], { stdio: 'ignore' })
  execFileSync('go', ['test', './internal/worker', '-run', '^TestWritePlaybackTimelineFixture$', '-count=1'], {
    cwd: repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR: fixtureDir },
  })
} catch (err) {
  ng.push(`Go の製品ビルダーから playback fixture を生成できない: ${err.message}`)
  await finish(ng)
}

const manifest = JSON.parse(readFileSync(path.join(fixtureDir, 'manifest.json'), 'utf8'))
const OFFSET_SECONDS = manifest.offsetSeconds
if (OFFSET_SECONDS !== 10) ng.push(`fixture offset が10秒ではない (${OFFSET_SECONDS})`)
const sourcePath = path.join(fixtureDir, manifest.source)
const encodedPath = path.join(fixtureDir, manifest.encoded)

function runFFprobe(args) {
  return JSON.parse(execFileSync('ffprobe', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }))
}

const probe = runFFprobe([
  '-v', 'error', '-show_streams', '-show_frames',
  '-show_entries', 'format=start_time:stream=codec_type,codec_name,start_time,r_frame_rate:frame=pts_time,best_effort_timestamp_time',
  '-select_streams', 'v:0', '-of', 'json', sourcePath,
])
const allStreams = runFFprobe([
  '-v', 'error', '-show_streams',
  '-show_entries', 'format=start_time:stream=codec_type,codec_name,start_time,r_frame_rate',
  '-of', 'json', sourcePath,
])
const videoStream = allStreams.streams.find((stream) => stream.codec_type === 'video')
const audioStream = allStreams.streams.find((stream) => stream.codec_type === 'audio')
const earliestStart = Math.min(...allStreams.streams.map((stream) => Number(stream.start_time)))
const videoStart = Number(videoStream?.start_time)
const audioStart = Number(audioStream?.start_time)
if (videoStream?.codec_name !== 'mpeg2video') ng.push(`fixture の映像 codec が MPEG-2 ではない (${videoStream?.codec_name})`)
if (videoStream?.r_frame_rate !== '30000/1001') ng.push(`fixture の frame rate が 30000/1001 ではない (${videoStream?.r_frame_rate})`)
if (!(earliestStart > 1)) ng.push(`fixture の先頭 PTS が 0 ではない (${earliestStart})`)
if (!(audioStart < videoStart) || Math.abs((videoStart - audioStart) - 0.7) > 0.06) {
  ng.push(`fixture の音声が映像より約 700 ms 先行していない (audio=${audioStart}, video=${videoStart})`)
}

const decodedFrames = probe.frames ?? []
const encodedTimes = runFFprobe([
  '-v', 'error', '-select_streams', 'v:0',
  '-show_entries', 'frame=pts_time,best_effort_timestamp_time', '-of', 'json', encodedPath,
]).frames.map((frame) => Number(frame.best_effort_timestamp_time ?? frame.pts_time)).sort((a, b) => a - b)
if (encodedTimes.length !== decodedFrames.length) {
  ng.push(`encoded の frame 数が原本と違う (encoded=${encodedTimes.length}, source=${decodedFrames.length})`)
}
const markerTimes = manifest.markerFrames.map((frameIndex, markerSlot) => {
  const frame = decodedFrames[frameIndex]
  const pts = Number(frame?.best_effort_timestamp_time ?? frame?.pts_time)
  if (!Number.isFinite(pts)) {
    ng.push(`ffprobe で目印 frame ${frameIndex} の PTS を得られない`)
    return { frame: frameIndex, markerSlot, pts: Number.NaN, expectedSeconds: Number.NaN }
  }
  // 目印の i 番目の frame は、encoded でも表示順の i 番目（frame 数の一致は下で検査する）。
  const encodedPts = encodedTimes[frameIndex]
  if (!Number.isFinite(encodedPts)) ng.push(`encoded の目印 frame ${frameIndex} の PTS を得られない`)
  return { frame: frameIndex, markerSlot, pts, legacySeconds: pts - earliestStart, expectedSeconds: encodedPts }
})
log(`  ffprobe: fps=${videoStream?.r_frame_rate}, earliest=${earliestStart.toFixed(6)}s, audio lead=${(videoStart - audioStart).toFixed(6)}s`)
log(`  目印 encoded PTS (主判定の基準): ${markerTimes.map((item) => `${item.frame}:${item.expectedSeconds.toFixed(6)}`).join(', ')}`)
log(`  目印 原本 PTS - earliest start_time (参考): ${markerTimes.map((item) => `${item.frame}:${item.legacySeconds.toFixed(6)}`).join(', ')}`)

recording.encodedAssets = [{ profile: ENCODE_PROFILE, sizeBytes: statSync(encodedPath).size }]
const liveRecording = { ...recording, encodedAssets: [] }
let activeRecording = liveRecording
await validateFixturesOrExit([
  ['recording for original HLS', ListRecordingsResponseItem, liveRecording],
  ['recording for encoded MP4', ListRecordingsResponseItem, recording],
], ng)

const engine = process.env.E2E_BROWSER ?? 'chrome'
log(`\n=== 実ブラウザ: ${engine} ===`)
const browser = engine === 'chrome'
  ? await launchBrowser('chromium', { channel: 'chrome' })
  : await launchBrowser(engine)
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
const playlistRequests = []
const mp4Requests = []

function growingPlaylist(text) {
  const out = []
  let segments = 0
  for (const line of text.split('\n')) {
    if (line.startsWith('#EXT-X-ENDLIST')) continue
    if (line.startsWith('#EXTINF:')) segments += 1
    if (segments > INITIAL_HLS_SEGMENTS) break
    out.push(line)
  }
  return out.join('\n') + '\n'
}

await context.addInitScript((markerPixels) => {
  window.__displayedMarkerSlot = () => {
    const video = document.querySelector('video')
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    if (!video || !ctx) return -2
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
    return markerPixels.findIndex(([x, y]) => {
      const pixel = ctx.getImageData(x, y, 1, 1).data
      return pixel[0] > 210 && pixel[1] > 210 && pixel[2] > 210
    })
  }
  window.__timelineMarks = []
  window.__timelineCaptureError = null
  window.__timelineCapture = null
  window.__startTimelineCapture = (sessionOffsetSeconds) => {
    const video = document.querySelector('video')
    if (!video || typeof video.requestVideoFrameCallback !== 'function') {
      window.__timelineCaptureError = 'video or requestVideoFrameCallback is unavailable'
      return false
    }
    const previous = window.__timelineCapture
    if (previous?.video && previous.callbackId !== undefined) {
      previous.video.cancelVideoFrameCallback?.(previous.callbackId)
    }
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    const capture = { video, callbackId: undefined }
    window.__timelineCaptureComplete = false
    window.__timelineCaptureError = null
    const onFrame = (_now, metadata) => {
      try {
        if (video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.videoWidth > 0 && ctx) {
          ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
          const markerSlot = markerPixels.findIndex(([x, y]) => {
            const pixel = ctx.getImageData(x, y, 1, 1).data
            return pixel[0] > 210 && pixel[1] > 210 && pixel[2] > 210
          })
          if (markerSlot >= 0) {
            video.pause()
            window.__timelineMarks.push({
              markerSlot,
              sessionOffsetSeconds,
              mediaTime: metadata.mediaTime,
              presentedFrames: metadata.presentedFrames,
              pausedAt: video.currentTime,
            })
            window.__timelineCaptureComplete = true
          }
        }
      } catch (err) {
        window.__timelineCaptureError = String(err)
      }
      if (window.__timelineCapture === capture && !window.__timelineCaptureComplete && !window.__timelineCaptureError) {
        capture.callbackId = video.requestVideoFrameCallback(onFrame)
      }
    }
    window.__timelineCapture = capture
    video.muted = true
    capture.callbackId = video.requestVideoFrameCallback(onFrame)
    void video.play().catch((err) => { window.__timelineCaptureError = String(err) })
    return true
  }
}, [
  [16, 9], [32, 9], [48, 9],
  [16, 18], [32, 18], [48, 18],
  [16, 27], [32, 27], [48, 27],
])

async function captureExpectedMarkers(page, sessionOffsetSeconds, expectedMarkers, label) {
  const observed = []
  for (const marker of expectedMarkers) {
    const localTime = marker.expectedSeconds - sessionOffsetSeconds
    const seekTime = Math.max(0, localTime - 0.25)
    try {
      await page.waitForFunction((target) => {
        const video = document.querySelector('video')
        if (!video || video.seekable.length === 0) return false
        for (let i = 0; i < video.seekable.length; i += 1) {
          if (video.seekable.start(i) <= target && video.seekable.end(i) >= target) return true
        }
        return false
      }, seekTime, { timeout: 10_000 })
      await page.locator('video').evaluate(async (video, target) => {
        video.pause()
        video.playbackRate = 0.5
        if (Math.abs(video.currentTime - target) < 0.02) return
        await new Promise((resolve) => {
          let settled = false
          const finish = () => {
            if (settled) return
            settled = true
            clearTimeout(timer)
            resolve()
          }
          const timer = setTimeout(finish, 2000)
          video.addEventListener('seeked', finish, { once: true })
          try {
            video.currentTime = target
          } catch {
            finish()
          }
        })
      }, seekTime)
      const previousCount = await page.evaluate(() => window.__timelineMarks.length)
      await page.evaluate((offset) => window.__startTimelineCapture(offset), sessionOffsetSeconds)
      await page.waitForFunction((count) => (
        window.__timelineMarks.length > count || window.__timelineCaptureError !== null
      ), previousCount, { timeout: 5000 })
      const capture = await page.evaluate((index) => window.__timelineMarks[index], previousCount)
      await page.waitForTimeout(300)
      const afterPause = await page.evaluate(() => ({ slot: window.__displayedMarkerSlot(), t: document.querySelector('video').currentTime }))
      if (capture) log(`  after-pause frame=${manifest.markerFrames[capture.markerSlot]} displayed slot=${afterPause.slot} want=${capture.markerSlot} drift=${((afterPause.t - capture.pausedAt) * 1000).toFixed(2)}ms`)
      if (capture) {
        const frame = manifest.markerFrames[capture.markerSlot]
        if (!Number.isInteger(frame)) {
          ng.push(`${label}: unknown marker slot ${capture.markerSlot}`)
        } else {
          observed.push({ ...capture, frame })
        }
      }
      else ng.push(`${label}: frame ${marker.frame} の目印を取得できない (${await page.evaluate(() => window.__timelineCaptureError)})`)
    } catch (err) {
      ng.push(`${label}: frame ${marker.frame} が表示されない (${err.message})`)
    }
  }
  return observed
}

async function seekDisplayChecks(page, sessionOffsetSeconds, expectedMarkers, label) {
  for (const marker of expectedMarkers) {
    // エディタは境界を量子化済みの整数 ms で持ち、その秒へシークする（境界カード・前 / 次のチャプター）。
    // その時刻で目印のフレーム自体が表示されなければ、見た境界と切られる位置がずれる。
    // 比較のため、フレームの中央（MP4 の PTS + 半フレーム）へのシークも測る。
    const framePts = marker.expectedSeconds + EXPECTED_SHIFT_FRAMES / SOURCE_FRAME_RATE
    const seekTargets = [
      ['boundary-ms', Math.round(framePts * 1000) / 1000],
      ['frame-center', framePts + HALF_FRAME_SECONDS],
    ]
    for (const [kind, globalSeconds] of seekTargets) {
      const target = globalSeconds - sessionOffsetSeconds
      try {
        await page.waitForFunction((t) => {
          const video = document.querySelector('video')
          if (!video) return false
          for (let i = 0; i < video.seekable.length; i += 1) {
            if (video.seekable.start(i) <= t && video.seekable.end(i) >= t) return true
          }
          return false
        }, target, { timeout: 10_000 })
        const slot = await page.locator('video').evaluate(async (video, t) => {
          video.pause()
          await new Promise((resolve) => {
            const timer = setTimeout(resolve, 3000)
            video.addEventListener('seeked', () => {
              clearTimeout(timer)
              if (typeof video.requestVideoFrameCallback === 'function') video.requestVideoFrameCallback(() => resolve())
              setTimeout(resolve, 500)
            }, { once: true })
            video.currentTime = t
          })
          return window.__displayedMarkerSlot()
        }, target)
        log(`  seek ${kind} ${globalSeconds.toFixed(6)}s (frame ${marker.frame}): displayed slot=${slot} want=${marker.markerSlot}`)
        if (slot !== marker.markerSlot) {
          ng.push(`${label}: ${kind} ${globalSeconds.toFixed(6)}s へシークしても frame ${marker.frame} の目印が出ない (slot=${slot})`)
        }
      } catch (err) {
        ng.push(`${label}: ${kind} ${globalSeconds.toFixed(6)}s へのシークを測れない (${err.message})`)
      }
    }
  }
}

async function timelineHandler({ path: requestPath, url, json, route }) {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers' || requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/live-profiles') return json([{ name: LIVE_PROFILE, height: 720 }])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([activeRecording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') return json(activeRecording)
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapters`) {
    return json({ version: 'timeline-v1', detectionPending: false, source: 'auto', spans: [] })
  }
  if (requestPath.startsWith(`/api/recordings/${RECORDING_ID}/`) && method !== 'GET') {
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/media/recordings/${RECORDING_ID}/seek-tiles`) return route.fulfill({ status: 404 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) return route.fulfill({ status: 404 })

  const originalPrefix = `/api/sites/${SITE}/recordings/${RECORDING_ID}/original-vod/`
  if (requestPath.startsWith(originalPrefix)) {
    const relative = requestPath.slice(originalPrefix.length)
    const offsetMatch = relative.match(/^offset\/(\d+)\//)
    const offset = offsetMatch ? Number(offsetMatch[1]) : 0
    const resource = offsetMatch ? relative.slice(offsetMatch[0].length) : relative
    if (resource === 'leave') return route.fulfill({ status: 204 })
    const manifestPath = offset === 0
      ? manifest.hls.offset0
      : offset === OFFSET_SECONDS
        ? manifest.hls.offset10
        : null
    if (!manifestPath) return route.fulfill({ status: 404, body: `fixture missing for offset ${offset}` })
    const outputDir = path.dirname(path.join(fixtureDir, manifestPath))
    if (resource === 'playlist.m3u8') {
      playlistRequests.push({ offset, resource })
      const master = path.join(fixtureDir, manifestPath)
      if (!existsSync(master)) return route.fulfill({ status: 404, body: `fixture missing for offset ${offset}` })
      return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: readFileSync(master) })
    }
    const file = path.join(outputDir, resource)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: `fixture missing: ${resource}` })
    let body = readFileSync(file)
    if (offset === 0 && /^hd\.0\.m3u8$/.test(resource)) body = Buffer.from(growingPlaylist(body.toString('utf8')))
    if (resource.endsWith('.m3u8')) playlistRequests.push({ offset, resource })
    return route.fulfill({
      status: 200,
      contentType: resource.endsWith('.ts') ? 'video/mp2t' : 'application/vnd.apple.mpegurl',
      body,
    })
  }

  if (requestPath === `/api/media/recordings/${RECORDING_ID}/file`) {
    if (activeRecording.encodedAssets.length === 0) return route.fulfill({ status: 404 })
    const bytes = readFileSync(encodedPath)
    mp4Requests.push(url.href)
    const range = /^bytes=(\d+)-(\d*)$/.exec(route.request().headers().range ?? '')
    if (!range) {
      return route.fulfill({
        status: 200,
        contentType: 'video/mp4',
        body: method === 'HEAD' ? undefined : bytes,
        headers: { 'Accept-Ranges': 'bytes', 'Content-Length': String(bytes.length) },
      })
    }
    const start = Number(range[1])
    const end = Math.min(range[2] ? Number(range[2]) : bytes.length - 1, bytes.length - 1)
    if (start >= bytes.length || end < start) {
      return route.fulfill({ status: 416, headers: { 'Content-Range': `bytes */${bytes.length}` } })
    }
    return route.fulfill({
      status: 206,
      contentType: 'video/mp4',
      body: method === 'HEAD' ? undefined : bytes.subarray(start, end + 1),
      headers: {
        'Accept-Ranges': 'bytes',
        'Content-Range': `bytes ${start}-${end}/${bytes.length}`,
        'Content-Length': String(end - start + 1),
      },
    })
  }
  return json([])
}

const hlsPage = await context.newPage()
await installApiStubs(hlsPage, timelineHandler)
await hlsPage.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
await hlsPage.getByTestId('recording-playback-start').click()
await hlsPage.locator('video').waitFor({ timeout: 15000 })
await hlsPage.waitForFunction(() => {
  const video = document.querySelector('video')
  return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA
}, undefined, { timeout: 15000 })

log('\n=== 原本 HLS offset 0 ===')
const rootMarkers = markerTimes.slice(0, 4)
const rootObserved = await captureExpectedMarkers(hlsPage, 0, rootMarkers, '原本 HLS offset 0')
function compareMarkers(label, observed, expected, sessionOffset) {
  if (observed.length !== expected.length) {
    ng.push(`${label}: 目印フレーム数が違う (got=${observed.length}, want=${expected.length})`)
  }
  const count = Math.min(observed.length, expected.length)
  for (let i = 0; i < count; i += 1) {
    if (observed[i].markerSlot !== expected[i].markerSlot || observed[i].frame !== expected[i].frame) {
      ng.push(`${label}: 目印フレームの順序が違う (got=${observed[i].frame}, want=${expected[i].frame})`)
    }
    const actual = sessionOffset + observed[i].mediaTime
    const expectedSeconds = expected[i].expectedSeconds + EXPECTED_SHIFT_FRAMES / SOURCE_FRAME_RATE
    const diff = actual - expectedSeconds
    const legacyDiff = actual - expected[i].legacySeconds
    const pausedGlobal = sessionOffset + observed[i].pausedAt
    const frameStart = expected[i].expectedSeconds
    log(`  paused frame=${observed[i].frame} into-frame=${((pausedGlobal - frameStart) / (1 / SOURCE_FRAME_RATE)).toFixed(3)}`)
    log(`  frame=${observed[i].frame} want=${expected[i].frame} encodedPts=${expectedSeconds.toFixed(6)} actual=${actual.toFixed(6)} diff=${diff >= 0 ? '+' : ''}${(diff * 1000).toFixed(2)}ms (参考: 原本PTS基準 ${legacyDiff >= 0 ? '+' : ''}${(legacyDiff * 1000).toFixed(2)}ms) presentedFrames=${observed[i].presentedFrames}`)
    if (!Number.isFinite(actual) || Math.abs(diff) > HALF_FRAME_SECONDS) {
      ng.push(`${label}: frame ${expected[i].frame} の差が半フレームを超える (${(diff * 1000).toFixed(2)}ms)`)
    }
  }
}
compareMarkers('原本 HLS offset 0', rootObserved, rootMarkers, 0)
await seekDisplayChecks(hlsPage, 0, rootMarkers, '原本 HLS offset 0')

await hlsPage.locator('video').evaluate((video) => video.pause())
const seekbar = hlsPage.getByTestId('seek-scrub')
const seekbarBox = await seekbar.boundingBox()
if (!seekbarBox) {
  ng.push('offset セッション測定用のシークバーを取得できない')
} else {
  const targetSeconds = OFFSET_SECONDS + 0.5
  const x = seekbarBox.x + seekbarBox.width * targetSeconds / recordingSeconds
  const y = seekbarBox.y + seekbarBox.height / 2
  await hlsPage.mouse.move(seekbarBox.x + seekbarBox.width * 0.2, y)
  await hlsPage.mouse.down()
  await hlsPage.mouse.move(x, y)
  await hlsPage.mouse.up()
}

const offsetRequestDeadline = Date.now() + 15_000
while (
  !playlistRequests.some((request) => request.offset === OFFSET_SECONDS && request.resource === 'playlist.m3u8') &&
  Date.now() < offsetRequestDeadline
) {
  await hlsPage.waitForTimeout(50)
}
if (!playlistRequests.some((request) => request.offset === OFFSET_SECONDS && request.resource === 'playlist.m3u8')) {
  ng.push(`seek で原本 HLS offset/${OFFSET_SECONDS} セッションを要求しない (${JSON.stringify(playlistRequests)})`)
} else {
  const hlsOffsetDeadline = Date.now() + 15_000
  while (Date.now() < hlsOffsetDeadline) {
    const state = await hlsPage.evaluate(() => {
      const video = document.querySelector('video')
      return { videoWidth: video?.videoWidth ?? 0, currentTime: video?.currentTime ?? -1 }
    })
    if (state.videoWidth > 0 && state.currentTime >= 0.25) break
    await hlsPage.waitForTimeout(50)
  }
  log(`\n=== 原本 HLS offset ${OFFSET_SECONDS} ===`)
  const offsetMarkers = markerTimes.slice(4)
  // 実験: E2E_TIMELINE_OFFSET_FRAME_FLOOR=1 はセッションの原点を N 秒以下の最後のフレーム境界とみなす。
  const sessionOffset = process.env.E2E_TIMELINE_OFFSET_FRAME_FLOOR === '1'
    ? Math.floor(OFFSET_SECONDS * SOURCE_FRAME_RATE) / SOURCE_FRAME_RATE
    : OFFSET_SECONDS
  log(`  session offset = ${sessionOffset.toFixed(6)}s`)
  const offsetObserved = await captureExpectedMarkers(hlsPage, sessionOffset, offsetMarkers, `原本 HLS offset ${OFFSET_SECONDS}`)
  compareMarkers(`原本 HLS offset ${OFFSET_SECONDS}`, offsetObserved, offsetMarkers, sessionOffset)
  await seekDisplayChecks(hlsPage, sessionOffset, offsetMarkers, `原本 HLS offset ${OFFSET_SECONDS}`)
}

await hlsPage.evaluate(() => localStorage.clear())
await hlsPage.close()
activeRecording = recording
const mp4Page = await context.newPage()
await installApiStubs(mp4Page, timelineHandler)
await mp4Page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
await mp4Page.locator('video').waitFor({ timeout: 15000 })
const mp4Ready = await mp4Page.waitForFunction(() => {
  const video = document.querySelector('video')
  return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_METADATA
}, undefined, { timeout: 15000 }).then(() => true).catch(() => false)
log('\n=== 非カット MP4 ===')
if (mp4Ready) {
  const mp4Observed = await captureExpectedMarkers(mp4Page, 0, markerTimes, '非カット MP4')
  compareMarkers('非カット MP4', mp4Observed, markerTimes, 0)
  await seekDisplayChecks(mp4Page, 0, markerTimes, '非カット MP4')
} else {
  const videoState = await mp4Page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    duration: video.duration,
    readyState: video.readyState,
    networkState: video.networkState,
    paused: video.paused,
    videoWidth: video.videoWidth,
    videoHeight: video.videoHeight,
    currentSrc: video.currentSrc,
    error: video.error ? { code: video.error.code, message: video.error.message } : null,
  }))
  ng.push(`非カット MP4 の metadata が読めない (video=${JSON.stringify(videoState)}, requests=${JSON.stringify(mp4Requests)})`)
}
if (mp4Requests.length === 0) ng.push('encoded MP4 の Range 配信を要求しない')
if (!playlistRequests.some((request) => request.offset === 0 && request.resource === 'playlist.m3u8')) {
  ng.push('原本 HLS offset 0 の master playlist を要求しない')
}
if (EXPECTED_SHIFT_FRAMES !== 0) log(`  変異: 期待時刻に ${EXPECTED_SHIFT_FRAMES} frame を加算`)
await finish(ng, browser)
