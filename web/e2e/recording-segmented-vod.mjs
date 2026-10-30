// 録画 HLS の再起動境界と、録画中 EVENT の playlist 成長・commit 境界を実ブラウザで測る。使い方は web/e2e/checks/playback.md。
import { createServer } from 'node:http'
import { readFileSync, statSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

import { createPlaywrightAdapter, log } from './lib.mjs'
import {
  buildGrowingEventPlaylist,
  EVENT_GROW_INTERVAL_MS,
  EVENT_SEGMENT_COUNT,
  ensurePlaybackFixtures,
  FIXTURE_ROOT,
  FRAME_BITS,
  FRAME_RATE,
  readMediaPlaylistRows,
  RESTART_FROM_SECONDS,
  SEGMENT_SECONDS,
  SPLIT_SECONDS,
} from './recording-segmented-fixture.mjs'

const require = createRequire(import.meta.url)
const HLS_BUNDLE = path.join(path.dirname(require.resolve('hls.js')), 'hls.min.js')
const envNumber = (name, fallback) => (process.env[name] === undefined ? fallback : Number(process.env[name]))
// 判定の位置。E2E_* で上書きできる（アプリの URL に向けるとき、その録画の segment 長と再起動境界に合わせる）。
const SEGMENT = envNumber('E2E_SEGMENT_SECONDS', SEGMENT_SECONDS)
const DEFAULT_BOUNDARY = envNumber('E2E_BOUNDARY_SECONDS', SPLIT_SECONDS)
// 内蔵フィクスチャ（60 秒）で、境界の手前から読み始めた hls.js の先読み（既定 30 秒）の外になる位置。
const FAR_SEEK = envNumber('E2E_FAR_SEEK_SECONDS', 52)
const MASTER_URL = process.env.E2E_MASTER_URL
const EVENT_ENDLIST_TIMEOUT_MS = envNumber('E2E_EVENT_ENDLIST_TIMEOUT_MS', 90_000)
const HLS_FORWARD_BUFFER_SECONDS = 30
// ENDLIST の後に、総尺への到達と終端までの再生を待つ上限。
const ENDED_TIMEOUT_MS = (3 * SEGMENT + 15) * 1000
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
      let progressStarted = false

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
        progressStarted = false
        lastProgressTime = video.currentTime
        lastProgressAt = performance.now()
        stallRecorded = false
      })
      setInterval(() => {
        if (!state.capture || video.paused || video.seeking) return
        const now = performance.now()
        if (video.currentTime > lastProgressTime + 0.01) {
          progressStarted = true
          lastProgressTime = video.currentTime
          lastProgressAt = now
          stallRecorded = false
        } else if (progressStarted && !stallRecorded && now - lastProgressAt >= 500) {
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

      // AudioWorklet が 128 サンプルごと（48 kHz なら約 2.7 ms）の左右 RMS を、全サンプルについて求める。
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
        return []
      }

      window.__e2e = {
        // 再生経路はアプリと同じく hls.js に一本化する。
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

          if (!window.Hls || !window.Hls.isSupported()) throw new Error('hls.js がこのブラウザで利用できない')
          state.path = 'hls.js'
          // アプリの録画再生は startAt が無いとき { startPosition: 0 } だけを渡す（live-player.tsx）。
          const hls = new window.Hls({ startPosition: 0 })
          state.hls = hls
          hls.on(window.Hls.Events.ERROR, (_event, data) => {
            state.hlsErrors.push({ details: data.details, fatal: !!data.fatal, at: video.currentTime, sn: data.frag?.sn })
          })
          const metadataReady = new Promise((resolve, reject) => {
            const timer = setTimeout(() => reject(new Error('hls.js の loadedmetadata が 15 秒以内に来ない')), 15_000)
            video.addEventListener('loadedmetadata', () => { clearTimeout(timer); resolve() }, { once: true })
            video.addEventListener('error', () => { clearTimeout(timer); reject(new Error('hls.js のメディア読み込みに失敗')) }, { once: true })
          })
          const manifestReady = new Promise((resolve, reject) => {
            hls.on(window.Hls.Events.MANIFEST_PARSED, resolve)
            hls.on(window.Hls.Events.ERROR, (_event, data) => {
              if (data.fatal) reject(new Error(data.details || data.type || 'fatal hls.js error'))
            })
          })
          hls.loadSource(masterURL)
          hls.attachMedia(video)
          await Promise.all([manifestReady, metadataReady])
          state.startStage = 'audio-setup'
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
          return { path: state.path, duration: video.duration }
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
          // resume() と play() を、クリックの同じタスク内で await より先に要求する。
          state.startStage = 'play'
          const resumed = state.audioContext.resume()
          // play() が解決する前に描かれるフレームも記録する（seek 後の最初のフレームを取りこぼさない）。
          state.capture = true
          progressStarted = false
          lastProgressTime = video.currentTime
          lastProgressAt = performance.now()
          const played = video.play()
          await Promise.all([resumed, played])
          state.startStage = 'playing'
          lastProgressTime = video.currentTime
          lastProgressAt = performance.now()
          stallRecorded = false
          return { currentTime: video.currentTime, paused: video.paused }
        },

        pause() {
          video.pause()
        },

        async selectTrack(index) {
          const tracks = audioTracks()
          if (tracks.length !== 3) throw new Error('音声 rendition が 3 本でない: ' + JSON.stringify(tracks))
          if (state.hls) {
            state.hls.audioTrack = index
            return audioTracks()
          }
          throw new Error('hls.js の audioTracks が 3 本でない')
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
            ended: video.ended,
            seeking: video.seeking,
            readyState: video.readyState,
            buffered,
            tracks: audioTracks(),
            stalls: state.stalls.slice(),
            audio: state.audio.slice(-180),
            hlsErrors: state.hlsErrors.slice(),
            frameError: state.frameError,
            startError: state.startError,
            startStage: state.startStage,
            audioContextState: state.audioContext?.state,
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
          return { quanta: window.length, loud, longest, sampleRate: state.audioContext?.sampleRate }
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

async function waitForServer(predicate, timeoutMs, description) {
  const startedAt = Date.now()
  while (Date.now() - startedAt < timeoutMs) {
    if (predicate()) return
    await delay(100)
  }
  throw new Error(`${description} を ${timeoutMs}ms 以内に確認できない`)
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

async function startServer(upstream, eventShortSegments) {
  const requests = []
  const segmentRanges = new Map()
  const holds = new Map()
  const playlistObservations = []
  const playlistHistory = new Map()
  const playlistViolations = []
  const recordedViolations = new Set()
  let activeCase
  let eventStartedAt
  let eventFinalSegmentPublished = false

  function rememberPlaylist(requestPath, search, text) {
    const lines = text.split(/\r?\n/)
    // Master playlist にも .m3u8 は付くが、media playlist だけにある TARGETDURATION で区別する。
    if (!lines.some((line) => line.startsWith('#EXT-X-TARGETDURATION:'))) return
    const rows = readMediaPlaylistRows(text)
    const key = `${requestPath}${search}`
    const previous = playlistHistory.get(key)
    const observedAt = Date.now()
    const endList = lines.includes('#EXT-X-ENDLIST')
    const grewWhileOpen = Boolean(previous && rows.length > previous.rows.length && !endList)
    if (previous) {
      if (rows.length < previous.rows.length) {
        const violationKey = `${key}:shrink:${previous.rows.length}:${rows.length}`
        if (!recordedViolations.has(violationKey)) {
          playlistViolations.push({ tag: 'playlist-shrink', path: key, previous: previous.rows.length, current: rows.length, playlistIndex: playlistObservations.length, at: observedAt })
          recordedViolations.add(violationKey)
        }
      }
      for (let index = 0; index < Math.min(previous.rows.length, rows.length); index += 1) {
        const before = previous.rows[index]
        const after = rows[index]
        if (before.uri === after.uri && before.extinf === after.extinf && before.programDateTime === after.programDateTime) continue
        const violationKey = `${key}:mutated:${index}`
        if (!recordedViolations.has(violationKey)) {
          playlistViolations.push({ tag: 'playlist-mutated', path: key, index, before, after, playlistIndex: playlistObservations.length, at: observedAt })
          recordedViolations.add(violationKey)
        }
      }
    }
    const observation = {
      key,
      path: requestPath,
      rows,
      duration: rows.reduce((total, row) => total + Number(row.extinf), 0),
      endList,
      grewWhileOpen,
      at: observedAt,
    }
    playlistObservations.push(observation)
    playlistHistory.set(key, observation)
  }

  function eventPlaylist(requestPath, source, now) {
    const mode = activeCase.eventMode
    const isNegative = mode === 'shrink' || mode === 'extinf-change'
    const boundary = activeCase.boundary ?? DEFAULT_BOUNDARY
    const initialCount = Math.max(1, Math.ceil((boundary + 1) / SEGMENT_SECONDS))
    if (eventStartedAt === undefined) eventStartedAt = now
    const elapsed = now - eventStartedAt
    const finalDelay = isNegative ? 2 * EVENT_GROW_INTERVAL_MS : (EVENT_SEGMENT_COUNT - initialCount) * EVENT_GROW_INTERVAL_MS
    const ended = elapsed >= finalDelay
    if (mode === 'growing') {
      const visibleCount = Math.min(EVENT_SEGMENT_COUNT, initialCount + Math.floor(elapsed / EVENT_GROW_INTERVAL_MS))
      if (ended) eventFinalSegmentPublished = true
      return buildGrowingEventPlaylist(source, {
        segmentCount: EVENT_SEGMENT_COUNT,
        visibleCount,
        ended,
        shortFinal: true,
      })
    }
    if (mode === 'shrink') {
      // 実機で観測した 240 → 182 行の縮退を、実在 segment の繰り返しで再現する。
      const sourceCount = 240
      return buildGrowingEventPlaylist(source, {
        segmentCount: sourceCount,
        visibleCount: ended ? 182 : sourceCount,
        ended,
      })
    }
    if (mode === 'extinf-change') {
      return buildGrowingEventPlaylist(source, {
        segmentCount: EVENT_SEGMENT_COUNT,
        visibleCount: EVENT_SEGMENT_COUNT,
        ended,
        mutation: 'extinf',
      })
    }
    throw new Error(`未知の EVENT fixture: ${mode} (${requestPath})`)
  }

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
      if (upstream) {
        result = await readUpstream(upstream, requestPath, url.search, request.headers.range)
      } else if (requestPath.startsWith('/event/') && activeCase?.eventMode) {
        const fixturePath = `/ts/restarted${requestPath.slice('/event'.length)}`
        const relativePath = requestPath.slice('/event/'.length)
        const shortSegment = activeCase.eventMode === 'growing' && eventFinalSegmentPublished ? eventShortSegments?.get(relativePath) : undefined
        result = shortSegment
          ? readFixtureFile(`/${shortSegment}`, request.headers.range)
          : readFixtureFile(fixturePath, request.headers.range)
        if (requestPath.endsWith('.m3u8') && requestPath !== '/event/master.m3u8' && result.status === 200) {
          result.body = Buffer.from(eventPlaylist(requestPath, result.body.toString('utf8'), Date.now()))
        }
      } else {
        result = readFixtureFile(requestPath, request.headers.range)
      }
    } catch (error) {
      result = { status: 502, headers: {}, body: Buffer.from(String(error)) }
    }
    if (requestPath.endsWith('.m3u8') && result.status === 200) {
      const text = result.body.toString('utf8')
      rememberSegments(requestPath, text, segmentRanges)
      rememberPlaylist(requestPath, url.search, text)
    }
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
    reset(entry) {
      requests.length = 0
      segmentRanges.clear()
      playlistObservations.length = 0
      playlistHistory.clear()
      playlistViolations.length = 0
      recordedViolations.clear()
      holds.clear()
      activeCase = entry
      eventStartedAt = undefined
      eventFinalSegmentPublished = false
    },
    // 要求の通し番号。seek の前後で要求を分けるために使う。
    mark: () => requests.length,
    playlistMark: () => playlistObservations.length,
    playlistObservationsSince: (mark) => playlistObservations.slice(mark),
    requestsBetween: (from, to) => requests.slice(from, to),
    // `time` 秒を含む segment の path。
    segmentsAt: (time) => [...segmentRanges].filter(([, [from, to]]) => from <= time && time < to).map(([name]) => name),
    hasRepeatedPlaylistGrowthSince(mark) {
      const seen = new Set()
      for (const observation of playlistObservations.slice(mark)) {
        if (seen.has(observation.key) && observation.grewWhileOpen) return true
        seen.add(observation.key)
      }
      return false
    },
    firstPlaylistSince: (mark) => playlistObservations[mark],
    hasEndListSince: (mark) => playlistObservations.slice(mark).some((observation) => observation.endList),
    latestPlaylistDurationSince: (mark) => Math.max(0, ...playlistObservations.slice(mark).map((observation) => observation.duration)),
    // ENDLIST を含んで配った media playlist の総尺。
    endListDurationSince: (mark) => Math.max(0, ...playlistObservations.slice(mark).filter((observation) => observation.endList).map((observation) => observation.duration)),
    unrequestedSeekTargetsSince(mark, notBefore) {
      const latest = new Map()
      for (const observation of playlistObservations.slice(mark)) latest.set(observation.key, observation)
      const targets = new Set()
      for (const observation of latest.values()) {
        let start = 0
        for (const row of observation.rows) {
          const end = start + Number(row.extinf)
          // その行の後ろに 2 行以上が載っている行だけを選ぶ。行き先を載っている範囲の端にしない。
          if (start >= notBefore && observation.duration - end >= 2 * SEGMENT - 0.01) targets.add(start)
          start = end
        }
      }
      const requested = new Set(requests)
      return [...targets].filter((target) => {
        const paths = [...segmentRanges]
          .filter(([, [from, to]]) => from <= target && target < to)
          .map(([segmentPath]) => segmentPath)
        return paths.length > 0 && paths.every((segmentPath) => !requested.has(segmentPath))
      }).sort((a, b) => a - b)
    },
    violationsSince: (mark) => playlistViolations.filter((violation) => violation.playlistIndex >= mark),
    // path の応答を release() まで保留する。
    hold(requestPath) {
      let release
      const hold = { received: false, released: new Promise((resolve) => { release = resolve }), release: () => release() }
      holds.set(requestPath, hold)
      return hold
    },
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
 * mediaTime と currentTime が一致したまま内容が飛ぶ回帰を、壁時計との比較で検出する。
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

function channelMatches(sample, expected) {
  return sample && Math.abs(sample.leftHz - expected[0]) <= 18 && Math.abs(sample.rightHz - expected[1]) <= 18 &&
    sample.leftRMS >= 0.02 && sample.rightRMS >= 0.02
}

// currentTime と mediaTime の差は 1 frame まで許す。
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
  const { currentTime, paused, ended, readyState, buffered, hlsErrors, frameError, startStage, audioContextState } = state
  throw new Error(`${description} を ${timeoutMs}ms 以内に確認できない: ${JSON.stringify({ currentTime, paused, ended, readyState, buffered, hlsErrors, frameError, startStage, audioContextState })}`)
}

// 伸びている間に遠距離シークできる最小の行数。先読みの端（境界 + 2.5 + 先読み 30 秒）以降の行、その後ろの 2 行、
// ENDLIST の前に載る最後の 1 行ぶん。実機の URL ではこの式を使わない。
function minimumEventRows(boundary) {
  return Math.ceil((boundary + 2.5 + HLS_FORWARD_BUFFER_SECONDS) / SEGMENT_SECONDS) + 4
}

// 伸びる EVENT の遠距離シーク先。先読みの外で未要求、後ろに 2 行以上が載っている行の先頭のうち最も手前を選ぶ。
async function chooseFarSeekTarget(adapter, server, playlistMark, growing) {
  if (!growing) return { target: FAR_SEEK, published: undefined }

  await adapter.evaluate(() => window.__e2e.pause())
  const state = await adapter.evaluate(() => window.__e2e.state())
  const bufferedEnd = Math.max(0, ...state.buffered.map(([, end]) => end))
  const forwardBufferHorizon = Math.max(bufferedEnd, state.currentTime + HLS_FORWARD_BUFFER_SECONDS)
  // 実機の URL は E2E_FAR_SEEK_SECONDS も下限にする。内蔵フィクスチャは先読みの外であれば足りる。
  const notBefore = MASTER_URL ? Math.max(FAR_SEEK, forwardBufferHorizon) : forwardBufferHorizon
  const findTargets = () => server.unrequestedSeekTargetsSince(playlistMark, notBefore)
  try {
    await waitForServer(
      () => findTargets().length > 0 || server.hasEndListSince(playlistMark),
      EVENT_ENDLIST_TIMEOUT_MS,
      `先読み範囲の外で、後ろに 2 行以上が載った未要求 segment がある playlist (${notBefore.toFixed(2)}s 以降)`,
    )
  } catch {
    // playlist の更新が止まったときも、現在公開済みで未要求の範囲を調べる。
  }
  const published = server.latestPlaylistDurationSince(playlistMark)
  const [target] = findTargets()
  if (target === undefined) {
    throw new Error(`先読み範囲の外に、後ろに 2 行以上が載った未要求 segment がない (published=${published.toFixed(3)}s, buffer=${bufferedEnd.toFixed(3)}s, horizon=${forwardBufferHorizon.toFixed(3)}s)`)
  }
  return { target, published }
}

/**
 * checkCase は 1 つの master を再生し、failures を `[名前] 説明` の形で返す。
 * 名前: progress（再生が進まない）/ timecode / seek-frame / seek-buffered / audio-gap / audio-frequency /
 * audio-unavailable / track / stall / content-jump / frame-freeze / hls-gap / hls-error /
 * event-growth / ended / playlist-shrink / playlist-mutated。
 */
async function checkCase(adapter, server, { masterURL, hold, boundary: BOUNDARY = DEFAULT_BOUNDARY, checkPrefetch = true, eventURL = false, requireEnded = false }) {
  const failures = []
  const add = (tag, message) => failures.push(`[${tag}] ${message}`)
  const playlistMark = server.playlistMark()
  await adapter.navigate(adapter.serverURL + HARNESS_PATH)
  const loaded = await adapter.evaluate(async (url) => window.__e2e.load(url), masterURL)
  await waitForState(adapter, (s) => s.tracks.length === 3, 10_000, '音声 rendition 3 本')
  const summary = { browserPath: loaded.path }
  let growing = false
  let growthEvidenceFailureReported = false
  let earlyEndListFailureReported = false
  if (eventURL) {
    await waitForServer(() => server.firstPlaylistSince(playlistMark) !== undefined, 10_000, '最初の media playlist')
    growing = !server.firstPlaylistSince(playlistMark).endList
  }
  const checkGrowthState = (stage) => {
    if (!growing) return
    if (!server.hasRepeatedPlaylistGrowthSince(playlistMark) && !growthEvidenceFailureReported) {
      add('event-growth', `${stage} までに同じ EVENT playlist の行が増えた応答を確認できない`)
      growthEvidenceFailureReported = true
    }
    if (server.hasEndListSince(playlistMark) && !earlyEndListFailureReported) {
      add('event-growth', `既存の判定を終える前に ENDLIST が配られた (${stage})`)
      earlyEndListFailureReported = true
    }
  }

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
      if (start > 2 || bad.length > 0 || Math.abs(first[0].frame - expected) > 1) {
        add('seek-frame', `seek の後の最初のフレームが目標 ${target}s (frame ${expected}) と合わない (シーク前のフレーム ${start} 枚の後): marker=${JSON.stringify(first.map((sample) => sample.frame))}, mediaTime=${JSON.stringify(first.map((sample) => Number(sample.mediaTime.toFixed(3))))}, currentTime=${JSON.stringify(first.map((sample) => Number(sample.currentTime.toFixed(3))))}`)
      }
    }
  }

  try {
    await seekAndCheck(BOUNDARY - 1.5 * SEGMENT, () => adapter.click('#start'))
    if (growing) {
      // 再生位置が進み、同じ media playlist を再取得して行が増えたことを確認する。
      // EVENT の既出行を評価するための追加 fetch は発行しない。
      await waitForServer(
        () => server.hasRepeatedPlaylistGrowthSince(playlistMark) || server.hasEndListSince(playlistMark),
        15_000,
        '同じ media playlist の成長中の再要求',
      )
      checkGrowthState('境界判定の開始')
    }
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
    if (process.env.E2E_DUMP_FRAMES) summary.dump = played.filter((sample) => sample.currentTime >= BOUNDARY - 0.3 && sample.currentTime <= BOUNDARY + 1.6).map((sample) => `cur=${sample.currentTime.toFixed(4)} media=${sample.mediaTime.toFixed(4)} marker=${sample.frame} grid=${(sample.frame / FRAME_RATE).toFixed(4)} media-grid=${((sample.mediaTime - sample.frame / FRAME_RATE) * 1000).toFixed(1)}ms at=${sample.at.toFixed(0)}`)
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
      (range) => window.__e2e.framesBetween(range[0], range[1]), [BOUNDARY - 0.5, BOUNDARY + 0.5]))
    const boundaryFrames = await adapter.evaluate((range) => window.__e2e.framesBetween(range[0], range[1]), [BOUNDARY - 0.5, BOUNDARY + 0.5])
    summary.boundaryFrames = boundaryFrames.length
    if (boundaryFrames.length === 0) add('progress', `境界 ${BOUNDARY}s の映像フレームを取得できない`)
    else if (boundaryFrames.length < MIN_BOUNDARY_FRAMES) add('content-jump', `境界 ±0.5s のフレームが ${boundaryFrames.length} 枚しかない (${MIN_BOUNDARY_FRAMES} 枚以上が要る。currentTime ごと内容が飛ぶと減る)`)
    // 不一致は 1 枚でも落とす。
    if (timecodeErrors.length > 0) {
      const sample = timecodeErrors[0]
      add('timecode', `currentTime=${sample.currentTime.toFixed(3)}s, mediaTime=${sample.mediaTime.toFixed(3)}s, marker=${sample.frameTime.toFixed(3)}s (media/current=${sample.mediaCurrentDeltaFrames} frames, marker/media=${sample.markerMediaDeltaFrames} frames; ${timecodeErrors.length}/${boundaryFrames.length} frames)`)
    }

    const quiet = await adapter.evaluate((range) => window.__e2e.quietRun(range[0], range[1]), [BOUNDARY - 0.8, BOUNDARY + 0.8])
    const webAudioAvailable = quiet.loud > 0
    summary.audio = webAudioAvailable ? `AudioWorklet 128-sample RMS (${quiet.quanta} quanta)` : 'WebAudio に音声が届かない'
    if (!webAudioAvailable) add('audio-unavailable', 'WebAudio に再生音声が届かず、境界の音声継続を測れない')
    if (webAudioAvailable) {
      if (quiet.longest >= 2) add('audio-gap', `境界 ±0.8s で 128 サンプルごとの RMS が ${quiet.longest} 回連続で無音 (約 ${(quiet.longest * 128 / quiet.sampleRate * 1000).toFixed(1)} ms 以上)`)
      const wrong = state.audio
        .filter((sample) => sample.currentTime >= BOUNDARY - 0.6 && sample.currentTime <= BOUNDARY + 0.6)
        .filter((sample) => !channelMatches(sample, [440, 880]))
      if (wrong.length >= 2) add('audio-frequency', `標準音声が境界で変化した: ${JSON.stringify(wrong.slice(0, 3))}`)
    }
    checkGrowthState('再生停止・境界音声・時刻判定')

    for (const [trackIndex, expected, label] of [[1, [440, 440], '主'], [2, [880, 880], '副'], [0, [440, 880], '標準']]) {
      await adapter.evaluate(async (index) => window.__e2e.selectTrack(index), trackIndex)
      state = await waitForState(adapter, (s) => channelMatches(s.audio.at(-1), expected), 8_000, `${label}音声への切替 (WebAudio の左右周波数)`)
    }
    checkGrowthState('音声 rendition 切替')

    const farSeek = await chooseFarSeekTarget(adapter, server, playlistMark, growing)
    summary.farSeekTarget = farSeek.target
    summary.publishedSeconds = farSeek.published
    const mark = server.mark()
    await seekAndCheck(farSeek.target, () => adapter.evaluate(async () => window.__e2e.start()))
    const target = server.segmentsAt(farSeek.target)
    const early = server.requestsBetween(0, mark).filter((requested) => target.includes(requested))
    summary.farSeekSegments = target.length
    if (!checkPrefetch) summary.skipped = 'シーク前の要求 (先読みの外になる位置がない)'
    else if (target.length === 0) add('seek-buffered', `シーク先 ${farSeek.target}s を含む segment を現在の playlist から特定できない`)
    else if (early.length > 0) add('seek-buffered', `シーク先 ${farSeek.target}s の segment をシーク前に要求していた (バッファ内でのシーク): ${early.join(', ')}`)
    state = await waitForState(adapter, (s) => !s.paused && s.currentTime >= farSeek.target + 1, 15_000, '遠距離シーク後の再生継続')
    checkGrowthState('遠距離 seek の着地')

    if (requireEnded && growing) {
      await waitForServer(() => server.hasEndListSince(playlistMark), EVENT_ENDLIST_TIMEOUT_MS, 'ENDLIST を含む media playlist')
      // ENDLIST の後、終端の 3 セグメント手前から終端まで再生して ended に届くことを見る。
      // 着地から 5 秒以上進むことも要る（着地がすでに終端なら ended は再生なしで立つ）。
      try {
        const total = server.endListDurationSince(playlistMark)
        await waitForState(adapter, (s) => Math.abs(s.duration - total) <= 0.5, ENDED_TIMEOUT_MS, `video.duration が ENDLIST 時の総尺 ${total.toFixed(3)}s に届く`)
        const landed = (await adapter.evaluate(async (seconds) => window.__e2e.seek(seconds), Math.max(0, total - 3 * SEGMENT))).landing
        await adapter.evaluate(async () => window.__e2e.start())
        state = await waitForState(adapter, (s) => s.ended && s.currentTime >= landed + 5, ENDED_TIMEOUT_MS, `着地 ${landed.toFixed(3)}s から 5 秒以上進んだ video.ended`)
        summary.ended = true
        summary.endedAt = state.currentTime
      } catch (error) {
        add('ended', error.message)
      }
    }

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
  summary.playlistRequests = server.playlistObservationsSince?.(playlistMark)?.length
  for (const violation of server.violationsSince(playlistMark)) {
    add(violation.tag, `${violation.path} の行 ${violation.index ?? ''}: ${violation.previous ?? JSON.stringify(violation.before)} → ${violation.current ?? JSON.stringify(violation.after)}`)
  }
  summary.failures = failures
  return summary
}

