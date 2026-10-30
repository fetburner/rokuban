// 録画の連続 HLS、ffmpeg 再起動をまたぐ HLS、録画中 EVENT playlist を実ブラウザで測る fixture。
// 原本 TS と各 run の変換は Go のテスト（internal/streamer の TestWriteSegmentedPlaybackFixture）が、
// 製品の buildSegmentTransformArgs で行う。ここは run の出力を playlist でつなぐだけで、ffmpeg の
// HLS の引数は組まない。
import { execFileSync } from 'node:child_process'
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

// フレーム番号の bit 数は FIXTURE_SECONDS * FRAME_RATE < 2 ** FRAME_BITS を満たす（60 * 29.97 = 1798 < 2048）。
// この 4 つと再起動の番号は Go のテストの定数と同じ値で、manifest と食い違えば作り直しを止める。
export const FIXTURE_SECONDS = 60
export const SEGMENT_SECONDS = 2
export const FRAME_RATE = 30000 / 1001
export const FRAME_BITS = 11
// 再起動の run の startNumber。製品は要求の 1 つ前の番号から変換を始めるので、境界は次の格子点になる。
// 29.97 fps では 2 秒の格子にフレームが割り切れず、再起動の位置によって先頭フレームの格子からのずれが
// 0〜1 フレームの間で変わる。番号 5 は約 10 ms、番号 16 は約 32 ms 早い（checks/playback.md）。
export const RESTART_NUMBER = 5
export const WORST_RESTART_NUMBER = 16
export const RESTART_FROM_SECONDS = RESTART_NUMBER * SEGMENT_SECONDS
export const SPLIT_SECONDS = RESTART_FROM_SECONDS + SEGMENT_SECONDS

export const FIXTURE_ROOT = path.resolve(process.env.E2E_FIXTURE_DIR ?? path.join(os.tmpdir(), 'rokuban-e2e-recording-segmented-v3'))
const FORMATS = process.env.E2E_FMP4 === '1' ? ['ts', 'fmp4'] : ['ts']
const runName = (format, name) => (format === 'ts' ? name : `fmp4-${name}`)
const GENERATED = path.join(FIXTURE_ROOT, 'generated')
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
// variant の添字は製品の -var_stream_map の並び（映像、標準、主、副）。
const VARIANTS = ['video', 'standard', 'main', 'sub']
const PROGRAM_DATE_TIME = Date.parse('2025-01-01T00:00:00.000Z')

