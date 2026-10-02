// 録画中の追っかけ再生の実ブラウザ判定。
//
// jsdom では測れないものだけを見る。録画中の録画詳細へ `#chase` で入り、
// 追っかけ位置のタイムラインを実際にドラッグし、pointer up まで stream を
// 張り直さないこと、選んだ offset の HLS セグメントから再生することを測る。
// あわせて EVENT playlist の成長・VOD と共通の再生速度・画質（プロファイル）の
// 切替（issue #874）を Chromium + 実 HLS セグメントで確認する。mirakc / DB の
// 録画ファイルは使わず、録画 API と HLS は page.route で差し替える。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:chase
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:chase   # ネイティブ HLS 経路
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

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []
const recordingStartAt = new Date(Date.now() - 25 * 60_000).toISOString()
const recordingStartedAt = new Date(Date.now() - 13 * 60_000).toISOString()
const evidenceDir = process.env.E2E_EVIDENCE_DIR

const recording = {
  id: 1,
  site: 'default',
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '録画中の番組',
  description: '追っかけ再生の位置タイムラインと映像の領域を確認する番組説明です。',
  startAt: recordingStartAt,
  durationMs: 60 * 60_000,
  status: 'recording',
  keepOriginal: 'always', cmDetection: { state: 'disabled' },
  startedAt: recordingStartedAt,
  createdAt: '2026-01-01T12:00:00Z',
  encodeProfiles: ['vod-h264'],
}
const recordingHeadOffsetSeconds = (Date.parse(recording.startedAt) - Date.parse(recording.startAt)) / 1000

/** ffmpeg の実 H.264/AAC セグメントを一度だけ作る。 */
function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-chase-fixture')
  const playlistPath = path.join(fixtureDir, 'playlist.m3u8')
  if (existsSync(playlistPath)) {
    const segments = readFileSync(playlistPath, 'utf8').match(/^#EXTINF:/gm) ?? []
    if (segments.length >= 3) return fixtureDir
  }

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(path.join(fixtureDir, 'segments'), { recursive: true })
  log(`追っかけ用 HLS フィクスチャを生成中... (${fixtureDir})`)
  execFileSync(
    'ffmpeg',
    [
      '-y',
      '-f',
      'lavfi',
      '-i',
      'testsrc=size=640x360:rate=25',
      '-f',
      'lavfi',
      '-i',
      'sine=frequency=440',
      '-t',
      '12',
      '-c:v',
      'libx264',
      '-profile:v',
      'baseline',
      '-level',
      '3.0',
      '-g',
      '50',
      '-keyint_min',
      '50',
      '-sc_threshold',
      '0',
      '-pix_fmt',
      'yuv420p',
      '-preset',
      'veryfast',
      '-c:a',
      'aac',
      '-b:a',
      '64k',
      '-f',
      'hls',
      '-hls_time',
      '2',
      '-hls_list_size',
      '0',
      '-hls_flags',
      'independent_segments',
      '-hls_base_url',
      'segments/',
      '-hls_segment_filename',
      'segments/segment_%03d.ts',
      'playlist.m3u8',
    ],
    { cwd: fixtureDir, stdio: 'ignore' },
  )
  return existsSync(playlistPath) ? fixtureDir : undefined
}

function eventPlaylists(fixtureDir) {
  const lines = readFileSync(path.join(fixtureDir, 'playlist.m3u8'), 'utf8')
    .split(/\r?\n/)
    .filter(Boolean)
  const firstEntry = lines.findIndex((line) => line.startsWith('#EXTINF:'))
  const header = lines
    .slice(0, firstEntry)
    .filter((line) => !line.startsWith('#EXT-X-PLAYLIST-TYPE:'))
  const entries = []
  for (let i = firstEntry; i < lines.length; i += 1) {
    if (!lines[i].startsWith('#EXTINF:')) continue
    entries.push([lines[i], lines[i + 1]])
  }
  const playlist = (count, end, sourceEntries = entries) =>
    [
      ...header,
      '#EXT-X-PLAYLIST-TYPE:EVENT',
      ...sourceEntries.slice(0, count).flat(),
      ...(end ? ['#EXT-X-ENDLIST'] : []),
    ].join('\n') + '\n'
  return { entries, playlist }
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const fixtureDir = ensureFixture()
if (fixtureDir === undefined) {
  log('  ffmpeg が無いため、追っかけの実ブラウザ判定は測れない（skip）')
  await finish(ng)
}

const { entries, playlist } = eventPlaylists(fixtureDir)
if (entries.length < 3) {
  ng.push(`フィクスチャのセグメント数が少なすぎる（${entries.length}）`)
  await finish(ng)
}
// The fixture has 2-second segments. The offset route deliberately serves a
// different playlist so the component test can inspect actual media selection.

// `E2E_BROWSER=webkit` で Safari 相当のネイティブ HLS 経路を通す。⑦ の位置の
// 持ち越しは hls.js（`startPosition`）とネイティブ（`loadedmetadata` / `canplay`
// での代入）で経路が別なので、両方で回す。
const engine = process.env.E2E_BROWSER ?? 'chromium'
log(`engine: ${engine}`)
const browser = await launchBrowser(engine)
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
})
await context.addInitScript(() => {
  const initializedKey = 'rokuban-e2e-chase-initialized'
  if (sessionStorage.getItem(initializedKey) === 'true') return
  localStorage.clear()
  sessionStorage.setItem(initializedKey, 'true')
})
const page = await context.newPage()
if (evidenceDir) mkdirSync(evidenceDir, { recursive: true })
async function captureEvidence(filename) {
  if (!evidenceDir) return
  const screenshotPath = path.join(evidenceDir, filename)
  await page.screenshot({ path: screenshotPath, fullPage: true })
  log(`  screenshot: ${screenshotPath}`)
}
const chaseLeaveHints = []
const playbackPositionWrites = []
const watchedWrites = []
let holdResumePositionSeed = false

