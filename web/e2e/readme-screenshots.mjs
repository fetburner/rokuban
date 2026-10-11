// README に載せるスクリーンショット（ホーム・番組表・録画の再生・ライブ、スマホの 3 画面）を撮る。
// 判定はしない。
//
// 公開リポジトリに載せるので、実放送の映像・番組情報は使わない。番組・局は架空で、
// 映像は Blender Foundation のオープンムービー（CC BY 3.0）の予告編から切り出す。
// 予告編は初回だけ取得して E2E_README_MEDIA_DIR（既定は OS の一時ディレクトリ）に置く。
// クレジットは README の画像の直下に書いてある。素材を変えたらそちらも直す。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm screenshots:readme
//
// 出力は ../docs/images/readme-*.jpg（追跡する）。
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import {
  GetRecordingResponse,
  ListContinueWatchingResponseItem,
  ListLiveProfilesResponseItem,
  ListProgramsResponseItem,
  ListRecordingsResponseItem,
  ListRecordingUpcomingResponseItem,
  ListReservationsResponseItem,
  ListServicesResponseItem,
} from '../src/api/zod.ts'
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
const MEDIA_DIR = process.env.E2E_README_MEDIA_DIR ?? path.join(os.tmpdir(), 'rokuban-readme-media')
const OUT_DIR = path.resolve(process.cwd(), '..', 'docs', 'images')
const MOVIES = {
  sintel: 'https://download.blender.org/durian/trailer/sintel_trailer-1080p.mp4',
  bbb: 'https://download.blender.org/peach/trailer/trailer_1080p.mov',
}

const ng = []
const FIXED_NOW = new Date('2026-08-12T21:10:00+09:00')
const nowMs = FIXED_NOW.getTime()
const MIN = 60_000
const HOUR = 60 * MIN
const iso = (ms) => new Date(ms).toISOString()
/** jst は JST の「その日の h:m」を絶対時刻にする（24 時以降は翌日扱い）。 */
const jst = (h, m = 0, dayOffset = 0) => Date.parse('2026-08-12T00:00:00+09:00') + (dayOffset * 24 + h) * HOUR + m * MIN
const SITE = 'default'

// --- 局と番組表 ---------------------------------------------------------------

// id は契約どおり networkId * 100000 + serviceId（Service.id の合成規則）。
const services = [
  { id: 3200100101, networkId: 32001, serviceId: 101, name: 'みなと総合', channelType: 'GR', channel: '13', remoteControlKeyId: 1, hasLogoData: false, hasPrograms: true },
  { id: 3200200202, networkId: 32002, serviceId: 202, name: 'みなと教育', channelType: 'GR', channel: '14', remoteControlKeyId: 2, hasLogoData: false, hasPrograms: true },
  { id: 3200400404, networkId: 32004, serviceId: 404, name: 'ひかり放送', channelType: 'GR', channel: '16', remoteControlKeyId: 4, hasLogoData: false, hasPrograms: true },
  { id: 3200500505, networkId: 32005, serviceId: 505, name: 'あさひテレビ', channelType: 'GR', channel: '18', remoteControlKeyId: 5, hasLogoData: false, hasPrograms: true },
  { id: 3200800808, networkId: 32008, serviceId: 808, name: 'さくらテレビ', channelType: 'GR', channel: '20', remoteControlKeyId: 8, hasLogoData: false, hasPrograms: true },
  { id: 400151, networkId: 4, serviceId: 151, name: 'BSそら', channelType: 'BS', channel: 'BS01_0', remoteControlKeyId: 0, hasLogoData: false, hasPrograms: true },
]

