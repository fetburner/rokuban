import { execFileSync } from 'node:child_process'
import { createHash, createHmac, randomUUID } from 'node:crypto'
import { createServer, request as httpRequest } from 'node:http'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

import { createPlaywrightAdapter, createSafariAdapter } from './lib.mjs'

const MINIO_URL = new URL(process.env.E2E_MINIO_URL ?? 'http://127.0.0.1:9000')
const ACCESS_KEY = process.env.E2E_MINIO_ACCESS_KEY ?? 'rokuban'
const SECRET_KEY = process.env.E2E_MINIO_SECRET_KEY ?? 'rokuban-test-secret'
const REGION = 'us-east-1'
const RUN_ID = randomUUID().replace(/-/g, '').slice(0, 12)
const GOOD_BUCKET = 'rokuban-1614-cors-' + RUN_ID
const BAD_BUCKET = 'rokuban-1614-no-cors-' + RUN_ID
const createdBuckets = []
const createdObjects = []
const RESUME_KEY = 'rokuban-s3-302-resume-position'
const VIDEO_TTL_SECONDS = Number(process.env.E2E_S3_VIDEO_TTL_SECONDS ?? 600)
const EXPIRED_TTL_SECONDS = 5
const VIDEO_SECONDS = 90
// MinIO の CORS 許可がこのオリジンに固定されているので、変えられない。
const FIXTURE_PORT = 4174
const S3_PROXY_PORT = 4175
const TARGET_SEEK_SECONDS = 32
const BROWSER = process.env.E2E_BROWSER ?? 'chrome'
const requests = []
const failures = []

if (MINIO_URL.protocol !== 'http:') {
  throw new Error('E2E_MINIO_URL must use http for the local MinIO fixture')
}
if (!Number.isInteger(VIDEO_TTL_SECONDS) || VIDEO_TTL_SECONDS < 1 || VIDEO_TTL_SECONDS > 604800) {
  throw new Error('E2E_S3_VIDEO_TTL_SECONDS must be an integer from 1 to 604800')
}
if (BROWSER !== 'chrome' && BROWSER !== 'safari') {
  throw new Error('E2E_BROWSER must be chrome or safari')
}

function sha256(data) {
  return createHash('sha256').update(data).digest('hex')
}

function hmac(key, value, encoding) {
  return createHmac('sha256', key).update(value).digest(encoding)
}