function formatPlaybackTime(seconds) {
  const whole = Math.floor(Math.abs(seconds))
  const hours = Math.floor(whole / 3600)
  const minutes = Math.floor((whole % 3600) / 60)
  const remainder = whole % 60
  const formatted = hours > 0
    ? `${hours}:${String(minutes).padStart(2, '0')}:${String(remainder).padStart(2, '0')}`
    : `${minutes}:${String(remainder).padStart(2, '0')}`
  return seconds < 0 ? `-${formatted}` : formatted
}

function parsePlaybackTime(value) {
  const match = value.match(/(-)?(\d+):(\d{2})(?::(\d{2}))?/)
  if (!match) return Number.NaN
  const sign = match[1] === '-' ? -1 : 1
  const major = Number(match[2])
  const minute = Number(match[3])
  const second = Number(match[4] ?? 0)
  return sign * (match[4] === undefined ? major * 60 + minute : major * 3600 + minute * 60 + second)
}

await installApiStubs(page, async ({ path: requestPath, url, json, route }) => {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json(['default'])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/live-profiles') return json([
    { name: 'hd', height: 720 },
    { name: 'sd', height: 480 },
  ])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) {
    return route.fulfill({ status: 404 })
  }
  if (requestPath === '/api/recordings' && method === 'GET') return json([recording])
  if (requestPath === '/api/recordings/1' && method === 'GET') return json(recording)
  if (requestPath === '/api/recordings/1/playback-position' && method === 'PUT') {
    const body = route.request().postDataJSON()
    if (!holdResumePositionSeed) recording.resumePositionMs = body.positionMs
    playbackPositionWrites.push(body.positionMs)
    return route.fulfill({ status: 204 })
  }
  if (requestPath === '/api/recordings/1/playback-position' && method === 'DELETE') {
    delete recording.resumePositionMs
    return route.fulfill({ status: 204 })
  }
  if (requestPath === '/api/recordings/1/watched' && method === 'PUT') {
    recording.watchedAt = new Date().toISOString()
    delete recording.resumePositionMs
    watchedWrites.push(recording.watchedAt)
    return route.fulfill({ status: 204 })
  }
  if (/^\/api\/sites\/default\/recordings\/1\/chase(?:\/offset\/\d+)?\/leave$/.test(requestPath) && method === 'POST') {
    chaseLeaveHints.push(requestPath)
    return route.fulfill({ status: 204 })
  }
  // `url` is intentionally read here so the handler remains total if a future
  // page query adds a harmless query parameter to one of the stubs.
  void url
  return json([])
})

const chaseBase = '/api/sites/default/recordings/1/chase'
let playlistRequests = 0
const profilePlaylistURLs = []
const playlistSizes = []
let playlistEnded = false
let offsetPlaylistRequests = 0
const offsetObservations = new Map()

function observationForOffset(offsetSeconds) {
  let observation = offsetObservations.get(offsetSeconds)
  if (observation === undefined) {
    observation = {
      playlistRequestedAt: undefined,
      segmentNames: [],
    }
    offsetObservations.set(offsetSeconds, observation)
  }
  return observation
}

await page.route(`**${chaseBase}/playlist.m3u8*`, async (route) => {
  // The growing EVENT playlist keys off the number of requests, not the profile.
  // A profile switch is expected to add a request (the URL changes) without
  // resetting the position, so the fixture deliberately serves the same body.
  const requestedURL = new URL(route.request().url())
  if (requestedURL.searchParams.has('profile')) profilePlaylistURLs.push(requestedURL.href)
  playlistRequests += 1
  const count = playlistRequests < 2 ? 1 : entries.length
  const end = playlistRequests >= 2
  playlistSizes.push(count)
  playlistEnded ||= end
  await route.fulfill({
    status: 200,
    contentType: 'application/vnd.apple.mpegurl',
    body: playlist(count, end),
  })
})

await page.route(`**${chaseBase}/offset/*/playlist.m3u8*`, async (route) => {
  offsetPlaylistRequests += 1
  const match = new URL(route.request().url()).pathname.match(/\/offset\/(\d+)\/playlist\.m3u8$/)
  const offsetSeconds = match === null ? Number.NaN : Number(match[1])
  const observation = observationForOffset(offsetSeconds)
  observation.playlistRequestedAt ??= Date.now()
  const sourceEntries = entries.slice(Math.floor(offsetSeconds / 2))
  await route.fulfill({
    status: 200,
    contentType: 'application/vnd.apple.mpegurl',
    body: playlist(sourceEntries.length, true, sourceEntries),
  })
})

await page.route(`**${chaseBase}/segments/*`, async (route) => {
  const name = new URL(route.request().url()).pathname.split('/').pop()
  const file = path.join(fixtureDir, 'segments', name)
  if (!existsSync(file)) {
    await route.fulfill({ status: 404, body: 'not found' })
    return
  }
  await route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
})

await page.route(`**${chaseBase}/offset/*/segments/*`, async (route) => {
  const pathname = new URL(route.request().url()).pathname
  const match = pathname.match(/\/offset\/(\d+)\/segments\/([^/]+)$/)
  const offsetSeconds = match === null ? Number.NaN : Number(match[1])
  const name = match === null ? pathname.split('/').pop() : match[2]
  const observation = observationForOffset(offsetSeconds)
  observation.segmentNames.push(name)
  const file = path.join(fixtureDir, 'segments', name)
  if (!existsSync(file)) {
    await route.fulfill({ status: 404, body: 'not found' })
    return
  }
  await route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
})

