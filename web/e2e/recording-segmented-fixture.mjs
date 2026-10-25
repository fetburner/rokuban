// 録画の連続 HLS と ffmpeg 再起動をまたぐ HLS を実ブラウザで比べる fixture。
import { execFileSync } from 'node:child_process'
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

// フレーム番号の bit 数は FIXTURE_SECONDS * FRAME_RATE < 2 ** FRAME_BITS を満たす（60 * 30 = 1800 < 2048）。
export const FIXTURE_SECONDS = 60
export const SEGMENT_SECONDS = 2
export const SPLIT_SECONDS = 12
export const RESTART_FROM_SECONDS = SPLIT_SECONDS - SEGMENT_SECONDS
export const FRAME_RATE = 30
export const FRAME_BITS = 11
// AAC の priming 1 フレーム分（1024 samples @ 48 kHz ≈ 21 ms）。
const AUDIO_GAP_SECONDS = 1024 / 48000
// 負例 (a): 格子点ではない位置から再起動する。
const OFF_GRID_RESTART_SECONDS = SPLIT_SECONDS - 1

export const FIXTURE_ROOT = path.join(os.tmpdir(), 'rokuban-e2e-recording-segmented-v2')
export const SOURCE_FILE = 'source.ts'
const SOURCE_PATH = path.join(FIXTURE_ROOT, SOURCE_FILE)
const VARIANTS = ['video', 'standard', 'main', 'sub']
const PROGRAM_DATE_TIME = Date.parse('2025-01-01T00:00:00.000Z')

function run(name, args) {
  try {
    execFileSync(name, args, { stdio: 'pipe' })
  } catch (error) {
    const detail = error.stderr?.toString().trim() || error.message
    throw new Error(`${name} ${args.join(' ')} に失敗した: ${detail}`, { cause: error })
  }
}

function markerFilter() {
  const filters = ['drawbox=x=16:y=8:w=496:h=64:color=black:t=fill']
  for (let bit = 0; bit < FRAME_BITS; bit += 1) {
    const x = 28 + bit * 40
    const divisor = 2 ** bit
    filters.push(
      `drawbox=x=${x}:y=18:w=28:h=44:color=white:t=fill:enable='gt(mod(floor(n/${divisor}),2),0)'`,
    )
  }
  return filters.join(',')
}

function ensureSource() {
  mkdirSync(FIXTURE_ROOT, { recursive: true })
  run('ffmpeg', [
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', `testsrc2=size=640x360:rate=${FRAME_RATE}`,
    '-f', 'lavfi', '-i',
    `aevalsrc=0.5*sin(2*PI*440*t)|0.5*sin(2*PI*880*t):s=48000:d=${FIXTURE_SECONDS}`,
    '-t', String(FIXTURE_SECONDS),
    '-map', '0:v:0', '-map', '1:a:0',
    '-vf', markerFilter(),
    '-c:v', 'libx264', '-preset', 'ultrafast', '-profile:v', 'baseline', '-level', '3.0',
    '-pix_fmt', 'yuv420p', '-g', String(FRAME_RATE * SEGMENT_SECONDS),
    '-keyint_min', String(FRAME_RATE * SEGMENT_SECONDS), '-sc_threshold', '0', '-bf', '0',
    '-c:a', 'aac', '-b:a', '128k', '-ac', '2',
    '-f', 'mpegts', SOURCE_PATH,
  ])
}

function generateHLS({ directory, format, seekSeconds = 0, durationSeconds, outputOffset, extension, silenceStandardRange }) {
  mkdirSync(path.join(directory, 'segments'), { recursive: true })
  const args = ['-hide_banner', '-nostats', '-loglevel', 'error', '-y']
  if (seekSeconds > 0) args.push('-ss', String(seekSeconds))
  args.push('-i', SOURCE_PATH, '-t', String(durationSeconds))
  if (outputOffset !== undefined) args.push('-output_ts_offset', String(outputOffset))
  args.push('-map', '0:v:0', '-map', '0:a:0', '-map', '0:a:0', '-map', '0:a:0')
  if (silenceStandardRange) {
    const [from, to] = silenceStandardRange
    args.push('-filter:a:0', `volume=0:enable='between(t,${from},${to})'`)
  }
  args.push(
    '-c:v:0', 'libx264', '-preset:v:0', 'ultrafast', '-profile:v:0', 'baseline', '-level:v:0', '3.0',
    '-pix_fmt:v:0', 'yuv420p', '-g:v:0', String(FRAME_RATE * SEGMENT_SECONDS),
    '-keyint_min:v:0', String(FRAME_RATE * SEGMENT_SECONDS), '-sc_threshold:v:0', '0', '-bf:v:0', '0',
    '-force_key_frames:v:0', `expr:gte(t,n_forced*${SEGMENT_SECONDS})`,
    '-c:a:0', 'aac', '-b:a:0', '96k',
    '-c:a:1', 'aac', '-b:a:1', '96k', '-filter:a:1', 'pan=stereo|c0=c0|c1=c0',
    '-c:a:2', 'aac', '-b:a:2', '96k', '-filter:a:2', 'pan=stereo|c0=c1|c1=c1',
    '-var_stream_map',
    'v:0,name:video,agroup:audio a:0,name:standard,agroup:audio,default:yes a:1,name:main,agroup:audio a:2,name:sub,agroup:audio',
    '-master_pl_name', 'master.m3u8',
    '-f', 'hls', '-hls_time', String(SEGMENT_SECONDS), '-hls_list_size', '0',
    '-hls_playlist_type', 'vod', '-hls_flags', 'independent_segments+program_date_time+temp_file',
    '-hls_segment_type', format === 'fmp4' ? 'fmp4' : 'mpegts',
    '-hls_segment_filename', path.join(directory, 'segments', `%v_%05d.${extension}`),
    '-hls_base_url', 'segments/', path.join(directory, '%v.m3u8'),
  )
  run('ffmpeg', args)
}

