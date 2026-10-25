// 録画 HLS の固定長 segment と ffmpeg 再起動境界を実ブラウザで測る。使い方は web/e2e/checks/playback.md。
import { createServer } from 'node:http'
import { spawn } from 'node:child_process'
import { readFileSync, statSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

import { launchBrowser, log } from './lib.mjs'
import {
  ensurePlaybackFixtures,
  FIXTURE_ROOT,
  FRAME_BITS,
  FRAME_RATE,
  RESTART_FROM_SECONDS,
  SEGMENT_SECONDS,
  SPLIT_SECONDS,
} from './recording-segmented-fixture.mjs'

const require = createRequire(import.meta.url)
const HLS_BUNDLE = path.join(path.dirname(require.resolve('hls.js')), 'hls.min.js')
const envNumber = (name, fallback) => (process.env[name] === undefined ? fallback : Number(process.env[name]))
// 判定の位置。E2E_* で上書きできる（アプリの URL に向けるとき、その録画の segment 長と再起動境界に合わせる）。
const SEGMENT = envNumber('E2E_SEGMENT_SECONDS', SEGMENT_SECONDS)
const BOUNDARY = envNumber('E2E_BOUNDARY_SECONDS', SPLIT_SECONDS)
// 内蔵フィクスチャ（60 秒）で、境界の手前から読み始めた hls.js の先読み（既定 30 秒）の外になる位置。
const FAR_SEEK = envNumber('E2E_FAR_SEEK_SECONDS', 52)
const MASTER_URL = process.env.E2E_MASTER_URL
const HARNESS_PATH = '/__e2e/'

const HARNESS_HTML = String.raw`<!doctype html>
<html lang="ja">
<head><meta charset="utf-8"><title>recording segmented HLS fixture</title></head>
<body>
  <button id="start" type="button">再生開始</button>
  <video id="video" playsinline preload="auto" crossorigin="anonymous" controls></video>
  <script src="/__e2e/hls.min.js"></script>
  <script>
    (() => {
      const video = document.querySelector('#video')
      const FRAME_BITS = __FRAME_BITS__
      const state = {
        path: null,
        frames: [],
        quanta: [],
        audio: [],
        stalls: [],
        hlsErrors: [],
        capture: false,
        hls: null,
        audioContext: null,
        audioAnalysers: null,
      }
      let canvas
      let canvasContext
      let lastProgressTime = 0
      let lastProgressAt = 0
      let stallRecorded = false

      function decodeFrameMarker() {
        if (!canvas) {
          canvas = document.createElement('canvas')
          canvas.width = 640
          canvas.height = 360
          canvasContext = canvas.getContext('2d', { willReadFrequently: true })
        }
        canvasContext.drawImage(video, 0, 0, canvas.width, canvas.height)
        let frame = 0
        for (let bit = 0; bit < FRAME_BITS; bit += 1) {
          const x = 28 + bit * 40 + 14
          const pixel = canvasContext.getImageData(x, 40, 1, 1).data
          if (pixel[0] > 128) frame |= 1 << bit
        }
        return frame
      }

      function captureVideoFrame(now, metadata) {
        if (state.capture && video.readyState >= 2) {
          try {
            const sample = {
              currentTime: video.currentTime,
              mediaTime: metadata.mediaTime,
              at: metadata.expectedDisplayTime ?? now,
              frame: decodeFrameMarker(),
            }
            state.frames.push(sample)
            if (state.frames.length > 4000) state.frames.shift()
          } catch (error) {
            state.frameError = String(error)
          }
        }
        frameCallbackId = video.requestVideoFrameCallback(captureVideoFrame)
      }

      // WebKit で、読み込み直しや再生開始の前後に callback が来なくなる実行があった。
      // 取りこぼしを避けるため、再生が始まるたびに張り直す。
      let frameCallbackId = video.requestVideoFrameCallback(captureVideoFrame)
      function rearmFrameCallback() {
        video.cancelVideoFrameCallback(frameCallbackId)
        frameCallbackId = video.requestVideoFrameCallback(captureVideoFrame)
      }
      video.addEventListener('playing', rearmFrameCallback)

      video.addEventListener('seeking', () => {
        lastProgressTime = video.currentTime
        lastProgressAt = performance.now()
        stallRecorded = false
      })
      setInterval(() => {
        if (!state.capture || video.paused || video.seeking) return
        const now = performance.now()
        if (video.currentTime > lastProgressTime + 0.01) {
          lastProgressTime = video.currentTime
          lastProgressAt = now
          stallRecorded = false
        } else if (!stallRecorded && now - lastProgressAt >= 500) {
          state.stalls.push({ currentTime: video.currentTime, durationMs: now - lastProgressAt })
          stallRecorded = true
        }
      }, 100)

      function peakFrequency(analyser) {
        const bins = new Float32Array(analyser.frequencyBinCount)
        analyser.getFloatFrequencyData(bins)
        let best = 0
        for (let index = 1; index < bins.length; index += 1) {
          if (bins[index] > bins[best]) best = index
        }
        return Math.round(best * state.audioContext.sampleRate / analyser.fftSize)
      }

      function rms(analyser) {
        const samples = new Float32Array(analyser.fftSize)
        analyser.getFloatTimeDomainData(samples)
        let power = 0
        for (const sample of samples) power += sample * sample
        return Math.sqrt(power / samples.length)
      }

      function readAudio() {
        if (!state.audioAnalysers || state.audioContext.state !== 'running') return
        state.audio.push({
          currentTime: video.currentTime,
          leftHz: peakFrequency(state.audioAnalysers.left),
          rightHz: peakFrequency(state.audioAnalysers.right),
          leftRMS: rms(state.audioAnalysers.left),
          rightRMS: rms(state.audioAnalysers.right),
        })
        if (state.audio.length > 1200) state.audio.shift()
      }

      // AudioWorklet が 128 サンプルごと（約 2.7 ms）の左右 RMS を、全サンプルについて求める。
      const WORKLET = 'class Rms extends AudioWorkletProcessor {' +
        'constructor() { super(); this.batch = [] }' +
        'process(inputs) {' +
        '  const input = inputs[0]; if (!input || input.length === 0) return true;' +
        '  const rms = (c) => { if (!c) return 0; let p = 0; for (let i = 0; i < c.length; i += 1) p += c[i] * c[i]; return Math.sqrt(p / c.length) };' +
        '  this.batch.push([rms(input[0]), rms(input[1] || input[0])]);' +
        '  if (this.batch.length >= 8) { this.port.postMessage(this.batch); this.batch = [] }' +
        '  return true } }' +
        'registerProcessor("rms", Rms)'

      function audioTracks() {
        if (state.hls) {
          return state.hls.audioTracks.map((track, index) => ({
            index,
            label: track.name || track.lang || String(index),
            enabled: index === state.hls.audioTrack,
          }))
        }
        const tracks = video.audioTracks
        if (!tracks) return []
        return Array.from(tracks).map((track, index) => ({
          index,
          label: track.label || track.language || String(index),
          enabled: track.enabled,
        }))
      }

      window.__e2e = {
        // 再生経路の選び方は web/src/lib/live.ts の supportsNativeHls と
        // web/src/components/live-player.tsx の 3 段の梯子の写し。
        async load(masterURL) {
          state.frames = []
          state.quanta = []
          state.audio = []
          state.stalls = []
          state.hlsErrors = []
          state.frameError = null
          state.capture = false
          if (state.hls) {
            state.hls.destroy()
            state.hls = null
          }
          video.pause()
          video.removeAttribute('src')
          video.load()

          const playlistMIME = video.canPlayType('application/vnd.apple.mpegurl')
          const segmentMIME = video.canPlayType('video/mp2t')
          const nativeHLS = playlistMIME !== '' && segmentMIME !== ''
          const hlsSupported = !!window.Hls && window.Hls.isSupported()
          if (!nativeHLS && hlsSupported) {
            state.path = 'hls.js'
            // アプリの録画再生は startAt が無いとき { startPosition: 0 } だけを渡す（live-player.tsx）。
            const hls = new window.Hls({ startPosition: 0 })
            state.hls = hls
            hls.on(window.Hls.Events.ERROR, (_event, data) => {
              state.hlsErrors.push({ details: data.details, fatal: !!data.fatal, at: video.currentTime })
            })
            await new Promise((resolve, reject) => {
              hls.on(window.Hls.Events.MANIFEST_PARSED, resolve)
              hls.on(window.Hls.Events.ERROR, (_event, data) => {
                if (data.fatal) reject(new Error(data.details || data.type || 'fatal hls.js error'))
              })
              hls.loadSource(masterURL)
              hls.attachMedia(video)
            })
          } else if (nativeHLS || playlistMIME !== '') {
            state.path = 'native-hls'
            await new Promise((resolve, reject) => {
              const timer = setTimeout(() => reject(new Error('native HLS の loadedmetadata が 15 秒以内に来ない')), 15_000)
              video.addEventListener('loadedmetadata', () => { clearTimeout(timer); resolve() }, { once: true })
              video.addEventListener('error', () => { clearTimeout(timer); reject(new Error('native HLS の読み込みに失敗')) }, { once: true })
              video.src = masterURL
              video.load()
            })
          } else {
            throw new Error('このブラウザに HLS / MediaSource の再生経路がない')
          }
          return {
            path: state.path,
            nativeMIME: playlistMIME || '(empty)',
            segmentMIME: segmentMIME || '(empty)',
            duration: video.duration,
          }
        },

        // 一時停止して seek し、seeked の位置を返す。フレームは再生を始めてから読む。
        async seek(seconds) {
          video.pause()
          await new Promise((resolve, reject) => {
            const timer = setTimeout(() => reject(new Error('seeked が 10 秒以内に来ない')), 10_000)
            video.addEventListener('seeked', () => { clearTimeout(timer); resolve() }, { once: true })
            video.currentTime = seconds
          })
          return { landing: video.currentTime }
        },

        async start() {
          if (!state.audioContext) {
            const AudioContext = window.AudioContext || window.webkitAudioContext
            if (!AudioContext) throw new Error('WebAudio がないため音声を測れない')
            const audioContext = new AudioContext()
            const source = audioContext.createMediaElementSource(video)
            const splitter = audioContext.createChannelSplitter(2)
            const left = audioContext.createAnalyser()
            const right = audioContext.createAnalyser()
            left.fftSize = right.fftSize = 8192
            left.smoothingTimeConstant = right.smoothingTimeConstant = 0
            source.connect(splitter)
            splitter.connect(left, 0)
            splitter.connect(right, 1)
            source.connect(audioContext.destination)
            state.audioContext = audioContext
            state.audioAnalysers = { left, right }
            setInterval(readAudio, 100)
            try {
              const url = URL.createObjectURL(new Blob([WORKLET], { type: 'text/javascript' }))
              await audioContext.audioWorklet.addModule(url)
              const node = new AudioWorkletNode(audioContext, 'rms', { numberOfOutputs: 0 })
              node.port.onmessage = (event) => {
                if (!state.capture) return
                const currentTime = video.currentTime
                for (const [l, r] of event.data) state.quanta.push({ currentTime, l, r })
                if (state.quanta.length > 200000) state.quanta.splice(0, 50000)
              }
              source.connect(node)
            } catch (error) {
              state.workletError = String(error)
            }
          }
          await state.audioContext.resume()
          await video.play()
          lastProgressTime = video.currentTime
          lastProgressAt = performance.now()
          stallRecorded = false
          state.capture = true
          return { currentTime: video.currentTime, paused: video.paused }
        },

        async selectTrack(index) {
          const tracks = audioTracks()
          if (tracks.length !== 3) throw new Error('音声 rendition が 3 本でない: ' + JSON.stringify(tracks))
          if (state.hls) {
            state.hls.audioTrack = index
            return audioTracks()
          }
          if (!video.audioTracks || video.audioTracks.length !== 3) {
            throw new Error('native HLS の audioTracks が 3 本でない')
          }
          for (let trackIndex = 0; trackIndex < video.audioTracks.length; trackIndex += 1) {
            video.audioTracks[trackIndex].enabled = trackIndex === index
          }
          return audioTracks()
        },

        state() {
          const buffered = []
          for (let index = 0; index < video.buffered.length; index += 1) {
            buffered.push([video.buffered.start(index), video.buffered.end(index)])
          }
          return {
            path: state.path,
            currentTime: video.currentTime,
            duration: video.duration,
            paused: video.paused,
            seeking: video.seeking,
            readyState: video.readyState,
            buffered,
            tracks: audioTracks(),
            stalls: state.stalls.slice(),
            audio: state.audio.slice(-180),
            hlsErrors: state.hlsErrors.slice(),
            frameError: state.frameError,
            startError: state.startError,
            workletError: state.workletError,
          }
        },

        framesBetween(from, to) {
          return state.frames.filter((sample) => sample.currentTime >= from && sample.currentTime <= to)
        },

        // 128 サンプルごとの RMS のうち、左右どちらかが 0.02 未満の連続数の最大値。
        quietRun(from, to) {
          const window = state.quanta.filter((sample) => sample.currentTime >= from && sample.currentTime <= to)
          let longest = 0
          let run = 0
          let loud = 0
          for (const sample of window) {
            if (sample.l < 0.02 || sample.r < 0.02) {
              run += 1
              longest = Math.max(longest, run)
            } else {
              run = 0
              loud += 1
            }
          }
          return { quanta: window.length, loud, longest }
        },
      }

      document.querySelector('#start').addEventListener('click', () => {
        window.__e2e.start().catch((error) => { state.startError = String(error) })
      })
    })()
  </script>
</body>
</html>`.replace('__FRAME_BITS__', String(FRAME_BITS))

const MIME_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.m3u8': 'application/vnd.apple.mpegurl',
  '.ts': 'video/mp2t',
  '.m4s': 'video/mp4',
  '.mp4': 'video/mp4',
  '.js': 'text/javascript; charset=utf-8',
}