log('\n=== ① 録画詳細の #chase で追っかけプレイヤーを開く ===')
await page.goto(`${URL_BASE}/recordings/1#chase`, { waitUntil: 'domcontentloaded' })
await page.getByRole('region', { name: '追っかけ再生' }).waitFor({ timeout: 15000 })
await page.locator('video').waitFor({ timeout: 15000 })

// The chase timeline belongs to the playback bar, with no second panel range.
const initialVideo = page.locator('video')
if (await initialVideo.evaluate((video) => video.controls)) {
  ng.push('① 追っかけ video に native controls が残っている')
}
const timelineSlider = page.getByRole('slider', { name: 'シークバー' })
if ((await timelineSlider.count()) !== 1) {
  ng.push('① 操作バーのシークバーが 1 本表示されない')
}
if ((await page.getByRole('slider', { name: '追っかけ再生の位置' }).count()) !== 0) {
  ng.push('① パネル側の追っかけ専用シークバーが残っている')
}
const externalTimelineCount = await page.evaluate(() =>
  Array.from(document.querySelectorAll('[data-testid^="chase-timeline-"]'))
    .filter((element) => element.closest('[data-testid="player-controls"]') === null)
    .length,
)
if (externalTimelineCount !== 0) {
  ng.push('① プレイヤー外に chase 専用タイムラインが残っている')
}
const initialFrameGeometry = await page.evaluate(() => {
  const region = document.querySelector('[aria-label="追っかけ再生"]')
  const shell = document.querySelector('[data-testid="recording-player-shell"]')
  const frame = document.querySelector('[data-testid="recording-player-frame"]')
  const controls = document.querySelector('[data-testid="player-controls"]')
  const track = document.querySelector('[data-testid="chase-timeline-track"]')
  const slider = document.querySelector('[data-testid="seek-scrub"]')
  if (!region || !shell || !frame || !controls || !track || !slider) return undefined
  return {
    regionWidth: region.getBoundingClientRect().width,
    shellWidth: shell.getBoundingClientRect().width,
    frameWidth: frame.getBoundingClientRect().width,
    controlsWidth: controls.getBoundingClientRect().width,
    trackWidth: track.getBoundingClientRect().width,
    sliderWidth: slider.getBoundingClientRect().width,
  }
})
if (initialFrameGeometry === undefined || [
  initialFrameGeometry.shellWidth,
  initialFrameGeometry.frameWidth,
  initialFrameGeometry.controlsWidth,
  initialFrameGeometry.trackWidth,
  initialFrameGeometry.sliderWidth,
].some((width) => width < initialFrameGeometry.regionWidth * 0.95)) {
  ng.push(`① chase player/control/track が詳細欄の幅の95%未満（${JSON.stringify(initialFrameGeometry)}）`)
} else {
  log(
    `  desktop player/bar/track: ${initialFrameGeometry.frameWidth.toFixed(0)}/${initialFrameGeometry.controlsWidth.toFixed(0)}/${initialFrameGeometry.trackWidth.toFixed(0)} / content ${initialFrameGeometry.regionWidth.toFixed(0)} px`,
  )
}

await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video !== null && Number.isFinite(video.duration) && video.duration > 0
  },
  { timeout: 15000 },
).catch(() => {
  ng.push('① 実 HLS の duration が確定しない')
})

if ((await timelineSlider.count()) !== 1) {
  ng.push('⑤ 操作バーの時間軸が表示されない')
  await finish(ng, browser)
}
if ((await page.getByLabel('追っかけ再生の開始位置（秒）').count()) !== 0) {
  ng.push('⑤ 内部秒数の入力欄が残っている')
}
if ((await page.getByRole('button', { name: 'この位置から再生' }).count()) !== 0) {
  ng.push('⑤ 位置選択の確定ボタンが残っている')
}
if ((await page.getByText(/まで（予定）/).count()) !== 1) {
  ng.push('⑤ タイムラインの予定終端が表示されない')
}

const initialTimeline = await timelineSlider.evaluate((slider) => {
  const track = slider.closest('[data-testid="chase-timeline-track"]')
  const recorded = track?.querySelector('[data-testid="chase-timeline-recorded"]')
  const liveEdge = track?.querySelector('[data-testid="chase-live-edge"]')
  const sliderRect = slider.getBoundingClientRect()
  const edgeRect = liveEdge?.getBoundingClientRect()
  return {
    max: Number(slider.getAttribute('aria-valuemax')),
    recordedWidth: Number.parseFloat(recorded?.style.width ?? '0'),
    hitWidth: sliderRect.width,
    hitHeight: sliderRect.height,
    edgeWidth: edgeRect?.width ?? 0,
    edgeHeight: edgeRect?.height ?? 0,
    track: track !== null,
  }
})
if (
  initialTimeline.hitWidth < 24 ||
  initialTimeline.hitHeight < 24 ||
  initialTimeline.edgeWidth < 24 ||
  initialTimeline.edgeHeight < 24 ||
  !initialTimeline.track
) {
  ng.push(
    `⑤ シークバーまたは先端印の当たり判定が不足（slider=${initialTimeline.hitWidth}x${initialTimeline.hitHeight}, edge=${initialTimeline.edgeWidth}x${initialTimeline.edgeHeight}, track=${initialTimeline.track}）`,
  )
}
await page
  .waitForFunction(
    (initialWidth) => {
      const recorded = document.querySelector('[data-testid="chase-timeline-recorded"]')
      return recorded !== null && Number.parseFloat(recorded.style.width) > initialWidth
    },
    initialTimeline.recordedWidth,
    { timeout: 3000 },
  )
  .catch(() => ng.push('⑤ 録画済みのつまみ範囲が 1 秒ごとに伸びない'))