// 17:00 から翌 1:00 までの 8 時間を局ごとに埋める。[番組名, ジャンル lv1, 分, 説明]
const schedules = {
  101: [
    ['イブニングニュース', 0, 60, '今日の出来事と各地の天気。'],
    ['ゆうがたクッキング', 10, 30, '夏野菜で作る、火を使わない簡単おかず。'],
    ['まち歩き「城壁の旧市街」', 8, 30, '石畳の路地に残る職人の工房を訪ねる。'],
    ['ニュース・天気', 0, 30, ''],
    ['クイズ ことばの森', 5, 45, '難読漢字から方言まで、ことばの達人が競う。'],
    ['ふるさと自然紀行', 8, 45, '朝霧の森に暮らす生きものたちの一日。'],
    ['夜9時のニュース', 0, 60, ''],
    ['ドラマ「港町の灯」第6話', 3, 45, '灯台守の娘が、閉ざされた航路の秘密に近づく。'],
    ['スポーツまとめ', 1, 30, ''],
    ['深夜の音楽室', 4, 45, '弦楽四重奏でたどる夏の夜。'],
    ['夜更けのドキュメント', 8, 60, '終電後の駅で働く人たち。'],
  ],
  202: [
    ['こども工作ランド', 10, 30, ''],
    ['もりのうさぎ', 7, 30, 'うさぎのバックと森の仲間たちの物語。'],
    ['えいごのじかん', 10, 30, ''],
    ['サイエンス探検', 8, 30, '雲はなぜ浮かぶのか。'],
    ['ばんごはんの台所', 10, 30, ''],
    ['ベランダ菜園', 10, 30, '夏の水やりと鉢の置き場所。'],
    ['まなびの数学', 10, 20, ''],
    ['まなびの物理', 10, 20, ''],
    ['ニュース 手話つき', 11, 20, ''],
    ['世界のドキュメント', 8, 60, '砂漠の隊商がたどる千年の道。'],
    ['語学講座 中国語', 10, 30, ''],
    ['クラシック音楽の時間', 4, 90, '交響曲第 2 番ほか。'],
    ['おとなの教養講座', 10, 60, ''],
  ],
  404: [
    ['ゆうがたニュース', 0, 120, ''],
    ['ぐるっと世界旅', 8, 60, '北の島の白夜をめぐる。'],
    ['夏の歌謡祭 2026', 4, 114, '世代をこえた名曲を生演奏で。'],
    ['ニュース', 0, 30, ''],
    ['トークの縁側', 5, 50, ''],
    ['スポーツナイト', 1, 46, ''],
    ['映画「星降る峠」', 6, 60, ''],
  ],
  505: [
    ['ゆうがたワイド', 2, 115, '街の話題と生活情報。'],
    ['天気', 0, 5, ''],
    ['どうぶつ大集合SP', 5, 114, '全国の動物園から人気者が集合。'],
    ['ニュース', 0, 6, ''],
    ['水曜ドラマ「逃げる街」', 3, 54, '消えた設計図をめぐり、刑事と建築家が手を組む。'],
    ['夜のニュースデスク', 0, 76, ''],
    ['ナイトクラブ', 5, 50, ''],
    ['星屑メモリー', 7, 30, ''],
    ['ねむれぬ夜のラジオ', 5, 30, ''],
  ],
  808: [
    ['ドラマ再放送「海辺の診療所」', 3, 60, ''],
    ['さくらニュース', 0, 60, ''],
    ['プロ野球 みなと×さくら', 1, 114, ''],
    ['ニュース', 0, 6, ''],
    ['水曜シネマ「荒野の約束」', 6, 114, ''],
    ['ナイトニュース', 0, 36, ''],
    ['竜の旅路 第8話「炎の街」', 7, 30, '竜を追って辿り着いた街で、少女はかつての師と再会する。'],
    ['港のマリン', 7, 30, ''],
    ['深夜のしゃべり場', 5, 30, ''],
  ],
  151: [
    ['鉄道でめぐる夏', 8, 60, ''],
    ['BSニュース', 0, 60, ''],
    ['まち歩き 石畳の港町', 8, 60, '坂の上の灯台まで歩く。'],
    ['砂漠紀行 〜風がつくる道〜', 8, 60, '砂丘の稜線は一晩で姿を変える。'],
    ['時代劇「月影の剣」', 3, 60, ''],
    ['世界のいま', 0, 60, ''],
    ['ジャズの夜', 4, 60, ''],
    ['美術館めぐり', 10, 60, ''],
  ],
}
const fillers = [
  ['ミッドナイトニュース', 0, 30, ''], ['深夜ドラマ再放送', 3, 60, ''], ['テレビショッピング', 2, 30, ''],
  ['名作アニメ劇場', 7, 60, ''], ['ライブアーカイブ', 4, 60, ''], ['夜明けの自然', 8, 30, ''],
]