function awsEncode(value) {
  return encodeURIComponent(value).replace(/[!'()*]/g, (char) => '%' + char.charCodeAt(0).toString(16).toUpperCase())
}

function canonicalQuery(searchParams) {
  return [...searchParams.entries()]
    .map(([key, value]) => [awsEncode(key), awsEncode(value)])
    .sort((left, right) => left[0] === right[0]
      ? (left[1] < right[1] ? -1 : left[1] > right[1] ? 1 : 0)
      : (left[0] < right[0] ? -1 : left[0] > right[0] ? 1 : 0))
    .map(([key, value]) => key + '=' + value)
    .join('&')
}

function amzTimestamp(date) {
  return date.toISOString().replace(/[:-]|\.\d{3}/g, '')
}

function signingKey(dateStamp) {
  const dateKey = hmac('AWS4' + SECRET_KEY, dateStamp)
  const regionKey = hmac(dateKey, REGION)
  const serviceKey = hmac(regionKey, 's3')
  return hmac(serviceKey, 'aws4_request')
}

function authorizationHeaders(method, url, body, extraHeaders = {}) {
  const date = new Date()
  const timestamp = amzTimestamp(date)
  const payloadHash = sha256(body)
  const headers = {
    host: url.host,
    'x-amz-content-sha256': payloadHash,
    'x-amz-date': timestamp,
    ...extraHeaders,
  }
  const signedNames = Object.keys(headers).map((name) => name.toLowerCase()).sort()
  const canonicalHeaders = signedNames
    .map((name) => name + ':' + String(headers[name]).trim().replace(/\s+/g, ' ') + '\n')
    .join('')
  const canonicalRequest = [
    method,
    url.pathname,
    canonicalQuery(url.searchParams),
    canonicalHeaders,
    signedNames.join(';'),
    payloadHash,
  ].join('\n')
  const dateStamp = timestamp.slice(0, 8)
  const scope = dateStamp + '/' + REGION + '/s3/aws4_request'
  const stringToSign = [
    'AWS4-HMAC-SHA256',
    timestamp,
    scope,
    sha256(canonicalRequest),
  ].join('\n')
  const signature = hmac(signingKey(dateStamp), stringToSign, 'hex')
  return {
    ...headers,
    Authorization: 'AWS4-HMAC-SHA256 Credential=' + ACCESS_KEY + '/' + scope
      + ', SignedHeaders=' + signedNames.join(';')
      + ', Signature=' + signature,
  }
}

async function signedS3Request(method, pathName, body = Buffer.alloc(0), extraHeaders = {}) {
  const url = new URL(pathName, MINIO_URL)
  const headers = authorizationHeaders(method, url, body, extraHeaders)
  const response = await fetch(url, { method, headers, body: body.length === 0 ? undefined : body })
  const responseText = await response.text()
  if (!response.ok && !(method === 'PUT' && response.status === 409 && responseText.includes('BucketAlreadyOwnedByYou'))) {
    throw new Error(method + ' ' + url.pathname + url.search + ' failed (' + response.status + '): ' + responseText)
  }
  return response
}

function presignedGetUrl(bucket, key, expiresIn, publicOrigin) {
  const now = new Date()
  const timestamp = amzTimestamp(now)
  const dateStamp = timestamp.slice(0, 8)
  const scope = dateStamp + '/' + REGION + '/s3/aws4_request'
  const url = new URL('/' + bucket + '/' + key, publicOrigin)
  url.searchParams.set('X-Amz-Algorithm', 'AWS4-HMAC-SHA256')
  url.searchParams.set('X-Amz-Credential', ACCESS_KEY + '/' + scope)
  url.searchParams.set('X-Amz-Date', timestamp)
  url.searchParams.set('X-Amz-Expires', String(expiresIn))
  url.searchParams.set('X-Amz-SignedHeaders', 'host')
  const canonicalRequest = [
    'GET',
    url.pathname,
    canonicalQuery(url.searchParams),
    'host:' + url.host + '\n',
    'host',
    'UNSIGNED-PAYLOAD',
  ].join('\n')
  const stringToSign = [
    'AWS4-HMAC-SHA256',
    timestamp,
    scope,
    sha256(canonicalRequest),
  ].join('\n')
  const signature = hmac(signingKey(dateStamp), stringToSign, 'hex')
  url.searchParams.set('X-Amz-Signature', signature)
  return { url: url.toString(), issuedAt: now.getTime(), expiresAt: now.getTime() + expiresIn * 1000 }
}

function logFailure(message) {
  failures.push(message)
  console.error('NG: ' + message)
}

async function listen(server, port) {
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(port, '127.0.0.1', resolve)
  })
  return 'http://127.0.0.1:' + server.address().port
}

async function waitForMinio() {
  const healthUrl = new URL('/minio/health/live', MINIO_URL)
  const deadline = Date.now() + 30000
  while (Date.now() < deadline) {
    try {
      const response = await fetch(healthUrl)
      if (response.ok) return
    } catch {}
    await delay(250)
  }
  throw new Error('MinIO did not become ready at ' + healthUrl)
}

async function createBucket(bucket) {
  await signedS3Request('PUT', '/' + bucket)
}

function makeVideo(tempDir) {
  const file = path.join(tempDir, 's3-302-fixture.mp4')
  execFileSync('ffmpeg', [
    '-hide_banner', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=24',
    '-f', 'lavfi', '-i', 'sine=frequency=660:sample_rate=44100',
    '-t', String(VIDEO_SECONDS),
    '-c:v', 'libx264', '-profile:v', 'baseline', '-level:v', '3.0', '-pix_fmt', 'yuv420p',
    '-preset', 'veryfast', '-b:v', '1000k', '-maxrate', '1000k', '-bufsize', '2000k',
    '-g', '48', '-keyint_min', '48', '-sc_threshold', '0',
    '-c:a', 'aac', '-b:a', '96k',
    '-movflags', '+faststart',
    file,
  ], { stdio: 'ignore' })
  return file
}