function positiveIntegerEnv(name, fallback) {
  if (process.env[name] === undefined) return fallback
  const value = Number(process.env[name])
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${name} は 1 以上の整数にする: ${process.env[name]}`)
  return value
}

// 録画中 EVENT fixture の公開間隔と終端までの行数。既定の間隔は録画と同じ、セグメント長ごとに 1 行。
export const EVENT_GROW_INTERVAL_MS = positiveIntegerEnv('E2E_EVENT_GROW_INTERVAL_MS', SEGMENT_SECONDS * 1_000)
export const EVENT_SEGMENT_COUNT = positiveIntegerEnv('E2E_EVENT_SEGMENT_COUNT', FIXTURE_SECONDS / SEGMENT_SECONDS)
export const EVENT_FINAL_SEGMENT_SECONDS = SEGMENT_SECONDS / 2

/** readMediaPlaylistRows は media playlist から既出行の比較に使う 3 項目を読む。 */
export function readMediaPlaylistRows(playlist) {
  const rows = []
  let programDateTime
  let extinf
  for (const line of playlist.split(/\r?\n/)) {
    if (line.startsWith('#EXT-X-PROGRAM-DATE-TIME:')) programDateTime = line.slice('#EXT-X-PROGRAM-DATE-TIME:'.length)
    else if (line.startsWith('#EXTINF:')) extinf = line.slice('#EXTINF:'.length).split(',')[0]
    else if (extinf !== undefined && line && !line.startsWith('#')) {
      rows.push({ uri: line, extinf, programDateTime })
      programDateTime = undefined
      extinf = undefined
    }
  }
  return rows
}

/** buildGrowingEventPlaylist は既存の segment 行を使い、録画中 EVENT playlist の応答を作る。 */
export function buildGrowingEventPlaylist(playlist, { segmentCount, visibleCount, ended = false, shortFinal = false, mutation } = {}) {
  const sourceLines = playlist.split(/\r?\n/).filter(Boolean)
  const sourceRows = readMediaPlaylistRows(playlist)
  if (!Number.isSafeInteger(segmentCount) || segmentCount < 1) {
    throw new Error(`EVENT fixture の行数 ${segmentCount} は 1 以上の整数にする`)
  }
  if (!Number.isSafeInteger(visibleCount) || visibleCount < 1 || visibleCount > segmentCount) {
    throw new Error(`EVENT fixture の公開行数 ${visibleCount} は不正 (1–${segmentCount})`)
  }
  const firstRowIndex = sourceLines.findIndex((line) => line.startsWith('#EXT-X-PROGRAM-DATE-TIME:') || line.startsWith('#EXTINF:'))
  if (firstRowIndex < 0) throw new Error('EVENT fixture に media playlist の segment 行がない')
  const lines = sourceLines.slice(0, firstRowIndex).filter((line) => line !== '#EXT-X-ENDLIST')
  const firstDate = Date.parse(sourceRows[0]?.programDateTime)
  if (!Number.isFinite(firstDate)) throw new Error('EVENT fixture の最初の PROGRAM-DATE-TIME が不正')
  const rows = Array.from({ length: segmentCount }, (_, index) => {
    const source = sourceRows[index % sourceRows.length]
    return {
      ...source,
      // 240 行の commit 縮退負例では、実在する fixture segment の URI を繰り返し、PDT は格子に沿わせる。
      programDateTime: new Date(firstDate + index * SEGMENT_SECONDS * 1_000).toISOString(),
    }
  })
  for (const [index, row] of rows.slice(0, visibleCount).entries()) {
    let duration = Number(row.extinf)
    if (shortFinal && index === segmentCount - 1) duration = EVENT_FINAL_SEGMENT_SECONDS
    if (mutation === 'extinf' && ended && index === 0) duration += 0.5
    lines.push(`#EXT-X-PROGRAM-DATE-TIME:${row.programDateTime}`)
    lines.push(`#EXTINF:${duration.toFixed(3)},`)
    lines.push(row.uri)
  }
  if (ended) lines.push('#EXT-X-ENDLIST')
  return `${lines.join('\n')}\n`
}

// kind → 再起動の run（null は連続 1 本）。run の名前は Go のテストが付ける。
const KINDS = {
  direct: { run: null },
  restarted: { run: 'restart', number: RESTART_NUMBER },
  'restarted-k16': { run: 'restart-k16', number: WORST_RESTART_NUMBER },
  'audio-gap': { run: 'audio-gap', number: RESTART_NUMBER },
  'off-grid': { run: 'off-grid', number: RESTART_NUMBER },
  'no-offset': { run: 'no-offset', number: RESTART_NUMBER },
}

function runOutput(name, args, options = {}) {
  try {
    return execFileSync(name, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], ...options })
  } catch (error) {
    const detail = error.stderr?.toString().trim() || error.stdout?.toString().trim() || error.message
    throw new Error(`${name} ${args.join(' ')} に失敗した: ${detail}`, { cause: error })
  }
}

/** createEventShortSegments は EVENT playlist の最後に載る fixture segment を切り詰め、EXTINF と一致する TS を用意する。 */
export function createEventShortSegments(fixture, segmentCount = EVENT_SEGMENT_COUNT) {
  const outputRoot = path.join(FIXTURE_ROOT, 'event-short')
  mkdirSync(outputRoot, { recursive: true })
  const result = new Map()
  for (const variant of VARIANTS) {
    const playlist = readFileSync(path.join(fixture.directory, `${variant}.m3u8`), 'utf8')
    const sourceRows = readMediaPlaylistRows(playlist)
    if (!Number.isSafeInteger(segmentCount) || segmentCount < 1 || segmentCount > sourceRows.length) {
      throw new Error(`EVENT fixture の終端行 ${segmentCount} が範囲外 (1–${sourceRows.length})`)
    }
    // buildGrowingEventPlaylist は sourceRows を循環して使う。設定行数が既定の 30 と違うときも、
    // 短い EXTINF を付けた最終行と、実際に切り詰める TS の URI を一致させる。
    const last = sourceRows[(segmentCount - 1) % sourceRows.length]
    if (!last) throw new Error(`${fixture.format}/${fixture.kind}/${variant}: 最後の segment がない`)
    const input = path.join(fixture.directory, last.uri)
    const output = path.join(outputRoot, `${variant}-${path.basename(last.uri)}`)
    const start = Number(runOutput('ffprobe', [
      '-v', 'error', '-show_entries', 'format=start_time',
      '-of', 'default=noprint_wrappers=1:nokey=1', input,
    ]).trim())
    if (!Number.isFinite(start)) throw new Error(`${input}: start_time を読めない`)
    runOutput('ffmpeg', [
      '-hide_banner', '-loglevel', 'error', '-y', '-copyts', '-i', input,
      '-to', String(start + EVENT_FINAL_SEGMENT_SECONDS), '-map', '0', '-c', 'copy',
      '-muxdelay', '0', '-muxpreload', '0', '-mpegts_copyts', '1', '-f', 'mpegts', output,
    ])
    const duration = Number(runOutput('ffprobe', [
      '-v', 'error', '-show_entries', 'format=duration',
      '-of', 'default=noprint_wrappers=1:nokey=1', output,
    ]).trim())
    if (!Number.isFinite(duration) || duration < EVENT_FINAL_SEGMENT_SECONDS - 0.25 || duration > EVENT_FINAL_SEGMENT_SECONDS + 0.25) {
      throw new Error(`${output}: 短い segment の長さが不正 (${duration}s; 期待は約 ${EVENT_FINAL_SEGMENT_SECONDS}s)`)
    }
    result.set(last.uri, path.relative(FIXTURE_ROOT, output))
  }
  return result
}

