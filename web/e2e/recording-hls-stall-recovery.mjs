// 世代付き録画 HLS を遠くへシークした直後、ネイティブ HLS の停滞を実 Safari で測る。
// 実装前の再現と修正後の 5 回確認は web/e2e/checks/playback.md に記録する。
import { createServer } from 'node:http'
import { createReadStream, existsSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'

import { ListRecordingsResponseItem } from '../src/api/zod.ts'
import { createSafariAdapter, log, validateFixturesOrExit, verifyBundleMatchesOrExit } from './lib.mjs'
import { ensurePlaybackFixtures, SEGMENT_SECONDS } from './recording-segmented-fixture.mjs'

const WEB_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const DIST_ROOT = path.join(WEB_ROOT, 'dist')
const RECORDING_ID = 375
const SITE = 'default'
const PROFILE = 'hd'
const FAR_SEEK_SECONDS = Number(process.env.E2E_STALL_SEEK_SECONDS ?? 52)
const REPEAT_COUNT = Number(process.env.E2E_REPEAT_COUNT ?? 1)
const SEGMENT_READY_AFTER_MS = 4_000
const PROGRESS_TIMEOUT_MS = 10_000
const TARGET_SEGMENT_INDEX = Math.floor(FAR_SEEK_SECONDS / SEGMENT_SECONDS)
const ng = []

const startAt = '2025-01-01T00:00:00.000Z'
const endAt = new Date(Date.parse(startAt) + 60_000).toISOString()
const recordingForCase = (caseNumber) => ({
  id: RECORDING_ID,
  site: SITE,
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '録画 HLS の停滞復旧判定',
  description: '実 Safari で遠距離シーク後の復旧を測る。',
  startAt,
  durationMs: 60_000,
  status: 'finished',
  keepOriginal: 'always',
  hlsPlayback: [{
    profile: PROFILE,
    masterUrl: `/api/media/stores/media/recordings/${RECORDING_ID}/hls/v${caseNumber}/${PROFILE}/master.m3u8`,
  }],
  cmDetection: { state: 'disabled' },
  startedAt: startAt,
  endedAt: endAt,
  createdAt: endAt,
  sizeBytes: 2_000_000,
  encodeProfiles: [],
  encodedAssets: [],
  resumePositionMs: 0,
})

await validateFixturesOrExit([
  ['世代付き録画 HLS', ListRecordingsResponseItem, recordingForCase(1)],
], ng)

if (!existsSync(path.join(DIST_ROOT, 'index.html'))) {
  throw new Error('web/dist/index.html がない。先に `corepack pnpm build` を実行する')
}

const fixtureResult = ensurePlaybackFixtures()
const fixture = fixtureResult.fixtures.find((item) => item.format === 'ts' && item.kind === 'restarted')
if (!fixture) throw new Error('recording-segmented-fixture.mjs が ts/restarted を作らなかった')

function playlistSegments(variant) {
  const playlist = readFileSync(path.join(fixture.directory, `${variant}.m3u8`), 'utf8')
  return playlist.split(/\r?\n/).filter((line) => line.startsWith('segments/'))
}

const targetedSegments = new Set()
const videoTargets = playlistSegments('video').slice(TARGET_SEGMENT_INDEX, TARGET_SEGMENT_INDEX + 3)
if (videoTargets.length !== 3) throw new Error(`遠距離シーク先 ${FAR_SEEK_SECONDS} 秒の 3 セグメントを特定できない`)
for (const variant of ['video', 'standard', 'main', 'sub']) {
  const segments = playlistSegments(variant).slice(TARGET_SEGMENT_INDEX, TARGET_SEGMENT_INDEX + 3)
  if (segments.length !== 3) throw new Error(`${variant} のシーク先と続く 2 セグメントを特定できない`)
  for (const segment of segments) targetedSegments.add(segment)
}
const videoTarget = videoTargets[0]

const cases = new Map()
let activeCase = 1
const caseState = (caseNumber) => {
  if (!cases.has(caseNumber)) {
    cases.set(caseNumber, {
      firstTargetAt: null,
      firstRequestAtBySegment: new Map(),
      targetRequests: [],
      playbackPositionWrites: [],
    })
  }
  return cases.get(caseNumber)
}

function sendJSON(response, value, status = 200) {
  response.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'cache-control': 'no-store',
  })
  response.end(JSON.stringify(value))
}