function programsFor(startISO, endISO, serviceIds) {
  const start = Date.parse(startISO)
  const end = Date.parse(endISO)
  // `service` クエリは Service.id（合成 id）。SI の serviceId ではない。
  const targets = serviceIds.length ? services.filter((s) => serviceIds.includes(String(s.id))) : services
  const out = []
  for (const service of targets) {
    // 前日 17:00 から 1 日ずつ同じ編成を敷く。8 時間の外は局ごとにずらした fillers で埋める。
    for (let day = -1; day <= 7; day++) {
      let t = jst(17, 0, day)
      const dayEnd = jst(17, 0, day + 1)
      let i = 0
      while (t < dayEnd) {
        const fixed = schedules[service.serviceId]
        const entry = fixed[i] ?? fillers[(i - fixed.length + services.indexOf(service)) % fillers.length]
        const [name, genre, minutes, description] = entry
        const durationMs = minutes * MIN
        if (t < end && t + durationMs > start) {
          out.push({
            programId: Math.floor(t / 1000) * 1000 + service.serviceId,
            networkId: service.networkId,
            serviceId: service.serviceId,
            eventId: (i + 1) * 10 + day + 1,
            startAt: iso(t),
            endAt: iso(t + durationMs),
            durationMs,
            name,
            description,
            genres: [genre],
            isFree: true,
          })
        }
        t += durationMs
        i++
      }
    }
  }
  return out
}

/** programAt は番組表の「いま」の日に `serviceId` で `name` を流している番組を引く。 */
function programAt(serviceId, name) {
  const service = services.find((s) => s.serviceId === serviceId)
  return programsFor(iso(jst(17)), iso(jst(17, 0, 1)), [String(service.id)]).find((p) => p.name === name)
}

const reservedPrograms = [
  [808, '竜の旅路 第8話「炎の街」', 'rule'],
  [202, 'クラシック音楽の時間', 'rule'],
  [101, '深夜の音楽室', 'manual'],
].map(([serviceId, name, source]) => [services.find((s) => s.serviceId === serviceId), programAt(serviceId, name), source])
const reservations = reservedPrograms.map(([service, program, source], i) => ({
  id: i + 1,
  site: SITE,
  programId: program.programId,
  source,
  ...(source === 'rule' ? { ruleId: 1 } : {}),
  state: 'active',
  title: program.name,
  serviceName: service.name,
  channelType: service.channelType,
  startAt: program.startAt,
  durationMs: program.durationMs,
  createdAt: iso(nowMs - 24 * HOUR),
  updatedAt: iso(nowMs - 24 * HOUR),
  series: null,
  skip: false,
}))

// --- 録画 ---------------------------------------------------------------------

const sakura = services[4]
const recordingBase = {
  site: SITE,
  source: 'rule',
  status: 'finished',
  keepOriginal: 'until_encoded',
  cmDetection: { state: 'disabled' },
  encodedAssets: [{ profile: 'h264-720p', sizeBytes: 812_000_000 }],
  sizeBytes: 3_420_000_000,
}
const fromService = (service) => ({
  serviceName: service.name,
  channelType: service.channelType,
  channel: service.channel,
  networkId: service.networkId,
  serviceId: service.serviceId,
})