function readFixtureFile(requestPath, range) {
  const filePath = path.resolve(FIXTURE_ROOT, `.${decodeURIComponent(requestPath)}`)
  if (!filePath.startsWith(`${FIXTURE_ROOT}${path.sep}`)) return { status: 403, headers: {}, body: Buffer.from('forbidden') }
  try {
    const stat = statSync(filePath)
    const common = {
      'Accept-Ranges': 'bytes',
      'Content-Type': MIME_TYPES[path.extname(filePath)] ?? 'application/octet-stream',
    }
    const body = readFileSync(filePath)
    const match = /^bytes=(\d+)-(\d*)$/.exec(range ?? '')
    if (!match) return { status: 200, headers: common, body }
    const start = Number(match[1])
    const end = Math.min(match[2] ? Number(match[2]) : stat.size - 1, stat.size - 1)
    if (start >= stat.size || end < start) return { status: 416, headers: { 'Content-Range': `bytes */${stat.size}` }, body: Buffer.alloc(0) }
    return {
      status: 206,
      headers: { ...common, 'Content-Range': `bytes ${start}-${end}/${stat.size}` },
      body: body.subarray(start, end + 1),
    }
  } catch {
    return { status: 404, headers: {}, body: Buffer.from('not found') }
  }
}

// E2E_MASTER_URL のオリジンへ同じ path で中継する。ハーネスのページとメディアを同一オリジンにして、
// hls.js の XHR の CORS と createMediaElementSource の無音を避ける。
async function readUpstream(upstream, requestPath, search, range) {
  const response = await fetch(`${upstream}${requestPath}${search}`, { headers: range ? { Range: range } : {} })
  const headers = {}
  for (const name of ['content-type', 'content-range', 'accept-ranges']) {
    const value = response.headers.get(name)
    if (value) headers[name] = value
  }
  return { status: response.status, headers, body: Buffer.from(await response.arrayBuffer()) }
}

