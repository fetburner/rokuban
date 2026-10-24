// HLS の連続生成・ffmpeg 再起動・遠距離シークを実ブラウザで測る。
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
  FRAME_RATE,
  RESTART_FROM_SECONDS,
  SEGMENT_SECONDS,
  SPLIT_SECONDS,
} from './recording-segmented-fixture.mjs'

const require = createRequire(import.meta.url)
const HLS_BUNDLE = path.join(path.dirname(require.resolve('hls.js')), 'hls.min.js')
const HARNESS_HTML = String.raw`<!doctype html>
<html lang="ja">
<head><meta charset="utf-8"><title>recording segmented HLS fixture</title></head>
<body>
  <button id="start" type="button">再生開始</button>
  <video id="video" playsinline preload="auto" crossorigin="anonymous" controls></video>
  <script src="/hls.min.js"></script>
  <script>
    (() => {
      const video = document.querySelector('#video')
      const state = {
        path: null,
        frames: [],
        audio: [],
        audioLevels: [],
        waits: [],
        stalls: [],
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
        for (let bit = 0; bit < 10; bit += 1) {
          const x = 28 + bit * 44 + 16
          const pixel = canvasContext.getImageData(x, 40, 1, 1).data
          if (pixel[0] > 128) frame |= 1 << bit
        }
        return frame
      }

      function captureVideoFrame(_now, metadata) {
        if (state.capture && video.readyState >= 2) {
          try {
            state.frames.push({
              currentTime: video.currentTime,
              mediaTime: metadata.mediaTime,
              frame: decodeFrameMarker(),
            })
            if (state.frames.length > 1200) state.frames.shift()
          } catch (error) {
            state.frameError = String(error)
          }
        }
        video.requestVideoFrameCallback(captureVideoFrame)
      }

      video.requestVideoFrameCallback(captureVideoFrame)

      video.addEventListener('waiting', () => {
        if (!state.capture || video.paused || video.seeking) return
        const wait = { at: performance.now(), currentTime: video.currentTime, type: 'waiting' }
        state.waits.push(wait)
      })
      video.addEventListener('stalled', () => {
        if (state.capture && !video.paused && !video.seeking) {
          state.waits.push({ at: performance.now(), currentTime: video.currentTime, type: 'stalled' })
        }
      })
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
          state.stalls.push({ currentTime: video.currentTime, durationMs: now - lastProgressAt, type: 'no-progress' })
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

      function readAudioLevels() {
        if (!state.capture || !state.audioAnalysers || state.audioContext.state !== 'running') return
        state.audioLevels.push({
          currentTime: video.currentTime,
          leftRMS: rms(state.audioAnalysers.levelLeft),
          rightRMS: rms(state.audioAnalysers.levelRight),
        })
        if (state.audioLevels.length > 12000) state.audioLevels.shift()
      }

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
        async load(masterURL) {
          state.frames = []
          state.audio = []
          state.audioLevels = []
          state.waits = []
          state.stalls = []
          state.frameError = null
          state.capture = false
          if (state.hls) {
            state.hls.destroy()
            state.hls = null
          }
          video.pause()
          video.removeAttribute('src')
          video.load()

          const nativeHLS = video.canPlayType('application/vnd.apple.mpegurl')
          const chrome = /Chrome\//.test(navigator.userAgent)
          if (window.Hls && window.Hls.isSupported() && (chrome || !nativeHLS)) {
            state.path = 'hls.js'
            const hls = new window.Hls({ enableWorker: false, maxBufferLength: 12, backBufferLength: 6 })
            state.hls = hls
            await new Promise((resolve, reject) => {
              hls.on(window.Hls.Events.MANIFEST_PARSED, resolve)
              hls.on(window.Hls.Events.ERROR, (_event, data) => {
                if (data.fatal) reject(new Error(data.details || data.type || 'fatal hls.js error'))
              })
              hls.loadSource(masterURL)
              hls.attachMedia(video)
            })
          } else if (nativeHLS) {
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
            nativeMIME: nativeHLS || '(empty)',
            duration: video.duration,
            tracks: audioTracks(),
          }
        },

        async seek(seconds) {
          if (Math.abs(video.currentTime - seconds) < 0.05) return video.currentTime
          await new Promise((resolve, reject) => {
            const timer = setTimeout(() => reject(new Error('seeked が 10 秒以内に来ない')), 10_000)
            video.addEventListener('seeked', () => { clearTimeout(timer); resolve() }, { once: true })
            video.currentTime = seconds
          })
          return video.currentTime
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
            const levelLeft = audioContext.createAnalyser()
            const levelRight = audioContext.createAnalyser()
            left.fftSize = right.fftSize = 8192
            left.smoothingTimeConstant = right.smoothingTimeConstant = 0
            levelLeft.fftSize = levelRight.fftSize = 256
            levelLeft.smoothingTimeConstant = levelRight.smoothingTimeConstant = 0
            source.connect(splitter)
            splitter.connect(left, 0)
            splitter.connect(right, 1)
            splitter.connect(levelLeft, 0)
            splitter.connect(levelRight, 1)
            source.connect(audioContext.destination)
            state.audioContext = audioContext
            state.audioAnalysers = { left, right, levelLeft, levelRight }
            setInterval(readAudio, 100)
            setInterval(readAudioLevels, 10)
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
          return {
            path: state.path,
            currentTime: video.currentTime,
            duration: video.duration,
            paused: video.paused,
            seeking: video.seeking,
            readyState: video.readyState,
            tracks: audioTracks(),
            waits: state.waits.slice(),
            stalls: state.stalls.slice(),
            frames: state.frames.slice(-180),
            audio: state.audio.slice(-180),
            frameError: state.frameError,
            startError: state.startError,
          }
        },

        audioLevelsBetween(from, to) {
          return state.audioLevels.filter((sample) => sample.currentTime >= from && sample.currentTime <= to)
        },
      }

      document.querySelector('#start').addEventListener('click', () => {
        window.__e2e.start().catch((error) => { state.startError = String(error) })
      })
    })()
  </script>
</body>
</html>`