// 「竜の旅路」は毎週水曜 23:30。id 7 が主役（ホームの続きから・録画詳細）。
const dragonEpisodes = [
  [5, '竜の旅路 第5話「砂嵐の向こう」', 3],
  [6, '竜の旅路 第6話「赤い鱗」', 2],
  [7, '竜の旅路 第7話「砂の海を越えて」', 1],
].map(([id, title, weeksAgo]) => ({
  ...recordingBase,
  ...fromService(sakura),
  id,
  eventId: id,
  title,
  description: '竜の子を追って砂漠へ踏み出した少女。熱風の向こうに見えたのは、伝説に語られた古い街の影だった。',
  series: '竜の旅路',
  seriesKey: '竜の旅路',
  startAt: iso(jst(23, 30) - weeksAgo * 7 * 24 * HOUR),
  durationMs: 30 * MIN,
  createdAt: iso(jst(23, 30) - weeksAgo * 7 * 24 * HOUR),
  startedAt: iso(jst(23, 30) - weeksAgo * 7 * 24 * HOUR),
  endedAt: iso(jst(24) - weeksAgo * 7 * 24 * HOUR),
  ...(weeksAgo > 1 ? { watchedAt: iso(jst(12) - (weeksAgo - 1) * 7 * 24 * HOUR) } : {}),
}))
const hero = { ...dragonEpisodes[2], resumePositionMs: 17 * MIN + 20_000 }

const others = [
  [21, 'もりのうさぎ 第12話「ちょうちょを追いかけて」', 202, 17, 30, 25],
  [22, '砂漠紀行 〜風がつくる道〜', 151, 20, 0, 60],
  [23, 'ふるさと自然紀行「朝霧の森」', 101, 20, 15, 45],
  [24, 'ドキュメント 雪嶺に挑む', 404, 19, 0, 60],
  [25, 'まち歩き「城壁の旧市街」', 101, 18, 30, 30],
].map(([id, title, serviceId, h, m, minutes], i) => ({
  ...recordingBase,
  ...fromService(services.find((s) => s.serviceId === serviceId)),
  id,
  eventId: id,
  title,
  startAt: iso(jst(h, m, -(i + 1))),
  durationMs: minutes * MIN,
  createdAt: iso(jst(h, m, -(i + 1))),
  resumePositionMs: (i + 2) * 3 * MIN,
}))

const onAir = {
  ...recordingBase,
  ...fromService(services[0]),
  id: 31,
  eventId: 31,
  title: '夜9時のニュース',
  status: 'recording',
  startAt: iso(jst(21)),
  durationMs: HOUR,
  createdAt: iso(jst(21)),
  startedAt: iso(jst(21)),
  sizeBytes: undefined,
  encodedAssets: [],
}
const recordings = [onAir, ...dragonEpisodes, ...others]
const continueWatching = [hero, ...others]
const liveProfiles = [{ name: '720p', height: 720 }]
const upcoming = [{
  site: SITE,
  programId: reservations[0].programId,
  networkId: sakura.networkId,
  serviceId: sakura.serviceId,
  startAt: reservations[0].startAt,
  durationMs: reservations[0].durationMs,
  name: reservations[0].title,
  isFree: true,
}]

await validateFixturesOrExit([
  ...services.map((s, i) => [`services[${i}]`, ListServicesResponseItem, s]),
  ...programsFor(iso(jst(17)), iso(jst(17, 0, 1)), []).map((p, i) => [`programs[${i}]`, ListProgramsResponseItem, p]),
  ...reservations.map((r, i) => [`reservations[${i}]`, ListReservationsResponseItem, r]),
  ...recordings.map((r, i) => [`recordings[${i}]`, ListRecordingsResponseItem, r]),
  ...continueWatching.map((r, i) => [`continueWatching[${i}]`, ListContinueWatchingResponseItem, r]),
  ['hero', GetRecordingResponse, hero],
  ['upcoming[0]', ListRecordingUpcomingResponseItem, upcoming[0]],
  ['liveProfiles[0]', ListLiveProfilesResponseItem, liveProfiles[0]],
], ng)

