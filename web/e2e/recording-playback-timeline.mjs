// 録画 HLS の実ブラウザ時間軸と ingest 世代の切り替え後の再生継続を測る。
// fixture は Go のテストが製品の HLS 引数ビルダーで作る。
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-playback-timeline
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, statSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import {
  ListProgramsResponseItem,
  ListRecordingsResponseItem,
  ListServicesResponseItem,
} from '../src/api/zod.ts'
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
// 目印の 3x3 の位置を、64x36 に縮めたフレームの画素で読む（fixture の drawbox と同じ並び）。
const MARKER_PIXELS = [
  [16, 9], [32, 9], [48, 9],
  [16, 18], [32, 18], [48, 18],
  [16, 27], [32, 27], [48, 27],
]
const INITIAL_HLS_SEGMENTS = 5
const REAL_HLS_URL = process.env.E2E_TIMELINE_REAL_HLS_URL ?? ''
const REAL_HLS_BOUNDARY_MS = Math.round((Number(process.env.E2E_TIMELINE_REAL_HLS_BOUNDARY_SECONDS ?? '1') || 1) * 1000)
if (REAL_HLS_URL) {
  const realHlsAddress = new URL(REAL_HLS_URL)
  if (realHlsAddress.protocol !== 'http:' || !['127.0.0.1', 'localhost', '[::1]'].includes(realHlsAddress.hostname)) {
    throw new Error('実録画 HLS の診断 URL は localhost のみ指定できます')
  }
}
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
  hlsPlayback: [], cmDetection: { state: 'disabled' },
  startedAt,
  endedAt,
  createdAt: '2026-10-03T00:00:00Z',
  encodeProfiles: [ENCODE_PROFILE],
  sizeBytes: 2_000_000,
  encodedAssets: [],
}
const apiHlsStartedAt = new Date(Date.now() - 30_000).toISOString()
const apiHlsPath = (generation, profile = 'hd') =>
  `/api/media/stores/media/recordings/${RECORDING_ID}/hls/v${generation}/${profile}/master.m3u8`
const apiHlsRecording = {
  ...recording,
  startAt: apiHlsStartedAt,
  startedAt: apiHlsStartedAt,
  durationMs: 120_000,
  endedAt: undefined,
  status: 'recording',
  hlsPlayback: [
    { profile: 'hd', masterUrl: apiHlsPath(1, 'hd') },
    { profile: 'sd', masterUrl: apiHlsPath(1, 'sd') },
  ],
}
// 410 復旧の判定は、録画が終わって取り込み中の録画で、temp を捨てて世代を替えるサーバーの遷移を再現する。
const apiHlsRecoveryFinished = {
  ...apiHlsRecording,
  startAt: new Date(now - 120_000).toISOString(),
  startedAt: new Date(now - 120_000).toISOString(),
  endedAt: new Date(now - 10_000).toISOString(),
  durationMs: 110_000,
  status: 'finished',
  resumePositionMs: 0,
}
const apiHlsRecoveryV1 = {
  ...apiHlsRecoveryFinished,
  ingest: { state: 'transferring', writtenBytes: 1_000_000, observedAt: new Date(now).toISOString() },
}
const apiHlsRecoveryEmpty = { ...apiHlsRecoveryFinished, ingest: { state: 'pending' }, hlsPlayback: [] }
const apiHlsRecoveryV2 = {
  ...apiHlsRecoveryFinished,
  ingest: { state: 'transferring', writtenBytes: 100_000, observedAt: new Date(now).toISOString() },
  hlsPlayback: [
    { profile: 'hd', masterUrl: apiHlsPath(2, 'hd') },
    { profile: 'sd', masterUrl: apiHlsPath(2, 'sd') },
  ],
}
const LIVE_NETWORK_ID = 1
const LIVE_SERVICE_ID = 9001
const LIVE_SERVICE_KEY = LIVE_NETWORK_ID * 100_000 + LIVE_SERVICE_ID
const liveService = {
  id: LIVE_SERVICE_KEY,
  networkId: LIVE_NETWORK_ID,
  serviceId: LIVE_SERVICE_ID,
  name: 'テスト局',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}
const liveProgramStartAt = new Date(now - 120_000).toISOString()
const liveProgram = {
  programId: 301,
  networkId: LIVE_NETWORK_ID,
  serviceId: LIVE_SERVICE_ID,
  eventId: 301,
  recordingId: RECORDING_ID,
  startAt: liveProgramStartAt,
  endAt: new Date(now + 180_000).toISOString(),
  durationMs: 300_000,
  name: '世代切替の実ブラウザ確認',
  description: '',
  genres: [0],
  isFree: true,
}
const liveRecoveryRecording = (generation, startAt = liveProgramStartAt) => ({
  ...apiHlsRecoveryV1,
  networkId: LIVE_NETWORK_ID,
  serviceId: LIVE_SERVICE_ID,
  eventId: liveProgram.eventId,
  serviceName: liveService.name,
  channel: liveService.channel,
  title: liveProgram.name,
  startAt,
  startedAt: startAt,
  durationMs: liveProgram.durationMs,
  endedAt: undefined,
  status: 'recording',
  sizeBytes: undefined,
  hlsPlayback: ['hd', 'sd'].map((profile) => ({ profile, masterUrl: apiHlsPath(generation, profile) })),
})
const liveRecoveryV1 = liveRecoveryRecording(1)
const liveRecoveryV2 = liveRecoveryRecording(2)
const encodedRecording = {
  ...recording,
  encodedAssets: [{ profile: ENCODE_PROFILE, sizeBytes: 1_000_000 }],
}