const grownTimeline = await timelineSlider.evaluate((slider) => {
  const recorded = slider.closest('[data-testid="chase-timeline-track"]')
    ?.querySelector('[data-testid="chase-timeline-recorded"]')
  return {
    max: Number(slider.getAttribute('aria-valuemax')),
    recordedWidth: Number.parseFloat(recorded?.style.width ?? '0'),
  }
})
if (grownTimeline.recordedWidth <= initialTimeline.recordedWidth) {
  ng.push(
    `⑤ 録画済みの表示が時間とともに伸びない（${initialTimeline.recordedWidth}% → ${grownTimeline.recordedWidth}%）`,
  )
}

if (evidenceDir) {
  await page.getByRole('button', { name: '再生設定' }).click()
  await captureEvidence('chase-desktop-settings.png')
  await page.getByRole('button', { name: '再生設定' }).click()

  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('region', { name: '追っかけ再生' }).scrollIntoViewIfNeeded()
  const phoneFrame = await page.getByTestId('recording-player-frame').boundingBox()
  if (phoneFrame !== null) {
    await page.mouse.move(phoneFrame.x + phoneFrame.width / 2, phoneFrame.y + phoneFrame.height - 12)
  }
  const phoneGeometry = await page.evaluate(() => {
    const region = document.querySelector('[aria-label="追っかけ再生"]')
    const frame = document.querySelector('[data-testid="recording-player-frame"]')
    const controls = document.querySelector('[data-testid="player-controls"]')
    const track = document.querySelector('[data-testid="chase-timeline-track"]')
    const slider = document.querySelector('[data-testid="seek-scrub"]')
    if (!region || !frame || !controls || !track || !slider) return undefined
    return {
      region: region.getBoundingClientRect().width,
      frame: frame.getBoundingClientRect().width,
      controls: controls.getBoundingClientRect().width,
      track: track.getBoundingClientRect().width,
      slider: slider.getBoundingClientRect().width,
    }
  })
  if (phoneGeometry === undefined || [phoneGeometry.frame, phoneGeometry.controls, phoneGeometry.track, phoneGeometry.slider]
    .some((width) => width < phoneGeometry.region * 0.95)) {
    ng.push(`① mobile chase player/control/track が詳細欄の幅の95%未満（${JSON.stringify(phoneGeometry)}）`)
  } else {
    log(
      `  phone player/bar/track: ${phoneGeometry.frame.toFixed(0)}/${phoneGeometry.controls.toFixed(0)}/${phoneGeometry.track.toFixed(0)} / content ${phoneGeometry.region.toFixed(0)} px`,
    )
  }
  await page.waitForFunction(
    () => document.querySelector('[data-testid="player-controls"]')?.getAttribute('aria-hidden') === 'false',
    { timeout: 3000 },
  )
  await captureEvidence('chase-phone-390px.png')
  await page.getByRole('button', { name: '再生設定' }).click()
  await captureEvidence('chase-phone-settings-390px.png')
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.setViewportSize({ width: 1280, height: 900 })
}

if (playlistRequests === 0) {
  ng.push('① chase playlist が一度も要求されない')
}
const startTime = await page.locator('video').evaluate((video) => video.currentTime)
if (startTime > 2) {
  ng.push(`① 追っかけの開始位置が先頭でない（currentTime=${startTime}）`)
}

log('\n=== ② EVENT playlist の成長 ===')
const growthDeadline = Date.now() + 12_000
while (Date.now() < growthDeadline && new Set(playlistSizes).size < 2) {
  await page.waitForTimeout(250)
}
if (new Set(playlistSizes).size < 2) {
  ng.push(`② EVENT playlist が成長しない（sizes=${playlistSizes.join(',') || 'none'}）`)
}

if (!playlistEnded) {
  ng.push('② 成長後の playlist が ENDLIST にならない')
}

log('\n=== ③ セッション範囲内へのシークとドラッグプレビュー ===')
async function dragTimelineTo(second) {
  const bounds = await timelineSlider.boundingBox()
  if (bounds === null) {
    ng.push('③ 操作バーのシーク領域を測れない')
    return { requestedAt: Date.now(), selected: Number.NaN }
  }

  const min = Number(await timelineSlider.getAttribute('aria-valuemin'))
  const max = Number(await timelineSlider.getAttribute('aria-valuemax'))
  const requested = Math.max(min, Math.min(max, Math.round(second)))
  const pointFor = (value) => bounds.x + ((value - min) / (max - min)) * bounds.width
  // Playwright sends integer CSS-pixel coordinates. A one-hour axis is therefore
  // quantized by several seconds per pixel even when the requested target is an
  // integer second. Match the value that the browser can actually send.
  const targetClientX = Math.round(pointFor(requested))
  const expected = Math.round(min + ((targetClientX - bounds.x) / bounds.width) * (max - min))
  // The live-edge button overlays the current thumb when it is at the edge.
  // Start from a clear part of the track so this remains a genuine slider drag.
  const dragStart = Math.min(max, min + Math.min(1, (max - min) / 2))
  const y = bounds.y + bounds.height / 2
  const requestsBeforeRelease = playlistRequests
  const requestedAt = Date.now()

  await page.mouse.move(pointFor(dragStart), y)
  await page.mouse.down()
  await page.mouse.move(targetClientX, y, { steps: 8 })
  await page.waitForTimeout(200)

  const previewValue = Number(await timelineSlider.getAttribute('aria-valuenow'))
  const accessiblePreview = await timelineSlider.getAttribute('aria-valuetext')
  if (previewValue !== expected || !accessiblePreview?.startsWith(`${formatPlaybackTime(expected)} /`)) {
    ng.push(
      `③ ドラッグ中の時刻プレビューが位置と一致しない（slider=${previewValue}/${expected}, aria=${accessiblePreview}）`,
    )
  }
  if (playlistRequests !== requestsBeforeRelease) {
    ng.push(`③ pointer up 前に playlist を再要求する（${playlistRequests} 件）`)
  }

  await page.mouse.up()
  return {
    requestedAt,
    selected: previewValue,
    pixelResolutionSeconds: (max - min) / bounds.width,
  }
}