// --- 映像素材 -----------------------------------------------------------------

// [録画 id, 素材, 秒]。Sintel は 2.39:1 の黒帯入りなので 16:9 に切り抜く。
const stills = [
  [5, 'sintel', 27.5], [6, 'sintel', 23.5], [7, 'sintel', 31.5],
  [21, 'bbb', 25.5], [22, 'sintel', 37.5], [23, 'bbb', 5.5], [24, 'sintel', 7], [25, 'sintel', 24.5],
]
// 主役（id 7）の動画は id 7 のサムネイルと同じ絵にする。ホームの主役は、シーク先の
// フレーム通知が readyState 1 のまま届くと 8 秒後に固定サムネイルへ戻る（この素材の
// Chromium で 3 回中 3 回）。どちらに転んでも同じショットになるようにしておく。
const HERO_FRAME = stills.find(([id]) => id === hero.id).slice(1)

function ffmpeg(args) {
  execFileSync('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', ...args], { stdio: 'pipe' })
}

async function moviePath(name) {
  const file = path.join(MEDIA_DIR, `${name}${path.extname(MOVIES[name])}`)
  if (!existsSync(file)) {
    log(`  取得: ${MOVIES[name]}`)
    const res = await fetch(MOVIES[name])
    if (!res.ok) throw new Error(`${MOVIES[name]}: HTTP ${res.status}`)
    writeFileSync(file, Buffer.from(await res.arrayBuffer()))
  }
  return file
}

const CROP_16_9 = 'crop=ih*16/9:ih,scale=1280:720'
async function grab(name, seconds, out) {
  const vf = name === 'sintel' ? `crop=iw:iw*818/1920,${CROP_16_9}` : CROP_16_9
  ffmpeg(['-ss', String(seconds), '-i', await moviePath(name), '-frames:v', '1', '-vf', vf, '-q:v', '3', out])
  return out
}

mkdirSync(MEDIA_DIR, { recursive: true })
const thumbnails = new Map()
for (const [id, name, seconds] of stills) {
  thumbnails.set(id, readFileSync(await grab(name, seconds, path.join(MEDIA_DIR, `still-${id}.jpg`))))
}
// 再生画面は静止画を録画の長さだけ流す 1 fps の MP4。どの位置へシークしても同じ絵が出る。
// 29.97 fps で 30 分ぶん作ると、撮るたびに数万フレームのエンコードで CPU を使い切る。
const heroStill = await grab(...HERO_FRAME, path.join(MEDIA_DIR, 'hero.jpg'))
const heroVideoPath = path.join(MEDIA_DIR, 'hero.mp4')
ffmpeg([
  '-loop', '1', '-framerate', '1', '-i', heroStill, '-t', String(hero.durationMs / 1000),
  '-c:v', 'libx264', '-tune', 'stillimage', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', heroVideoPath,
])
const heroVideo = readFileSync(heroVideoPath)

// ライブは「みなと教育」の世界のドキュメント（砂漠の隊商）。静止画の HLS を 12 秒ぶん作る。
// 実配信と同じく映像と音声を持たせ（音声は無音）、ENDLIST を付けずライブの形にする。
const LIVE_SERVICE = services[1]
const LIVE_DIR = path.join(MEDIA_DIR, 'live')
mkdirSync(path.join(LIVE_DIR, 'segments'), { recursive: true })
const liveStill = await grab(...stills.find(([id]) => id === 22).slice(1), path.join(MEDIA_DIR, 'live.jpg'))
ffmpeg([
  '-loop', '1', '-framerate', '30000/1001', '-i', liveStill,
  '-f', 'lavfi', '-i', 'anullsrc=r=48000:cl=stereo', '-t', '12',
  '-c:v', 'libx264', '-tune', 'stillimage', '-pix_fmt', 'yuv420p', '-c:a', 'aac',
  '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0', '-hls_flags', 'omit_endlist', '-hls_base_url', 'segments/',
  '-hls_segment_filename', path.join(LIVE_DIR, 'segments', 'segment_%03d.ts'), path.join(LIVE_DIR, 'playlist.m3u8'),
])

