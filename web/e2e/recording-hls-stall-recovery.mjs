// 世代付き録画 HLS を遠くへシークした直後、ネイティブ HLS の停滞を実 Safari で測る。
// シークは利用者と同じ実ポインタでシークバーを押す。測定結果は web/e2e/checks/playback.md に記録する。
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

const PAUSE_WAIT_MS = 8_000

/** monitor はページの中で媒体イベントと製品の play / pause / load 呼び出しを記録する。 */
function installMonitor({ timeoutMs }) {
  return (async () => {
    const deadline = performance.now() + timeoutMs
    let video = null
    while (performance.now() < deadline && !(video = document.querySelector('video'))) {
      await new Promise((resolve) => setTimeout(resolve, 20))
    }
    if (!video) return false
    const state = { origin: performance.now(), events: [], calls: { play: 0, pause: 0, load: 0 }, seekAt: null }
    window.__e2e = state
    const record = (name) => state.events.push([Math.round(performance.now() - state.origin), name, Number(video.currentTime.toFixed(2)), video.paused, video.readyState])
    for (const name of ['loadstart', 'emptied', 'loadedmetadata', 'canplay', 'waiting', 'stalled', 'playing', 'play', 'pause', 'seeking', 'seeked', 'error']) {
      video.addEventListener(name, () => record(name))
    }
    // 製品の呼び出し回数。Safari 自身の pause はここに入らない。
    for (const name of ['play', 'pause', 'load']) {
      const original = HTMLMediaElement.prototype[name]
      HTMLMediaElement.prototype[name] = function (...args) {
        if (this === video) state.calls[name] += 1
        return original.apply(this, args)
      }
    }
    document.addEventListener('pointerdown', () => { state.seekAt ??= performance.now() }, { capture: true })
    window.__e2eVideo = video
    return true
  })()
}

/** mediaSessionSetup はページが読み込まれた後、製品の登録より前に setActionHandler を包む。 */
function wrapMediaSession() {
  window.__handlers = {}
  if (typeof MediaSession === 'undefined') return false
  const original = MediaSession.prototype.setActionHandler
  MediaSession.prototype.setActionHandler = function (action, handler) {
    window.__handlers[action] = handler
    return original.call(this, action, handler)
  }
  return true
}