function readSegments(runDirectory, variant) {
  const lines = readFileSync(path.join(runDirectory, `${variant}.m3u8`), 'utf8')
    .split(/\r?\n/)
    .filter(Boolean)
  const segments = []
  let mapURI
  let pendingDuration
  for (const line of lines) {
    if (line.startsWith('#EXT-X-MAP:')) {
      mapURI = /URI="([^"]+)"/.exec(line)?.[1]
      continue
    }
    if (line.startsWith('#EXTINF:')) {
      pendingDuration = Number(line.slice('#EXTINF:'.length).split(',')[0])
      continue
    }
    if (pendingDuration !== undefined && !line.startsWith('#')) {
      segments.push({ duration: pendingDuration, uri: line, mapURI })
      pendingDuration = undefined
    }
  }
  // 複製した AAC rendition には ffmpeg が約 21 ms の端数 segment を付ける。
  // 映像側の格子にないこの端数は playlist に含めず、全 variant の長さを揃える。
  const complete = [...segments]
  while (complete.length > 0 && complete.at(-1).duration < 0.1) complete.pop()
  if (complete.some((segment) => segment.duration < 0.1)) {
    throw new Error(`${variant}.m3u8 に端数でない短い segment がある (${runDirectory})`)
  }
  if (complete.length === 0) throw new Error(`${variant}.m3u8 に segment がない (${runDirectory})`)
  return complete
}

function copyMediaEntry(runDirectory, outputDirectory, variant, entry, prefix, index) {
  const sourceSegment = path.resolve(runDirectory, entry.uri)
  const extension = path.extname(entry.uri)
  const segmentName = `${prefix}-${variant}-${String(index).padStart(3, '0')}${extension}`
  const outputSegment = path.join(outputDirectory, 'segments', segmentName)
  copyFileSync(sourceSegment, outputSegment)

  let mapName
  if (entry.mapURI) {
    const mapExtension = path.extname(entry.mapURI)
    mapName = `${prefix}-${variant}-init${mapExtension}`
    copyFileSync(path.resolve(runDirectory, entry.mapURI), path.join(outputDirectory, 'segments', mapName))
  }
  return { duration: entry.duration, segmentName, mapName }
}

// 境界に EXT-X-DISCONTINUITY は付けない。アプリは playlist を格子から先に合成するので、
// ffmpeg をどこで再起動したかを知らず、付けられない。
function writeMediaPlaylist(outputDirectory, variant, entries) {
  const isFMP4 = entries.some((entry) => entry.mapName)
  const lines = [
    '#EXTM3U',
    '#EXT-X-VERSION:7',
    '#EXT-X-INDEPENDENT-SEGMENTS',
    `#EXT-X-TARGETDURATION:${SEGMENT_SECONDS + 1}`,
    '#EXT-X-MEDIA-SEQUENCE:0',
    '#EXT-X-PLAYLIST-TYPE:VOD',
  ]
  let mediaTime = 0
  // fMP4 の init segment は最初の ffmpeg 実行のものだけを 1 つ宣言する。
  // アプリは再起動の位置を知らないので、境界で MAP を宣言し直せない。
  if (isFMP4) lines.push(`#EXT-X-MAP:URI="segments/${entries[0].mapName}"`)
  entries.forEach((entry) => {
    const date = new Date(PROGRAM_DATE_TIME + mediaTime * 1000).toISOString()
    lines.push(`#EXT-X-PROGRAM-DATE-TIME:${date}`)
    lines.push(`#EXTINF:${entry.duration.toFixed(3)},`)
    lines.push(`segments/${entry.segmentName}`)
    mediaTime += entry.duration
  })
  lines.push('#EXT-X-ENDLIST')
  writeFileSync(path.join(outputDirectory, `${variant}.m3u8`), `${lines.join('\n')}\n`)
}