// --- スタブ -------------------------------------------------------------------

function serveRange(route, bytes) {
  const range = /^bytes=(\d+)-(\d*)$/.exec(route.request().headers().range ?? '')
  const start = range ? Number(range[1]) : 0
  const end = range?.[2] ? Math.min(Number(range[2]), bytes.length - 1) : bytes.length - 1
  return route.fulfill({
    status: range ? 206 : 200,
    contentType: 'video/mp4',
    body: bytes.subarray(start, end + 1),
    headers: {
      'Accept-Ranges': 'bytes',
      'Content-Length': String(end - start + 1),
      ...(range ? { 'Content-Range': `bytes ${start}-${end}/${bytes.length}` } : {}),
    },
  })
}

async function handler({ path: p, url, json, route }) {
  if (p === '/api/events') return sseKeepAlive(route)
  if (p === '/api/live-profiles') return json(liveProfiles)
  if (p.endsWith('/live/playlist.m3u8')) {
    return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: readFileSync(path.join(LIVE_DIR, 'playlist.m3u8')) })
  }
  const segment = /\/live\/segments\/(segment_\d+\.ts)$/.exec(p)
  if (segment) {
    const file = path.join(LIVE_DIR, 'segments', segment[1])
    if (!existsSync(file)) return route.fulfill({ status: 404 })
    return route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
  }
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (p === '/api/encode-profiles') return json([{ name: 'h264-720p', container: 'mp4' }])
  if (p === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (p === '/api/storage') {
    return json([{ root: 'media', path: '/media', totalBytes: 4e12, usedBytes: 1.3e12, availableBytes: 2.7e12, observedAt: iso(nowMs) }])
  }
  if (p === '/api/reservations') return json(reservations)
  if (p === '/api/recordings/continue-watching') return json(continueWatching)
  if (p === '/api/recordings') {
    if (url.searchParams.get('seriesOf') !== null) return json([...dragonEpisodes].reverse())
    const status = url.searchParams.get('status')
    return json(recordings.filter((r) => status === null || r.status === status))
  }
  const rec = /^\/api\/recordings\/(\d+)(\/[a-z-]+)?$/.exec(p)
  if (rec) {
    const id = Number(rec[1])
    if (rec[2] === '/upcoming') return json(id === hero.id ? upcoming : [])
    if (rec[2] === '/chapters') return json({ version: 'readme', detectionPending: false, source: 'auto', spans: [] })
    if (rec[2] === undefined) {
      const found = id === hero.id ? hero : recordings.find((r) => r.id === id)
      return found ? json(found) : route.fulfill({ status: 404 })
    }
    return json([])
  }
  const media = /^\/api\/media\/recordings\/(\d+)\/(thumbnail|frame|file)$/.exec(p)
  if (media) {
    const id = Number(media[1])
    if (media[2] === 'file' && id === hero.id) return serveRange(route, heroVideo)
    const image = thumbnails.get(id)
    if (media[2] !== 'file' && image) return route.fulfill({ status: 200, contentType: 'image/jpeg', body: image })
    return route.fulfill({ status: 404 })
  }
  if (p === `/api/sites/${SITE}/services`) return json(services)
  if (p === `/api/sites/${SITE}/programs`) {
    return json(programsFor(
      url.searchParams.get('start') ?? iso(nowMs),
      url.searchParams.get('end') ?? iso(nowMs + 6 * HOUR),
      url.searchParams.getAll('service'),
    ))
  }
  if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
  if (/\/reservation$/.test(p)) return route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' })
  if (p.startsWith('/api/media/')) return route.fulfill({ status: 404 })
  return json([])
}