// 製品のビルダーで run を作る。出力は GENERATED の manifest.json から読む。
function generateRuns() {
  runOutput('ffmpeg', ['-version'])
  runOutput('ffprobe', ['-version'])
  rmSync(GENERATED, { recursive: true, force: true })
  runOutput('go', ['test', './internal/streamer', '-run', '^TestWriteSegmentedPlaybackFixture$', '-count=1'], {
    cwd: REPO_ROOT,
    env: { ...process.env, ROKUBAN_SEGMENTED_PLAYBACK_FIXTURE_DIR: GENERATED, ROKUBAN_SEGMENTED_PLAYBACK_FMP4: FORMATS.includes('fmp4') ? '1' : '0' },
  })
  const manifest = JSON.parse(readFileSync(path.join(GENERATED, 'manifest.json'), 'utf8'))
  const expected = { fixtureSeconds: FIXTURE_SECONDS, segmentSeconds: SEGMENT_SECONDS, frameBits: FRAME_BITS, frameRate: '30000/1001' }
  for (const [key, value] of Object.entries(expected)) {
    if (manifest[key] !== value) throw new Error(`manifest.${key} = ${manifest[key]} が fixture の ${value} と違う`)
  }
  for (const format of FORMATS) {
    for (const [kind, spec] of Object.entries(KINDS)) {
      const run = runName(format, spec.run ?? 'direct')
      const startNumber = manifest.runs[run]?.startNumber
      if (startNumber !== (spec.number ?? 0)) throw new Error(`${format}/${kind}: run ${run} の startNumber が ${startNumber} で ${spec.number ?? 0} と違う`)
    }
  }
  return manifest
}

// run の variant playlist を読み、segment の番号・長さ・path を返す。
function readSegments(manifest, run, variantIndex) {
  const directory = path.join(GENERATED, manifest.runs[run].directory)
  const lines = readFileSync(path.join(directory, `${manifest.profileName}.${variantIndex}.m3u8`), 'utf8')
    .split(/\r?\n/)
    .filter(Boolean)
  const segments = []
  let pendingDuration
  for (const line of lines) {
    if (line.startsWith('#EXTINF:')) {
      pendingDuration = Number(line.slice('#EXTINF:'.length).split(',')[0])
    } else if (pendingDuration !== undefined && !line.startsWith('#')) {
      const number = Number(/_seg(\d+)\.(?:ts|m4s)$/.exec(line)?.[1])
      segments.push({ number, duration: pendingDuration, file: path.resolve(directory, line) })
      pendingDuration = undefined
    }
  }
  // 音声 rendition の末尾には、映像より長い音声が作る約 21 ms の端数 segment が付く。
  // 映像の格子にないこの端数は playlist に含めず、全 variant の長さを揃える。
  while (segments.length > 0 && segments.at(-1).duration < 0.1) segments.pop()
  if (segments.length === 0 || segments.some((segment) => segment.duration < 0.1)) {
    throw new Error(`${run}/${variantIndex}: 端数でない短い segment がある、または segment がない`)
  }
  return segments
}