function contentType(filename) {
  switch (path.extname(filename)) {
    case '.html': return 'text/html; charset=utf-8'
    case '.js': return 'text/javascript; charset=utf-8'
    case '.css': return 'text/css; charset=utf-8'
    case '.json': return 'application/json; charset=utf-8'
    case '.svg': return 'image/svg+xml'
    case '.png': return 'image/png'
    case '.webp': return 'image/webp'
    case '.woff2': return 'font/woff2'
    case '.m3u8': return 'application/vnd.apple.mpegurl'
    case '.ts': return 'video/mp2t'
    default: return 'application/octet-stream'
  }
}

function sendFile(response, filename) {
  if (!existsSync(filename) || !statSync(filename).isFile()) {
    response.writeHead(404)
    response.end()
    return
  }
  response.writeHead(200, {
    'content-type': contentType(filename),
    'content-length': statSync(filename).size,
    'cache-control': filename.endsWith('.m3u8') ? 'no-store' : 'public, max-age=0',
  })
  createReadStream(filename).pipe(response)
}

function apiResponse(request, response, url) {
  const method = request.method ?? 'GET'
  const pathname = url.pathname
  if (method === 'GET' && pathname === '/api/sites') return sendJSON(response, [SITE])
  if (method === 'GET' && pathname === '/api/capabilities') return sendJSON(response, { live: true, cmDetect: false })
  if (method === 'GET' && pathname === '/api/live-profiles') return sendJSON(response, [{ name: PROFILE, height: 720 }])
  if (method === 'GET' && pathname === '/api/recordings') return sendJSON(response, [recordingForCase(activeCase)])
  if (method === 'GET' && pathname === `/api/recordings/${RECORDING_ID}`) {
    return sendJSON(response, recordingForCase(activeCase))
  }
  if (method === 'GET' && pathname === `/api/recordings/${RECORDING_ID}/chapters`) {
    return sendJSON(response, { source: 'auto', version: 'auto:detected:1', detectionPending: false, spans: [] })
  }
  if (method === 'GET' && pathname === `/api/recordings/${RECORDING_ID}/drop-stats`) return sendJSON(response, [])
  if (method === 'PUT' && pathname === `/api/recordings/${RECORDING_ID}/playback-position`) {
    let body = ''
    request.setEncoding('utf8')
    request.on('data', (chunk) => { body += chunk })
    request.on('end', () => {
      const parsed = JSON.parse(body)
      caseState(activeCase).playbackPositionWrites.push({ at: Date.now(), positionMs: parsed.positionMs })
      response.writeHead(204)
      response.end()
    })
    return
  }
  if (method === 'DELETE' && pathname === `/api/recordings/${RECORDING_ID}/playback-position`) {
    response.writeHead(204)
    response.end()
    return
  }
  if (method === 'GET' && pathname === '/api/events') {
    response.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' })
    response.write('retry: 86400000\n\n')
    request.on('close', () => response.end())
    return
  }
  if (method !== 'GET' && pathname.endsWith('/leave')) {
    response.writeHead(204)
    response.end()
    return
  }
  if (method === 'GET' && (
    pathname === '/api/encode-profiles' || pathname === '/api/rules' || pathname === '/api/breakers' ||
    pathname === '/api/reservations' || pathname === '/api/encode-queue' || pathname === '/api/recording-shelves' ||
    /^\/api\/sites\/[^/]+\/services$/.test(pathname)
  )) return sendJSON(response, pathname === '/api/encode-queue' ? { queued: 0, running: 0 } : [])

  return sendJSON(response, { error: `unexpected API request: ${method} ${pathname}` }, 404)
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url ?? '/', `http://${request.headers.host ?? '127.0.0.1'}`)
  const caseNumber = Number(url.searchParams.get('e2eCase'))
  if (url.pathname === `/recordings/${RECORDING_ID}` && Number.isInteger(caseNumber) && caseNumber > 0) {
    activeCase = caseNumber
    caseState(activeCase)
  }

  if (url.pathname.startsWith('/api/')) {
    const mediaMatch = new RegExp(`^/api/media/stores/media/recordings/${RECORDING_ID}/hls/v(\\d+)/${PROFILE}/(.+)$`).exec(url.pathname)
    if (request.method === 'GET' && mediaMatch) {
      const requestedCase = Number(mediaMatch[1])
      const relative = decodeURIComponent(mediaMatch[2])
      const filename = path.resolve(fixture.directory, relative)
      if (!filename.startsWith(`${path.resolve(fixture.directory)}${path.sep}`) && filename !== path.join(fixture.directory, 'master.m3u8')) {
        response.writeHead(400)
        response.end()
        return
      }
      if (relative.startsWith('segments/') && targetedSegments.has(relative)) {
        const state = caseState(requestedCase)
        const requestAt = Date.now()
        const firstRequestAt = state.firstRequestAtBySegment.get(relative) ?? requestAt
        state.firstRequestAtBySegment.set(relative, firstRequestAt)
        if (relative === videoTarget && state.firstTargetAt === null) state.firstTargetAt = requestAt
        const key = `${relative}:${state.targetRequests.filter((item) => item.relative === relative).length}`
        const readyAt = firstRequestAt + SEGMENT_READY_AFTER_MS
        const targetRequest = { relative, requestAt, firstRequestAt, readyAt, key }
        state.targetRequests.push(targetRequest)
        await delay(Math.max(0, readyAt - Date.now()))
        if (response.destroyed) return
        targetRequest.servedAt = Date.now()
      }
      return sendFile(response, filename)
    }
    return apiResponse(request, response, url)
  }

  if (request.method !== 'GET' && request.method !== 'HEAD') {
    response.writeHead(405)
    response.end()
    return
  }
  const decodedPath = decodeURIComponent(url.pathname)
  const candidate = path.resolve(DIST_ROOT, `.${decodedPath}`)
  const safe = candidate === DIST_ROOT || candidate.startsWith(`${DIST_ROOT}${path.sep}`)
  const filename = safe && existsSync(candidate) && statSync(candidate).isFile()
    ? candidate
    : path.join(DIST_ROOT, 'index.html')
  if (request.method === 'HEAD') {
    if (!existsSync(filename)) {
      response.writeHead(404)
      response.end()
      return
    }
    response.writeHead(200, { 'content-type': contentType(filename), 'content-length': statSync(filename).size })
    response.end()
    return
  }
  return sendFile(response, filename)
})