function waitForPlaying({ timeoutMs }) {
  return (async () => {
    const state = window.__e2e
    const video = window.__e2eVideo
    const deadline = performance.now() + timeoutMs
    while (performance.now() < deadline) {
      if (video.readyState >= HTMLMediaElement.HAVE_METADATA && !video.paused && video.currentTime > 1) {
        return { started: true, currentTime: video.currentTime }
      }
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    return { started: false, paused: video.paused, readyState: video.readyState, currentTime: video.currentTime, events: state.events }
  })()
}

/** 操作バーを出し、シークバーの位置と長さを返す。 */
function scrubRect() {
  const scrub = document.querySelector('[data-testid="seek-scrub"]')
  const frame = document.querySelector('[data-testid="recording-player-frame"]')
  if (!scrub || !frame) return null
  const box = scrub.getBoundingClientRect()
  const frameBox = frame.getBoundingClientRect()
  return {
    x: box.left, y: box.top + box.height / 2, width: box.width,
    frameX: frameBox.left + frameBox.width / 2, frameY: frameBox.top + frameBox.height / 2,
    duration: Number(scrub.getAttribute('aria-valuemax')),
  }
}

/** observe は seekAt（シークバーを押した時刻）から 10 秒、再生が戻るかを見る。 */
function observeRecovery({ target, timeoutMs }) {
  return (async () => {
    const state = window.__e2e
    const video = window.__e2eVideo
    const seekAt = state.seekAt ?? performance.now()
    const deadline = seekAt + timeoutMs
    const positionAtSeek = (state.events.find((event) => event[1] === 'seeking' && event[0] >= seekAt - state.origin - 5)?.[2]) ?? null
    const errorVisible = () => /録画映像が届いていません|録画映像を再生できませんでした|録画 HLS の再生でエラーが発生しました/.test(document.body.innerText)
    let progressed = false
    while (performance.now() < deadline) {
      if (document.querySelector('video') !== video || errorVisible()) break
      if (!video.paused && video.currentTime >= target + 0.25) { progressed = true; break }
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    const sinceSeek = state.events.filter((event) => event[0] >= seekAt - state.origin)
    return {
      progressed,
      sameVideo: document.querySelector('video') === video,
      errorVisible: errorVisible(),
      elapsedMs: Math.round(performance.now() - seekAt),
      positionAtSeek,
      endTime: Number(video.currentTime.toFixed(2)),
      paused: video.paused,
      reloaded: sinceSeek.some((event) => event[1] === 'loadstart'),
      calls: state.calls,
      events: sinceSeek.map((event) => [event[0] - Math.round(seekAt - state.origin), ...event.slice(1)]),
    }
  })()
}

async function startCase(safari, caseNumber) {
  await safari.navigate(`${URL_BASE}/recordings/${RECORDING_ID}?e2eCase=${caseNumber}`)
  const pageReady = await safari.evaluate(async ({ selector, timeoutMs }) => {
    const deadline = performance.now() + timeoutMs
    while (performance.now() < deadline) {
      const button = document.querySelector(selector)
      if (button && !button.disabled) return true
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    return false
  }, { selector: '[data-testid="recording-playback-start"]', timeoutMs: 30_000 })
  if (!pageReady) return { error: '録画詳細画面の再生ボタンが現れない' }
  await safari.evaluate(wrapMediaSession)
  // 押した操作が効かなかったときだけ 1 回押し直し、押し直したことは結果に残す。
  let installed = false
  for (let attempt = 1; attempt <= 2 && !installed; attempt += 1) {
    await safari.click('[data-testid="recording-playback-start"]').catch(() => {})
    installed = await safari.evaluate(installMonitor, { timeoutMs: attempt === 1 ? 4_000 : 10_000 })
    if (!installed) {
      const dom = await safari.evaluate(() => {
        const button = document.querySelector('[data-testid="recording-playback-start"]')
        return {
          url: location.href,
          readyState: document.readyState,
          startButton: button ? { disabled: button.disabled, text: button.textContent } : null,
          hasPlayerFrame: document.querySelector('[data-testid="recording-player-frame"]') !== null,
          body: document.body.innerText.slice(0, 300),
        }
      })
      log(`  開始操作 ${attempt} 回目の後に video 要素がない: ${JSON.stringify(dom)}`)
      if (attempt === 2) return { error: `video 要素がない (${JSON.stringify(dom)})` }
    }
  }
  const started = await safari.evaluate(waitForPlaying, { timeoutMs: 20_000 })
  if (!started.started) return { error: `初回再生に失敗 (${JSON.stringify(started)})` }
  return {}
}

/** seekByPointer は利用者と同じく、操作バーを出してシークバーの FAR_SEEK_SECONDS の位置を押す。 */
async function seekByPointer(safari) {
  let rect = await safari.evaluate(scrubRect)
  if (!rect) return null
  // 前の回と同じ座標への移動は mousemove を出さず、操作バーが出ないまま押してしまう。一度ずらしてから戻す。
  await safari.pointerMove(rect.frameX - 40, rect.frameY - 40)
  await safari.pointerMove(rect.frameX, rect.frameY)
  await delay(300)
  rect = await safari.evaluate(scrubRect)
  await safari.pointerClick(rect.x + rect.width * (FAR_SEEK_SECONDS / rect.duration), rect.y)
  return rect
}

let safari
try {
  await verifyBundleMatchesOrExit(URL_BASE, ng)
  safari = await createSafariAdapter()
  log(`\n=== ${safari.version}: 世代付き録画 HLS の遠距離シーク (${REPEAT_COUNT} 回 + 一時停止 2 回) ===`)
  const modes = [...Array(REPEAT_COUNT).fill('seek'), 'user-pause', 'media-session-pause']
  for (const [index, mode] of modes.entries()) {
    const caseNumber = index + 1
    const label = `回 ${caseNumber} (${mode})`
    const state = caseState(caseNumber)
    const started = await startCase(safari, caseNumber)
    if (started.error) {
      ng.push(`${label}: ${started.error}`)
      continue
    }
    const seekRect = await seekByPointer(safari)
    // 押してもシークが始まらない回がある（実ポインタの click が操作バーに届かない）。製品の NG と
    // 区別して、判定スクリプト側の NG として出す。押し直すと再生が先へ進んだ位置で測ってしまう。
    const seekStarted = () => safari.evaluate(async () => {
      const state = window.__e2e
      const deadline = performance.now() + 1_500
      while (performance.now() < deadline) {
        if (state.seekAt !== null && state.events.some((event) => event[1] === 'seeking' && event[0] >= state.seekAt - state.origin - 5)) return true
        await new Promise((resolve) => setTimeout(resolve, 20))
      }
      return false
    })
    if (!seekRect) {
      ng.push(`${label}: シークバーが見つからない`)
      continue
    }
    if (!await seekStarted()) {
      ng.push(`${label}: 判定スクリプト側: シークバーを押しても seeking が出ない（座標 ${JSON.stringify(seekRect)}）`)
      continue
    }

    let pauseOperation = null
    if (mode !== 'seek') {
      // 止まった（waiting が出た）ところで、利用者の操作で一時停止する。
      const stalled = await safari.evaluate(async () => {
        const state = window.__e2e
        const deadline = performance.now() + 3_000
        while (performance.now() < deadline) {
          if (state.events.some((event) => event[1] === 'waiting' && event[0] >= state.seekAt - state.origin)) return true
          await new Promise((resolve) => setTimeout(resolve, 20))
        }
        return false
      })
      if (!stalled) {
        const diag = await safari.evaluate(() => {
          const state = window.__e2e
          const video = window.__e2eVideo
          return { seekAt: state.seekAt, paused: video.paused, currentTime: video.currentTime, readyState: video.readyState, calls: state.calls, events: state.events.slice(-12), rect: document.querySelector('[data-testid="seek-scrub"]')?.getBoundingClientRect().toJSON() ?? null }
        }).catch((error) => String(error))
        ng.push(`${label}: シーク後に waiting が出ない (押した座標 ${JSON.stringify(seekRect)}, ${JSON.stringify(diag)})`)
        continue
      }
      if (mode === 'user-pause') {
        await safari.click('button[aria-label="一時停止"]')
        pauseOperation = '操作バーの一時停止ボタン'
      } else {
        const called = await safari.evaluate(() => {
          const handler = window.__handlers?.pause
          if (typeof handler !== 'function') return false
          handler({ action: 'pause' })
          return true
        })
        if (!called) {
          log(`${label}: ページから mediaSession の pause ハンドラを呼べない（登録なし）`)
          ng.push(`${label}: mediaSession の pause ハンドラが登録されていない`)
          continue
        }
        pauseOperation = 'mediaSession の pause ハンドラ'
      }
    }

    let result
    let casePassed
    if (mode === 'seek') {
      result = await safari.evaluate(observeRecovery, { target: FAR_SEEK_SECONDS, timeoutMs: PROGRESS_TIMEOUT_MS })
    } else {
      // 5 秒の読み直し（と Safari 自身の再開）が起きないことを PAUSE_WAIT_MS 見る。
      await delay(PAUSE_WAIT_MS)
      result = await safari.evaluate(() => {
        const state = window.__e2e
        const video = window.__e2eVideo
        const since = state.events.filter((event) => event[0] >= state.seekAt - state.origin)
        return {
          sameVideo: document.querySelector('video') === video,
          paused: video.paused,
          endTime: Number(video.currentTime.toFixed(2)),
          reloaded: since.some((event) => event[1] === 'loadstart'),
          calls: state.calls,
          events: since.map((event) => [event[0] - Math.round(state.seekAt - state.origin), ...event.slice(1)]),
        }
      })
    }
    await delay(100)
    const measuredDelays = state.targetRequests
      .filter((request) => request.relative === videoTarget && request.servedAt !== undefined)
      .map((request) => request.servedAt - request.firstRequestAt)
    const badPositionWrites = state.firstTargetAt === null
      ? []
      : state.playbackPositionWrites.filter((write) => write.at >= state.firstTargetAt && write.positionMs < FAR_SEEK_SECONDS * 1000 - 500)
    const delayed = measuredDelays.some((elapsed) => elapsed >= SEGMENT_READY_AFTER_MS - 100)
    if (mode === 'seek') {
      const expectedSeek = Math.abs((result.positionAtSeek ?? 0) - FAR_SEEK_SECONDS) <= 1
      casePassed = result.progressed && result.sameVideo && !result.errorVisible &&
        expectedSeek && state.firstTargetAt !== null && delayed && badPositionWrites.length === 0
    } else {
      casePassed = result.sameVideo && result.paused && !result.reloaded && result.endTime < FAR_SEEK_SECONDS + 0.25 &&
        result.calls.pause >= 1 && state.firstTargetAt !== null && badPositionWrites.length === 0
    }
    const reloadNote = mode === 'seek' ? (result.reloaded ? '製品が読み直した' : '読み直しなし（Safari の自力回復）') : pauseOperation
    log(`${label}: ${casePassed ? 'OK' : 'NG'} [${reloadNote}] ${JSON.stringify({
      result,
      targetSegment: videoTarget,
      targetRequestCount: state.targetRequests.filter((request) => request.relative === videoTarget).length,
      responseDelaysMs: measuredDelays,
      playbackPositionWrites: state.playbackPositionWrites.map((write) => write.positionMs),
    })}`)
    if (!casePassed) ng.push(`${label}: 判定を満たさない`)
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
  log(`\nOK: シーク ${REPEAT_COUNT}/${REPEAT_COUNT} 回でシークバー操作から 10 秒以内の再生再開を確認し、一時停止の 2 回は読み直しも再生もなかった`)
}