// --- 撮影 ---------------------------------------------------------------------

log(`URL: ${URL_BASE}`)
await verifyBundleMatchesOrExit(URL_BASE, ng)
const browser = await launchBrowser('chromium')
const context = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  deviceScaleFactor: 2,
  colorScheme: 'dark',
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
mkdirSync(OUT_DIR, { recursive: true })

async function shoot(name, pathname, viewport, ready) {
  const page = await context.newPage()
  await page.setViewportSize(viewport)
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, handler)
  await page.goto(URL_BASE + pathname, { waitUntil: 'networkidle' })
  try {
    await ready(page)
  } catch (error) {
    const state = await page.evaluate(() => [...document.querySelectorAll('video')].map((v) => ({
      state: v.getAttribute('data-resume-state'), readyState: v.readyState, seeking: v.seeking, t: v.currentTime, src: v.getAttribute('src'),
      error: v.error?.message,
    })))
    ng.push(`${name}: 撮影の準備が整わない (${error.message}; ${JSON.stringify(state)})`)
    // 追跡している画像を崩れた絵で上書きしない。
    await page.close()
    return
  }
  // 写る範囲の画像の読み込み完了を待つ。画面外の loading="lazy" は読み込まれないので除く。
  await page.waitForFunction(() => [...document.images].every((img) => {
    const r = img.getBoundingClientRect()
    return img.complete || r.bottom <= 0 || r.top >= innerHeight
  }), undefined, { timeout: 10_000 })
  await page.mouse.move(0, 0)
  const out = path.join(OUT_DIR, `readme-${name}.jpg`)
  await page.screenshot({ path: out, type: 'jpeg', quality: 82 })
  log(`  ${out}`)
  await page.close()
}

await shoot('home', '/?mode=watch', { width: 1440, height: 800 }, async (page) => {
  await page.waitForFunction(() => {
    const video = document.querySelector('[data-testid="home-hero-resume-video"]')
    return ['ready', 'failed'].includes(video?.getAttribute('data-resume-state'))
  }, undefined, { timeout: 15_000 })
})

await shoot('programs', '/programs?view=grid', { width: 1440, height: 900 }, async (page) => {
  await page.locator('[data-testid="program-grid-cell"]').first().waitFor({ timeout: 15_000 })
})

await shoot('recording', `/recordings/${hero.id}`, { width: 1440, height: 1130 }, async (page) => {
  await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && !video.seeking
  }, undefined, { timeout: 15_000 })
})

await shoot('live', `/live?service=${LIVE_SERVICE.id}`, { width: 1440, height: 900 }, async (page) => {
  // 「再生」ボタンはプレイヤーを開くだけで、再生は始めない（選択と視聴開始を分ける）。
  // 利用者と同じく、開いたプレイヤーの再生ボタンをもう一度押す。
  await page.getByRole('button', { name: /再生/ }).click()
  await page.waitForFunction(() => document.querySelector('video')?.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA, undefined, { timeout: 15_000 })
  await page.getByRole('button', { name: /再生/ }).first().click()
  await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && !video.paused && video.currentTime > 0.5
  }, undefined, { timeout: 15_000 })
})

await shoot('home-mobile', '/?mode=watch', { width: 390, height: 844 }, async (page) => {
  await page.waitForFunction(() => {
    const video = document.querySelector('[data-testid="home-hero-resume-video"]')
    return ['ready', 'failed'].includes(video?.getAttribute('data-resume-state'))
  }, undefined, { timeout: 15_000 })
})

await shoot('programs-mobile', '/programs', { width: 390, height: 844 }, async (page) => {
  await page.locator('[data-program-id]').first().waitFor({ timeout: 15_000 })
})

await shoot('recording-mobile', `/recordings/${hero.id}`, { width: 390, height: 844 }, async (page) => {
  await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && !video.seeking
  }, undefined, { timeout: 15_000 })
})

await finish(ng, browser)