const MIME_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.m3u8': 'application/vnd.apple.mpegurl',
  '.ts': 'video/mp2t',
  '.m4s': 'video/mp4',
  '.mp4': 'video/mp4',
  '.js': 'text/javascript; charset=utf-8',
}

async function startFixtureServer() {
  let stalledSegmentPath
  let stallInjected = false
  const server = createServer((request, response) => {
    const requestPath = new URL(request.url, 'http://localhost').pathname
    if (requestPath === '/') {
      response.writeHead(200, { 'Content-Type': MIME_TYPES['.html'] })
      response.end(HARNESS_HTML)
      return
    }
    const filePath = requestPath === '/hls.min.js'
      ? HLS_BUNDLE
      : path.resolve(FIXTURE_ROOT, `.${decodeURIComponent(requestPath)}`)
    if (requestPath !== '/hls.min.js' && !filePath.startsWith(`${FIXTURE_ROOT}${path.sep}`)) {
      response.writeHead(403)
      response.end('forbidden')
      return
    }
    const serveFile = () => {
      try {
        const stat = statSync(filePath)
        const range = /^bytes=(\d+)-(\d*)$/.exec(request.headers.range ?? '')
        const common = {
          'Accept-Ranges': 'bytes',
          'Content-Type': MIME_TYPES[path.extname(filePath)] ?? 'application/octet-stream',
          'Cache-Control': 'no-store',
        }
        if (range) {
          const start = Number(range[1])
          const end = Math.min(range[2] ? Number(range[2]) : stat.size - 1, stat.size - 1)
          if (start >= stat.size || end < start) {
            response.writeHead(416, { 'Content-Range': `bytes */${stat.size}` })
            response.end()
            return
          }
          response.writeHead(206, {
            ...common,
            'Content-Length': end - start + 1,
            'Content-Range': `bytes ${start}-${end}/${stat.size}`,
          })
          response.end(readFileSync(filePath).subarray(start, end + 1))
          return
        }
        response.writeHead(200, { ...common, 'Content-Length': stat.size })
        response.end(readFileSync(filePath))
      } catch {
        response.writeHead(404)
        response.end('not found')
      }
    }
    if (requestPath === stalledSegmentPath && !stallInjected) {
      stallInjected = true
      setTimeout(serveFile, 4_000)
      return
    }
    serveFile()
  })
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const address = server.address()
  return {
    server,
    url: `http://127.0.0.1:${address.port}`,
    setStalledSegment(requestPath) {
      stalledSegmentPath = requestPath
      stallInjected = false
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
    throw new Error('safaridriver が起動しない。README の Safari Remote Automation 設定を確認する')
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

function channelMatches(sample, expected) {
  return sample && Math.abs(sample.leftHz - expected[0]) <= 18 && Math.abs(sample.rightHz - expected[1]) <= 18 &&
    sample.leftRMS >= 0.02 && sample.rightRMS >= 0.02
}

function isAudioFailure(failure) {
  return failure.startsWith('再起動境界で音声') || failure.startsWith('標準音声')
}

function markerMismatches(frames, from, to) {
  return frames
    .filter((sample) => sample.currentTime >= from && sample.currentTime <= to)
    .map((sample) => ({
      ...sample,
      frameTime: sample.frame / FRAME_RATE,
      markerMediaDeltaFrames: sample.frame - Math.round(sample.mediaTime * FRAME_RATE),
      mediaCurrentDeltaFrames: Math.round(sample.mediaTime * FRAME_RATE) - Math.round(sample.currentTime * FRAME_RATE),
    }))
    .filter((sample) => Math.abs(sample.markerMediaDeltaFrames) > 1.05 || Math.abs(sample.mediaCurrentDeltaFrames) > 1.05)
}

async function waitForState(adapter, predicate, timeoutMs, description) {
  const startedAt = Date.now()
  let state
  while (Date.now() - startedAt < timeoutMs) {
    state = await adapter.evaluate(() => window.__e2e.state())
    if (predicate(state)) return state
    if (state.startError) throw new Error(`再生開始に失敗: ${state.startError}`)
    await delay(150)
  }
  throw new Error(`${description} を ${timeoutMs}ms 以内に確認できない: ${JSON.stringify(state && { currentTime: state.currentTime, paused: state.paused, readyState: state.readyState, frameError: state.frameError, audio: state.audio.at(-1), tracks: state.tracks })}`)
}

async function checkCase(adapter, fixture, expectedFailure = null, engine) {
  const failures = []
  const playlistKind = fixture.playlistKind ?? fixture.kind
  const masterURL = `${adapter.serverURL}/` + `${fixture.format}/${playlistKind}/master.m3u8`
  await adapter.navigate(adapter.serverURL)
  const loaded = await adapter.evaluate(async (url) => window.__e2e.load(url), masterURL)
  const tracks = await waitForState(adapter, (state) => state.tracks.length === 3, 10_000, '音声 rendition 3 本')
  if (!tracks) throw new Error('音声 rendition を読み込めない')

  await adapter.evaluate(async () => window.__e2e.seek(9))
  await adapter.click('#start')
  let state = await waitForState(adapter, (s) => !s.paused && s.currentTime >= 9.5, 15_000, '再生開始')
  state = await waitForState(adapter, (s) => s.currentTime >= 14.5, 15_000, '再起動境界後の再生継続')

  const boundaryFrames = state.frames.filter((sample) => sample.currentTime >= SPLIT_SECONDS - 0.5 && sample.currentTime <= SPLIT_SECONDS + 0.5)
  const timecodeErrors = markerMismatches(state.frames, SPLIT_SECONDS - 0.5, SPLIT_SECONDS + 0.5)
  if (boundaryFrames.length === 0) failures.push(`境界 ${SPLIT_SECONDS}s の映像 frame を取得できない`)
  if (timecodeErrors.length > 0) {
    const sample = timecodeErrors[0]
    failures.push(`timecode mismatch: currentTime=${sample.currentTime.toFixed(3)}s, mediaTime=${sample.mediaTime.toFixed(3)}s, marker=${sample.frameTime.toFixed(3)}s (media/current=${sample.mediaCurrentDeltaFrames.toFixed(1)} frames, marker/media=${sample.markerMediaDeltaFrames.toFixed(1)} frames)`)
  }

  const boundaryAudio = state.audio.filter((sample) => sample.currentTime >= SPLIT_SECONDS - 0.6 && sample.currentTime <= SPLIT_SECONDS + 0.6)
  const boundaryLevels = await adapter.evaluate(
    (range) => window.__e2e.audioLevelsBetween(range[0], range[1]),
    [SPLIT_SECONDS - 0.6, SPLIT_SECONDS + 0.6],
  )
  if (boundaryAudio.length < 3) failures.push(`境界音声の測定サンプルが少ない (${boundaryAudio.length})`)
  const webAudioAvailable = boundaryLevels.some((sample) => sample.leftRMS >= 0.02 || sample.rightRMS >= 0.02)
  if (!webAudioAvailable && loaded.path !== 'native-hls') {
    failures.push('WebAudio に再生音声が届かず、境界の音声継続を測れない')
  }
  if (!webAudioAvailable && engine === 'safari') {
    failures.push('Safari native HLS の音声信号を WebAudio で測れず、音声継続を確認できない')
  }
  if (webAudioAvailable) {
    if (boundaryLevels.length < 20) failures.push(`境界音量の測定サンプルが少ない (${boundaryLevels.length})`)
    let longestQuietRun = 0
    let quietRun = 0
    for (const sample of boundaryLevels) {
      if (sample.leftRMS < 0.02 || sample.rightRMS < 0.02) {
        quietRun += 1
        longestQuietRun = Math.max(longestQuietRun, quietRun)
      } else {
        quietRun = 0
      }
    }
    if (longestQuietRun >= 2) failures.push(`再起動境界で音声が ${longestQuietRun} 回連続で無音になった`)
    const wrongFrequencies = boundaryAudio.filter((sample) => !channelMatches(sample, [440, 880]))
    if (wrongFrequencies.length >= 2) failures.push(`標準音声が境界で変化した: ${JSON.stringify(wrongFrequencies.slice(0, 3))}`)
  }

  for (const [trackIndex, expected, label] of [
    [1, [440, 440], '主'],
    [2, [880, 880], '副'],
    [0, [440, 880], '標準'],
  ]) {
    const beforeSwitch = state.currentTime
    await adapter.evaluate(async (index) => window.__e2e.selectTrack(index), trackIndex)
    if (loaded.path === 'native-hls' && !webAudioAvailable) {
      state = await waitForState(
        adapter,
        (s) => s.tracks[trackIndex]?.enabled && s.tracks.filter((track) => track.enabled).length === 1 && s.currentTime >= beforeSwitch + 0.3,
        5_000,
        `${label} native audioTracks の選択と再生継続`,
      )
      if (!state.tracks[trackIndex]?.enabled || state.tracks.filter((track) => track.enabled).length !== 1) {
        failures.push(`${label}音声の audioTracks 選択が反映されない: ${JSON.stringify(state.tracks)}`)
      }
    } else {
      state = await waitForState(adapter, (s) => channelMatches(s.audio.at(-1), expected), 8_000, `${label} audio frequency`)
      if (!channelMatches(state.audio.at(-1), expected)) {
        failures.push(`${label}音声の切替が反映されない: ${JSON.stringify(state.audio.at(-1))}`)
      }
    }
  }

  const farSeekLanding = await adapter.evaluate(async () => window.__e2e.seek(20))
  if (!Number.isFinite(farSeekLanding) || Math.abs(farSeekLanding - 20) > 1 / FRAME_RATE) {
    failures.push(`遠距離シークの seeked 位置が要求値から 1 frame を超えてずれた (${farSeekLanding})`)
  }
  state = await waitForState(adapter, (s) => !s.paused && s.currentTime >= 20.5, 15_000, '遠距離シーク後の再生')
  const seekFrames = state.frames.filter((sample) => sample.currentTime >= 20.2 && sample.currentTime <= 21.2)
  const seekErrors = markerMismatches(state.frames, 20.2, 21.2)
  if (seekFrames.length === 0) failures.push('遠距離シーク位置の frame を取得できない')
  if (seekErrors.length > 0) {
    const sample = seekErrors[0]
    failures.push(`far seek timecode mismatch: currentTime=${sample.currentTime.toFixed(3)}s, mediaTime=${sample.mediaTime.toFixed(3)}s, marker=${sample.frameTime.toFixed(3)}s (media/current=${sample.mediaCurrentDeltaFrames.toFixed(1)} frames, marker/media=${sample.markerMediaDeltaFrames.toFixed(1)} frames)`)
  }
  if (state.stalls.length > 0) failures.push(`stall count=${state.stalls.length}: ${JSON.stringify(state.stalls)}`)
  if (expectedFailure === 'stall' && state.stalls.length === 0) {
    failures.push('negative stall control が再生の停止を検出できない')
  }

  const summary = {
    format: fixture.format,
    fixture: fixture.kind,
    browserPath: loaded.path,
    nativeMIME: loaded.nativeMIME,
    duration: loaded.duration,
    boundaryFrames: boundaryFrames.length,
    boundaryAudioSamples: boundaryAudio.length,
    boundaryAudioLevelSamples: boundaryLevels.length,
    audioCheck: webAudioAvailable
      ? 'WebAudio frequency + 256-sample RMS'
      : engine === 'safari'
        ? 'unavailable (Safari native HLS)'
        : 'native audioTracks (WebAudio signal unavailable)',
    farSeekLanding,
    farSeekFrames: seekFrames.length,
    stallCount: state.stalls.length,
    failures,
  }
  if (expectedFailure === 'timecode' && !failures.some((failure) => failure.includes('timecode mismatch') || failure.includes('far seek timecode mismatch'))) {
    failures.push(`negative control が時刻ずれを検出できない: ${JSON.stringify(failures)}`)
  }
  if (expectedFailure === 'audio' && !failures.some(isAudioFailure)) {
    failures.push(`negative control が音声の途切れを検出できない: ${JSON.stringify(failures)}`)
  }
  return summary
}

async function main() {
  const generated = ensurePlaybackFixtures()
  const engine = process.env.E2E_BROWSER ?? 'chrome'
  if (!['chrome', 'webkit', 'safari'].includes(engine)) throw new Error(`E2E_BROWSER が不正: ${engine}`)
  const { server, url, setStalledSegment } = await startFixtureServer()
  let adapter
  const fixtures = generated.fixtures.filter(({ format, kind }) =>
    (kind !== 'misaligned' && kind !== 'audio-gap') ||
    (format === 'ts' && (kind !== 'audio-gap' || engine === 'chrome')),
  )
  const restartedTS = generated.fixtures.find(({ format, kind }) => format === 'ts' && kind === 'restarted')
  if (engine === 'chrome') fixtures.push({ ...restartedTS, kind: 'stall-control', playlistKind: 'restarted' })
  const failures = []

  try {
    adapter = engine === 'safari' ? await createSafariAdapter() : await createPlaywrightAdapter(engine)
    adapter.serverURL = url
    log(`browser: ${engine} ${adapter.version}`)
    for (const fixture of fixtures) {
      const expectedFailure = fixture.kind === 'misaligned'
        ? 'timecode'
        : fixture.kind === 'audio-gap'
          ? 'audio'
          : fixture.kind === 'stall-control'
            ? 'stall'
          : null
      setStalledSegment(fixture.kind === 'stall-control'
        ? '/ts/restarted/segments/restart-video-000.ts'
        : undefined)
      let summary
      try {
        summary = await checkCase(adapter, fixture, expectedFailure, engine)
      } catch (error) {
        log(`NG: ${fixture.format}/${fixture.kind}: ${error.message}`)
        failures.push(`${fixture.format}/${fixture.kind}: ${error.message}`)
        continue
      } finally {
        if (fixture.kind === 'stall-control') setStalledSegment(undefined)
      }
      const expectationMet = expectedFailure === 'timecode'
        ? summary.failures.some((failure) => failure.includes('timecode mismatch') || failure.includes('far seek timecode mismatch'))
        : expectedFailure === 'audio'
          ? summary.failures.some(isAudioFailure)
          : expectedFailure === 'stall'
            ? summary.stallCount > 0
          : summary.failures.length === 0
      const status = expectationMet ? 'OK' : 'NG'
      log(`${status}: ${fixture.format}/${fixture.kind} via ${summary.browserPath} (canPlayType=${summary.nativeMIME}); stalls=${summary.stallCount}; boundary frames=${summary.boundaryFrames}; boundary audio=${summary.boundaryAudioSamples}; audio level samples=${summary.boundaryAudioLevelSamples} (${summary.audioCheck}); far-seek frames=${summary.farSeekFrames}`)
      if (summary.failures.length > 0) summary.failures.forEach((failure) => log(`  ${expectedFailure ? 'expected failure' : 'NG'}: ${failure}`))
      if (!expectationMet) failures.push(`${fixture.format}/${fixture.kind}: ${summary.failures.join('; ') || 'negative control did not fail'}`)
    }
  } finally {
    if (adapter) await adapter.close()
    await new Promise((resolve) => server.close(resolve))
  }

  log('\n=== 結果 ===')
  if (failures.length === 0) log('すべて期待どおり')
  else failures.forEach((failure) => log(`NG: ${failure}`))
  if (failures.length > 0) process.exitCode = 1
  log(`\nsegment=${SEGMENT_SECONDS}s, restart=${RESTART_FROM_SECONDS}s → ${SPLIT_SECONDS}s, FPS=${FRAME_RATE}`)
}

main().catch((error) => {
  console.error(error.stack ?? error)
  process.exitCode = 1
})