// playlist の EXTINF を累積して、segment の path → [開始, 終了) 秒を覚える。
function rememberSegments(playlistPath, text, segmentRanges) {
  let at = 0
  let duration
  for (const line of text.split(/\r?\n/)) {
    if (line.startsWith('#EXTINF:')) duration = Number(line.slice('#EXTINF:'.length).split(',')[0])
    else if (duration !== undefined && line && !line.startsWith('#')) {
      segmentRanges.set(new URL(line, `http://x${playlistPath}`).pathname, [at, at + duration])
      at += duration
      duration = undefined
    }
  }
}

async function startServer(upstream) {
  const requests = []
  const segmentRanges = new Map()
  const holds = new Map()
  const server = createServer(async (request, response) => {
    const url = new URL(request.url, 'http://localhost')
    const requestPath = url.pathname
    if (requestPath === HARNESS_PATH) {
      response.writeHead(200, { 'Content-Type': MIME_TYPES['.html'] })
      response.end(HARNESS_HTML)
      return
    }
    if (requestPath === `${HARNESS_PATH}hls.min.js`) {
      response.writeHead(200, { 'Content-Type': MIME_TYPES['.js'] })
      response.end(readFileSync(HLS_BUNDLE))
      return
    }
    requests.push(requestPath)
    const hold = holds.get(requestPath)
    if (hold && !hold.received) {
      hold.received = true
      await hold.released
    }
    let result
    try {
      result = upstream
        ? await readUpstream(upstream, requestPath, url.search, request.headers.range)
        : readFixtureFile(requestPath, request.headers.range)
    } catch (error) {
      result = { status: 502, headers: {}, body: Buffer.from(String(error)) }
    }
    if (requestPath.endsWith('.m3u8') && result.status === 200) rememberSegments(requestPath, result.body.toString('utf8'), segmentRanges)
    response.writeHead(result.status, { ...result.headers, 'Content-Length': result.body.length, 'Cache-Control': 'no-store' })
    response.end(result.body)
  })
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  return {
    server,
    url: `http://127.0.0.1:${server.address().port}`,
    // 要求の通し番号。seek の前後で要求を分けるために使う。
    mark: () => requests.length,
    reset() { requests.length = 0; segmentRanges.clear() },
    requestsBetween: (from, to) => requests.slice(from, to),
    // `time` 秒を含む segment の path。
    segmentsAt: (time) => [...segmentRanges].filter(([, [from, to]]) => from <= time && time < to).map(([name]) => name),
    // path の応答を release() まで保留する。
    hold(requestPath) {
      let release
      const hold = { received: false, released: new Promise((resolve) => { release = resolve }), release: () => release() }
      holds.set(requestPath, hold)
      return hold
    },
  }
}