function writeMasterPlaylist(outputDirectory) {
  const lines = [
    '#EXTM3U',
    '#EXT-X-VERSION:7',
    '#EXT-X-INDEPENDENT-SEGMENTS',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="標準",LANGUAGE="jpn",DEFAULT=YES,AUTOSELECT=YES,URI="standard.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="主",LANGUAGE="jpn",DEFAULT=NO,AUTOSELECT=NO,URI="main.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="副",LANGUAGE="jpn",DEFAULT=NO,AUTOSELECT=NO,URI="sub.m3u8"',
    // hls.js の先読みは max(30 秒, maxBufferSize(60 MB) / BANDWIDTH)。20 Mbps なら 30 秒になり、遠距離シークがバッファの外になる。
    '#EXT-X-STREAM-INF:BANDWIDTH=20000000,AVERAGE-BANDWIDTH=20000000,RESOLUTION=640x360,CODECS="avc1.42c01e,mp4a.40.2",AUDIO="audio"',
    'video.m3u8',
  ]
  writeFileSync(path.join(outputDirectory, 'master.m3u8'), `${lines.join('\n')}\n`)
}

function writeFixture({ outputDirectory, firstRun, restartedRun, format, kind }) {
  mkdirSync(path.join(outputDirectory, 'segments'), { recursive: true })
  for (const variant of VARIANTS) {
    let entries
    if (restartedRun) {
      const first = readSegments(firstRun, variant)
      const restarted = readSegments(restartedRun, variant)
      if (restarted.length < 2) throw new Error(`${kind}/${variant}: restart fixture に重複 segment がない`)
      const firstCopied = first.map((entry, index) =>
        copyMediaEntry(firstRun, outputDirectory, variant, entry, 'first', index),
      )
      // 再起動前の 1 格子点から始め、重複する最初の segment を捨てる。
      const restartedCopied = restarted.slice(1).map((entry, index) =>
        copyMediaEntry(restartedRun, outputDirectory, variant, entry, 'restart', index),
      )
      entries = [...firstCopied, ...restartedCopied]
    } else {
      entries = readSegments(firstRun, variant).map((entry, index) =>
        copyMediaEntry(firstRun, outputDirectory, variant, entry, 'continuous', index),
      )
    }
    writeMediaPlaylist(outputDirectory, variant, entries)
  }
  writeMasterPlaylist(outputDirectory)
  return { format, kind, directory: outputDirectory }
}