function copyEntry(outputDirectory, variant, segment, prefix, index) {
  const name = `${prefix}-${variant}-${String(index).padStart(3, '0')}${path.extname(segment.file)}`
  copyFileSync(segment.file, path.join(outputDirectory, 'segments', name))
  return name
}

// 境界に EXT-X-DISCONTINUITY は付けない。アプリは playlist を格子から先に合成するので、
// ffmpeg をどこで再起動したかを知らず、付けられない。EXTINF も格子の長さ（最後だけ残り）で書く。
function writeMediaPlaylist(outputDirectory, variant, names, initNames) {
  const lines = [
    '#EXTM3U',
    `#EXT-X-VERSION:${initNames ? 7 : 3}`,
    '#EXT-X-INDEPENDENT-SEGMENTS',
    `#EXT-X-TARGETDURATION:${SEGMENT_SECONDS}`,
    '#EXT-X-MEDIA-SEQUENCE:0',
    '#EXT-X-PLAYLIST-TYPE:EVENT',
  ]
  names.forEach((name, index) => {
    if (initNames && (index === 0 || initNames[index] !== initNames[index - 1])) lines.push(`#EXT-X-MAP:URI="${initNames[index]}"`)
    const mediaTime = index * SEGMENT_SECONDS
    const duration = index === names.length - 1 ? FIXTURE_SECONDS - mediaTime : SEGMENT_SECONDS
    lines.push(`#EXT-X-PROGRAM-DATE-TIME:${new Date(PROGRAM_DATE_TIME + mediaTime * 1000).toISOString()}`)
    lines.push(`#EXTINF:${duration.toFixed(3)},`)
    lines.push(`segments/${name}`)
  })
  lines.push('#EXT-X-ENDLIST')
  writeFileSync(path.join(outputDirectory, `${variant}.m3u8`), `${lines.join('\n')}\n`)
}

function writeMasterPlaylist(outputDirectory, codecs) {
  const lines = [
    '#EXTM3U',
    '#EXT-X-VERSION:3',
    '#EXT-X-INDEPENDENT-SEGMENTS',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="標準",LANGUAGE="jpn",DEFAULT=YES,AUTOSELECT=YES,URI="standard.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="主",LANGUAGE="jpn",DEFAULT=NO,AUTOSELECT=NO,URI="main.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="副",LANGUAGE="jpn",DEFAULT=NO,AUTOSELECT=NO,URI="sub.m3u8"',
    // ffmpeg の master の BANDWIDTH は実測の約 1.9 Mbps で、hls.js の先読み max(30 秒, maxBufferSize(60 MB) / BANDWIDTH)
    // が 250 秒になり、遠距離シークがバッファの中になる。20 Mbps なら 30 秒になるので、ここは固定値にする。
    `#EXT-X-STREAM-INF:BANDWIDTH=20000000,AVERAGE-BANDWIDTH=20000000,RESOLUTION=640x360,CODECS="${codecs}",AUDIO="audio"`,
    'video.m3u8',
  ]
  writeFileSync(path.join(outputDirectory, 'master.m3u8'), `${lines.join('\n')}\n`)
}

function writeFixture(manifest, kind, format) {
  const spec = KINDS[kind]
  const outputDirectory = path.join(FIXTURE_ROOT, format, kind)
  rmSync(outputDirectory, { recursive: true, force: true })
  mkdirSync(path.join(outputDirectory, 'segments'), { recursive: true })
  VARIANTS.forEach((variant, variantIndex) => {
    let names
    let initNames
    const read = (name) => readSegments(manifest, runName(format, name), variantIndex)
    const copyInit = (name) => {
      const directory = path.join(GENERATED, manifest.runs[runName(format, name)].directory)
      const filename = `${name}-${variant}.mp4`
      copyFileSync(path.join(directory, `init_${variantIndex}.mp4`), path.join(outputDirectory, filename))
      return filename
    }
    if (spec.run === null) {
      names = read('direct').map((segment, index) => copyEntry(outputDirectory, variant, segment, 'continuous', index))
      if (format === 'fmp4') {
        const init = copyInit('direct')
        initNames = names.map(() => init)
      }
    } else {
      // 再起動前は連続の run の先頭から、再起動の run が起動した番号まで（その番号の segment は再起動側が重複して作る）。
      // 再起動後は、再起動の run の 2 本目から。
      const first = read('direct').filter((segment) => segment.number <= spec.number)
      const restarted = read(spec.run).filter((segment) => segment.number > spec.number)
      names = [
        ...first.map((segment, index) => copyEntry(outputDirectory, variant, segment, 'first', index)),
        ...restarted.map((segment, index) => copyEntry(outputDirectory, variant, segment, 'restart', index)),
      ]
      if (format === 'fmp4') {
        const firstInit = copyInit('direct')
        // playlist は再起動位置を知らないので、init も境界で切り替えない。
        initNames = names.map(() => firstInit)
      }
    }
    if (names.length !== FIXTURE_SECONDS / SEGMENT_SECONDS) {
      throw new Error(`${format}/${kind}/${variant}: segment が ${names.length} 本で ${FIXTURE_SECONDS / SEGMENT_SECONDS} 本と合わない`)
    }
    writeMediaPlaylist(outputDirectory, variant, names, initNames)
  })
  writeMasterPlaylist(outputDirectory, manifest.codecs)
  const boundary = spec.run === null ? undefined : (spec.number + 1) * SEGMENT_SECONDS
  return { format, kind, directory: outputDirectory, boundary }
}