async function createPlaywrightAdapter(engine) {
  const browser = engine === 'chrome'
    ? await launchBrowser('chromium', { channel: 'chrome', args: ['--autoplay-policy=no-user-gesture-required'] })
    : await launchBrowser('webkit')
  const page = await browser.newPage({ viewport: { width: 960, height: 640 } })
  return {
    version: browser.version(),
    navigate: (url) => page.goto(url, { waitUntil: 'domcontentloaded' }),
    click: (selector) => page.locator(selector).click(),
    evaluate: (fn, arg) => page.evaluate(fn, arg),
    close: () => browser.close(),
  }
}

async function createSafariAdapter() {
  const port = Number(process.env.E2E_SAFARI_PORT ?? 44444)
  const base = `http://127.0.0.1:${port}`
  const child = spawn('safaridriver', ['--port', String(port)], { stdio: 'ignore' })
  let spawnError
  child.once('error', (error) => { spawnError = error })
  const request = async (method, url, body) => {
    const response = await fetch(base + url, {
      method,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    const result = await response.json().catch(() => ({}))
    if (!response.ok || result.value?.error) {
      throw new Error(`Safari WebDriver ${method} ${url}: ${JSON.stringify(result)}`)
    }
    return result.value
  }

  let ready = false
  const startedAt = Date.now()
  while (Date.now() - startedAt < 20_000) {
    if (spawnError) throw new Error(`safaridriver を起動できない: ${spawnError.message}`)
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(`safaridriver が ${child.exitCode ?? child.signalCode} で終了した`)
    }
    try {
      await request('GET', '/status')
      ready = true
      break
    } catch {
      await delay(200)
    }
  }
  if (!ready) {
    child.kill('SIGTERM')
    throw new Error('safaridriver が起動しない。checks/playback.md の Safari の設定を確認する')
  }

  try {
    const session = await request('POST', '/session', {
      capabilities: { alwaysMatch: { browserName: 'safari', acceptInsecureCerts: true } },
    })
    const sessionId = session.sessionId
    await request('POST', `/session/${sessionId}/timeouts`, { script: 30_000, pageLoad: 30_000, implicit: 0 })
    return {
      version: `Safari ${session.capabilities?.browserVersion ?? '(version unavailable)'}`,
      navigate: (url) => request('POST', `/session/${sessionId}/url`, { url }),
      click: async (selector) => {
        const element = await request('POST', `/session/${sessionId}/element`, { using: 'css selector', value: selector })
        const elementId = element['element-6066-11e4-a52e-4f735466cecf'] ?? element.ELEMENT
        if (!elementId) throw new Error(`Safari WebDriver が要素を返さない: ${JSON.stringify(element)}`)
        await request('POST', `/session/${sessionId}/element/${elementId}/click`, {})
      },
      evaluate: async (fn, arg) => {
        const value = await request('POST', `/session/${sessionId}/execute/async`, {
          script: `const done = arguments[arguments.length - 1]; Promise.resolve((${fn.toString()})(arguments[0])).then((value) => done({ ok: true, value }), (error) => done({ ok: false, error: String(error && error.stack || error) }));`,
          args: [arg],
        })
        if (!value?.ok) throw new Error(value?.error ?? 'Safari WebDriver script failed')
        return value.value
      },
      close: async () => {
        await request('DELETE', `/session/${sessionId}`).catch(() => {})
        child.kill('SIGTERM')
      },
    }
  } catch (error) {
    child.kill('SIGTERM')
    throw error
  }
}

// hls.js が non-fatal で報告する、内容の欠けや停止の現れ。
const HLS_GAP_DETAILS = ['bufferSeekOverHole', 'bufferNudgeOnStall', 'bufferStalledError', 'bufferAppendNoProgress']
// 境界 ±0.5 秒に 30 fps で 30 枚前後が描かれる。currentTime ごと内容が飛ぶと半分になる。
const MIN_BOUNDARY_FRAMES = 20
// 再生中に frame callback がこれより空いたら、映像が止まったとみなす。
const FREEZE_MS = 250

/**
 * contentJumps は連続する frame callback の間で、フレーム番号の進みが表示時刻の差から外れた箇所を返す。
 * ネイティブ HLS は PTS に従って currentTime ごと飛ぶので、marker・mediaTime・currentTime が一致したまま
 * 内容が飛ぶ。それは壁時計との比較でしか見えない。
 */
function contentJumps(frames) {
  const jumps = []
  for (let index = 1; index < frames.length; index += 1) {
    const [before, after] = [frames[index - 1], frames[index]]
    const elapsedMs = after.at - before.at
    const delta = after.frame - before.frame
    if (delta < -1 || delta > (elapsedMs / 1000) * FRAME_RATE + 3) {
      jumps.push({ from: before.frame, to: after.frame, elapsedMs, currentTime: before.currentTime })
    }
  }
  return jumps
}

const QUIET_QUANTUM_MS = (128 / 48000) * 1000

function channelMatches(sample, expected) {
  return sample && Math.abs(sample.leftHz - expected[0]) <= 18 && Math.abs(sample.rightHz - expected[1]) <= 18 &&
    sample.leftRMS >= 0.02 && sample.rightRMS >= 0.02
}

// currentTime が mediaTime とずれてよい frame 数。ネイティブ HLS（WebKit）は callback の時点で currentTime が
// 表示中のフレームより 2 frame 先を指す単発があったので 2、hls.js は 1 とする。
function markerMismatches(frames, currentTolerance = 1) {
  return frames
    .map((sample) => ({
      ...sample,
      frameTime: sample.frame / FRAME_RATE,
      markerMediaDeltaFrames: sample.frame - Math.round(sample.mediaTime * FRAME_RATE),
      mediaCurrentDeltaFrames: Math.round(sample.mediaTime * FRAME_RATE) - Math.round(sample.currentTime * FRAME_RATE),
    }))
    .filter((sample) => Math.abs(sample.markerMediaDeltaFrames) > 1.05 || Math.abs(sample.mediaCurrentDeltaFrames) > currentTolerance + 0.05)
}

async function waitForState(adapter, predicate, timeoutMs, description) {
  const startedAt = Date.now()
  let state
  while (Date.now() - startedAt < timeoutMs) {
    state = await adapter.evaluate(() => window.__e2e.state())
    if (state.startError) throw new Error(`再生開始に失敗: ${state.startError}`)
    if (predicate(state)) return state
    await delay(100)
  }
  const { currentTime, paused, readyState, buffered, hlsErrors, frameError } = state
  throw new Error(`${description} を ${timeoutMs}ms 以内に確認できない: ${JSON.stringify({ currentTime, paused, readyState, buffered, hlsErrors, frameError })}`)
}

/**
 * checkCase は 1 つの master を再生し、failures を `[名前] 説明` の形で返す。
 * 名前: progress（再生が進まない）/ timecode / seek-frame / seek-buffered / audio-gap / audio-frequency /
 * audio-unavailable / track / stall / content-jump / frame-freeze / hls-gap / hls-error。
 */
async function checkCase(adapter, server, { masterURL, hold }) {
  const failures = []
  const add = (tag, message) => failures.push(`[${tag}] ${message}`)
  await adapter.navigate(adapter.serverURL + HARNESS_PATH)
  const loaded = await adapter.evaluate(async (url) => window.__e2e.load(url), masterURL)
  await waitForState(adapter, (s) => s.tracks.length === 3, 10_000, '音声 rendition 3 本')
  const native = loaded.path === 'native-hls'
  const summary = { browserPath: loaded.path, nativeMIME: loaded.nativeMIME, segmentMIME: loaded.segmentMIME }

  // seek して再生を始め、シーク先から描かれる最初の数フレームを判定する。
  // 再生を始めるまでの間に、シーク前のフレームが 1〜2 回だけ callback に出るブラウザがある
  // （mediaTime がシーク先から遠い）。それは数えず、シーク先 ±0.5 秒のフレームが出るまでの枚数を 2 枚までとする。
  const seekAndCheck = async (target, resume) => {
    const seeked = await adapter.evaluate(async (seconds) => window.__e2e.seek(seconds), target)
    const expected = Math.round(target * FRAME_RATE)
    if (!Number.isFinite(seeked.landing) || Math.abs(seeked.landing - target) > 1 / FRAME_RATE) {
      add('seek-frame', `seeked 位置が要求 ${target}s から 1 frame を超えてずれた (${seeked.landing})`)
    }
    await resume()
    await waitForState(adapter, (s) => !s.paused && s.currentTime >= target + 0.6, 15_000, `${target}s への seek の後の再生`)
    const frames = await adapter.evaluate((range) => window.__e2e.framesBetween(range[0], range[1]), [target - 0.3, target + 0.8])
    const start = frames.findIndex((sample) => Math.abs(sample.mediaTime - target) <= 0.5)
    summary[`seek${target}`] = frames.slice(0, 8).map((sample) => `${sample.frame}@${sample.mediaTime.toFixed(2)}`).join(' ')
    if (frames.length === 0) {
      add('seek-frame', `${target}s への seek の後にフレームが描かれない`)
    } else if (start < 0) {
      add('seek-frame', `${target}s への seek の後に、シーク先 ±0.5s のフレームが描かれない: marker=${JSON.stringify(frames.slice(0, 5).map((sample) => sample.frame))}, mediaTime=${JSON.stringify(frames.slice(0, 5).map((sample) => Number(sample.mediaTime.toFixed(3))))}`)
    } else {
      const first = frames.slice(start, start + 5)
      // seek 直後は currentTime が表示中のフレームより先に進んでいることがある。フレームの内容は自分の mediaTime と比べる。
      const bad = first.filter((sample) => Math.abs(sample.frame - Math.round(sample.mediaTime * FRAME_RATE)) > 1)
      if (start > 2 || bad.length > 0 || Math.abs(first[0].frame - expected) > FRAME_RATE * 0.3) {
        add('seek-frame', `seek の後の最初のフレームが目標 ${target}s (frame ${expected}) と合わない (シーク前のフレーム ${start} 枚の後): marker=${JSON.stringify(first.map((sample) => sample.frame))}, mediaTime=${JSON.stringify(first.map((sample) => Number(sample.mediaTime.toFixed(3))))}, currentTime=${JSON.stringify(first.map((sample) => Number(sample.currentTime.toFixed(3))))}`)
      }
    }
    return true
  }

  try {
    const resumed = await seekAndCheck(BOUNDARY - 1.5 * SEGMENT, () => adapter.click('#start'))
    if (!resumed) await adapter.click('#start')
    let state = await waitForState(adapter, (s) => !s.paused && s.currentTime >= BOUNDARY - 1.5 * SEGMENT + 0.5, 15_000, '再生開始')
    if (hold) {
      // 保留した映像 segment は再生が境界に着くまで返さない。止まるまで待ってから返す。
      try {
        await waitForState(adapter, (s) => s.currentTime >= BOUNDARY - 0.3 && hold.received, 15_000, '保留した segment を要求したまま境界の手前へ到達')
        await waitForState(adapter, (s) => s.stalls.length > 0, 4_000, 'stall')
      } finally {
        hold.release()
      }
    }
    state = await waitForState(adapter, (s) => s.currentTime >= BOUNDARY + 2.5, 15_000, '再起動境界の後の再生継続')

    const played = await adapter.evaluate((range) => window.__e2e.framesBetween(range[0], range[1]), [BOUNDARY - 1.5 * SEGMENT + 0.5, BOUNDARY + 2.6])
    if (process.env.E2E_DUMP_FRAMES) summary.dump = played.filter((sample) => sample.currentTime >= BOUNDARY - 0.3 && sample.currentTime <= BOUNDARY + 1.6).map((sample) => `cur=${sample.currentTime.toFixed(3)} media=${sample.mediaTime.toFixed(3)} marker=${sample.frame} at=${sample.at.toFixed(0)}`)
    // 再生を始めた直後の 0.5 秒は、読み込みの待ちで callback が空くので含めない。
    const jumps = contentJumps(played)
    if (jumps.length > 0) {
      const jump = jumps[0]
      add('content-jump', `フレーム番号が壁時計に対して${jump.to < jump.from ? '戻った' : '飛んだ'}: ${jump.from} -> ${jump.to} (表示時刻の差 ${jump.elapsedMs.toFixed(0)} ms, currentTime=${jump.currentTime.toFixed(3)}s; ${jumps.length} 箇所)`)
    }
    const freezes = played.slice(1).map((sample, index) => ({ from: played[index], elapsedMs: sample.at - played[index].at })).filter((gap) => gap.elapsedMs > FREEZE_MS)
    if (freezes.length > 0) {
      add('frame-freeze', `フレームが ${freezes[0].elapsedMs.toFixed(0)} ms 描かれなかった (frame ${freezes[0].from.frame}, currentTime=${freezes[0].from.currentTime.toFixed(3)}s; ${freezes.length} 箇所)`)
    }
    const timecodeErrors = markerMismatches(await adapter.evaluate(
      (range) => window.__e2e.framesBetween(range[0], range[1]), [BOUNDARY - 0.5, BOUNDARY + 0.5]), native ? 2 : 1)
    const boundaryFrames = await adapter.evaluate((range) => window.__e2e.framesBetween(range[0], range[1]), [BOUNDARY - 0.5, BOUNDARY + 0.5])
    summary.boundaryFrames = boundaryFrames.length
    if (boundaryFrames.length === 0) add('progress', `境界 ${BOUNDARY}s の映像フレームを取得できない`)
    else if (boundaryFrames.length < MIN_BOUNDARY_FRAMES) add('content-jump', `境界 ±0.5s のフレームが ${boundaryFrames.length} 枚しかない (${MIN_BOUNDARY_FRAMES} 枚以上が要る。currentTime ごと内容が飛ぶと減る)`)
    // 内容（marker）と mediaTime の不一致は 1 枚でも落とす。mediaTime と currentTime の不一致は、
    // ネイティブ HLS で currentTime が 2 frame ぶれる単発があるので、境界のフレームの 2 割を超えたときだけ落とす。
    const timecodeFailed = timecodeErrors.some((sample) => Math.abs(sample.markerMediaDeltaFrames) > 1.05) ||
      timecodeErrors.length > boundaryFrames.length * 0.2
    if (timecodeFailed) {
      const sample = timecodeErrors[0]
      add('timecode', `currentTime=${sample.currentTime.toFixed(3)}s, mediaTime=${sample.mediaTime.toFixed(3)}s, marker=${sample.frameTime.toFixed(3)}s (media/current=${sample.mediaCurrentDeltaFrames} frames, marker/media=${sample.markerMediaDeltaFrames} frames; ${timecodeErrors.length}/${boundaryFrames.length} frames)`)
    }

    const quiet = await adapter.evaluate((range) => window.__e2e.quietRun(range[0], range[1]), [BOUNDARY - 0.8, BOUNDARY + 0.8])
    const webAudioAvailable = quiet.loud > 0
    summary.audio = webAudioAvailable ? `AudioWorklet 128-sample RMS (${quiet.quanta} quanta)` : 'WebAudio に音声が届かない'
    if (!webAudioAvailable && loaded.path !== 'native-hls') add('audio-unavailable', 'WebAudio に再生音声が届かず、境界の音声継続を測れない')
    if (!webAudioAvailable && adapter.engine === 'safari') add('audio-unavailable', 'Safari のネイティブ HLS の音声が WebAudio に届かず、音声継続を確認できない')
    if (webAudioAvailable) {
      if (quiet.longest >= 2) add('audio-gap', `境界 ±0.8s で 128 サンプルごとの RMS が ${quiet.longest} 回連続で無音 (約 ${(quiet.longest * QUIET_QUANTUM_MS).toFixed(1)} ms 以上)`)
      const wrong = state.audio
        .filter((sample) => sample.currentTime >= BOUNDARY - 0.6 && sample.currentTime <= BOUNDARY + 0.6)
        .filter((sample) => !channelMatches(sample, [440, 880]))
      if (wrong.length >= 2) add('audio-frequency', `標準音声が境界で変化した: ${JSON.stringify(wrong.slice(0, 3))}`)
    }

    for (const [trackIndex, expected, label] of [[1, [440, 440], '主'], [2, [880, 880], '副'], [0, [440, 880], '標準']]) {
      const beforeSwitch = state.currentTime
      await adapter.evaluate(async (index) => window.__e2e.selectTrack(index), trackIndex)
      if (!webAudioAvailable && loaded.path === 'native-hls') {
        state = await waitForState(
          adapter,
          (s) => s.tracks[trackIndex]?.enabled && s.tracks.filter((track) => track.enabled).length === 1 && s.currentTime >= beforeSwitch + 0.3,
          5_000,
          `${label} のネイティブ audioTracks の選択と再生継続`,
        )
      } else {
        state = await waitForState(adapter, (s) => channelMatches(s.audio.at(-1), expected), 8_000, `${label}音声への切替 (WebAudio の左右周波数)`)
      }
    }

    const mark = server.mark()
    const farResumed = await seekAndCheck(FAR_SEEK, () => adapter.evaluate(async () => window.__e2e.start()))
    const target = server.segmentsAt(FAR_SEEK)
    const early = server.requestsBetween(0, mark).filter((requested) => target.includes(requested))
    summary.farSeekSegments = target.length
    // ネイティブ HLS の先読みは hls.js の設定で決まらず、60 秒すべてを先に要求するので、この判定は省く。
    if (native) summary.skipped = 'シーク前の要求 (native-hls は先読みをブラウザが決める)'
    else if (target.length === 0) add('seek-buffered', `シーク先 ${FAR_SEEK}s を含む segment を playlist から特定できない`)
    else if (early.length > 0) add('seek-buffered', `シーク先 ${FAR_SEEK}s の segment をシーク前に要求していた (バッファ内でのシーク): ${early.join(', ')}`)
    if (!farResumed) await adapter.evaluate(async () => window.__e2e.start())
    state = await waitForState(adapter, (s) => !s.paused && s.currentTime >= FAR_SEEK + 1, 15_000, '遠距離シーク後の再生継続')

    summary.hlsErrors = state.hlsErrors
    for (const error of state.hlsErrors.filter((entry) => entry.fatal)) add('hls-error', `${error.details} (currentTime=${error.at})`)
    // 穴を飛び越えた・再生が止まって押し出された・append が進まない、は内容の欠けか停止の現れ。
    const gaps = state.hlsErrors.filter((entry) => !entry.fatal && HLS_GAP_DETAILS.includes(entry.details))
    if (gaps.length > 0) add('hls-gap', `${gaps.length} 件: ${[...new Set(gaps.map((entry) => `${entry.details}@${entry.at.toFixed(1)}s`))].slice(0, 6).join(', ')}`)
    if (state.stalls.length > 0) add('stall', `${state.stalls.length} 回: ${JSON.stringify(state.stalls)}`)
  } catch (error) {
    // 途中の待ちが尽きたとき。原因を読めるよう、バッファ範囲と hls.js のエラーを含めて名前を付ける。
    add('progress', error.message)
  }
  summary.failures = failures
  return summary
}

async function main() {
  const engine = process.env.E2E_BROWSER ?? 'chrome'
  if (!['chrome', 'webkit', 'safari'].includes(engine)) throw new Error(`E2E_BROWSER が不正: ${engine}`)
  const formats = (process.env.E2E_FORMATS ?? 'ts').split(',')
  if (formats.some((format) => !['ts', 'fmp4'].includes(format))) throw new Error(`E2E_FORMATS が不正: ${process.env.E2E_FORMATS}`)

  // 正例は内蔵フィクスチャか E2E_MASTER_URL。負例は内蔵フィクスチャでしか作れない。
  const cases = []
  let upstream
  if (MASTER_URL) {
    const master = new URL(MASTER_URL)
    upstream = master.origin
    cases.push({ name: 'external', masterPath: `${master.pathname}${master.search}` })
  } else {
    const generated = ensurePlaybackFixtures(formats)
    for (const { format, kind } of generated.fixtures) {
      // ネイティブ HLS は no-offset を正しく再生する（境界と遠距離シークの後でフレーム番号が連続する）ので、
      // その経路では正例として扱う。hls.js だけが、連続でない取得で PTS の跳びを再生に持ち込む。
      const nativeFine = kind === 'no-offset' && engine !== 'chrome'
      const negative = nativeFine ? undefined : { 'off-grid': ['timecode', 'content-jump', 'frame-freeze'], 'no-offset': ['seek-frame', 'content-jump', 'frame-freeze'], 'audio-gap': ['audio-gap'] }[kind]
      if (negative && engine !== 'chrome' && kind === 'audio-gap') continue
      cases.push({ name: `${format}/${kind}`, masterPath: `/${format}/${kind}/master.m3u8`, expect: negative, note: nativeFine ? '正しく再生される想定 (native-hls)' : undefined })
    }
    if (engine === 'chrome' && formats.includes('ts')) {
      // 境界の映像 segment の応答を、再生が境界の手前に来るまで保留する。
      cases.push({ name: 'ts/stall', masterPath: '/ts/restarted/master.m3u8', expect: ['stall'], holdPath: '/ts/restarted/segments/restart-video-000.ts' })
    }
  }

  const only = process.env.E2E_CASES?.split(',')
  const selected = only ? cases.filter((entry) => only.includes(entry.name)) : cases
  const served = await startServer(upstream)
  let adapter
  const problems = []
  try {
    adapter = engine === 'safari' ? await createSafariAdapter() : await createPlaywrightAdapter(engine)
    adapter.engine = engine
    adapter.serverURL = served.url
    log(`browser: ${engine} ${adapter.version}`)
    for (const entry of selected) {
      served.reset()
      const hold = entry.holdPath ? served.hold(entry.holdPath) : undefined
      let summary
      try {
        summary = await checkCase(adapter, served, { masterURL: `${served.url}${entry.masterPath}`, hold })
      } catch (error) {
        log(`NG: ${entry.name}: ${error.message}`)
        problems.push(`${entry.name}: ${error.message}`)
        continue
      }
      const met = entry.expect
        ? summary.failures.some((failure) => entry.expect.some((tag) => failure.startsWith(`[${tag}]`)))
        : summary.failures.length === 0
      log(`${met ? 'OK' : 'NG'}: ${entry.name}${entry.note ? ` (${entry.note})` : ''}${entry.expect ? ` (期待する失敗: ${entry.expect.join(' か ')})` : ''} via ${summary.browserPath} (canPlayType: playlist=${summary.nativeMIME}, video/mp2t=${summary.segmentMIME}); boundary frames=${summary.boundaryFrames}; audio=${summary.audio}; far-seek segments=${summary.farSeekSegments}${summary.skipped ? `; 省略: ${summary.skipped}` : ''}`)
      if (summary.dump) log(`  境界のフレーム:\n    ${summary.dump.join('\n    ')}`)
      if (summary.hlsErrors?.length) log(`  hls.js のエラー: ${JSON.stringify(summary.hlsErrors)}`)
      for (const failure of summary.failures) log(`  ${entry.expect ? '失敗' : 'NG'}: ${failure}`)
      if (!met) problems.push(`${entry.name}: ${summary.failures.join('; ') || `期待した失敗 [${entry.expect.join('] か [')}] を検出できない`}`)
    }
  } finally {
    if (adapter) await adapter.close()
    await new Promise((resolve) => served.server.close(resolve))
  }

  log('\n=== 結果 ===')
  if (problems.length === 0) log('すべて期待どおり')
  else problems.forEach((problem) => log(`NG: ${problem}`))
  if (problems.length > 0) process.exitCode = 1
  log(`\nsegment=${SEGMENT}s, boundary=${BOUNDARY}s (内蔵は ${RESTART_FROM_SECONDS}s から再起動), far seek=${FAR_SEEK}s, FPS=${FRAME_RATE}`)
}

main().catch((error) => {
  console.error(error.stack ?? error)
  process.exitCode = 1
})