function validateFixture(fixture, expectedSegments) {
  const master = readFileSync(path.join(fixture.directory, 'master.m3u8'), 'utf8')
  if ((master.match(/#EXT-X-MEDIA:TYPE=AUDIO/g) ?? []).length !== 3) {
    throw new Error(`${fixture.format}/${fixture.kind}: 音声 rendition が 3 本ではない`)
  }
  for (const variant of VARIANTS) {
    const playlist = readFileSync(path.join(fixture.directory, `${variant}.m3u8`), 'utf8')
    const segments = playlist.split(/\r?\n/).filter((line) => line.startsWith('#EXTINF:'))
    const dates = playlist.split(/\r?\n/).filter((line) => line.startsWith('#EXT-X-PROGRAM-DATE-TIME:'))
    if (!playlist.includes('#EXT-X-ENDLIST') || segments.length !== expectedSegments || dates.length !== expectedSegments) {
      throw new Error(`${fixture.format}/${fixture.kind}/${variant}: playlist の segment/PDT 数が不正 (${segments.length}/${dates.length})`)
    }
    if (fixture.format === 'fmp4' && !playlist.includes('#EXT-X-MAP:')) {
      throw new Error(`${fixture.format}/${fixture.kind}/${variant}: init segment がない`)
    }
    const initURIs = [...playlist.matchAll(/#EXT-X-MAP:URI="([^"]+)"/g)].map((match) => match[1])
    for (const uri of initURIs) {
      if (!existsSync(path.join(fixture.directory, uri))) {
        throw new Error(`${fixture.format}/${fixture.kind}/${variant}: init segment がない (${uri})`)
      }
    }
    if (playlist.includes('#EXT-X-DISCONTINUITY')) {
      throw new Error(`${fixture.format}/${fixture.kind}/${variant}: 再起動境界に discontinuity を付けてはならない`)
    }
    const durationSeconds = segments.reduce((total, line) => total + Number(line.slice('#EXTINF:'.length).split(',')[0]), 0)
    if (fixture.kind !== 'off-grid' && Math.abs(durationSeconds - FIXTURE_SECONDS) > 1 / FRAME_RATE) {
      throw new Error(`${fixture.format}/${fixture.kind}/${variant}: playlist の尺が ${durationSeconds.toFixed(3)} 秒で ${FIXTURE_SECONDS} 秒と合わない`)
    }
    for (const uri of playlist.split(/\r?\n/).filter((line) => line.startsWith('segments/'))) {
      if (!existsSync(path.join(fixture.directory, uri))) {
        throw new Error(`${fixture.format}/${fixture.kind}/${variant}: segment がない (${uri})`)
      }
      if (fixture.format === 'fmp4' && !uri.endsWith('.m4s')) {
        throw new Error(`${fixture.format}/${fixture.kind}/${variant}: fMP4 segment の拡張子が不正 (${uri})`)
      }
      if (fixture.format === 'ts' && !uri.endsWith('.ts')) {
        throw new Error(`${fixture.format}/${fixture.kind}/${variant}: TS segment の拡張子が不正 (${uri})`)
      }
    }
  }
}

/**
 * ensurePlaybackFixtures は source.ts と HLS fixture を作る。formats は `ts` と `fmp4` のうち作る形式。
 * 負例は TS だけ作る（fMP4 は比較用の正例だけ）。
 */
export function ensurePlaybackFixtures(formats = ['ts']) {
  run('ffmpeg', ['-version'])
  run('ffprobe', ['-version'])
  ensureSource()
  const sourceInfo = JSON.parse(runOutput('ffprobe', [
    '-v', 'error', '-show_entries', 'stream=codec_name,codec_type,channels', '-of', 'json', SOURCE_PATH,
  ]))
  const streams = sourceInfo.streams ?? []
  const audio = streams.find((stream) => stream.codec_type === 'audio')
  if (!streams.some((stream) => stream.codec_name === 'h264') || audio?.codec_name !== 'aac' || audio.channels !== 2) {
    throw new Error(`source.ts は H.264 + stereo AAC でなければならない: ${JSON.stringify(streams)}`)
  }

  const fixtures = []
  for (const format of formats) {
    const extension = format === 'ts' ? 'ts' : 'm4s'
    const workRoot = path.join(FIXTURE_ROOT, 'work', format)
    const firstRun = path.join(workRoot, 'first')
    const directRun = path.join(workRoot, 'direct')
    const add = (kind, restarted) => fixtures.push(writeFixture({
      outputDirectory: path.join(FIXTURE_ROOT, format, kind),
      firstRun: restarted ? firstRun : directRun,
      restartedRun: restarted,
      format,
      kind,
    }))
    const restart = (name, options) => {
      const directory = path.join(workRoot, name)
      const seekSeconds = options.seekSeconds ?? RESTART_FROM_SECONDS
      generateHLS({
        directory, format, extension, seekSeconds,
        durationSeconds: FIXTURE_SECONDS - seekSeconds,
        outputOffset: options.outputOffset,
        silenceStandardRange: options.silenceStandardRange,
      })
      return directory
    }
    generateHLS({ directory: directRun, format, durationSeconds: FIXTURE_SECONDS, extension })
    generateHLS({ directory: firstRun, format, durationSeconds: SPLIT_SECONDS, extension })
    add('direct')
    add('restarted', restart('restart', { outputOffset: RESTART_FROM_SECONDS }))
    if (format !== 'ts') continue
    // 再起動直後の標準音声へ 1 AAC フレーム分の無音を入れる。
    add('audio-gap', restart('audio-gap', {
      outputOffset: RESTART_FROM_SECONDS,
      silenceStandardRange: [SEGMENT_SECONDS, SEGMENT_SECONDS + AUDIO_GAP_SECONDS],
    }))
    // 負例 (a): 格子点ではない位置から再起動する（`-output_ts_offset` は位置に合わせる）。
    add('off-grid', restart('off-grid', {
      seekSeconds: OFF_GRID_RESTART_SECONDS,
      outputOffset: OFF_GRID_RESTART_SECONDS,
    }))
    // 負例 (b): 正しい格子点から再起動するが `-output_ts_offset` を付けない。
    add('no-offset', restart('no-offset', {}))
  }

  for (const fixture of fixtures) validateFixture(fixture, FIXTURE_SECONDS / SEGMENT_SECONDS)
  return { root: FIXTURE_ROOT, source: SOURCE_PATH, fixtures }
}

function runOutput(name, args) {
  try {
    return execFileSync(name, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
  } catch (error) {
    const detail = error.stderr?.toString().trim() || error.message
    throw new Error(`${name} ${args.join(' ')} に失敗した: ${detail}`, { cause: error })
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const result = ensurePlaybackFixtures((process.env.E2E_FORMATS ?? 'ts').split(','))
  console.log(`fixture ready: ${result.root}`)
  console.log(`source: ${result.source}`)
  for (const fixture of result.fixtures) console.log(`${fixture.format}/${fixture.kind}: ${fixture.directory}`)
}