await validateFixturesOrExit([
  ['original-only recording', ListRecordingsResponseItem, recording],
  ['encoded recording', ListRecordingsResponseItem, encodedRecording],
  ['recording API HLS', ListRecordingsResponseItem, apiHlsRecording],
  ['recording API HLS v1 before generation change', ListRecordingsResponseItem, apiHlsRecoveryV1],
  ['recording API HLS during generation change', ListRecordingsResponseItem, apiHlsRecoveryEmpty],
  ['recording API HLS v2 after generation change', ListRecordingsResponseItem, apiHlsRecoveryV2],
  ['live page service', ListServicesResponseItem, liveService],
  ['live page program', ListProgramsResponseItem, liveProgram],
  ['live page recording API HLS v1', ListRecordingsResponseItem, liveRecoveryV1],
  ['live page recording API HLS v2', ListRecordingsResponseItem, liveRecoveryV2],
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
const markerTimes = manifest.markerFrames.map((frameIndex, markerSlot) => {
  const frame = decodedFrames[frameIndex]
  const pts = Number(frame?.best_effort_timestamp_time ?? frame?.pts_time)
  if (!Number.isFinite(pts)) {
    ng.push(`ffprobe で目印 frame ${frameIndex} の PTS を得られない`)
    return { frame: frameIndex, markerSlot, pts: Number.NaN, expectedSeconds: Number.NaN }
  }
  return { frame: frameIndex, markerSlot, pts, legacySeconds: pts - earliestStart, expectedSeconds: Number.NaN }
})
log(`  ffprobe: fps=${videoStream?.r_frame_rate}, earliest=${earliestStart.toFixed(6)}s, audio lead=${(videoStart - audioStart).toFixed(6)}s`)
log(`  目印 原本 PTS - earliest start_time (参考): ${markerTimes.map((item) => `${item.frame}:${item.legacySeconds.toFixed(6)}`).join(', ')}`)

// 録画 API HLS の途中シークの期待値は、ブラウザが実際に再生する HLS fixture（offset 0 の master）の
// 目印フレームの時刻にする。HLS は先頭に音声の先行ぶん（約 0.7 秒）のフレームを足すので、原本の
// frame 番号では引けない。そこで縮小した各フレームの目印の画素（__displayedMarkerSlot と同じ位置・閾値）
// で目印を探し、その PTS から variant の最初の映像フレームの PTS を引く。
function hlsFixtureMarkerSeconds() {
  const masterPath = path.join(fixtureDir, manifest.hls.offset0)
  const ptsList = runFFprobe([
    '-v', 'error', '-select_streams', 'v:0',
    '-show_entries', 'frame=pts_time,best_effort_timestamp_time', '-of', 'json', masterPath,
  ]).frames.map((frame) => Number(frame.best_effort_timestamp_time ?? frame.pts_time)).sort((a, b) => a - b)
  const width = 64
  const height = 36
  const gray = execFileSync('ffmpeg', [
    '-v', 'error', '-i', masterPath, '-map', '0:v:0', '-fps_mode', 'passthrough',
    '-vf', `scale=${width}:${height}`, '-f', 'rawvideo', '-pix_fmt', 'gray', '-',
  ], { maxBuffer: 64 * 1024 * 1024 })
  const frameCount = gray.length / (width * height)
  if (frameCount !== ptsList.length) ng.push(`HLS fixture の復号フレーム数が ffprobe と違う (decoded=${frameCount}, probed=${ptsList.length})`)
  const seconds = new Map()
  for (let i = 0; i < frameCount; i += 1) {
    MARKER_PIXELS.forEach(([x, y], slot) => {
      if (gray[i * width * height + y * width + x] > 210 && !seconds.has(slot)) seconds.set(slot, ptsList[i] - ptsList[0])
    })
  }
  return seconds
}
const hlsMarkerSeconds = hlsFixtureMarkerSeconds()
for (const marker of markerTimes) {
  marker.expectedSeconds = hlsMarkerSeconds.get(marker.markerSlot) ?? Number.NaN
  if (!Number.isFinite(marker.expectedSeconds)) ng.push(`HLS fixture に目印 ${marker.markerSlot} のフレームが無い`)
}
log(`  目印 HLS fixture の時刻 (録画 API HLS の基準): ${markerTimes.map((item) => `${item.frame}:${item.expectedSeconds.toFixed(6)}`).join(', ')}`)
log(`  HLS fixture - 原本 PTS の差 (ms): ${markerTimes.map((item) => `${item.frame}:${((item.expectedSeconds - item.legacySeconds) * 1000).toFixed(3)}`).join(', ')}`)

recording.encodedAssets = [{ profile: ENCODE_PROFILE, sizeBytes: statSync(encodedPath).size }]
const liveRecording = { ...recording, encodedAssets: [] }
let activeRecording = liveRecording
let chaptersForHls = false // 録画 HLS のページでも章を返す（再生位置との対応を確認する）
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
const recordingHlsRequests = []
const recordingHls410Requests = []
const recordingHlsApiResponses = []
// この世代の要求（master・child playlist・セグメント）は全部 410 にする。
let expiredRecordingHlsGeneration = null
// 410 の後の最初の詳細 GET で empty を返し、それ以降は next() を返す（サーバーの v1 → 空 → v2）。
let recordingHlsGap = null
// v2 の master を返す直前に読んだ、止まる直前の再生位置。
let recordingHlsStopPositionSeconds = null
let liveGenerationRecovery = false
let liveRecordingHlsProgressBaseline = null

function growingPlaylist(text, segmentLimit = INITIAL_HLS_SEGMENTS) {
  const out = []
  let segments = 0
  for (const line of text.split('\n')) {
    if (line.startsWith('#EXT-X-ENDLIST')) continue
    if (line.startsWith('#EXTINF:')) segments += 1
    if (segments > segmentLimit) break
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
  window.__videoFrameFingerprint = () => {
    const video = document.querySelector('video')
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    if (!video || !ctx) return null
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
    const pixels = ctx.getImageData(0, 0, canvas.width, canvas.height).data
    let hash = 2_166_136_261
    for (const pixel of pixels) hash = Math.imul(hash ^ pixel, 16_777_619)
    return (hash >>> 0).toString(16).padStart(8, '0')
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
            window.__timelineMarks.push({
              markerSlot,
              sessionOffsetSeconds,
              mediaTime: metadata.mediaTime,
              presentedFrames: metadata.presentedFrames,
            })
            window.__timelineCaptureComplete = true
            video.pause()
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
}, MARKER_PIXELS)

async function timelineHandler({ path: requestPath, json, route }) {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (liveGenerationRecovery && requestPath === `/api/sites/${SITE}/services`) return json([liveService])
  if (liveGenerationRecovery && requestPath === `/api/sites/${SITE}/programs`) return json([liveProgram])
  if (requestPath === '/api/breakers' || requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/live-profiles') return json([{ name: LIVE_PROFILE, height: 720 }])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0, awaitingReview: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([activeRecording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') {
    if (recordingHlsGap?.armed) {
      const { empty, next } = recordingHlsGap
      recordingHlsGap = null
      activeRecording = next()
      recordingHlsApiResponses.push('empty')
      return json(empty)
    }
    if (recordingHlsGap) recordingHlsApiResponses.push('v1')
    else if (activeRecording.hlsPlayback.some(({ masterUrl }) => masterUrl === apiHlsPath(2, 'hd'))) recordingHlsApiResponses.push('v2')
    return json(activeRecording)
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapter-edits` && method === 'PUT') {
    const body = route.request().postDataJSON()
    return json({ version: 'timeline-v2', detectionPending: false, source: 'user', spans: body.spans })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapters`) {
    const spans = REAL_HLS_URL && chaptersForHls
      ? [{ startMs: REAL_HLS_BOUNDARY_MS, endMs: REAL_HLS_BOUNDARY_MS + 1000, label: '検証境界', cut: false }]
      : activeRecording.encodedAssets.length > 0 || chaptersForHls
        ? markerTimes.map((marker, index) => ({
          startMs: Math.round(marker.expectedSeconds * 1000),
          endMs: Math.round((marker.expectedSeconds + 1 / SOURCE_FRAME_RATE) * 1000),
          label: `目印 ${index + 1}`,
          cut: false,
        }))
        : []
    return json({ version: 'timeline-v1', detectionPending: false, source: 'auto', spans })
  }
  if (requestPath.startsWith(`/api/recordings/${RECORDING_ID}/`) && method !== 'GET') {
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/media/recordings/${RECORDING_ID}/seek-tiles`) return route.fulfill({ status: 404 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) return route.fulfill({ status: 404 })

  const livePrefix = `/api/sites/${SITE}/networks/${LIVE_NETWORK_ID}/services/${LIVE_SERVICE_ID}/live/`
  if (liveGenerationRecovery && requestPath.startsWith(livePrefix)) {
    const resource = requestPath.slice(livePrefix.length)
    const fixtureMaster = path.join(fixtureDir, manifest.hls.offset0)
    const outputDir = path.dirname(fixtureMaster)
    const file = path.resolve(outputDir, resource)
    if (resource !== 'playlist.m3u8' && !file.startsWith(`${outputDir}${path.sep}`)) {
      return route.fulfill({ status: 404, body: 'invalid live fixture path' })
    }
    if (resource === 'playlist.m3u8') {
      return route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: readFileSync(fixtureMaster),
      })
    }
    if (!existsSync(file)) return route.fulfill({ status: 404, body: `live fixture missing: ${resource}` })
    let body = readFileSync(file)
    if (resource.endsWith('.m3u8')) body = Buffer.from(growingPlaylist(body.toString('utf8')))
    return route.fulfill({
      status: 200,
      contentType: resource.endsWith('.ts') ? 'video/mp2t' : 'application/vnd.apple.mpegurl',
      body,
    })
  }

  const apiHlsMatch = new RegExp(`^/api/media/stores/media/recordings/${RECORDING_ID}/hls/v(\\d+)/([^/]+)/(.+)$`).exec(requestPath)
  if (apiHlsMatch) {
    const generation = Number(apiHlsMatch[1])
    const profile = apiHlsMatch[2]
    const resource = apiHlsMatch[3]
    const fixtureMaster = path.join(fixtureDir, manifest.hls.offset0)
    const outputDir = path.dirname(fixtureMaster)
    recordingHlsRequests.push({ url: requestPath, resource, generation, profile })
    if (generation === expiredRecordingHlsGeneration) {
      recordingHls410Requests.push(requestPath)
      if (recordingHlsGap) recordingHlsGap.armed = true
      return route.fulfill({ status: 410, body: 'expired recording HLS generation' })
    }
    if (generation === 2 && resource === 'master.m3u8' && recordingHlsStopPositionSeconds === null) {
      // v2 の映像はまだ届いていないので、ここで読む位置は v1 で止まる直前の再生位置である。
      recordingHlsStopPositionSeconds = await route.request().frame()
        .evaluate(() => window.__lastPlayingSeconds ?? null).catch(() => null)
    }
    if (liveGenerationRecovery && generation === 1 && resource === 'master.m3u8' && liveRecordingHlsProgressBaseline === null) {
      liveRecordingHlsProgressBaseline = await route.request().frame()
        .evaluate(() => window.__progressedSeconds ?? 0).catch(() => 0)
    }
    if (resource === 'master.m3u8') {
      return route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: readFileSync(fixtureMaster),
      })
    }
    const file = path.join(outputDir, resource)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: `fixture missing: ${resource}` })
    let body = readFileSync(file)
    // 取り込み中の EVENT playlist として、映像も音声も ENDLIST を外す。v2 は全セグメントを載せる。
    // WebKit は v1 のバッファの終わり（約 10 秒）で止まるので、その先が要る。
    if (/^hd\.\d+\.m3u8$/.test(resource)) {
      body = Buffer.from(growingPlaylist(body.toString('utf8'), generation === 2 ? Infinity : INITIAL_HLS_SEGMENTS))
    }
    return route.fulfill({
      status: 200,
      contentType: resource.endsWith('.ts') ? 'video/mp2t' : 'application/vnd.apple.mpegurl',
      body,
    })
  }

  return json([])
}

async function stubRecordingEventSource(page) {
  await page.addInitScript(() => {
    const eventSources = []
    const seekedPositions = []
    const observedVideos = new WeakSet()
    const observeVideo = (node) => {
      if (!(node instanceof HTMLVideoElement) || observedVideos.has(node)) return
      observedVideos.add(node)
      node.addEventListener('seeked', () => seekedPositions.push(node.currentTime))
      let previous = null
      window.__progressedSeconds = 0
      node.addEventListener('timeupdate', () => {
        if (!node.paused && node.currentTime > 0) window.__lastPlayingSeconds = node.currentTime
        // 再生で進んだ秒だけを足す（シークの飛びは足さない）。
        const step = previous === null ? 0 : node.currentTime - previous
        if (!node.paused && step > 0 && step < 0.5) window.__progressedSeconds += step
        previous = node.currentTime
      })
    }
    class StubEventSource extends EventTarget {
      constructor(url) {
        super()
        this.url = url
        eventSources.push(this)
      }
      close() {}
    }
    window.EventSource = StubEventSource
    window.__recordingEventSources = eventSources
    window.__recordingHlsSeekedPositions = seekedPositions
    window.__sendRecordingEvent = (topic) => {
      for (const source of eventSources) source.dispatchEvent(new Event(topic))
    }
    const observer = new MutationObserver(() => {
      document.querySelectorAll('video').forEach(observeVideo)
    })
    observer.observe(document, { childList: true, subtree: true })
    document.querySelectorAll('video').forEach(observeVideo)
  })
}

async function runRecordingApiHlsCheck() {
  // 録画開始は実行時に決める。時間軸の右端は録画済み時間なので、CI の起動の遅れで長くなると
  // 1 px が 1 フレームより長くなり、途中シークで窓に入る整数 px が無くなる。EVENT playlist の
  // seekable（約 10 秒）より 2 秒以上長ければ足りる。
  const apiHlsStartedAtNow = new Date(Date.now() - 13_000).toISOString()
  const apiHlsLiveRecording = {
    ...apiHlsRecording,
    startAt: apiHlsStartedAtNow,
    startedAt: apiHlsStartedAtNow,
    sizeBytes: undefined,
    ingest: { state: 'transferring', writtenBytes: 100, observedAt: new Date().toISOString() },
  }
  activeRecording = {
    ...apiHlsLiveRecording,
    ingest: { state: 'pending' },
    hlsPlayback: [],
  }
  recordingHlsRequests.length = 0
  const page = await context.newPage()
  await installApiStubs(page, timelineHandler)
  await stubRecordingEventSource(page)
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  await page.getByText('再生可能なファイルがありません').waitFor({ timeout: 15000 })
  if (recordingHlsRequests.length > 0) ng.push('hlsPlayback が空の間に HLS を要求した')
  await page.waitForFunction(() => window.__recordingEventSources.length > 0, undefined, { timeout: 10000 })
  activeRecording = apiHlsLiveRecording
  const pendingRefreshed = page.waitForResponse((response) => {
    const responseURL = new URL(response.url())
    return response.request().method() === 'GET' && responseURL.pathname === `/api/recordings/${RECORDING_ID}`
  }, { timeout: 10000 })
  await page.evaluate(() => window.__sendRecordingEvent('recordings'))
  const pendingRefreshSucceeded = await pendingRefreshed.then((response) => response.ok()).catch(() => false)
  if (!pendingRefreshSucceeded) {
    ng.push('pending から録画 API HLS への recordings SSE 後に詳細 API を取り直せない')
    await page.close()
    return
  }
  log('\n=== 録画 API HLS: pending から recordings SSE で v1 を選び直す ===')
  await page.getByTestId('recording-playback-start').waitFor({ state: 'visible', timeout: 10000 })
  await page.getByTestId('recording-playback-start').click()
  await page.locator('video').waitFor({ timeout: 15000 })

  const v1Started = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && !video.paused && window.__progressedSeconds >= 0.5
  }, undefined, { timeout: 15000 }).then(() => true).catch(() => false)
  if (!v1Started) {
    ng.push('pending から v1 へ替わった後、実ブラウザで currentTime が進まない')
    await page.close()
    return
  }

  const mediaReady = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.seekable.length > 0
  }, undefined, { timeout: 20000 }).then(() => true).catch(() => false)
  if (!mediaReady) {
    ng.push(`録画 API HLS の映像データが届かない (requests=${JSON.stringify(recordingHlsRequests)})`)
    await page.close()
    return
  }

  // max は Date.now() - startedAt なので、複数の中間位置を調べる間も 1 秒ごとに
  // 伸びる。メディア準備後の同じ時点で browser / Node の基準時刻を固定し、座標の
  // 算出とシーク後の検査で px → 秒の対応を保つ。timer は進むので再生待ちには影響しない。
  const timelineAsOf = Date.now()
  await page.clock.setFixedTime(new Date(timelineAsOf))

  const slider = page.getByRole('slider', { name: 'シークバー' })
  const timelineSeconds = Number(await slider.getAttribute('aria-valuemax'))
  const mediaState = await page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    seekableEnd: video.seekable.end(video.seekable.length - 1),
    readyState: video.readyState,
    videoWidth: video.videoWidth,
  }))
  const recordedProgressSeconds = (timelineAsOf - Date.parse(apiHlsStartedAtNow)) / 1000
  log(`\n=== 録画 API HLS: 録画中の時間軸 ===`)
  log(`  右端=${timelineSeconds.toFixed(2)}s / 壁時計の録画済み=${recordedProgressSeconds.toFixed(2)}s / HLS seekable=${mediaState.seekableEnd.toFixed(2)}s`)
  if (Math.abs(timelineSeconds - recordedProgressSeconds) > 2.5) {
    ng.push(`録画中の時間軸の右端が録画済み時間ではない (axis=${timelineSeconds}, recorded=${recordedProgressSeconds})`)
  }
  if (!(timelineSeconds > mediaState.seekableEnd + 2)) {
    ng.push(`録画中の時間軸が EVENT playlist の末尾で止まっている (axis=${timelineSeconds}, seekable=${mediaState.seekableEnd})`)
  }
  if (!recordingHlsRequests.some(({ url }) => url === apiHlsPath(1))) {
    ng.push(`Recording.hlsPlayback.masterUrl を直接要求しない (${JSON.stringify(recordingHlsRequests)})`)
  }

  // 途中の位置へのシーク: 時間軸を整数 px でクリックし、currentTime が録画開始からの秒になり、
  // 表示中のフレームがその秒のフレーム（目印）であることを測る。WebKit は mouse.click の座標を
  // 整数 px に切り捨て、1 px が 1 フレーム以上（35 ms 前後）なので、小数座標で 1 フレームを狙えない。
  // そこで、整数 px のうち、アプリが代入する秒 (x - box.x) / width * axisMax が目印フレームの窓
  // （HLS fixture の目印フレームの時刻から 1 フレーム。hlsFixtureMarkerSeconds）の中に入るものを選ぶ。
  // 秒の計算は Chrome と WebKit で同じで、encoded には依存しない。
  await page.locator('video').evaluate((video) => video.pause())
  const box = await slider.boundingBox()
  const axisMax = Number(await slider.getAttribute('aria-valuemax'))
  const frameSeconds = 1 / SOURCE_FRAME_RATE
  // 窓の端を避ける余白。時間軸が 1 秒ごとに伸びて axisMax が変わる分を、上下とも同じ幅で吸収する。
  const margin = 0.006
  const picks = []
  for (const marker of markerTimes) {
    if (picks.length === 2) break
    if (!(marker.expectedSeconds < mediaState.seekableEnd - 1)) continue
    for (let x = Math.ceil(box.x); x < box.x + box.width; x += 1) {
      const seconds = (x - box.x) / box.width * axisMax
      if (seconds >= marker.expectedSeconds + margin && seconds < marker.expectedSeconds + frameSeconds - margin) {
        picks.push({ marker, x, seconds })
        break
      }
    }
  }
  // 1 px が 1 フレームより長い（CI は起動が遅く時間軸が 50 秒近くなり、約 47 ms）と、窓に入る px が無い
  // 目印がある。seekable 内の 4 つの目印のうち 1 つでも見つかれば判定できる。
  if (picks.length < 1) ng.push(`途中シークの判定に使える整数 px が見つからない (picks=${picks.length}, width=${box.width}, axisMax=${axisMax})`)
  for (const { marker, x, seconds } of picks) {
    await page.mouse.click(x, box.y + box.height / 2)
    const landed = await page.waitForFunction(({ slot, from, to }) => {
      const video = document.querySelector('video')
      return video && !video.seeking && video.currentTime >= from && video.currentTime < to &&
        window.__displayedMarkerSlot() === slot
    }, { slot: marker.markerSlot, from: marker.expectedSeconds, to: marker.expectedSeconds + frameSeconds }, { timeout: 5000 })
      .then(() => true).catch(() => false)
    const observed = await page.evaluate(() => ({
      currentTime: document.querySelector('video').currentTime,
      slot: window.__displayedMarkerSlot(),
    }))
    log(`  中間シーク frame ${marker.frame}: x=${x} 目標=${seconds.toFixed(4)}s / currentTime=${observed.currentTime.toFixed(4)}s / 表示=${observed.slot} (期待 ${marker.markerSlot})`)
    if (!landed) {
      // 診断: 同じ位置へ再 seek して表示が直るかを確認する。
      const again = await page.locator('video').evaluate(async (video) => {
        const t = video.currentTime
        video.pause()
        await new Promise((resolve) => {
          video.addEventListener('seeked', resolve, { once: true })
          video.currentTime = t
          setTimeout(resolve, 3000)
        })
        await new Promise((resolve) => setTimeout(resolve, 500))
        return { t, now: video.currentTime, slot: window.__displayedMarkerSlot() }
      })
      log(`  診断 frame ${marker.frame}: 同じ位置へ再 seek 後 ${JSON.stringify(again)}`)
      ng.push(`途中へのシークで currentTime が録画秒にならない、または表示が目印 ${marker.frame} ではない (${JSON.stringify(observed)}, want=${marker.expectedSeconds})`)
    }
  }

  // 末尾へのシーク。これは「ブラウザ込みで、末尾への操作が playlist の末尾に収まる」ことだけを測る。
  // 変異確認で seekTarget の playlistEndSeconds による丸めを外しても通った（HTML のシーク手順で
  // ブラウザが seekable の最寄りへ丸める）ので、アプリの丸めはこの判定では守れていない。
  // アプリの丸めは playback-mode.test.ts の clamp のテストが守る。
  await slider.focus()
  await page.keyboard.press('End')
  const clamped = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    if (!video || video.seekable.length === 0) return false
    const end = video.seekable.end(video.seekable.length - 1)
    return Math.abs(video.currentTime - end) < 0.5
  }, undefined, { timeout: 5000 }).then(() => true).catch(() => false)
  if (!clamped) {
    const afterSeek = await page.locator('video').evaluate((video) => ({
      currentTime: video.currentTime,
      seeking: video.seeking,
      seekableEnd: video.seekable.length ? video.seekable.end(video.seekable.length - 1) : null,
    }))
    ng.push(`時間軸の末尾への操作で currentTime が playlist 末尾に収まらない (${JSON.stringify(afterSeek)})`)
  }

  const masterRequestsBeforeCommit = recordingHlsRequests.filter(({ resource }) => resource === 'master.m3u8').length
  activeRecording = {
    ...apiHlsLiveRecording,
    status: 'finished',
    endedAt: new Date().toISOString(),
    resumePositionMs: 10_000,
    // ingest commit 後も API が返す master URL は維持される。
    hlsPlayback: apiHlsRecording.hlsPlayback,
  }
  const eventSourceCount = await page.evaluate(() => window.__recordingEventSources.length)
  if (eventSourceCount === 0) ng.push('録画状態の SSE を購読していない')
  const refreshed = page.waitForResponse((response) => {
    const responseURL = new URL(response.url())
    return response.request().method() === 'GET' && responseURL.pathname === `/api/recordings/${RECORDING_ID}`
  }, { timeout: 10000 })
  await page.evaluate(() => window.__sendRecordingEvent('recordings'))
  const refreshedSuccessfully = await refreshed.then((response) => response.ok()).catch(() => false)
  if (!refreshedSuccessfully) ng.push('commit 後の詳細 API を取り直せない')
  await page.waitForTimeout(250)
  const masterRequestsAfterCommit = recordingHlsRequests.filter(({ resource }) => resource === 'master.m3u8').length
  log(`  commit 前後の master 要求数=${masterRequestsBeforeCommit}/${masterRequestsAfterCommit}`)
  if (masterRequestsAfterCommit !== masterRequestsBeforeCommit) {
    ng.push('ingest commit をまたぐとき、同じ master URL を維持した再生を張り直した')
  }

  await page.evaluate(() => localStorage.clear())
  await page.close()
}

async function runRecordingApiHlsRecoveryCheck() {
  // サーバーの世代は v1 → 空 → v2 と動く。ここでは 410 復旧だけを独立して測るため、SSE は送らない。
  const fresh = () => new Date().toISOString()
  const v1 = { ...apiHlsRecoveryV1, ingest: { ...apiHlsRecoveryV1.ingest, observedAt: fresh() } }
  activeRecording = v1
  recordingHlsRequests.length = 0
  recordingHls410Requests.length = 0
  recordingHlsApiResponses.length = 0
  expiredRecordingHlsGeneration = null
  recordingHlsStopPositionSeconds = null
  recordingHlsGap = {
    armed: false,
    empty: apiHlsRecoveryEmpty,
    next: () => ({ ...apiHlsRecoveryV2, ingest: { ...apiHlsRecoveryV2.ingest, observedAt: fresh() } }),
  }
  const page = await context.newPage()
  await installApiStubs(page, timelineHandler)
  await stubRecordingEventSource(page)
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  log(`\n=== 録画 API HLS: v1 → 空 → v2 の世代の切り替えで、止まる直前の位置から続ける ===`)

  await page.getByTestId('recording-playback-start').waitFor({ timeout: 15000 })
  await page.getByTestId('recording-playback-start').click()
  // currentTime だけを見ると、WebKit が EVENT playlist の端から始めて開始位置へ戻す前の値（約 4 秒）で
  // 通ってしまう。再生で実際に 3 秒以上進んだことを待つ。
  const started = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && !video.paused && window.__progressedSeconds >= 3
  }, undefined, { timeout: 15000 }).then(() => true).catch(() => false)
  if (!started) {
    ng.push('録画 API HLS の 410 復旧判定前に実ブラウザ再生が始まらない')
    await page.close()
    return
  }
  const sessionStartSeconds = apiHlsRecoveryV1.resumePositionMs / 1000
  // 応答を待つ（要求の時点では、止まる直前の位置を読む handler がまだ終わっていない）。
  const nextMaster = page.waitForResponse((response) => new URL(response.url()).pathname === apiHlsPath(2, 'hd'), { timeout: 30000 })
    .then(() => true).catch(() => false)
  const seekedStart = await page.evaluate(() => window.__recordingHlsSeekedPositions.length)
  // ここから v1 の master・child playlist・セグメントは全部 410 を返し続ける。
  expiredRecordingHlsGeneration = 1
  const expiredAt = Date.now()

  // 待つのは 30 秒まで。lib/events.ts の 60 秒 invalidate が v2 を拾う前に打ち切り、停止検知と
  // 410 からの再取得の経路だけで v2 へ移れることを測る。
  const v2Loaded = await nextMaster
  log(`  410 開始から v2 master 要求まで=${((Date.now() - expiredAt) / 1000).toFixed(1)}s / 詳細 API の応答=${JSON.stringify(recordingHlsApiResponses)}`)
  if (recordingHls410Requests.length < 1) {
    ng.push(`再生中の v1 が 410 にならない (${JSON.stringify(recordingHls410Requests)})`)
  }
  if (!v2Loaded) {
    const current = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused })).catch(() => null)
    ng.push(`410 の後 30 秒以内に v2/hd master を要求しない (video=${JSON.stringify(current)}, api=${JSON.stringify(recordingHlsApiResponses)}, 410=${recordingHls410Requests.length})`)
    await page.close()
    return
  }
  if (recordingHlsApiResponses.indexOf('empty') < 0 || recordingHlsApiResponses.indexOf('v2') < recordingHlsApiResponses.indexOf('empty')) {
    ng.push(`詳細 API が v1 → 空 → v2 の順に返っていない (${JSON.stringify(recordingHlsApiResponses)})`)
  }
  const stopSeconds = recordingHlsStopPositionSeconds
  if (typeof stopSeconds !== 'number' || !(stopSeconds - sessionStartSeconds > 1)) {
    ng.push(`止まる直前の位置がセッション開始と見分けられない (stop=${stopSeconds}, sessionStart=${sessionStartSeconds})`)
    await page.close()
    return
  }
  const resumedAt = await page.waitForFunction(({ seekedStart }) => {
    const video = document.querySelector('video')
    if (!video || video.paused || video.seeking || video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return false
    // 作り直しで video 要素が替わるので、最初の再生位置は seeked の記録から取る。
    return window.__recordingHlsSeekedPositions.slice(seekedStart).at(-1) ?? video.currentTime
  }, { seekedStart }, { timeout: 10000 })
    .then((handle) => handle.jsonValue())
    .catch(() => null)
  log(`  セッション開始=${sessionStartSeconds.toFixed(2)}s / 止まる直前=${stopSeconds.toFixed(2)}s / v2 の再開位置=${typeof resumedAt === 'number' ? resumedAt.toFixed(2) : resumedAt}s`)
  if (typeof resumedAt !== 'number' || Math.abs(resumedAt - stopSeconds) > 1) {
    const current = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused }))
    ng.push(`v2 で止まる直前の位置（±1 秒）から再開しない (stop=${stopSeconds}, resumed=${resumedAt}, current=${JSON.stringify(current)})`)
    await page.close()
    return
  }
  const playingContinued = await page.waitForFunction((from) => {
    const video = document.querySelector('video')
    return video && !video.paused && video.currentTime > from + 0.5
  }, resumedAt, { timeout: 10000 }).then(() => true).catch(() => false)
  if (!playingContinued) ng.push('410 後に v2 で再開位置から再生が続かない')
  expiredRecordingHlsGeneration = null
  recordingHlsGap = null
  await page.evaluate(() => localStorage.clear())
  await page.close()
}

async function runLiveRecordingApiHlsRecoveryCheck() {
  const startAt = new Date(Date.now() - 120_000).toISOString()
  const program = {
    ...liveProgram,
    startAt,
    endAt: new Date(Date.now() + 180_000).toISOString(),
  }
  const makeRecording = (generation) => liveRecoveryRecording(generation, startAt)
  activeRecording = makeRecording(1)
  recordingHlsRequests.length = 0
  recordingHls410Requests.length = 0
  recordingHlsApiResponses.length = 0
  expiredRecordingHlsGeneration = null
  recordingHlsGap = null
  recordingHlsStopPositionSeconds = null
  liveRecordingHlsProgressBaseline = null
  liveGenerationRecovery = true

  const page = await context.newPage()
  page.on('pageerror', (error) => ng.push(`ライブ世代切替のページ例外: ${error.message}`))
  await installApiStubs(page, async (request) => {
    if (request.path === `/api/sites/${SITE}/programs`) return request.json([program])
    return timelineHandler(request)
  })
  await stubRecordingEventSource(page)
  await page.goto(`${URL_BASE}/live?site=${SITE}&service=${LIVE_SERVICE_KEY}&profile=${LIVE_PROFILE}`, { waitUntil: 'domcontentloaded' })
  log('\n=== ライブ画面: 最初から再生中の録画 HLS が v1 → v2 へ位置と再生を保つ ===')

  await page.getByRole('button', { name: `${liveService.name}を再生` }).waitFor({ timeout: 15000 })
  await page.getByRole('button', { name: `${liveService.name}を再生` }).click()
  const livePlay = page.getByRole('button', { name: '再生' })
  await livePlay.waitFor({ timeout: 10000 })
  await livePlay.click()
  const liveStarted = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && !video.paused && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.currentTime > 0
  }, undefined, { timeout: 20000 }).then(() => true).catch(() => false)
  if (!liveStarted) {
    const state = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused, readyState: video.readyState })).catch(() => null)
    ng.push(`ライブ HLS fixture が再生されない (${JSON.stringify(state)})`)
    liveGenerationRecovery = false
    await page.close()
    return
  }
  const startOver = page.getByTestId('live-start-over')
  await startOver.waitFor({ state: 'visible', timeout: 10000 })
  await startOver.click()
  const recordingStarted = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return document.querySelector('[data-testid="recording-hls-source-label"]') && video && !video.paused &&
      video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.currentTime > 0
  }, undefined, { timeout: 20000 }).then(() => true).catch(() => false)
  if (!recordingStarted || liveRecordingHlsProgressBaseline === null) {
    const state = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused, readyState: video.readyState })).catch(() => null)
    ng.push(`ライブの「最初から」から録画 HLS v1 の実再生が始まらない (${JSON.stringify(state)}, baseline=${liveRecordingHlsProgressBaseline})`)
    liveGenerationRecovery = false
    await page.close()
    return
  }
  const advancedFromRecordingStart = await page.waitForFunction((baseline) => {
    const video = document.querySelector('video')
    return video && !video.paused && video.currentTime > 3 && window.__progressedSeconds >= baseline + 3
  }, liveRecordingHlsProgressBaseline, { timeout: 15000 }).then(() => true).catch(() => false)
  if (!advancedFromRecordingStart) {
    ng.push('ライブ画面の録画 HLS v1 が 3 秒以上再生されないため、再生位置復元を検査できない')
    liveGenerationRecovery = false
    await page.close()
    return
  }

  const nextMaster = page.waitForResponse((response) => new URL(response.url()).pathname === apiHlsPath(2, 'hd'), { timeout: 30000 })
    .then(() => true).catch(() => false)
  activeRecording = makeRecording(2)
  expiredRecordingHlsGeneration = 1
  const expiredAt = Date.now()
  const v2Loaded = await nextMaster
  log(`  410 から v2 master 要求まで=${((Date.now() - expiredAt) / 1000).toFixed(1)}s / 詳細 API 応答=${JSON.stringify(recordingHlsApiResponses)}`)
  if (recordingHls410Requests.length < 1) {
    ng.push(`ライブ画面の録画 HLS v1 が 410 にならない (${JSON.stringify(recordingHls410Requests)})`)
  }
  if (!v2Loaded) {
    const current = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused })).catch(() => null)
    ng.push(`ライブ画面の録画 HLS が v2 master を要求しない (video=${JSON.stringify(current)}, api=${JSON.stringify(recordingHlsApiResponses)}, 410=${recordingHls410Requests.length})`)
    liveGenerationRecovery = false
    await page.close()
    return
  }
  const stopSeconds = recordingHlsStopPositionSeconds
  if (typeof stopSeconds !== 'number' || !(stopSeconds > 3)) {
    ng.push(`ライブ画面の v2 handoff 時に再生中の位置を記録できない (stop=${stopSeconds})`)
    liveGenerationRecovery = false
    await page.close()
    return
  }
  const resumedAt = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && !video.paused && !video.seeking && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA
      ? video.currentTime
      : false
  }, undefined, { timeout: 10000 }).then((handle) => handle.jsonValue()).catch(() => null)
  log(`  v1 停止位置=${stopSeconds.toFixed(2)}s / v2 の再開位置=${typeof resumedAt === 'number' ? resumedAt.toFixed(2) : resumedAt}s`)
  if (typeof resumedAt !== 'number' || Math.abs(resumedAt - stopSeconds) > 1) {
    const current = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused }))
    ng.push(`ライブ画面の v2 が v1 停止位置（±1 秒）から再生中でない (stop=${stopSeconds}, resumed=${resumedAt}, current=${JSON.stringify(current)})`)
    liveGenerationRecovery = false
    await page.close()
    return
  }
  const playbackContinued = await page.waitForFunction((from) => {
    const video = document.querySelector('video')
    return video && !video.paused && video.currentTime > from + 0.5
  }, resumedAt, { timeout: 10000 }).then(() => true).catch(() => false)
  if (!playbackContinued) ng.push('ライブ画面の録画 HLS が v2 で再開位置から再生を続けない')

  expiredRecordingHlsGeneration = null
  liveGenerationRecovery = false
  await page.evaluate(() => localStorage.clear())
  await page.close()
}

await runRecordingApiHlsCheck()
await runRecordingApiHlsRecoveryCheck()
await runLiveRecordingApiHlsRecoveryCheck()
await finish(ng, browser)