await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video !== null && video.seekable.length > 0 && video.seekable.end(video.seekable.length - 1) >= 4
  },
  { timeout: 10000 },
).catch(() => ng.push('③ in-range seekの前提となるseekable rangeが4秒まで揃わない'))
const playlistRequestsBeforeSeek = playlistRequests
const writesBeforeSeek = playbackPositionWrites.length
const { requestedAt: seekRequestedAt, selected: seekTarget } = await dragTimelineTo(recordingHeadOffsetSeconds + 3)
const expectedSeekTime = Math.max(0, Math.min(seekTarget - recordingHeadOffsetSeconds, 12))
if (playlistRequests !== playlistRequestsBeforeSeek) {
  ng.push('③ in-range dragでpointer upまでにplaylistを再要求する')
}
await page.waitForFunction(
  (expectedTime) => {
    const video = document.querySelector('video')
    return video !== null && Math.abs(video.currentTime - expectedTime) < 0.25
  },
  expectedSeekTime,
  { timeout: 5000 },
).catch(() => ng.push('③ in-range seekで currentTime が選択位置へ移らない'))
if (playlistRequests !== playlistRequestsBeforeSeek) {
  ng.push('③ in-range seekでplaylistを取り直した')
}
if (Date.now() - seekRequestedAt > 5000) ng.push('③ in-range seekが5秒以内に確定しない')
await page.locator('video').evaluate((video) => video.pause())
const positionDeadline = Date.now() + 5000
while (playbackPositionWrites.length === writesBeforeSeek && Date.now() < positionDeadline) {
  await page.waitForTimeout(50)
}
if (!playbackPositionWrites.slice(writesBeforeSeek).some((positionMs) => positionMs >= 2000)) {
  ng.push('③ in-range seek位置が原本時間軸のmsで保存されない')
}
if (watchedWrites.length > 0) ng.push('③ 録画中の追っかけで視聴済み印を付けた')

log('\n=== ④ 保存位置復元と明示0秒 ===')
recording.resumePositionMs = 5_000
holdResumePositionSeed = true
await page.reload({ waitUntil: 'domcontentloaded' })
await page.locator('video').waitFor({ timeout: 15000 })
await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video !== null && video.currentTime >= 4.5
  },
  { timeout: 10000 },
).catch(async () => {
  ng.push('④ startOffset未選択の追っかけで保存位置を復元しない')
})
holdResumePositionSeed = false
const baseRequestsBeforeZero = playlistRequests
const offsetRequestsBeforeZero = offsetPlaylistRequests
await timelineSlider.focus()
await page.keyboard.press('Home')
await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video !== null && video.currentTime < 1
  },
  { timeout: 5000 },
).catch(() => ng.push('④ Home/0秒で保存位置から先頭へ移動しない'))
if (playlistRequests !== baseRequestsBeforeZero) ng.push('④ 0秒へのin-range seekでplaylistを再要求する')
if (offsetPlaylistRequests !== offsetRequestsBeforeZero) ng.push('④ 0秒へのin-range seekでoffset playlistを要求する')
if (chaseLeaveHints.some((path) => path.includes('/offset/'))) {
  ng.push('④ 0秒へのin-range seekでoffset付きleaveヒントを送った')
}