async function main() {
  const engine = process.env.E2E_BROWSER ?? 'chrome'
  if (!['chrome', 'webkit'].includes(engine)) throw new Error(`E2E_BROWSER が不正: ${engine}`)
  // 正例は内蔵フィクスチャか E2E_MASTER_URL。負例は内蔵フィクスチャでしか作れない。
  const cases = []
  let upstream
  let eventShortSegments
  if (MASTER_URL) {
    const master = new URL(MASTER_URL)
    upstream = master.origin
    cases.push({ name: 'external', masterPath: `${master.pathname}${master.search}`, boundary: DEFAULT_BOUNDARY, eventURL: true, requireEnded: true })
  } else {
    const generated = ensurePlaybackFixtures()
    eventShortSegments = generated.eventShortSegments
    for (const { format, kind, boundary } of generated.fixtures) {
      const negative = { 'off-grid': ['timecode', 'content-jump', 'frame-freeze'], 'no-offset': ['seek-frame', 'content-jump', 'frame-freeze'], 'audio-gap': ['audio-gap'] }[kind]
      cases.push({ name: `${format}/${kind}`, masterPath: `/${format}/${kind}/master.m3u8`, expect: negative, boundary, checkPrefetch: kind !== 'restarted-k16', optIn: kind === 'restarted-k16' })
    }
    // 境界の映像 segment の応答を、再生が境界の手前に来るまで保留する。
    for (const format of new Set(generated.fixtures.map((fixture) => fixture.format))) {
      const extension = format === 'ts' ? 'ts' : 'm4s'
      cases.push({ name: `${format}/stall`, masterPath: `/${format}/restarted/master.m3u8`, expect: ['stall'], holdPath: `/${format}/restarted/segments/restart-video-000.${extension}`, boundary: SPLIT_SECONDS })
    }
    cases.push({ name: 'event/growing', masterPath: '/event/master.m3u8', eventMode: 'growing', eventURL: true, requireEnded: true, boundary: SPLIT_SECONDS })
    cases.push({ name: 'event/shrink', masterPath: '/event/master.m3u8', eventMode: 'shrink', expect: ['playlist-shrink'], boundary: SPLIT_SECONDS })
    cases.push({ name: 'event/extinf-change', masterPath: '/event/master.m3u8', eventMode: 'extinf-change', expect: ['playlist-mutated'], boundary: SPLIT_SECONDS })
  }

  const only = process.env.E2E_CASES ? process.env.E2E_CASES.split(',') : undefined
  // optIn のケースは、既定では回さない。E2E_CASES に名前を書くと回る。
  const selected = only ? cases.filter((entry) => only.includes(entry.name)) : cases.filter((entry) => !entry.optIn)
  if (selected.length === 0) throw new Error(`E2E_CASES=${process.env.E2E_CASES} に一致するケースがない (${cases.map((entry) => entry.name).join(', ')})`)
  if (selected.some((entry) => entry.eventMode === 'growing') && EVENT_SEGMENT_COUNT < minimumEventRows(SPLIT_SECONDS)) {
    throw new Error(`E2E_EVENT_SEGMENT_COUNT=${EVENT_SEGMENT_COUNT} は短すぎる。伸びている間に先読み外へ遠距離シークするには ${minimumEventRows(SPLIT_SECONDS)} 行以上が必要`)
  }
  const served = await startServer(upstream, eventShortSegments)
  let adapter
  const problems = []
  try {
    adapter = await createPlaywrightAdapter(engine)
    adapter.serverURL = served.url
    log(`browser: ${engine} ${adapter.version}`)
    for (const entry of selected) {
      served.reset(entry)
      const hold = entry.holdPath ? served.hold(entry.holdPath) : undefined
      let summary
      try {
        const masterURL = `${served.url}${entry.masterPath}`
        summary = await checkCase(adapter, served, {
          masterURL,
          hold,
          boundary: entry.boundary,
          checkPrefetch: entry.checkPrefetch,
          eventURL: entry.eventURL,
          requireEnded: entry.requireEnded,
        })
      } catch (error) {
        log(`NG: ${entry.name}: ${error.message}`)
        problems.push(`${entry.name}: ${error.message}`)
        continue
      }
      const expectedFailures = entry.expect ? summary.failures.filter((failure) => entry.expect.some((tag) => failure.startsWith(`[${tag}]`))) : []
      const met = entry.expect ? expectedFailures.length > 0 : summary.failures.length === 0
      const metrics = `boundary frames=${summary.boundaryFrames}; audio=${summary.audio}; far-seek=${summary.farSeekTarget ?? 'n/a'}s (published ${summary.publishedSeconds === undefined ? 'n/a' : `${summary.publishedSeconds.toFixed(3)}s`}; ${summary.farSeekSegments ?? 'n/a'} segments); playlist requests=${summary.playlistRequests ?? 'n/a'}${summary.ended ? `; ended=${summary.endedAt.toFixed(3)}s` : ''}${summary.skipped ? `; 省略: ${summary.skipped}` : ''}`
      log(`${met ? 'OK' : 'NG'}: ${entry.name}${entry.note ? ` (${entry.note})` : ''}${entry.expect ? ` (期待する失敗: ${entry.expect.join(' か ')})` : ''} via ${summary.browserPath}; ${metrics}`)
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
  log(`\nsegment=${SEGMENT}s, boundary=${DEFAULT_BOUNDARY}s (内蔵は ${RESTART_FROM_SECONDS}s から再起動), far seek=${FAR_SEEK}s, EVENT=${EVENT_SEGMENT_COUNT} rows/${EVENT_GROW_INTERVAL_MS}ms, endlist timeout=${EVENT_ENDLIST_TIMEOUT_MS}ms, FPS=${FRAME_RATE}`)
}

main().catch((error) => {
  console.error(error.stack ?? error)
  process.exitCode = 1
})