function parseExpiresAt(searchParams) {
  const dateValue = searchParams.get('X-Amz-Date')
  const seconds = Number(searchParams.get('X-Amz-Expires'))
  if (!dateValue || !Number.isFinite(seconds)) return null
  const date = dateValue.replace(/^(\d{4})(\d\d)(\d\d)T(\d\d)(\d\d)(\d\d)Z$/, '$1-$2-$3T$4:$5:$6Z')
  const issuedAt = Date.parse(date)
  return Number.isFinite(issuedAt) ? issuedAt + seconds * 1000 : null
}

function htmlForScenario(scenario) {
  const videoPath = '/media/video?scenario=' + scenario
  const subtitlePath = '/media/subtitles?scenario=' + scenario
  const config = JSON.stringify({ scenario, videoPath, subtitlePath, resumeKey: RESUME_KEY })
    .replace(/</g, '\\u003c')
  return [
    '<!doctype html>',
    '<html lang="en"><head><meta charset="utf-8"><title>S3 302 probe</title></head>',
    '<body><button id="play" type="button">Play</button>',
    '<video id="player" crossorigin="anonymous" muted playsinline preload="metadata"></video>',
    '<script>',
    '(function () {',
    '  const config = ' + config + ';',
    '  const video = document.getElementById("player");',
    '  const state = { scenario: config.scenario, crossOrigin: video.getAttribute("crossorigin"), metadata: false, playing: false, currentTime: 0, trackReadyState: 0, cueCount: 0, trackMode: "disabled", seekedCount: 0, mediaError: null, restoreTarget: null, rangeProbe: null };',
    '  window.__state = state;',
    '  const track = document.createElement("track");',
    '  track.kind = "subtitles"; track.label = "Fixture subtitles"; track.srclang = "en"; track.default = true; track.src = config.subtitlePath;',
    '  video.appendChild(track);',
    '  video.addEventListener("loadedmetadata", function () {',
    '    state.metadata = true; state.duration = video.duration;',
    '    if (config.scenario === "restore") {',
    '      const saved = Number(localStorage.getItem(config.resumeKey));',
    '      state.restoreTarget = saved;',
    '      if (Number.isFinite(saved)) video.currentTime = saved;',
    '    }',
    '  });',
    '  video.addEventListener("timeupdate", function () { state.currentTime = video.currentTime; });',
    '  video.addEventListener("playing", function () { state.playing = true; });',
    '  video.addEventListener("pause", function () { state.paused = true; });',
    '  video.addEventListener("seeked", function () { state.seekedCount += 1; state.lastSeekedTime = video.currentTime; });',
    '  video.addEventListener("error", function () { state.mediaError = { code: video.error && video.error.code, message: video.error && video.error.message }; });',
    '  function readTrack() {',
    '    state.trackReadyState = track.readyState;',
    '    state.cueCount = track.track && track.track.cues ? track.track.cues.length : 0;',
    '    state.trackMode = track.track ? track.track.mode : "missing";',
    '  }',
    '  track.addEventListener("load", readTrack); track.addEventListener("error", readTrack);',
    '  setInterval(readTrack, 100);',
    '  document.getElementById("play").addEventListener("click", async function () {',
    '    try { await video.play(); state.playResult = "playing"; }',
    '    catch (error) { state.playResult = String(error); }',
    '  });',
    '  window.runRangeProbe = function () {',
    '    state.rangeProbe = { pending: true };',
    '    fetch("/media/range", { headers: { Range: "bytes=0-1023" }, cache: "no-store" })',
    '      .then(function (response) {',
    '        state.rangeProbe = { status: response.status, contentRange: response.headers.get("Content-Range"), acceptRanges: response.headers.get("Accept-Ranges"), error: null };',
    '        return response.body ? response.body.cancel() : undefined;',
    '      })',
    '      .catch(function (error) { state.rangeProbe = { status: null, contentRange: null, acceptRanges: null, error: String(error) }; });',
    '  };',
    '  window.savePosition = function () { localStorage.setItem(config.resumeKey, String(video.currentTime)); return video.currentTime; };',
    '  window.seekTo = function (seconds) { state.seekedCount = 0; state.seekTarget = seconds; video.currentTime = seconds; return true; };',
    '  video.src = config.videoPath;',
    '})();',
    '</script></body></html>',
  ].join('\n')
}