log('\n=== ⑤ 予定尺を超えた録画のタイムライン ===')
recording.durationMs = 5_000
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByRole('region', { name: '追っかけ再生' }).waitFor({ timeout: 15000 })
const extendedSlider = page.getByRole('slider', { name: 'シークバー' })
await extendedSlider.waitFor({ timeout: 5000 })
const extensionGeometry = await page.evaluate(() => {
  const track = document.querySelector('[data-testid="chase-timeline-track"]')
    ?.querySelector('[data-testid="seek-scrub"]')
  const recorded = document.querySelector('[data-testid="chase-timeline-recorded"]')
  const marker = document.querySelector('[data-testid="chase-timeline-planned-end"]')
  if (!track || recorded === null || marker === null) return undefined
  const trackRect = track.getBoundingClientRect()
  const recordedRect = recorded.getBoundingClientRect()
  const markerRect = marker.getBoundingClientRect()
  return {
    trackLeft: trackRect.left,
    trackRight: trackRect.right,
    recordedRight: recordedRect.right,
    markerX: markerRect.left,
  }
})
if (extensionGeometry === undefined) {
  ng.push('⑤ 予定終端を超えた録画で予定マーカーが表示されない')
} else {
  if (extensionGeometry.markerX <= extensionGeometry.trackLeft || extensionGeometry.markerX >= extensionGeometry.trackRight) {
    ng.push('⑤ 予定終端マーカーが拡張されたタイムライン内に収まらない')
  }
  if (extensionGeometry.recordedRight > extensionGeometry.trackRight + 1) {
    ng.push('⑤ 録画済み表示が予定尺を超えてタイムラインからはみ出す')
  }
}
const extendedEndLabel = await page.getByTestId('chase-timeline-end').textContent()
if (!extendedEndLabel?.includes('録画中')) {
  ng.push(`⑤ 予定尺を超えた録画の右端に録画中の時刻が表示されない（${extendedEndLabel}）`)
}
await captureEvidence('chase-extended-desktop.png')
const liveEdgeButton = page.getByRole('button', { name: '録画の先端へ' })
const liveEdgeBounds = await liveEdgeButton.boundingBox()
if (liveEdgeBounds === null || liveEdgeBounds.width < 24 || liveEdgeBounds.height < 24) {
  ng.push(`⑤ 録画先端印の当たり判定が24px未満（${liveEdgeBounds?.width}x${liveEdgeBounds?.height}）`)
}
const offsetRequestsBeforeLiveEdge = offsetPlaylistRequests
const leaveHintsBeforeLiveEdge = chaseLeaveHints.length
await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video !== null &&
      video.readyState >= HTMLMediaElement.HAVE_METADATA &&
      video.seekable.length > 0 &&
      video.seekable.end(video.seekable.length - 1) > 10
  },
  { timeout: 10000 },
).catch(() => ng.push('⑤ 録画先端へ移動する前に metadata と seekable range が揃わない'))
await liveEdgeButton.scrollIntoViewIfNeeded()
const liveEdgeClickPoint = await liveEdgeButton.boundingBox()
if (liveEdgeClickPoint !== null) {
  await page.mouse.move(liveEdgeClickPoint.x + liveEdgeClickPoint.width / 2, liveEdgeClickPoint.y + liveEdgeClickPoint.height / 2)
}
await page.waitForFunction(
  () => document.querySelector('[data-testid="player-controls"]')?.getAttribute('aria-hidden') === 'false',
  { timeout: 3000 },
).catch(() => ng.push('⑤ 先端ボタンを押す前に操作バーが表示されない'))
const liveEdgeTargetInfo = await page.evaluate(() => {
  const slider = document.querySelector('[data-testid="seek-scrub"]')
  const marker = document.querySelector('[data-testid="chase-live-edge"]')
  const video = document.querySelector('video')
  if (!slider || !marker || !video || video.seekable.length === 0) return undefined
  const min = Number(slider.getAttribute('aria-valuemin'))
  const max = Number(slider.getAttribute('aria-valuemax'))
  const fraction = Number.parseFloat(marker.style.left) / 100
  const valueText = slider.getAttribute('aria-valuetext') ?? ''
  const recordedEndLabel = valueText.split('録画済み').at(-1)?.trim() ?? ''
  return {
    min,
    markerAxisSeconds: min + fraction * (max - min),
    recordedEndLabel,
    seekableEnd: video.seekable.end(video.seekable.length - 1),
  }
})
if (liveEdgeTargetInfo === undefined) {
  ng.push('⑤ 録画先端の位置を操作バーから読めない')
}
const liveEdgeRecordedEnd = liveEdgeTargetInfo === undefined
  ? Number.NaN
  : parsePlaybackTime(liveEdgeTargetInfo.recordedEndLabel)
const liveEdgeMarkerAxisSeconds = liveEdgeTargetInfo?.markerAxisSeconds ?? Number.NaN
if (Number.isFinite(liveEdgeRecordedEnd) &&
  Math.abs(liveEdgeRecordedEnd - liveEdgeMarkerAxisSeconds - 1) > 0.25) {
  ng.push(`⑤ 赤い先端markerが録画済み上限の1秒手前でない（録画済み=${liveEdgeRecordedEnd}, marker=${liveEdgeMarkerAxisSeconds}）`)
}
const expectedLiveEdgeCurrentTime = liveEdgeTargetInfo === undefined
  ? Number.NaN
  : Math.max(0, Math.min(
      Math.round(liveEdgeMarkerAxisSeconds - recordingHeadOffsetSeconds),
      liveEdgeTargetInfo.seekableEnd,
    ))
