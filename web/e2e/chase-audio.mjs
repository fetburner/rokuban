// 追っかけの音声切替を、実 ffmpeg の音声 rendition と実ブラウザで確かめる。
//
// Chromium は WebAudio で標準 / 主 / 副の左右周波数を測る。各トラックを 15 秒
// 聴いてから戻り、EVENT playlist に PDT が無くても再生が止まらないことを見る。
// さらに同じ rendition 形式での範囲内 seek、offset セッションへの張り直し、保存位置
// からの開始を測る。WebKit は native HLS の audioTracks と再生継続を測る。
// streamer がこの形を出すことは TestBuildChaseFFmpegArgs_RealFFmpegAudioRenditions
// で実 ffmpeg を使って固定する。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:chase
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:chase
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync } from 'node:fs'
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
const RECORDING_ID = 1
const PROFILE = 'hd'
const FIXTURE_SECONDS = 180
const DWELL_MS = 15_000
const BASE_PLAYLIST_INITIAL_SEGMENTS = 45
const BASE_PLAYLIST_MAX_SEGMENTS = 79
const ng = []
const skipped = []
const startAt = new Date(Date.now() - 240_000).toISOString()
const recording = {
  id: RECORDING_ID,
  site: SITE,
  source: 'manual',
  serviceName: '二重音声テスト局',
  channelType: 'GR',
  channel: '99',
  networkId: 1,
  serviceId: 9201,
  eventId: 1,
  title: '追っかけ音声のテスト',
  description: '標準・主・副の音声 rendition を切り替える番組です。',
  startAt,
  durationMs: 60 * 60_000,
  status: 'recording',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  startedAt: startAt,
  createdAt: '2026-01-01T12:00:00Z',
  encodeProfiles: ['vod-h264'],
}