await new Promise((resolve, reject) => {
  server.once('error', reject)
  server.listen(0, '127.0.0.1', resolve)
})
const address = server.address()
const URL_BASE = `http://127.0.0.1:${address.port}`

let safari
try {
  await verifyBundleMatchesOrExit(URL_BASE, ng)
  safari = await createSafariAdapter()
  log(`\n=== ${safari.version}: 世代付き録画 HLS の遠距離シーク (${REPEAT_COUNT} 回) ===`)
  for (let caseNumber = 1; caseNumber <= REPEAT_COUNT; caseNumber += 1) {
    const state = caseState(caseNumber)
    const pageURL = `${URL_BASE}/recordings/${RECORDING_ID}?e2eCase=${caseNumber}`
    await safari.navigate(pageURL)
    const pageReady = await safari.evaluate(async ({ selector, timeoutMs }) => {
      const deadline = performance.now() + timeoutMs
      while (performance.now() < deadline) {
        const button = document.querySelector(selector)
        if (button && !button.disabled) return true
        await new Promise((resolve) => setTimeout(resolve, 50))
      }
      return false
    }, { selector: '[data-testid="recording-playback-start"]', timeoutMs: 30_000 })
    if (!pageReady) {
      ng.push(`回 ${caseNumber}: 録画詳細画面の再生ボタンが現れない`)
      continue
    }
    await safari.click('[data-testid="recording-playback-start"]')
    const started = await safari.evaluate(async ({ timeoutMs }) => {
      const video = document.querySelector('video')
      if (!video) return { started: false, reason: 'video 要素がない' }
      video.muted = true
      const deadline = performance.now() + timeoutMs
      while (performance.now() < deadline) {
        if (video.readyState >= HTMLMediaElement.HAVE_METADATA && !video.paused && video.currentTime > 1) {
          return { started: true, currentTime: video.currentTime, source: video.currentSrc }
        }
        await new Promise((resolve) => setTimeout(resolve, 50))
      }
      return { started: false, reason: '再生開始を確認できない', paused: video.paused, readyState: video.readyState, currentTime: video.currentTime }
    }, { timeoutMs: 20_000 })
    if (!started.started) {
      ng.push(`回 ${caseNumber}: 初回再生に失敗 (${JSON.stringify(started)})`)
      continue
    }

    const result = await safari.evaluate(async ({ target, timeoutMs }) => {
      const video = document.querySelector('video')
      if (!video) return { seeked: false, reason: 'video 要素がない' }
      const originalVideo = video
      let seeked = false
      try {
        await new Promise((resolve, reject) => {
          const timer = setTimeout(() => reject(new Error('seeked が届かない')), 8_000)
          video.addEventListener('seeked', () => {
            clearTimeout(timer)
            seeked = true
            resolve(undefined)
          }, { once: true })
          video.currentTime = target
        })
      } catch (error) {
        return { seeked, reason: String(error), currentTime: video.currentTime, paused: video.paused }
      }
      const seekedAt = performance.now()
      const positionAtSeek = video.currentTime
      const deadline = seekedAt + timeoutMs
      let endTime = video.currentTime
      while (performance.now() < deadline) {
        const currentVideo = document.querySelector('video')
        const errorText = document.body.innerText
        const errorVisible = errorText.includes('録画映像が届いていません') ||
          errorText.includes('録画映像を再生できませんでした') || errorText.includes('録画 HLS の再生でエラーが発生しました')
        endTime = video.currentTime
        if (currentVideo !== originalVideo || errorVisible) {
          return {
            seeked,
            progressed: false,
            sameVideo: currentVideo === originalVideo,
            errorVisible,
            elapsedMs: performance.now() - seekedAt,
            positionAtSeek,
            endTime,
            paused: video.paused,
          }
        }
        if (!video.paused && endTime >= target + 0.25) {
          return {
            seeked,
            progressed: true,
            sameVideo: true,
            errorVisible: false,
            elapsedMs: performance.now() - seekedAt,
            positionAtSeek,
            endTime,
            paused: video.paused,
          }
        }
        await new Promise((resolve) => setTimeout(resolve, 50))
      }
      const currentVideo = document.querySelector('video')
      const errorText = document.body.innerText
      return {
        seeked,
        progressed: false,
        sameVideo: currentVideo === originalVideo,
        errorVisible: errorText.includes('録画映像が届いていません') ||
          errorText.includes('録画映像を再生できませんでした') || errorText.includes('録画 HLS の再生でエラーが発生しました'),
        elapsedMs: performance.now() - seekedAt,
        positionAtSeek,
        endTime,
        paused: video.paused,
      }
    }, { target: FAR_SEEK_SECONDS, timeoutMs: PROGRESS_TIMEOUT_MS })

    await delay(100)
    const measuredDelays = state.targetRequests
      .filter((request) => request.relative === videoTarget && request.servedAt !== undefined)
      .map((request) => request.servedAt - request.firstRequestAt)
    const badPositionWrites = state.firstTargetAt === null
      ? []
      : state.playbackPositionWrites.filter((write) => write.at >= state.firstTargetAt && write.positionMs < FAR_SEEK_SECONDS * 1000 - 500)
    const expectedSeek = Math.abs((result.positionAtSeek ?? 0) - FAR_SEEK_SECONDS) <= 1
    const delayed = measuredDelays.some((elapsed) => elapsed >= SEGMENT_READY_AFTER_MS - 100)
    const casePassed = result.seeked && result.progressed && result.sameVideo && !result.errorVisible &&
      expectedSeek && state.firstTargetAt !== null && delayed && badPositionWrites.length === 0
    log(`回 ${caseNumber}: ${casePassed ? 'OK' : 'NG'} ${JSON.stringify({
      result,
      targetSegment: videoTarget,
      targetRequestCount: state.targetRequests.filter((request) => request.relative === videoTarget).length,
      responseDelaysMs: measuredDelays,
      playbackPositionWrites: state.playbackPositionWrites.map((write) => write.positionMs),
    })}`)
    if (!casePassed) ng.push(`回 ${caseNumber}: 10 秒以内に再開する条件を満たさない`)
  }
} catch (error) {
  ng.push(error instanceof Error ? error.message : String(error))
} finally {
  await safari?.close().catch(() => {})
  await new Promise((resolve) => server.close(resolve))
}

if (ng.length > 0) {
  for (const message of ng) log(`NG: ${message}`)
  process.exitCode = 1
} else {
  log(`\nOK: ${REPEAT_COUNT}/${REPEAT_COUNT} 回でシーク後 10 秒以内の再生再開を確認`)
}