await liveEdgeButton.click()
await page.waitForTimeout(250)
await page.waitForFunction(
  (expectedTarget) => {
    const video = document.querySelector('video')
    if (video === null || video.seekable.length === 0) return false
    return Math.abs(video.currentTime - expectedTarget) < 0.25
  },
  expectedLiveEdgeCurrentTime,
  { timeout: 5000 },
).catch(() => ng.push('⑤ 先端の印を押しても指定した録画端（または seekable 上限）へ移動しない'))
const liveEdgeState = await page.locator('video').evaluate((video) => ({
  currentTime: video.currentTime,
  duration: video.duration,
  seekableEnd: video.seekable.length > 0 ? video.seekable.end(video.seekable.length - 1) : null,
  readyState: video.readyState,
  paused: video.paused,
}))
log(`  先端ボタン直後: current=${liveEdgeState.currentTime.toFixed(2)}, target=${expectedLiveEdgeCurrentTime.toFixed(2)}, seekable=${liveEdgeState.seekableEnd}, duration=${liveEdgeState.duration}, ready=${liveEdgeState.readyState}, paused=${liveEdgeState.paused}`)
if (offsetPlaylistRequests !== offsetRequestsBeforeLiveEdge) ng.push('⑤ 先端の印からoffset playlistを要求してsessionを作り直す')
if (chaseLeaveHints.length !== leaveHintsBeforeLiveEdge) ng.push('⑤ 先端の印からsession leave hintを送る')
// Keep the later save-position/VOD assertion independent from the marker check.
const requestsBeforeReturnSeek = playlistRequests
const { selected: returnSeekTarget } = await dragTimelineTo(recordingHeadOffsetSeconds + 5)
const expectedReturnSeekTime = Math.max(0, Math.min(returnSeekTarget - recordingHeadOffsetSeconds, 12))
const returnSeekState = await page.evaluate(() => ({
  currentTime: document.querySelector('video')?.currentTime,
  value: document.querySelector('[data-testid="seek-scrub"]')?.getAttribute('aria-valuenow'),
}))
log(`  先端から戻す drag 後: current=${returnSeekState.currentTime}, slider=${returnSeekState.value}`)
await page.waitForFunction(
  (expectedTime) => {
    const video = document.querySelector('video')
    return video !== null && Math.abs(video.currentTime - expectedTime) < 0.25
  },
  expectedReturnSeekTime,
  { timeout: 5000 },
).catch(() => ng.push('⑤ 録画先端から操作バーで5秒へ戻れない'))
if (playlistRequests !== requestsBeforeReturnSeek) ng.push('⑤ 先端から範囲内へ戻る操作でplaylistを再要求する')
const offsetRequestsBeforeEndKey = offsetPlaylistRequests
await timelineSlider.focus()
await page.keyboard.press('End')
await page.waitForFunction(
  (expectedTarget) => {
    const video = document.querySelector('video')
    return video !== null && video.seekable.length > 0 && Math.abs(video.currentTime - expectedTarget) < 0.25
  },
  expectedLiveEdgeCurrentTime,
  { timeout: 5000 },
).catch(() => ng.push('⑤ End キーで録画先端へ移動しない'))
if (offsetPlaylistRequests !== offsetRequestsBeforeEndKey) ng.push('⑤ End キーでoffset playlistを要求してsessionを作り直す')
const writesBeforeReturnToFive = playbackPositionWrites.length
const { selected: savedSeekTarget } = await dragTimelineTo(recordingHeadOffsetSeconds + 5)
const expectedSavedSeekTime = Math.max(0, Math.min(savedSeekTarget - recordingHeadOffsetSeconds, 12))
await page.waitForFunction(
  (expectedTime) => {
    const video = document.querySelector('video')
    return video !== null && !video.seeking && Math.abs(video.currentTime - expectedTime) < 0.25
  },
  expectedSavedSeekTime,
  { timeout: 5000 },
).catch(() => ng.push('⑤ End キーから操作バーで5秒へ戻れない'))
await page.locator('video').evaluate((video) => video.pause())
const returnWriteDeadline = Date.now() + 5000
while (
  !playbackPositionWrites.slice(writesBeforeReturnToFive).some((positionMs) => Math.abs(positionMs - 5000) < 100) &&
  Date.now() < returnWriteDeadline
) {
  await page.waitForTimeout(50)
}
if (!playbackPositionWrites.slice(writesBeforeReturnToFive).some((positionMs) => Math.abs(positionMs - expectedSavedSeekTime * 1000) < 100)) {
  ng.push('⑤ 範囲内へ戻した5秒位置が原本時間軸のmsで保存されない')
}

log('\n=== ⑥ VOD と共通の再生速度 ===')
await page.evaluate(() => localStorage.setItem('rokuban:playback-rate', '1.5'))
await page.reload({ waitUntil: 'domcontentloaded' })
await page.locator('video').waitFor({ timeout: 15000 })
await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video?.playbackRate === 1.5 && video.defaultPlaybackRate === 1.5
  },
  { timeout: 5000 },
).catch(() => ng.push('④ 保存済みの VOD 共通速度が追っかけ video に適用されない'))

await page.locator('video').evaluate((video) => {
  video.playbackRate = 1.25
})
await page.waitForFunction(
  () => localStorage.getItem('rokuban:playback-rate') === '1.25',
  { timeout: 5000 },
).catch(() => ng.push('④ 追っかけの ratechange が VOD 共通設定に保存されない'))

// --- ⑦ 画質（プロファイル）の切替（issue #874） ---
//
// 追っかけのセレクタは `/recordings/$id?liveProfile=<name>#chase` に置く。
// ここで測るのは jsdom では原理的に測れない 2 点である。
//
//   - 切替の前後で**再生位置が連続する**（`<video>` が作り直されると先頭に戻る）
//   - 切替で**離脱ヒントが飛ばない**（セッションを手放す合図であってはならない）
//
// 壊し方: `LivePlayer` を `key={profile}` で作り直す（位置が 0 に戻る）。
// あるいは `playbackProfile` に live の画質名を流す（位置のキーが画質ごとに分かれる）。
log('\n=== ⑦ 画質（プロファイル）の切替 ===')
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByRole('region', { name: '追っかけ再生' }).waitFor({ timeout: 15000 })
await page.getByRole('button', { name: '再生設定' }).click()
const profileSelect = page.locator('select[aria-label="画質"]')
await profileSelect.waitFor({ timeout: 15000 })
const defaultProfile = await profileSelect.inputValue()
if (defaultProfile !== 'hd') {
  ng.push(`⑦ 既定の画質が一覧の先頭でない（${defaultProfile}）`)
}
if (profilePlaylistURLs.length !== 0) {
  ng.push(`⑦ 既定のままで ?profile= を要求する（${profilePlaylistURLs.join(' / ')}）`)
}