function validateFixture(fixture) {
  const label = `${fixture.format}/${fixture.kind}`
  const master = readFileSync(path.join(fixture.directory, 'master.m3u8'), 'utf8')
  if ((master.match(/#EXT-X-MEDIA:TYPE=AUDIO/g) ?? []).length !== 3) throw new Error(`${label}: 音声 rendition が 3 本ではない`)
  const expectedSegments = FIXTURE_SECONDS / SEGMENT_SECONDS
  for (const variant of VARIANTS) {
    const playlist = readFileSync(path.join(fixture.directory, `${variant}.m3u8`), 'utf8')
    const lines = playlist.split(/\r?\n/)
    const segments = lines.filter((line) => line.startsWith('#EXTINF:'))
    const dates = lines.filter((line) => line.startsWith('#EXT-X-PROGRAM-DATE-TIME:'))
    if (!playlist.includes('#EXT-X-ENDLIST') || segments.length !== expectedSegments || dates.length !== expectedSegments) {
      throw new Error(`${label}/${variant}: playlist の segment/PDT 数が不正 (${segments.length}/${dates.length})`)
    }
    if (playlist.includes('#EXT-X-DISCONTINUITY')) throw new Error(`${label}/${variant}: 再起動境界に discontinuity を付けてはならない`)
    const seconds = segments.reduce((total, line) => total + Number(line.slice('#EXTINF:'.length).split(',')[0]), 0)
    if (Math.abs(seconds - FIXTURE_SECONDS) > 1 / FRAME_RATE) throw new Error(`${label}/${variant}: playlist の尺が ${seconds.toFixed(3)} 秒で ${FIXTURE_SECONDS} 秒と合わない`)
    const maps = lines.filter((line) => line.startsWith('#EXT-X-MAP:')).map((line) => /URI="([^"]+)"/.exec(line)?.[1])
    const expectedMaps = fixture.format === 'fmp4' ? 1 : 0
    if (maps.length !== expectedMaps || maps.some((uri) => !uri)) throw new Error(`${label}/${variant}: init map が不正`)
    for (const uri of [...lines.filter((line) => line.startsWith('segments/')), ...maps]) {
      if (!existsSync(path.join(fixture.directory, uri))) throw new Error(`${label}/${variant}: segment がない (${uri})`)
    }
  }
}

/**
 * ensurePlaybackFixtures は Go のテストで製品のビルダーから run を作り、HLS fixture を組む。
 * E2E_FMP4=1 のときはコンテナだけを替えた比較用の fMP4 も作る。
 */
export function ensurePlaybackFixtures() {
  mkdirSync(FIXTURE_ROOT, { recursive: true })
  const manifest = generateRuns()
  const fixtures = FORMATS.flatMap((format) => Object.keys(KINDS).map((kind) => writeFixture(manifest, kind, format)))
  for (const fixture of fixtures) validateFixture(fixture)
  const eventSource = fixtures.find((fixture) => fixture.format === 'ts' && fixture.kind === 'restarted')
  const eventShortSegments = createEventShortSegments(eventSource, EVENT_SEGMENT_COUNT)
  return { root: FIXTURE_ROOT, source: path.join(GENERATED, manifest.source), fixtures, eventShortSegments }
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const result = ensurePlaybackFixtures()
  console.log(`fixture ready: ${result.root}`)
  console.log(`source: ${result.source}`)
  for (const fixture of result.fixtures) console.log(`${fixture.format}/${fixture.kind}: ${fixture.directory}`)
}