function startFixtureServers(videoKey, subtitleKey) {
  let publicOrigin
  let s3Origin

  const originServer = createServer(async (req, res) => {
    const url = new URL(req.url, publicOrigin)
    if (req.method === 'GET' && url.pathname === '/probe') {
      const scenario = url.searchParams.get('scenario') ?? 'normal'
      if (!['normal', 'negative-cors', 'restore', 'expiry'].includes(scenario)) {
        res.writeHead(400).end('unknown scenario')
        return
      }
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' })
      res.end(htmlForScenario(scenario))
      return
    }
    if ((req.method === 'GET' || req.method === 'HEAD') && (url.pathname === '/media/video' || url.pathname === '/media/range' || url.pathname === '/media/subtitles')) {
      const subtitle = url.pathname === '/media/subtitles'
      const negativeCors = url.searchParams.get('scenario') === 'negative-cors'
      const expiry = url.searchParams.get('scenario') === 'expiry'
      const bucket = subtitle && negativeCors ? BAD_BUCKET : GOOD_BUCKET
      const key = subtitle ? subtitleKey : videoKey
      const ttl = expiry && !subtitle ? EXPIRED_TTL_SECONDS : VIDEO_TTL_SECONDS
      const signed = presignedGetUrl(bucket, key, ttl, s3Origin)
      const requestRecord = {
        route: 'redirect',
        at: Date.now(),
        method: req.method,
        path: url.pathname,
        scenario: url.searchParams.get('scenario') ?? '',
        range: req.headers.range ?? null,
        expiresAt: signed.expiresAt,
        ttlSeconds: ttl,
        status: 302,
      }
      requests.push(requestRecord)
      res.writeHead(302, { Location: signed.url, 'Cache-Control': 'no-store, no-cache, must-revalidate' })
      res.end()
      return
    }
    res.writeHead(404, { 'Cache-Control': 'no-store' }).end('not found')
  })

  const minioHost = MINIO_URL.hostname
  const minioPort = Number(MINIO_URL.port || 80)
  const proxyServer = createServer((req, res) => {
    const url = new URL(req.url, s3Origin)
    const bucket = url.pathname.split('/')[1]
    if (![GOOD_BUCKET, BAD_BUCKET].includes(bucket)) {
      res.writeHead(404).end('unknown bucket')
      return
    }
    const record = {
      route: 's3',
      at: Date.now(),
      method: req.method,
      bucket,
      path: url.pathname,
      range: req.headers.range ?? null,
      origin: req.headers.origin ?? null,
      expiresAt: parseExpiresAt(url.searchParams),
      status: null,
    }
    requests.push(record)
    const headers = { ...req.headers, host: req.headers.host }
    const isSubtitle = url.pathname.endsWith('/' + subtitleKey)
    // The pinned MinIO community build uses global CORS; omit Origin to model a bucket rule that rejects this origin.
    if (bucket === BAD_BUCKET || (process.env.E2E_S3_DISABLE_CORS === '1' && isSubtitle)) delete headers.origin
    delete headers.connection
    delete headers['proxy-connection']
    const upstream = httpRequest({
      hostname: minioHost,
      port: minioPort,
      method: req.method,
      path: req.url,
      headers,
    }, (upstreamResponse) => {
      record.status = upstreamResponse.statusCode ?? 502
      res.writeHead(upstreamResponse.statusCode ?? 502, upstreamResponse.headers)
      upstreamResponse.pipe(res)
    })
    upstream.on('error', (error) => {
      record.status = 502
      res.writeHead(502, { 'Content-Type': 'text/plain' }).end(String(error))
    })
    req.pipe(upstream)
  })

  return {
    async start() {
      publicOrigin = await listen(originServer, FIXTURE_PORT)
      s3Origin = await listen(proxyServer, S3_PROXY_PORT)
      return { publicOrigin, s3Origin }
    },
    close() {
      return Promise.all([originServer, proxyServer].map((server) => new Promise((resolve) => {
        server.close(resolve)
        server.closeAllConnections()
      })))
    },
    publicOrigin: () => publicOrigin,
    s3Origin: () => s3Origin,
  }
}

/** waitFor は browser.evaluate(fn, arg) が真を返すまで 100 ms 間隔で繰り返す。 */
async function waitFor(browser, fn, arg, timeout = 15000) {
  const until = Date.now() + timeout
  while (Date.now() < until) {
    if (await browser.evaluate(fn, arg)) return
    await delay(100)
  }
  throw new Error('待ち条件が ' + timeout + ' ms 以内に成立しない: ' + fn.toString())
}