const chaseVideo = page.locator('video')
await chaseVideo.waitFor({ timeout: 15000 })
await chaseVideo.evaluate(async (element) => {
  element.muted = true
  await element.play().catch(() => {})
})
await page
  .waitForFunction(
    () => {
      const element = document.querySelector('video')
      return element !== null && element.currentTime > 2
    },
    { timeout: 10000 },
  )
  .catch(() => ng.push('⑦ 実再生が 2 秒まで進まない'))

const leaveHintsBeforeSwitch = chaseLeaveHints.length
const writesBeforePause = playbackPositionWrites.length
const positionBeforeSwitch = await chaseVideo.evaluate((element) => {
  element.pause()
  return element.currentTime
})
const pauseWriteDeadline = Date.now() + 5000
while (playbackPositionWrites.length === writesBeforePause && Date.now() < pauseWriteDeadline) {
  await page.waitForTimeout(50)
}
// 切替の途中で「続きから」が上書きされないことを見る。最終値は切替後の seek で
// 正しい値に戻ってしまうので、profile 切替中に API へ送った位置をすべて記録する。
let savedPositionWriteIndex = playbackPositionWrites.length
const recordSavedPositions = () => { savedPositionWriteIndex = playbackPositionWrites.length }
const savedPositions = () => playbackPositionWrites.slice(savedPositionWriteIndex).map((positionMs) => positionMs / 1000)
recordSavedPositions()
log(`  切替前の再生位置: ${positionBeforeSwitch.toFixed(2)} 秒`)

await profileSelect.selectOption('sd')
const switchDeadline = Date.now() + 10_000
while (
  !profilePlaylistURLs.some((u) => u.includes('profile=sd')) &&
  Date.now() < switchDeadline
) {
  await page.waitForTimeout(100)
}
if (!profilePlaylistURLs.some((u) => u.includes('profile=sd'))) {
  ng.push('⑦ 画質を切り替えても ?profile=sd のプレイリスト要求が飛ばない')
}
if (chaseLeaveHints.length !== leaveHintsBeforeSwitch) {
  ng.push('⑦ 画質の切替で離脱ヒントが飛んだ（セッションを手放す合図であってはならない）')
}
await page.waitForTimeout(1500)
const positionAfterSwitch = await chaseVideo.evaluate((element) => element.currentTime)
log(`  切替後の再生位置: ${positionAfterSwitch.toFixed(2)} 秒`)
if (Math.abs(positionAfterSwitch - positionBeforeSwitch) > 1.5) {
  ng.push(
    `⑦ 画質の切替で再生位置が巻き戻った（${positionBeforeSwitch.toFixed(2)} → ${positionAfterSwitch.toFixed(2)} 秒）`,
  )
}
{
  const written = await savedPositions()
  log(`  切替中に保存された位置: [${written.join(', ')}]`)
  if (written.some((v) => Math.abs(v - positionBeforeSwitch) > 1.5)) {
    ng.push(`⑦ 画質の切替の途中で「続きから」が別の位置で上書きされた（[${written.join(', ')}]）`)
  }
}

// --- ⑧ 追っかけで見た位置から、完了後の VOD を開く（issue #975 受け入れ 2） ---
//
// ⑦ までに追っかけで見た位置はサーバー（スタブ）へ原本の ms で保存されている。録画を
// 完了にして `/recordings/1` を開き直すと、原本 VOD（original-vod の HLS）が同じ位置
// ± 数秒から始まる。壊し方: 原本 VOD の `resumePositionMs` を渡さない / 復元を無効にする。
log('\n=== ⑧ 完了後の VOD を開くと追っかけの位置から始まる ===')
const chasedMs = recording.resumePositionMs
if (chasedMs === undefined || chasedMs < 2000 || chasedMs > 9000) {
  ng.push(`⑧ 前提が成立しない: 追っかけの保存位置が 2〜9 秒の範囲に無い（${chasedMs}）`)
  await finish(ng, browser)
}
recording.status = 'finished'
recording.sizeBytes = 1_000_000
recording.encodedAssets = []
const originalVODBase = '/api/sites/default/recordings/1/original-vod'
await page.route(`**${originalVODBase}/playlist.m3u8*`, async (route) => {
  await route.fulfill({
    status: 200,
    contentType: 'application/vnd.apple.mpegurl',
    body: playlist(entries.length, true),
  })
})
await page.route(`**${originalVODBase}/segments/*`, async (route) => {
  const name = new URL(route.request().url()).pathname.split('/').pop()
  const file = path.join(fixtureDir, 'segments', name)
  if (!existsSync(file)) {
    await route.fulfill({ status: 404, body: 'not found' })
    return
  }
  await route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
})
await page.goto(`${URL_BASE}/recordings/1`, { waitUntil: 'domcontentloaded' })
await page.locator('video').waitFor({ timeout: 15000 })
await page
  .waitForFunction(
    () => {
      const element = document.querySelector('video')
      return element !== null && Number.isFinite(element.duration) && element.duration > 5
    },
    { timeout: 15000 },
  )
  .catch(() => ng.push('⑧ 完了後の VOD の duration が確定しない'))
await page.waitForTimeout(1000)
const vodStart = await page.locator('video').evaluate((element) => element.currentTime)
log(`  追っかけの保存位置 ${(chasedMs / 1000).toFixed(2)} 秒 → VOD の開始位置 ${vodStart.toFixed(2)} 秒`)
if (Math.abs(vodStart - chasedMs / 1000) > 3) {
  ng.push(
    `⑧ 完了後の VOD が追っかけで見た位置から始まらない（保存 ${(chasedMs / 1000).toFixed(2)} 秒 → currentTime ${vodStart.toFixed(2)} 秒）`,
  )
}

await finish(ng, browser)