function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), `rokuban-e2e-chase-audio-${FIXTURE_SECONDS}s`)
  const masterPath = path.join(fixtureDir, `${PROFILE}.m3u8`)
  if (existsSync(masterPath)) {
    const master = readFileSync(masterPath, 'utf8')
    if ((master.match(/#EXT-X-MEDIA:TYPE=AUDIO/g) ?? []).length === 3) return fixtureDir
  }

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(path.join(fixtureDir, 'segments'), { recursive: true })
  const inputPath = path.join(fixtureDir, 'input.ts')
  log(`追っかけ音声用 HLS を生成中... (${fixtureDir})`)
  execFileSync(
    'ffmpeg',
    [
      '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
      '-f', 'lavfi', '-i', 'testsrc2=size=320x180:rate=25',
      '-f', 'lavfi', '-i', 'sine=f=440:r=48000',
      '-f', 'lavfi', '-i', 'sine=f=880:r=48000',
      '-filter_complex', '[1:a][2:a]join=inputs=2:channel_layout=stereo[a]',
      '-map', '0:v:0', '-map', '[a]', '-t', String(FIXTURE_SECONDS),
      '-c:v', 'mpeg2video', '-b:v', '600k', '-c:a', 'aac', '-f', 'mpegts', inputPath,
    ],
    { stdio: 'ignore' },
  )
  execFileSync(
    'ffmpeg',
    [
      '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
      '-probesize', '5M', '-analyzeduration', '3M', '-f', 'mpegts', '-i', inputPath,
      '-map', '0:v:0', '-map', '0:a:0', '-map', '0:a:0', '-map', '0:a:0',
      '-c:v', 'libx264', '-c:a', 'aac',
      '-filter:a:1', 'pan=stereo|c0=c0|c1=c0',
      '-filter:a:2', 'pan=stereo|c0=c1|c1=c1',
      '-preset', 'veryfast', '-force_key_frames', 'expr:gte(t,n_forced*2)',
      '-var_stream_map', 'v:0,agroup:aud a:0,agroup:aud,default:yes a:1,agroup:aud a:2,agroup:aud',
      '-master_pl_name', `${PROFILE}.m3u8`,
      '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0',
      '-hls_playlist_type', 'event', '-hls_flags', 'temp_file',
      '-hls_segment_filename', path.join(fixtureDir, 'segments', `${PROFILE}.%v_seg%05d.ts`),
      '-hls_base_url', 'segments/', `${PROFILE}.%v.m3u8`,
    ],
    { cwd: fixtureDir, stdio: 'ignore' },
  )
  return existsSync(masterPath) ? fixtureDir : undefined
}

function parseEventPlaylist(filePath) {
  const lines = readFileSync(filePath, 'utf8').split(/\r?\n/).filter(Boolean)
  const firstEntry = lines.findIndex((line) => line.startsWith('#EXTINF:'))
  const header = lines
    .slice(0, firstEntry)
    .filter((line) => !line.startsWith('#EXT-X-PLAYLIST-TYPE:') && line !== '#EXT-X-ENDLIST')
  const entries = []
  for (let i = firstEntry; i >= 0 && i < lines.length; i += 1) {
    if (lines[i].startsWith('#EXTINF:')) entries.push([lines[i], lines[i + 1]])
  }
  return { header, entries }
}

function renderEventPlaylist(source, entries, count) {
  return [
    ...source.header,
    '#EXT-X-PLAYLIST-TYPE:EVENT',
    ...entries.slice(0, count).flat(),
  ].join('\n') + '\n'
}

async function waitForProbe(probe, timeoutMs) {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    const result = await probe()
    if (result !== null && result !== undefined) return result
    await new Promise((r) => setTimeout(r, 200))
  }
  return null
}

async function selectChaseAudio(page, choice) {
  const label = choice === 'main' ? '主音声' : choice === 'sub' ? '副音声' : '標準'
  await page.getByTestId('recording-player-frame').hover()
  await page.getByRole('button', { name: '再生設定' }).click()
  const menuState = await page.evaluate(() => Array.from(document.querySelectorAll('[role="menu"]')).map((menu) => ({
    label: menu.getAttribute('aria-label'),
    text: menu.innerText,
  })))
  const settings = page.getByRole('menu', { name: '再生設定' })
  const audioRow = settings.getByRole('menuitem', { name: '音声', exact: true })
  if (await audioRow.count() !== 1) throw new Error(`追っかけ設定に音声の行がない: ${JSON.stringify(menuState)} (URL: ${page.url()})`)
  await audioRow.click()
  await page.getByRole('menuitemradio', { name: label, exact: true }).click()
}

async function startPlaybackWithAnalyser(page, expected = [440, 880]) {
  await page.locator('video').waitFor({ timeout: 20_000 })
  await page.evaluate(async () => {
    const video = document.querySelector('video')
    const context = new AudioContext()
    const source = context.createMediaElementSource(video)
    const split = context.createChannelSplitter(2)
    const left = context.createAnalyser()
    const right = context.createAnalyser()
    left.fftSize = right.fftSize = 8192
    source.connect(split)
    split.connect(left, 0)
    split.connect(right, 1)
    source.connect(context.destination)
    await context.resume()
    const peak = (analyser) => {
      const bins = new Float32Array(analyser.frequencyBinCount)
      analyser.getFloatFrequencyData(bins)
      let at = 0
      for (let i = 1; i < bins.length; i += 1) if (bins[i] > bins[at]) at = i
      return Math.round((at * context.sampleRate) / analyser.fftSize)
    }
    window.__measureChaseAudio = () => ({
      left: peak(left),
      right: peak(right),
      currentTime: Number(video.currentTime.toFixed(1)),
    })
    await video.play()
  })
  const selected = await waitForChannels(page, expected[0], expected[1], 20_000)
  if (!selected.ok) throw new Error(`追っかけの指定音声が再生されない: ${JSON.stringify(selected.last)}`)
}

async function waitForChannels(page, wantLeft, wantRight, timeoutMs) {
  const start = Date.now()
  const initial = await page.evaluate(() => window.__measureChaseAudio())
  let last = initial
  while (Date.now() - start < timeoutMs) {
    last = await page.evaluate(() => window.__measureChaseAudio())
    if (Math.abs(last.left - wantLeft) <= 12 && Math.abs(last.right - wantRight) <= 12) {
      return { ok: true, last, initial, elapsed: Date.now() - start }
    }
    await new Promise((r) => setTimeout(r, 250))
  }
  return { ok: false, last, initial }
}

async function checkChromiumAudio(page, masterRequests) {
  log('\n=== Chromium: 音声 rendition を実際に聞き分け、戻っても再生が続く ===')
  await startPlaybackWithAnalyser(page)
  const before = masterRequests.length
  const steps = [
    ['main', '主音声', 440, 440],
    ['sub', '副音声', 880, 880],
    [undefined, '標準', 440, 880],
    ['main', '主音声へ戻る', 440, 440],
  ]
  for (const [choice, label, wantLeft, wantRight] of steps) {
    const startURL = page.url()
    const positionBeforeSwitch = await page.locator('video').evaluate((video) => video.currentTime)
    const switchStartedAt = Date.now()
    await selectChaseAudio(page, choice)
    const url = new URL(page.url())
    const got = await waitForChannels(page, wantLeft, wantRight, 15_000)
    const switchElapsedSeconds = (Date.now() - switchStartedAt) / 1000
    const expectedPosition = positionBeforeSwitch + switchElapsedSeconds
    if (Math.abs(got.last.currentTime - expectedPosition) > 5) {
      ng.push(`追っかけ ${label}: 音声切替で再生位置が連続しない（${positionBeforeSwitch.toFixed(1)}→${got.last.currentTime.toFixed(1)} 秒、切替 ${switchElapsedSeconds.toFixed(1)} 秒）`)
    }
    const expectedParam = choice ?? null
    if (!got.ok) {
      ng.push(`追っかけ ${label}: L/R=${got.last.left}/${got.last.right} Hz、再生位置 ${got.initial.currentTime}→${got.last.currentTime}`)
      continue
    }
    await page.waitForTimeout(DWELL_MS)
    const held = await page.evaluate(() => window.__measureChaseAudio())
    if (Math.abs(held.left - wantLeft) > 12 || Math.abs(held.right - wantRight) > 12 || held.currentTime - got.last.currentTime < DWELL_MS / 2000) {
      ng.push(`追っかけ ${label}: ${DWELL_MS}ms 後に L/R=${held.left}/${held.right} Hz、再生位置 ${got.last.currentTime}→${held.currentTime}`)
    } else {
      log(`  OK: ${label} → ${got.last.left}/${got.last.right} Hz、${DWELL_MS}ms 再生を維持`)
    }
    if ((url.searchParams.get('audio') ?? null) !== expectedParam || new URL(startURL).hash !== url.hash) {
      ng.push(`追っかけ ${label}: ?audio= の値または #chase が保たれない（${url.href}）`)
    }
  }
  if (masterRequests.length !== before) {
    ng.push(`追っかけ音声の切替で master playlist を取り直した（${masterRequests.length - before} 回）`)
  } else {
    log('  OK: 音声切替で master playlist を取り直さない')
  }
}

async function checkWebKitAudio(page, masterRequests) {
  log('\n=== WebKit: native HLS の audioTracks を切り替えて再生が続く ===')
  await page.locator('video').waitFor({ timeout: 20_000 })
  await page.locator('video').evaluate(async (video) => {
    const wasMuted = video.muted
    if (video.paused) video.muted = true
    await video.play()
    if (!wasMuted) video.muted = false
  })
  const tracks = await waitForProbe(async () => {
    const count = await page.locator('video').evaluate((video) => video.audioTracks?.length ?? 0)
    return count === 3 ? count : null
  }, 20_000)
  if (tracks === null) {
    ng.push('追っかけ native HLS の video.audioTracks が 3 本にならない')
    return
  }

  const before = masterRequests.length
  for (const [choice, index, label] of [
    ['main', 1, '主音声'],
    ['sub', 2, '副音声'],
    [undefined, 0, '標準'],
    ['main', 1, '主音声へ戻る'],
  ]) {
    const start = await page.locator('video').evaluate((video) => video.currentTime)
    const switchStartedAt = Date.now()
    await selectChaseAudio(page, choice)
    const enabled = await waitForProbe(async () => {
      const state = await page.locator('video').evaluate((video) =>
        Array.from(video.audioTracks).map((track) => track.enabled),
      )
      return JSON.stringify(state) === JSON.stringify([0, 1, 2].map((n) => n === index)) ? state : null
    }, 10_000)
    const switchedTo = await page.locator('video').evaluate((video) => video.currentTime)
    const switchElapsedSeconds = (Date.now() - switchStartedAt) / 1000
    if (enabled !== null && Math.abs(switchedTo - (start + switchElapsedSeconds)) > 5) {
      ng.push(`追っかけ ${label}: 音声切替で再生位置が連続しない（${start.toFixed(1)}→${switchedTo.toFixed(1)} 秒、切替 ${switchElapsedSeconds.toFixed(1)} 秒）`)
    }
    await page.waitForTimeout(DWELL_MS)
    const currentTime = await page.locator('video').evaluate((video) => video.currentTime)
    if (enabled === null) {
      ng.push(`追っかけ ${label}: native audioTracks の選択が反映されない`)
    } else if (currentTime - start < DWELL_MS / 2000) {
      ng.push(`追っかけ ${label}: 選択後に再生が止まる（${start}→${currentTime} 秒）`)
    } else {
      log(`  OK: ${label} → audioTracks=${JSON.stringify(enabled)}、${DWELL_MS}ms 再生を維持`)
    }
  }
  if (masterRequests.length !== before) ng.push('追っかけ音声の切替で master playlist を取り直した')
}

async function seekTo(page, seconds) {
  const slider = page.getByTestId('seek-scrub')
  await page.getByTestId('recording-player-frame').hover()
  const bounds = await slider.boundingBox()
  if (bounds === null) throw new Error('追っかけシークバーを測れない')
  const min = Number(await slider.getAttribute('aria-valuemin'))
  const max = Number(await slider.getAttribute('aria-valuemax'))
  const requested = Math.max(min, Math.min(max, Math.round(seconds)))
  const point = (value) => bounds.x + ((value - min) / (max - min)) * bounds.width
  const targetX = Math.round(point(requested))
  const selected = Math.round(min + ((targetX - bounds.x) / bounds.width) * (max - min))
  const y = bounds.y + bounds.height / 2
  await page.mouse.move(point(min + 1), y)
  await page.mouse.down()
  await page.mouse.move(targetX, y, { steps: 8 })
  await page.mouse.up()
  return selected
}

async function waitForOffset(masterRequests, count, timeoutMs = 20_000) {
  return waitForProbe(async () => masterRequests.length > count ? masterRequests.at(-1) : null, timeoutMs)
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: 録画 fixture の zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const fixtureDir = ensureFixture()
if (fixtureDir === undefined) {
  skipped.push('ffmpeg が無いため追っかけ音声 rendition を生成できず測れない')
  log('\n=== 測れなかった項目 ===')
  skipped.forEach((message) => log(`  SKIP: ${message}`))
  await finish(ng, null)
}

const masterPath = path.join(fixtureDir, `${PROFILE}.m3u8`)
const master = readFileSync(masterPath, 'utf8')
const renditionFiles = [0, 1, 2, 3].map((index) => path.join(fixtureDir, `${PROFILE}.${index}.m3u8`))
const renditions = renditionFiles.map(parseEventPlaylist)
const videoEntries = renditions[0].entries
if ((master.match(/#EXT-X-MEDIA:TYPE=AUDIO/g) ?? []).length !== 3 || videoEntries.length < 80) {
  throw new Error(`追っかけ HLS fixture が不完全: audio=${(master.match(/#EXT-X-MEDIA:TYPE=AUDIO/g) ?? []).length}, video segments=${videoEntries.length}`)
}

const engine = process.env.E2E_BROWSER ?? 'chromium'
log(`engine: ${engine}`)
let browser
try {
  browser = await launchBrowser(engine, engine === 'chromium' ? { args: ['--autoplay-policy=no-user-gesture-required'] } : {})
} catch (err) {
  skipped.push(`${engine} を起動できない: ${err}`)
  log('\n=== 測れなかった項目 ===')
  skipped.forEach((message) => log(`  SKIP: ${message}`))
  await finish(ng, null)
}

const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
const page = await context.newPage()
await context.addInitScript(() => localStorage.clear())
const chaseBase = `/api/sites/${SITE}/recordings/${RECORDING_ID}/chase`
const masterRequests = []
const offsetPlaylistRequests = []
const offsetVideoFirstSegment = new Map()
let playbackPositionWrites = 0
const recordingResumeReads = []
let holdSavedPosition = false
let baseStartedAt

await installApiStubs(page, async ({ path: requestPath, json: replyJSON, route }) => {
  const method = route.request().method()
  if (requestPath === '/api/sites') return replyJSON([SITE])
  if (requestPath === '/api/capabilities') return replyJSON({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers') return replyJSON([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return replyJSON([])
  if (requestPath === '/api/live-profiles') return replyJSON([{ name: PROFILE, height: 720 }])
  if (requestPath === '/api/encode-queue') return replyJSON({ queued: 0, running: 0 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) return route.fulfill({ status: 404 })
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') {
    recordingResumeReads.push(recording.resumePositionMs)
    return replyJSON(recording)
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/playback-position` && method === 'PUT') {
    playbackPositionWrites += 1
    if (!holdSavedPosition) recording.resumePositionMs = route.request().postDataJSON().positionMs
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/playback-position` && method === 'DELETE') {
    delete recording.resumePositionMs
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapters`) {
    return replyJSON({ version: 'chapters-v1', detectionPending: false, source: 'auto', spans: [] })
  }
  if (requestPath === `${chaseBase}/leave` || requestPath.startsWith(`${chaseBase}/offset/`) && requestPath.endsWith('/leave')) {
    return route.fulfill({ status: 204 })
  }
  return replyJSON([])
})

await page.route((url) => url.pathname === `${chaseBase}/playlist.m3u8` || /^\/api\/sites\/default\/recordings\/1\/chase\/offset\/\d+\/playlist\.m3u8$/.test(url.pathname), async (route) => {
  const url = new URL(route.request().url())
  const match = url.pathname.match(/\/offset\/(\d+)\/playlist\.m3u8$/)
  const offset = match === null ? undefined : Number(match[1])
  if (offset === undefined) {
    baseStartedAt ??= Date.now()
    masterRequests.push(url.href)
  } else {
    offsetPlaylistRequests.push(offset)
    masterRequests.push(url.href)
  }
  await route.fulfill({
    status: 200,
    contentType: 'application/vnd.apple.mpegurl',
    headers: { 'cache-control': 'no-store' },
    body: master,
  })
})

await page.route((url) => /\/chase(?:\/offset\/\d+)?\/hd\.[0-3]\.m3u8$/.test(url.pathname), async (route) => {
  const url = new URL(route.request().url())
  const match = url.pathname.match(/\/offset\/(\d+)\/hd\.(\d)\.m3u8$/)
  const offset = match === null ? undefined : Number(match[1])
  const renditionMatch = url.pathname.match(/\/hd\.(\d)\.m3u8$/)
  const renditionIndex = Number(renditionMatch?.[1])
  const source = renditions[renditionIndex]
  const entries = offset === undefined ? source.entries : source.entries.slice(Math.floor(offset / 2))
  // Native HLS starts near the EVENT edge. Grow this window with playback, then stop at
  // 158 seconds so WebKit can finish the 4 × 15 second checks while the 160 second seek
  // still has to create an offset session.
  const baseCount = BASE_PLAYLIST_INITIAL_SEGMENTS + Math.floor((Date.now() - baseStartedAt) / 1000)
  const count = offset === undefined ? Math.min(entries.length, BASE_PLAYLIST_MAX_SEGMENTS, baseCount) : entries.length
  const body = renderEventPlaylist(source, entries, count)
  if (offset !== undefined && renditionIndex === 0) offsetVideoFirstSegment.set(offset, entries[0]?.[1])
  await route.fulfill({
    status: 200,
    contentType: 'application/vnd.apple.mpegurl',
    headers: { 'cache-control': 'no-store' },
    body,
  })
})

await page.route((url) => /\/chase(?:\/offset\/\d+)?\/segments\/[^/]+\.ts$/.test(url.pathname), async (route) => {
  const url = new URL(route.request().url())
  const name = url.pathname.split('/').pop()
  const file = path.join(fixtureDir, 'segments', name)
  if (!existsSync(file)) return route.fulfill({ status: 404, body: 'not found' })
  return route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
})

try {
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}#chase`, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video !== null && video.readyState >= 1 && video.videoWidth > 0
  }, undefined, { timeout: 30_000 })

  if (engine === 'chromium') await checkChromiumAudio(page, masterRequests)
  else if (engine === 'webkit') await checkWebKitAudio(page, masterRequests)
  else ng.push(`未対応のブラウザ: ${engine}`)

  log('\n=== 音声 rendition 形式でのセッション内 seek ===')
  const beforeInRangeSeek = masterRequests.length
  const inRange = await seekTo(page, 10)
  await page.waitForFunction((target) => {
    const video = document.querySelector('video')
    return video !== null && Math.abs(video.currentTime - target) < 1
  }, inRange, { timeout: 10_000 }).catch(() => ng.push(`同じ EVENT セッション内の ${inRange}s seek が反映されない`))
  if (masterRequests.length !== beforeInRangeSeek) ng.push('同じ EVENT セッション内の seek で master playlist を取り直した')
  else log(`  OK: ${inRange}s seek は同じ rendition セッションを使う`)

  log('\n=== 音声 rendition 形式での offset 張り直し ===')
  const requestedOffsetBefore = offsetPlaylistRequests.length
  const outOfRange = await seekTo(page, 160)
  const requestedURL = await waitForOffset(offsetPlaylistRequests, requestedOffsetBefore)
  if (requestedURL === null) {
    ng.push(`現在の EVENT セッション範囲外 ${outOfRange}s の seek で offset playlist を要求しない`)
  } else {
    const requestedOffset = offsetPlaylistRequests.at(-1)
    const expectedOffset = Math.floor(outOfRange)
    if (requestedOffset !== expectedOffset) ng.push(`offset playlist は ${requestedOffset}s を要求、選択位置は ${outOfRange}s`)
    const expectedSegment = videoEntries[Math.floor(expectedOffset / 2)]?.[1]
    const firstSegment = await waitForProbe(
      async () => offsetVideoFirstSegment.get(requestedOffset) ?? null,
      20_000,
    )
    if (firstSegment !== expectedSegment) ng.push(`offset ${requestedOffset}s の最初の動画 segment が一致しない（${firstSegment} / ${expectedSegment}）`)
    await page.waitForFunction(() => {
      const video = document.querySelector('video')
      return video !== null && video.readyState >= 1 && video.currentTime < 2
    }, undefined, { timeout: 10_000 }).catch(() => ng.push('張り直し後の追っかけが新しい EVENT playlist の先頭から始まらない'))
    if (requestedOffset === expectedOffset && firstSegment === expectedSegment) log(`  OK: offset ${requestedOffset}s の master と先頭 segment を取得`)
  }

  log('\n=== 保存位置から音声を選んで開始 ===')
  const savedOffsetBefore = offsetPlaylistRequests.length
  holdSavedPosition = true
  recording.resumePositionMs = 145_000
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}?audio=sub#chase`, { waitUntil: 'domcontentloaded' })
  // Force a fresh detail query even if the browser reused this same-document route transition.
  await page.reload({ waitUntil: 'domcontentloaded' })
  const savedOffsetURL = await waitForOffset(offsetPlaylistRequests, savedOffsetBefore)
  if (savedOffsetURL === null || offsetPlaylistRequests.at(-1) !== 145) {
    ng.push(`保存位置 145s から追っかけを始めず、offset 要求は ${offsetPlaylistRequests.at(-1) ?? '無し'}（recording resume reads=${JSON.stringify(recordingResumeReads)}, URL=${page.url()}）`)
  } else {
    await page.waitForFunction(() => {
      const video = document.querySelector('video')
      return video !== null && video.readyState >= 1 && video.videoWidth > 0
    }, undefined, { timeout: 30_000 }).catch(() => ng.push('保存位置の offset rendition が読み込まれない'))
    if (engine === 'chromium') {
      await startPlaybackWithAnalyser(page, [880, 880])
      log('  OK: 保存位置 145s から始まり、副音声が選ばれる')
    } else {
      const tracks = await waitForProbe(async () => {
        const state = await page.locator('video').evaluate((video) =>
          Array.from(video.audioTracks ?? []).map((track) => track.enabled),
        )
        return JSON.stringify(state) === '[false,false,true]' ? state : null
      }, 15_000)
      if (tracks === null) ng.push('保存位置開始時に WebKit が副音声を選択しない')
      else log('  OK: 保存位置 145s から始まり、副音声の track が選ばれる')
    }
  }
} catch (err) {
  ng.push(`追っかけ音声 e2e の実行中に失敗: ${err}`)
} finally {
  await browser.close()
}

log('\n=== 測れなかった項目 ===')
if (skipped.length === 0) log('  なし')
else skipped.forEach((message) => log(`  SKIP: ${message}`))

await finish(ng, null)