function expect(condition, message) {
  if (!condition) logFailure(message)
}

function conciseRequests(records) {
  return records.map((record) => ({
    route: record.route,
    method: record.method,
    path: record.path,
    bucket: record.bucket,
    range: record.range,
    status: record.status,
    expiresAt: record.expiresAt ? new Date(record.expiresAt).toISOString() : null,
  }))
}

async function main() {
  await waitForMinio()
  const tempDir = await mkdtemp(path.join(os.tmpdir(), 'rokuban-s3-302-'))
  let browser
  let fixture
  try {
    const videoPath = makeVideo(tempDir)
    const video = await readFile(videoPath)
    const videoKey = 'video-' + randomUUID() + '.mp4'
    const subtitleKey = 'subtitles-' + randomUUID() + '.vtt'
    const vtt = Buffer.from('WEBVTT\n\n00:00:00.000 --> 00:00:05.000\nS3 subtitle loaded\n\n00:00:05.000 --> 00:00:10.000\n302 subtitle still visible\n')
    await createBucket(GOOD_BUCKET)
    createdBuckets.push(GOOD_BUCKET)
    await createBucket(BAD_BUCKET)
    createdBuckets.push(BAD_BUCKET)
    await signedS3Request('PUT', '/' + GOOD_BUCKET + '/' + videoKey, video, { 'content-type': 'video/mp4' })
    createdObjects.push({ bucket: GOOD_BUCKET, key: videoKey })
    await signedS3Request('PUT', '/' + GOOD_BUCKET + '/' + subtitleKey, vtt, { 'content-type': 'text/vtt; charset=utf-8' })
    createdObjects.push({ bucket: GOOD_BUCKET, key: subtitleKey })
    await signedS3Request('PUT', '/' + BAD_BUCKET + '/' + subtitleKey, vtt, { 'content-type': 'text/vtt; charset=utf-8' })
    createdObjects.push({ bucket: BAD_BUCKET, key: subtitleKey })

    fixture = startFixtureServers(videoKey, subtitleKey)
    const origins = await fixture.start()

    browser = BROWSER === 'chrome' ? await createPlaywrightAdapter('chrome') : await createSafariAdapter()
    console.log('Browser: ' + BROWSER + ' ' + browser.version)
    console.log('Fixture: ' + origins.publicOrigin + ' -> ' + origins.s3Origin)
    console.log('MinIO: ' + MINIO_URL.origin + ', MP4 size: ' + video.length + ' bytes, duration: ' + VIDEO_SECONDS + 's')

    const normalRequestStart = requests.length
    await browser.navigate(origins.publicOrigin + '/probe?scenario=normal')
    await waitFor(browser, () => window.__state && window.__state.metadata)
    try {
      await waitFor(browser, () => window.__state && window.__state.trackReadyState === 2 && window.__state.cueCount > 0, undefined, 20000)
    } catch (error) {
      console.error('Subtitle state: ' + JSON.stringify(await browser.evaluate(() => window.__state)))
      throw error
    }
    const normalReady = await browser.evaluate(() => window.__state)
    expect(normalReady.crossOrigin === 'anonymous', 'fixture <video> is missing crossorigin="anonymous"')
    expect(normalReady.trackMode === 'showing', 'cross-origin WebVTT track did not enter showing mode')
    expect(normalReady.cueCount > 0, 'cross-origin WebVTT track loaded no cues')

    await browser.evaluate(() => window.runRangeProbe())
    await waitFor(browser, () => window.__state && window.__state.rangeProbe && !window.__state.rangeProbe.pending)
    const rangeProbe = await browser.evaluate(() => window.__state.rangeProbe)
    expect(rangeProbe.status === 206, 'Range fetch after 302 returned ' + rangeProbe.status + ' ' + (rangeProbe.error ?? ''))
    expect(Boolean(rangeProbe.contentRange), 'Content-Range was not readable through CORS')
    expect(rangeProbe.acceptRanges === 'bytes', 'Accept-Ranges was not readable through CORS')

    await browser.click('#play')
    await waitFor(browser, () => window.__state && window.__state.playing && window.__state.currentTime >= 1)
    await delay(3000)
    const beforeSeek = requests.length
    await browser.evaluate((seconds) => window.seekTo(seconds), TARGET_SEEK_SECONDS)
    await waitFor(browser, () => window.__state && (window.__state.seekedCount > 0 || window.__state.mediaError))
    const afterSeek = await browser.evaluate(() => ({ currentTime: document.querySelector('#player').currentTime, seekedCount: window.__state.seekedCount, error: window.__state.mediaError }))
    expect(afterSeek.seekedCount > 0 && Math.abs(afterSeek.currentTime - TARGET_SEEK_SECONDS) < 0.5,
      'normal signed URL seek did not complete: ' + JSON.stringify(afterSeek))
    expect(!afterSeek.error, 'normal signed URL playback errored while seeking: ' + JSON.stringify(afterSeek.error))
    const seekRequests = conciseRequests(requests.slice(beforeSeek))
    const after302Range = seekRequests.filter((request) => request.range)
    const seekDestination = {
      originRangeRequests: after302Range.filter((request) => request.route === 'redirect').length,
      signedUrlRangeRequests: after302Range.filter((request) => request.route === 's3').length,
    }
    expect(seekDestination.signedUrlRangeRequests > 0, 'seek produced no Range request at the signed S3 destination')

    await browser.evaluate(() => window.savePosition())
    const savedPosition = await browser.evaluate((key) => Number(localStorage.getItem(key)), RESUME_KEY)
    const restoreStart = requests.length
    await browser.navigate(origins.publicOrigin + '/probe?scenario=restore')
    await waitFor(browser, () => window.__state && window.__state.metadata && window.__state.restoreTarget !== null)
    await waitFor(browser, (saved) => window.__state && window.__state.seekedCount > 0 && Math.abs(window.__state.lastSeekedTime - saved) < 0.5, savedPosition)
    const restoredPosition = await browser.evaluate(() => ({ saved: window.__state.restoreTarget, currentTime: document.querySelector('#player').currentTime, seekedCount: window.__state.seekedCount, error: window.__state.mediaError }))
    expect(Math.abs(restoredPosition.currentTime - savedPosition) < 0.5, 'saved playback position was not restored after a 302: ' + JSON.stringify(restoredPosition))
    expect(restoredPosition.seekedCount > 0 && !restoredPosition.error, 'restored position did not complete a seek: ' + JSON.stringify(restoredPosition))
    await browser.click('#play')
    await waitFor(browser, (saved) => window.__state && window.__state.playing && window.__state.currentTime > saved + 0.5, savedPosition)
    const resumedPlayback = await browser.evaluate(() => ({ currentTime: document.querySelector('#player').currentTime, error: window.__state.mediaError }))
    expect(resumedPlayback.currentTime > savedPosition + 0.5 && !resumedPlayback.error,
      'playback did not advance from the restored position: ' + JSON.stringify(resumedPlayback))
    const restoreRequests = requests.slice(restoreStart).filter((request) => request.route === 's3' && request.range)
    expect(restoreRequests.some((request) => request.status === 206), 'restoring the saved position made no successful Range request to the signed S3 destination')

    const negativeStart = requests.length
    await browser.navigate(origins.publicOrigin + '/probe?scenario=negative-cors')
    await waitFor(browser, () => window.__state && window.__state.metadata)
    await waitFor(browser, () => window.__state && window.__state.trackReadyState === 3)
    const negative = await browser.evaluate(() => ({ readyState: window.__state.trackReadyState, cueCount: window.__state.cueCount, mediaError: window.__state.mediaError }))
    expect(negative.readyState === 3 && negative.cueCount === 0, 'subtitle track unexpectedly loaded without bucket CORS: ' + JSON.stringify(negative))
    const negativeRequests = requests.slice(negativeStart).filter((request) => request.route === 's3' && request.bucket === BAD_BUCKET)
    expect(negativeRequests.some((request) => request.status === 200), 'no-CORS VTT object did not reach MinIO successfully; the browser failure may not be a CORS failure')

    const expiryStart = requests.length
    await browser.navigate(origins.publicOrigin + '/probe?scenario=expiry')
    await waitFor(browser, () => window.__state && window.__state.metadata)
    await browser.click('#play')
    await waitFor(browser, () => window.__state && window.__state.playing && window.__state.currentTime >= 1)
    const expiringRedirect = requests.slice(expiryStart).find((request) => request.route === 'redirect' && request.path === '/media/video')
    if (!expiringRedirect) throw new Error('expiry scenario issued no signed redirect')
    await delay(Math.max(0, expiringRedirect.expiresAt - Date.now() + 300))
    const expiredAt = expiringRedirect.expiresAt
    const expirySeekStart = requests.length
    await browser.evaluate(() => window.seekTo(64))
    const expiryDeadline = Date.now() + 15000
    let afterExpiry = []
    let expirySeek = null
    while (Date.now() < expiryDeadline) {
      afterExpiry = requests.slice(expirySeekStart).filter((request) => request.range && request.at >= expiredAt)
      expirySeek = await browser.evaluate(() => ({ currentTime: document.querySelector('#player').currentTime, error: window.__state.mediaError, seekedCount: window.__state.seekedCount }))
      if (expirySeek.error || expirySeek.seekedCount > 0) break
      await delay(100)
    }
    if (!expirySeek) throw new Error('could not read media state after the expired seek')
    expect(afterExpiry.length > 0, 'could not observe a ranged media request after the signed URL expired')
    const refreshedAtOrigin = afterExpiry.some((request) => request.route === 'redirect')
    const rejectedByMinIO = afterExpiry.some((request) => request.route === 's3' && request.status === 403)
    const settledRange = afterExpiry.some((request) => request.route === 's3' && request.status !== null)
    expect(refreshedAtOrigin || rejectedByMinIO, 'the expired-range result was neither a fresh origin redirect nor MinIO 403')
    expect(settledRange, 'the expired seek did not receive a final response from the signed S3 destination')
    expect(Boolean(expirySeek.error) || expirySeek.seekedCount > 0, 'the expired seek produced neither a media error nor a completed seek')
    if (rejectedByMinIO && !refreshedAtOrigin) expect(Boolean(expirySeek.error), 'MinIO rejected the expired signed URL but the video reported no media error')
    if (refreshedAtOrigin) expect(!expirySeek.error && expirySeek.seekedCount > 0, 'the browser refreshed at the origin but the new signed URL did not complete the seek: ' + JSON.stringify(expirySeek))
    const expiryResult = {
      signedUrlExpiresAt: new Date(expiringRedirect.expiresAt).toISOString(),
      rangedRequestsAfterExpiry: conciseRequests(afterExpiry),
      mediaState: expirySeek,
      refreshedAtOrigin,
      rejectedByMinIO,
    }

    console.log('Normal subtitle: ' + JSON.stringify({
      mode: normalReady.trackMode,
      cues: normalReady.cueCount,
      rangeStatus: rangeProbe.status,
      contentRangeExposed: Boolean(rangeProbe.contentRange),
      acceptRangesExposed: rangeProbe.acceptRanges,
    }))
    console.log('Seek destination after 302: ' + JSON.stringify(seekDestination))
    console.log('Seeked to: ' + JSON.stringify(afterSeek))
    console.log('Saved position restore: ' + JSON.stringify(restoredPosition))
    console.log('Playback after restore: ' + JSON.stringify(resumedPlayback))
    console.log('No-CORS negative: ' + JSON.stringify(negative))
    console.log('Short-expiry result: ' + JSON.stringify(expiryResult))
    console.log('Observed browser requests: ' + JSON.stringify(conciseRequests(requests.slice(normalRequestStart))))

    if (failures.length > 0) {
      process.exitCode = 1
      console.error(failures.length + ' assertion(s) failed')
    } else {
      console.log('PASS: S3 302 browser probe')
    }
  } finally {
    if (browser) await browser.close().catch((error) => console.error('Could not close browser: ' + error.message))
    if (fixture) await fixture.close()
    for (const object of createdObjects.reverse()) {
      await signedS3Request('DELETE', '/' + object.bucket + '/' + object.key).catch(() => {})
    }
    for (const bucket of createdBuckets.reverse()) {
      await signedS3Request('DELETE', '/' + bucket).catch(() => {})
    }
    await rm(tempDir, { recursive: true, force: true })
  }
}

main().catch((error) => {
  console.error(error.stack ?? String(error))
  if (requests.length > 0) console.error('Request trace: ' + JSON.stringify(conciseRequests(requests)))
  process.exitCode = 1
})
