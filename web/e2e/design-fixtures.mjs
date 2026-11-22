import {
  GetEncodeQueueResponse,
  GetStorageResponseItem,
  ListCircuitBreakersResponseItem,
  ListContinueWatchingResponseItem,
  ListLabelRulesResponseItem,
  ListProgramsResponseItem,
  ListRecordingShelvesResponseItem,
  ListRecordingUpcomingResponseItem,
  ListRecordingsResponseItem,
  ListReservationsResponseItem,
  ListRulesResponseItem,
  ListServicesResponseItem,
} from '../src/api/zod.ts'
import { recordingMedia } from './lib.mjs'

/**
 * 時刻を固定する。番組表・「いま」の線・録画の日時がショットごとに動くと、
 * 前回との差が「実装を変えたから」なのか「時間が経ったから」なのか分からなくなる。
 * `clock.setFixedTime` は Date だけを固定してタイマーは動かす（`pauseAt` と違い、
 * React Query や debounce を止めない）。
 */
export const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')

// --- スタブ（API の応答） ---------------------------------------------------

export const SITE = 'default'
// 2 サイト運用（`showSite`）の判定専用。`multiSite` オプション付きでしか
// `/api/sites` に出さない --- 既定の全画面ショット/判定を単一サイトのまま保つため。
export const SITE2 = 'sub'
const HOUR = 3_600_000
const TOKYO_OFFSET_MS = 9 * HOUR

export const services = [
  { id: 3273601024, networkId: 32736, serviceId: 1024, name: 'NHK総合', channelType: 'GR', channel: '27', remoteControlKeyId: 1, hasLogoData: false, hasPrograms: true },
  { id: 3273701032, networkId: 32737, serviceId: 1032, name: 'NHKEテレ', channelType: 'GR', channel: '26', remoteControlKeyId: 2, hasLogoData: false, hasPrograms: true },
  { id: 3273801040, networkId: 32738, serviceId: 1040, name: 'テレビ大阪', channelType: 'GR', channel: '18', remoteControlKeyId: 7, hasLogoData: false, hasPrograms: true },
  { id: 400101, networkId: 4, serviceId: 101, name: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', remoteControlKeyId: 0, hasLogoData: false, hasPrograms: true },
]
// ライブ一覧のスクロール / 多段表示だけに使う長いリスト。番組 API を持たないので
// 初期選択には影響せず、最初の実データ局（NHK総合）を維持する。
const wideLiveServices = [
  ...services,
  ...Array.from({ length: 200 }, (_, i) => {
    const networkId = 32800 + i
    const serviceId = 2000 + i
    return {
      id: networkId * 100000 + serviceId,
      networkId,
      serviceId,
      name: `デザイン確認局${i + 1}`,
      channelType: 'GR',
      channel: String(100 + i),
      remoteControlKeyId: 10 + (i % 10),
      hasLogoData: false,
      hasPrograms: false,
    }
  }),
]

/** 番組名は固定の輪番。ジャンルの淡色が並ぶ様子を見たいので lv1 も回す。 */
const titles = [
  ['ニュース７', 0], ['大相撲中継', 1], ['あさイチ', 2], ['連続テレビ小説', 3],
  ['クラシック音楽館', 4], ['ブラタモリ', 5], ['日曜洋画劇場', 6], ['アニメ劇場', 7],
]

/**
 * programsFor は要求された窓（start / end）を実際に埋める番組を作る。
 * 窓を無視して固定配列を返すと、画面が窓をどう決めているかに依存して
 * 「たまたま空」のショットが撮れてしまう。
 */
function programsFor(startISO, endISO, serviceIds) {
  const start = Date.parse(startISO)
  const end = Date.parse(endISO)
  const targets = serviceIds?.length
    ? services.filter((s) => serviceIds.includes(String(s.serviceId)))
    : services
  const out = []
  // 30 分境界に丸めた「窓より 1 コマ前」から並べる。放送中の番組（開始が窓より前）
  // を必ず 1 つ含めるため --- ここが空だと ON AIR の判定が撮れない。
  const slot = 1800_000
  for (const service of targets) {
    let t = Math.floor(start / slot) * slot - slot
    let i = service.serviceId % titles.length
    while (t < end) {
      const [name, genre] = titles[i % titles.length]
      const duration = (i % 3 === 0 ? 2 : 1) * slot
      out.push({
        programId: Math.floor(t / 1000) * 100 + (service.serviceId % 100),
        networkId: service.networkId,
        serviceId: service.serviceId,
        eventId: i + 1,
        startAt: new Date(t).toISOString(),
        endAt: new Date(t + duration).toISOString(),
        durationMs: duration,
        name: `${name}`,
        description:
          out.length % 5 === 0
            ? ''
            : '放送内容の説明です。広い画面でも先頭の一行だけを揃えて表示します。地は無彩 3 値、色は信号のみ。',
        genres: [genre],
        isFree: i % 5 !== 0,
      })
      t += duration
      i++
    }
  }
  return out
}

const nowMs = FIXED_NOW.getTime()
export const iso = (ms) => new Date(ms).toISOString()
/** #779 の多行トースト判定だけで使う専用番組。共有の `titles` 輪番には混ぜない。 */
export const toastLayoutProgram = {
  programId: 779001,
  networkId: 32736,
  serviceId: 1024,
  eventId: 779,
  startAt: iso(nowMs + 30 * 60_000),
  endAt: iso(nowMs + 60 * 60_000),
  durationMs: 30 * 60_000,
  name: 'トーストの複数行表示を確認するための非常に長い番組名です。狭い画面でも確実に折り返されます。',
  description: '多行トーストの縦中央揃えを実ブラウザで確認するための番組。',
  genres: [0],
  isFree: true,
}
const encodeQueue = { queued: 2, running: 1, awaitingReview: 1 }
const storageRoots = [
  {
    store: 'takamatsu',
    root: 'media',
    path: '/media',
    totalBytes: 1_000_000_000_000,
    usedBytes: 300_000_000_000,
    availableBytes: 700_000_000_000,
    observedAt: iso(nowMs),
  },
  {
    store: 'takamatsu',
    root: 'scratch',
    path: '/scratch',
    totalBytes: 500_000_000_000,
    usedBytes: 100_000_000_000,
    availableBytes: 400_000_000_000,
    observedAt: iso(nowMs),
  },
]

const reservations = [
  { id: 1, site: SITE, programId: 9001, source: 'rule', ruleId: 1, state: 'active', title: '連続テレビ小説', serviceName: 'NHKEテレ', channelType: 'GR', startAt: iso(nowMs + HOUR), durationMs: 900_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), series: null, skip: false },
  { id: 2, site: SITE, programId: 9002, source: 'manual', state: 'active', title: '大相撲中継', serviceName: 'NHK総合', channelType: 'GR', startAt: iso(nowMs + 2 * HOUR), durationMs: 5_400_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), series: null, skip: false },
  { id: 3, site: SITE, programId: 9003, source: 'rule', state: 'detached', title: 'クラシック音楽館', serviceName: 'ＮＨＫＢＳ', channelType: 'BS', startAt: iso(nowMs + 5 * HOUR), durationMs: 3_600_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), series: null, skip: false },
  { id: 4, site: SITE, programId: 9004, source: 'rule', state: 'orphaned', title: '日曜洋画劇場', serviceName: 'テレビ大阪', channelType: 'GR', startAt: iso(nowMs + 26 * HOUR), durationMs: 7_200_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), series: null, skip: false },
]

/** issue #686 の状態別レイアウト判定用。容量警告だけを増やし、他の条件は揃える。 */
const layoutCapacityReservations = Array.from({ length: 7 }, (_, i) => ({
  ...reservations[0],
  id: 100 + i,
  programId: 9100 + i,
  title: `容量判定予約${i + 1}`,
  startAt: iso(nowMs + (i + 1) * HOUR),
}))

const layoutStorageRoots = {
  normal: storageRoots,
  capacity: storageRoots.map((root) =>
    root.root === 'media'
      ? { ...root, usedBytes: 999_900_000_000, availableBytes: 100_000_000 }
      : root,
  ),
  stale: storageRoots.map((root) => ({ ...root, observedAt: iso(nowMs - 2 * HOUR) })),
}

/** 予約 2 の時間帯に重ねる。琥珀の警告バッジ・帯を必ず 1 つ出すため。 */
/**
 * nextHourBoundaryMs は与えられた時刻より後の直近の毎時 0 分（Asia/Tokyo）を返す。
 * 番組境界は :00 / :30 に落ちることが圧倒的に多く、不足区間の境界も同じ単位
 * （サーバー側の判定）なので、「ちょうど正時に始まる不足区間」フィクスチャを
 * ここで作る（issue #460 レビュー blocker。:34 起点の固定時刻だけでは
 * この最頻ケースを避けてしまっていた）。
 */
function nextHourBoundaryMs(ms) {
  const tokyoWallTime = new Date(ms + TOKYO_OFFSET_MS)
  tokyoWallTime.setUTCMinutes(0, 0, 0)
  if (tokyoWallTime.getTime() <= ms + TOKYO_OFFSET_MS) {
    tokyoWallTime.setUTCHours(tokyoWallTime.getUTCHours() + 1)
  }
  return tokyoWallTime.getTime() - TOKYO_OFFSET_MS
}

const overages = [
  { site: SITE, startAt: iso(nowMs + 2 * HOUR), endAt: iso(nowMs + 3 * HOUR), shortfall: 1, jammedTypes: ['BS'] },
  // 隣接するが重ならない 2 本目。`internal/capacity/capacity.go` の `Compute`
  // は同一サイト内の区間を重ねて返さない（`pages/programs.tsx` はグリッドを
  // 1 サイトに絞って渡す）ので、「同時刻に重なる 2 本」はサーバーが返せない
  // 状態 --- ここは「隣接する 2 本の見えるラベルが両方読める」ことだけを
  // 機械判定する（issue #460 レビュー should 1）。
  { site: SITE, startAt: iso(nowMs + 3 * HOUR), endAt: iso(nowMs + 3.5 * HOUR), shortfall: 1, jammedTypes: ['GR'] },
  // ちょうど正時に始まる 3 本目（issue #460 レビュー blocker）。ラベルが帯の
  // 上端にアンカーされるので、この区間だと時間軸の目盛り（例: 「05:00」）と
  // 同じ y に来る --- avoidTickRow が効いているかを design-appearance.mjs で機械判定する。
  // 高さは 1 時間（120px）あるので、押し下げてもラベルは自分の帯の内側に
  // 収まる（4 本目の対比: 9〜18 分の短い帯だと収まらない）。
  {
    site: SITE,
    startAt: iso(nextHourBoundaryMs(nowMs + 5 * HOUR)),
    endAt: iso(nextHourBoundaryMs(nowMs + 5 * HOUR) + HOUR),
    shortfall: 3,
    jammedTypes: ['CS'],
  },
  // 正時に始まる短い帯（10 分 = 9〜18 分の範囲）+ 直後に隣接する帯（issue #460
  // 再レビュー実測と同じ形: [03:00, 03:10) の CS と [03:10, 04:00) の GR）。
  // `avoidTickRow` は帯の高さを見ずに tickAvoidHeightPx（20px）押し下げるので、
  // 10 分帯（高さ 20px）だと押し下げた先が自分の帯の下端 = 直後の帯の上端と
  // 一致し、直後の帯のラベル（押し下げられない）と完全に重なっていた
  // （直す前の実装では `design-appearance.mjs` で `labelOverlaps` が発火する）。4 本目（CS）は
  // 見えるラベルを意図的に持たない（`expectedVisibleLabelTexts` 参照。
  // `capacity-band.tsx` の `CapacityBandLabel`）。
  {
    site: SITE,
    startAt: iso(nextHourBoundaryMs(nowMs + 7 * HOUR)),
    endAt: iso(nextHourBoundaryMs(nowMs + 7 * HOUR) + 10 * 60_000),
    shortfall: 1,
    jammedTypes: ['CS'],
  },
  {
    site: SITE,
    startAt: iso(nextHourBoundaryMs(nowMs + 7 * HOUR) + 10 * 60_000),
    endAt: iso(nextHourBoundaryMs(nowMs + 7 * HOUR) + HOUR),
    shortfall: 2,
    jammedTypes: ['GR'],
  },
]

/**
 * 見えるはずのラベルの文字（`shortageLabelCompact` の形。順不同で照合する）。
 *
 * **件数ではなく集合で照合する。** 件数だけだと、意図的に隠しているはずの
 * 10 分の CS 帯が（回帰で）描かれるようになり、同時に他のどれか 1 本のラベルが
 * （別の回帰で）消えても、合計件数は変わらず通ってしまう --- 件数は identity
 * を見ていない（issue #460 再々レビュー）。CS 帯（10 分、`overages` の 4 本目）
 * だけがここに含まれない。
 */
export const expectedVisibleLabelTexts = ['BS-1', 'GR-1', 'CS-3', 'GR-2']

// `keepOriginal` は「retention policy を変更できる」機能で応答スキーマが
// 必須化しており（zod に `.optional()`/`.default()` が無い）、これが無いと
// 各判定の `validateFixturesOrExit` が落ちる。issue #686 とは無関係の既存の穴
// （フィクスチャがそちらの必須化に追従していなかった）で、ここで揃える。
/**
 * シリーズ一覧（`/series`）のフィクスチャ。`/api/recording-shelves` はサーバーが
 * 件数降順で返すので同じ順に並べる。NULL の棚（値なし）は画面に出ない側の確認用、
 * 「NHK高校講座」は分類ルール（下）が勝つ棚＝「手動」の札が付く側。
 */
const seriesShelves = [
  { value: 'NHK高校講座', title: 'NHK高校講座　日本史　第1回', count: 120, playableCount: 100, unwatchedCount: 20, latestStartAt: '2026-08-10T10:00:00Z', representativeId: 11, thumbnailUrl: '/api/media/recordings/11/thumbnail' },
  { value: 'ドラマ', title: 'ドラマ　夜のさざなみ　第3話', count: 40, playableCount: 38, unwatchedCount: 5, latestStartAt: '2026-08-11T13:00:00Z', representativeId: 12, thumbnailUrl: '/api/media/recordings/12/thumbnail' },
  { value: '作品X', title: 'アニメ　作品X　第2話', count: 12, playableCount: 0, unwatchedCount: 0, latestStartAt: '2026-08-12T12:30:00Z', representativeId: 13, thumbnailUrl: '/api/media/recordings/13/thumbnail' },
  { value: '単発', title: '単発の特番', count: 1, playableCount: 1, unwatchedCount: 1, latestStartAt: '2026-08-09T09:00:00Z', representativeId: 14, thumbnailUrl: '/api/media/recordings/14/thumbnail' },
  { title: '【特集】', count: 2, playableCount: 0, unwatchedCount: 2, latestStartAt: '2026-08-12T13:00:00Z', representativeId: 15, thumbnailUrl: '/api/media/recordings/15/thumbnail' },
]
const seriesLabelRules = [
  { id: 1, key: 'series', keyword: '日本史', value: 'NHK高校講座 日本史', priority: 5, valueKey: 'NHK高校講座', createdAt: '2026-08-01T00:00:00Z', updatedAt: '2026-08-01T00:00:00Z' },
  // 番組ハブ側: 起点の録画 12（series '音楽館'）が手動棚になる。
  { id: 11, key: 'series', value: '音楽館', valueKey: '音楽館', keyword: '音楽館', priority: 10, createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
]

export const recordings = [
  { id: 11, site: SITE, source: 'rule', serviceName: 'NHK総合', channelType: 'GR', channel: '27', networkId: 32736, serviceId: 1024, eventId: 11, title: 'ニュース７', startAt: iso(nowMs - 600_000), durationMs: 1_800_000, status: 'recording', keepOriginal: 'always', media: recordingMedia(11, []), hlsPlayback: [], cmDetection: { state: 'disabled' }, createdAt: iso(nowMs - 600_000), startedAt: iso(nowMs - 600_000) },
  // encodedAssets を持たせて詳細ページ（/recordings/$id）で <video> が実ブラウザで
  // 出ることを撮る（キーボード到達性の判定 ⑤）。`encodedProfiles`（非推奨の後方
  // 互換フィールド）だけでは `RecordingPlayer` が <video> を出さない
  // （`encodedAssets` を見るため）ので両方持たせる。
  { id: 12, site: SITE, source: 'manual', serviceName: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', networkId: 4, serviceId: 101, eventId: 12, title: 'クラシック音楽館', description: '番組の内容を補う説明です。画面幅が 360px のときも本文を読みやすい文字サイズで折り返し、詳細欄が横にはみ出さないことを確認するための文章です。', series: '音楽館', seriesKey: 'クラシック音楽館', startAt: iso(nowMs - 26 * HOUR), durationMs: 5_400_000, status: 'finished', keepOriginal: 'always', media: recordingMedia(12, ['hevc-1080p']), hlsPlayback: [], cmDetection: { state: 'disabled' }, sizeBytes: 8_123_456_789, createdAt: iso(nowMs - 26 * HOUR), dropSummary: { packets: 1_500_000, drops: 12, errors: 0, scrambled: 3 }, encodedAssets: [{ profile: 'hevc-1080p', sizeBytes: 2_345_678_901 }] },
  { id: 13, site: SITE, source: 'rule', serviceName: 'テレビ大阪', channelType: 'GR', channel: '18', networkId: 32738, serviceId: 1040, eventId: 13, title: 'アニメ劇場', startAt: iso(nowMs - 50 * HOUR), durationMs: 1_800_000, status: 'failed', keepOriginal: 'always', media: recordingMedia(13, []), hlsPlayback: [], cmDetection: { state: 'disabled' }, createdAt: iso(nowMs - 50 * HOUR) },
  // 原本・エンコード資産のない録画。結論の「準備中」バッジのコントラスト測定に使う。
  { id: 14, site: SITE, source: 'rule', serviceName: 'NHKEテレ', channelType: 'GR', channel: '26', networkId: 32737, serviceId: 1032, eventId: 14, title: '連続テレビ小説', startAt: iso(nowMs - 74 * HOUR), durationMs: 900_000, status: 'finished', keepOriginal: 'always', media: recordingMedia(14, []), hlsPlayback: [], cmDetection: { state: 'disabled' }, sizeBytes: undefined, ingest: { state: 'pending' }, createdAt: iso(nowMs - 74 * HOUR) },
]

// `homeModeFixture` の警告一覧は、失敗録画・orphaned 予約・容量超過・ドロップの順。
// それぞれ recordings[2] / reservations[3] / overages[0] / recordings[1] の fixture に対応する。
export const expectedHomeWarningKinds = ['failed', 'not-recorded', 'overage', 'drop']

/** ホーム「見る」側の帯と「次に見る 1 本」専用の再開位置フィクスチャ。 */
const homeContinueWatching = [
  {
    ...recordings[1],
    id: 21,
    eventId: 21,
    title: '葬送のフリーレン 第3話「人を殺す魔法」',
    serviceName: '日テレ',
    startAt: '2026-09-26T14:30:00.000Z',
    durationMs: 24 * 60_000,
    resumePositionMs: 14 * 60_000 + 40_000,
    dropSummary: undefined,
  },
  ...[
    '孤独のグルメ',
    'ドキュメント72時間',
    'アメトーーク!',
    'カンブリア宮殿',
    'サイエンスZERO',
  ].map((title, i) => ({
    ...recordings[1],
    id: 22 + i,
    eventId: 22 + i,
    title,
    startAt: iso(nowMs - (i + 1) * HOUR),
    resumePositionMs: (i + 1) * 60_000 + 10_000,
    watchedAt: undefined,
    dropSummary: undefined,
  })),
]

// 2560px の「ほかの新着」で 1 行に並べられる枚数が 6 を超えることを確認する専用材料。
const homeWideContinueWatching = [
  ...homeContinueWatching,
  ...Array.from({ length: 8 }, (_, i) => ({
    ...homeContinueWatching[1],
    id: 40 + i,
    eventId: 40 + i,
    title: `広幅テスト番組 ${i + 1}`,
    startAt: iso(nowMs - (8 + i) * HOUR),
  })),
]

const recordingDetailScenarios = {
  completed: {
    ...recordings[1],
    startedAt: iso(nowMs - 26 * HOUR),
    endedAt: iso(nowMs - 24.5 * HOUR),
    hlsPlayback: [], cmDetection: { state: 'detected', ranges: [{ startMs: 60_000, endMs: 90_000 }] },
  },
  'short-series': {
    ...recordings[1],
    series: '天気',
    startedAt: iso(nowMs - 26 * HOUR),
    endedAt: iso(nowMs - 24.5 * HOUR),
    hlsPlayback: [], cmDetection: { state: 'detected', ranges: [{ startMs: 60_000, endMs: 90_000 }] },
  },
  recording: {
    ...recordings[1],
    status: 'recording',
    startAt: iso(nowMs - 45 * 60_000),
    durationMs: 3_600_000,
    sizeBytes: undefined,
    encodedAssets: [],
    encodeProfiles: [],
    encodeStatus: [],
    startedAt: iso(nowMs - 45 * 60_000),
    endedAt: undefined,
    ingest: { state: 'transferring', writtenBytes: 900_000_000, expectedBytes: 1_200_000_000, observedAt: iso(nowMs - 2_000) },
    hlsPlayback: [], cmDetection: { state: 'detecting' },
  },
  'encode-waiting': {
    ...recordings[1],
    sizeBytes: undefined,
    encodedAssets: [],
    encodeProfiles: ['hevc-1080p'],
    encodeStatus: [{ profile: 'hevc-1080p', state: 'running' }],
    hlsPlayback: [], cmDetection: { state: 'detected', ranges: [{ startMs: 60_000, endMs: 90_000 }] },
  },
  trash: {
    ...recordings[1],
    deletedAt: iso(nowMs - 30 * 60_000),
    encodedAssets: [],
    hlsPlayback: [], cmDetection: { state: 'disabled' },
  },
}

const recordingDetailChapters = {
  version: 'fixture-v1',
  detectionPending: false,
  source: 'user',
  spans: [{ startMs: 60_000, endMs: 90_000, label: 'CM', cut: true }],
}

// 番組ハブは録画一覧と同じ fixture から切り出し、`seriesOf` と `order` を実際に
// 反映する。最新の録画中/失敗行と、再生できる最新話を同時に置く。
const seriesHubRecordings = [
  recordings[1],
  {
    ...recordings[1],
    id: 16,
    eventId: 16,
    title: 'クラシック音楽館 第2回',
    startAt: iso(nowMs - 2 * HOUR),
  },
  {
    ...recordings[2],
    id: 17,
    eventId: 17,
    title: 'クラシック音楽館 第3回',
    series: '音楽館',
    seriesKey: 'クラシック音楽館',
    startAt: iso(nowMs - HOUR),
    networkId: 4,
    serviceId: 101,
    serviceName: 'ＮＨＫＢＳ',
    channelType: 'BS',
    channel: 'BS15_0',
  },
]
const seriesHubUpcoming = [
  {
    site: SITE,
    programId: 9016,
    networkId: 4,
    serviceId: 101,
    startAt: iso(nowMs + HOUR),
    durationMs: 1_800_000,
    name: 'クラシック音楽館 第4回',
    isFree: true,
  },
]

/**
 * transferringRecording は site タグ（`showSite`）と `IngestBadge` の
 * 合成後コントラストを測るための専用フィクスチャ（issue #308 のレビューで
 * 判明した穴。`recordings` に site が 1 つしか無いので `showSite` が常に偽、
 * どの録画にも `ingest` フィールドが無いので `IngestBadge` が一度も描画され
 * ない）。`multiSite` + `extraRecording` オプション付きのときだけ一覧に混ぜる
 * ので、既定の全画面ショット/判定には影響しない。
 */
const transferringRecording = {
  id: 15,
  site: SITE2,
  source: 'manual',
  serviceName: 'テレビ神奈川',
  channelType: 'GR',
  channel: '13',
  networkId: 32739,
  serviceId: 1048,
  eventId: 15,
  title: 'ローカル番組',
  startAt: iso(nowMs - 10 * HOUR),
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always', media: recordingMedia(15, []), hlsPlayback: [], cmDetection: { state: 'disabled' },
  createdAt: iso(nowMs - 10 * HOUR),
  ingest: {
    state: 'transferring',
    writtenBytes: 600_000_000,
    expectedBytes: 1_000_000_000,
    observedAt: iso(nowMs - 5_000),
  },
}

const rules = [
  { id: 1, name: '朝ドラ', enabled: true, priority: 10, keepOriginal: 'always', cmDetection: { state: 'disabled' }, textMatches: [{ target: 'name', mode: 'keyword', value: '連続テレビ小説' }], createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
  { id: 2, name: '（条件なし）', enabled: false, priority: 20, keepOriginal: 'until_encoded', createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
]

const breakers = [
  { site: SITE, store: '', name: 'ruler_deletes', trippedAt: iso(nowMs - 3 * HOUR), pending: 42, threshold: 20, detail: { total: 42, programs: [{ programId: 9101, title: '大相撲中継' }, { programId: 9102, title: 'ブラタモリ' }] } },
]

// --- 契約検証: フィクスチャが orval 生成の zod スキーマと一致するか ---
//
// 「唯一の視覚オラクル」であるデザイン判定のフィクスチャが API 契約から
// 遅れていても、これまでは誰も気付かなかった（issue #468。ルールの
// `textMatches` が旧形 `{ field, kind }` のまま `target/mode` に追従して
// おらず、ルール一覧に「undefinedに…を含む」が描かれたまま exit 0 していた）。
// 判定本体は `validateFixturesOrExit`（e2e/lib.mjs）--- `verifyBundleMatchesOrExit`
// と同じ前提条件チェックなので、badge-links.mjs 等の兄弟スクリプトとも共有する。
export const fixturePairs = [
  ...services.map((s, i) => [`services[${i}]`, ListServicesResponseItem, s]),
  ...wideLiveServices.slice(services.length).map((s, i) => [
    `wideLiveServices[${i}]`,
    ListServicesResponseItem,
    s,
  ]),
  ...programsFor(iso(nowMs), iso(nowMs + 6 * HOUR)).map((p, i) => [`programs[${i}]`, ListProgramsResponseItem, p]),
  ['toastLayoutProgram', ListProgramsResponseItem, toastLayoutProgram],
  ...reservations.map((r, i) => [`reservations[${i}]`, ListReservationsResponseItem, r]),
  // transferringRecording も既定オプション（multiSite + extraRecording）で
  // 実際にブラウザへ配る（apiHandler 参照）ので検証対象に含める。
  ...[...recordings, transferringRecording].map((r) => [`recordings#${r.id}`, ListRecordingsResponseItem, r]),
  ...homeContinueWatching.map((r, i) => [
    `homeContinueWatching[${i}]`,
    ListContinueWatchingResponseItem,
    r,
  ]),
  ...homeWideContinueWatching.slice(homeContinueWatching.length).map((r, i) => [
    `homeWideContinueWatching[${i}]`,
    ListContinueWatchingResponseItem,
    r,
  ]),
  ...seriesHubRecordings.map((r) => [`seriesHubRecordings#${r.id}`, ListRecordingsResponseItem, r]),
  ...seriesHubUpcoming.map((p, i) => [`seriesHubUpcoming[${i}]`, ListRecordingUpcomingResponseItem, p]),
  ...rules.map((r, i) => [`rules[${i}]`, ListRulesResponseItem, r]),
  ...seriesShelves.map((r, i) => [`seriesShelves[${i}]`, ListRecordingShelvesResponseItem, r]),
  ...seriesLabelRules.map((r, i) => [`seriesLabelRules[${i}]`, ListLabelRulesResponseItem, r]),
  ...breakers.map((b, i) => [`breakers[${i}]`, ListCircuitBreakersResponseItem, b]),
  ['encodeQueue', GetEncodeQueueResponse, encodeQueue],
  ...storageRoots.map((root, i) => [`storage[${i}]`, GetStorageResponseItem, root]),
  ...layoutStorageRoots.capacity.map((root, i) => [`layoutCapacityStorage[${i}]`, GetStorageResponseItem, root]),
  ...layoutStorageRoots.stale.map((root, i) => [`layoutStaleStorage[${i}]`, GetStorageResponseItem, root]),
  ...layoutCapacityReservations.map((r, i) => [`layoutCapacityReservations[${i}]`, ListReservationsResponseItem, r]),
]

/**
 * installApiStubs は `/api/**` をすべてブラウザ側で差し替える。
 *
 * `withBreaker` でサーキットブレーカーのバナー（destructive の帯）を出し分ける。
 * バナーは全ページに居座る要素なので、既定では出さない --- 出したままだと
 * どのショットもバナー込みになり、ページ本体の地の判定に混ざる。
 *
 * `delayPath` / `delayMs` は「読み込み中」の走査線（`Skeleton` / `ListSkeleton`。
 * components/page.tsx）を撮るための遅延フック。API が即座に返る作りなので、
 * 遅延を挟まないと画面遷移からスクリーンショットまでの間に必ず解決してしまい、
 * 読み込み中の状態を撮れない。
 *
 * `emptyHome` はホーム（M8-3）の「全セクションが空」を撮る/判定するための
 * フック。予約・容量超過・録画をすべて空にする（ブレーカーは元々 `withBreaker`
 * が制御している）。
 *
 * `multiSite` / `extraRecording` は録画一覧の site タグ（`showSite`）と
 * `IngestBadge` の合成後コントラストを測るための専用フック（issue #308）。
 * `showSite` は `/api/sites` が 2 件以上返すときだけ真になり、`IngestBadge` は
 * `ingest` フィールドを持つ録画がないと一度も描画されない --- 既定のフィク
 * スチャはどちらも満たさないので、これらを付けたときだけ `transferringRecording`
 * （2 つ目の site）を一覧に混ぜる。既定の全画面ショット/判定は影響を受けない。
 *
 * `layoutScenario` は issue #686 の到達距離判定専用で、正常・容量不足・古い観測・
 * ストレージ取得失敗・観測なし・エンコード待機列・警告多数を分ける。
 *
 * `toastLayout` は issue #779 の多行トースト判定専用で、共有の番組輪番を変えずに
 * 専用番組 1 件だけを返す。
 */
/**
 * apiHandler は デザイン判定の各シナリオに応じた `/api/**` の応答を作る
 * ハンドラを返す（`installApiStubs`（e2e/lib.mjs）の配線に渡す）。
 */
export function apiHandler({
  withBreaker = false,
  homeModeFixture = false,
  homeWideFixture = false,
  wideLiveFixture = false,
  homeOpsFixture = false,
  homeOpsSseFixture = null,
  delayPath = null,
  delayMs = 0,
  emptyHome = false,
  multiSite = false,
  extraRecording = false,
  layoutScenario = 'default',
  toastLayout = false,
  recordingDetailScenario = null,
  extraOpsRecordings = [],
} = {}) {
  return async ({ path: p, url, json, route }) => {
    if (delayPath !== null && p === delayPath) {
      await new Promise((r) => setTimeout(r, delayMs))
    }
    if (p === '/api/events' && homeOpsSseFixture !== null) {
      // Let the scenario establish a manual scroll and advance its clock before this
      // recording event invalidates live query data and rerenders HomePage.
      await homeOpsSseFixture()
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' },
        body: 'retry: 86400000\nevent: recordings\ndata: {"topic":"recordings"}\n\n',
      })
    }
    // SSE（/api/events）は明示のスタブを持たず catch-all（200 json []）に落ちる。
    if (p === '/api/sites') return json(multiSite ? [SITE, SITE2] : [SITE])
    // ライブへの導線（主ナビの「ライブ」・/live 画面）はサーバーの live.enabled に
    // 連動する（issue #209）。ここは「有効なデプロイ」の見た目を撮るための判定なので
    // true を返す --- 返さないと主ナビが 5 項目になり、/live はチャンネル一覧ではなく
    // 「無効です」の空状態になる
    if (p === '/api/capabilities') return json({ live: true, cmDetect: false })
    if (p === '/api/breakers') {
      return json(withBreaker || layoutScenario === 'many-warnings' ? breakers : [])
    }
    if (p === '/api/encode-queue') {
      // `no-observation` はストレージ観測（media root）だけでなくエンコード待機列の
      // 取得も失敗する「両方欠損」ケース専用に使う --- 管理情報の帯そのものが
      // 描かれないこと（recordings.tsx の空の帯抑制）はこの組み合わせでしか
      // 機械判定できない（片方でも生きていれば帯は残る）。
      if (layoutScenario === 'no-observation') {
        return route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: '{"error":"encode queue unavailable"}',
        })
      }
      const layoutQueue = ['normal', 'capacity', 'stale', 'storage-failure'].includes(layoutScenario)
        ? { queued: 0, running: 0, awaitingReview: 0 }
        : encodeQueue
      return json(layoutQueue)
    }
    if (p === '/api/storage') {
      if (emptyHome) return json([])
      if (layoutScenario === 'storage-failure') {
        return route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"storage unavailable"}' })
      }
      if (layoutScenario === 'no-observation') return json([])
      if (layoutScenario === 'capacity') return json(layoutStorageRoots.capacity)
      if (layoutScenario === 'stale' || layoutScenario === 'many-warnings') return json(layoutStorageRoots.stale)
      return json(layoutStorageRoots.normal)
    }
    if (p === '/api/encode-profiles') return json([{ name: 'hevc-1080p', container: 'mp4' }])
    if (p === '/api/rules') return json(rules)
    if (p === '/api/recording-shelves') return json(seriesShelves)
    if (p === '/api/label-rules') return json(seriesLabelRules)
    if (p === '/api/reservations') {
      if (emptyHome) return json([])
      if (homeOpsFixture) {
        return json(multiSite
          ? [...reservations, {
              ...reservations[0],
              id: 5,
              site: SITE2,
              programId: 9005,
              title: '別サイトの予約',
              startAt: iso(nowMs + 2 * HOUR),
            }]
          : reservations)
      }
      return json(layoutScenario === 'capacity' ? layoutCapacityReservations : reservations)
    }
    if (p === '/api/capacity/overages') {
      // #1020 のモード比較ショットは mock の警告 4 種（orphaned 予約・容量超過・ドロップ・失敗）
      // を再現する。ほかの既存シナリオでは従来の全超過 fixture を使う。
      if (emptyHome) return json([])
      if (homeOpsFixture) {
        return json([{
          ...overages[1],
          // One site-wide overage applies to every jammed type row. It must not
          // select or identify an individual reservation as the loser.
          jammedTypes: ['GR', 'BS'],
        }])
      }
      return json(homeModeFixture ? overages.slice(0, 1) : overages)
    }
    if (p === '/api/recordings/continue-watching') {
      return json(
        emptyHome
          ? []
          : homeWideFixture
            ? homeWideContinueWatching
            : homeContinueWatching,
      )
    }
    if (p === '/api/recordings') {
      const seriesOf = url.searchParams.get('seriesOf')
      if (seriesOf !== null) {
        const ascending = url.searchParams.get('order') === 'asc'
        const sorted = [...seriesHubRecordings].sort((a, b) => {
          const diff = Date.parse(a.startAt) - Date.parse(b.startAt)
          return ascending ? diff : -diff
        })
        return json(sorted)
      }
      // ホームは status 別に timeline の limit=200 を付けた 3 本（完了・失敗は from/to 付き）を取得し、
      // これとは別に drop 20 件・failed 20 件の警告範囲を取得する。
      // homeOpsFixture では finished/failed を窓内へ動かし、ブロックと警告を両方撮る。
      // 既定の録画一覧（`pages/recordings.tsx`）は status を付けずに常に
      // limit=50 を送るので、ここでの絞り込みはそちらの見た目に影響しない。
      // 実サーバーの既定（program_start_at 降順）に合わせて並べ替えてから絞る。
      const opsTimelineRecordings = homeOpsFixture ? [
        { ...recordings[0], startAt: iso(nowMs - 10 * 60_000), startedAt: iso(nowMs - 10 * 60_000) },
        { ...recordings[1], startAt: iso(nowMs - 90 * 60_000), createdAt: iso(nowMs - 90 * 60_000) },
        { ...recordings[2], startAt: iso(nowMs - 40 * 60_000), createdAt: iso(nowMs - 40 * 60_000) },
        recordings[3],
        ...extraOpsRecordings,
      ] : recordings
      const source = emptyHome
        ? []
        : extraRecording
          ? [...recordings, transferringRecording]
          : opsTimelineRecordings
      const status = url.searchParams.get('status')
      const limit = Number(url.searchParams.get('limit') ?? source.length)
      const filtered = status ? source.filter((r) => r.status === status) : source
      const from = url.searchParams.get('from')
      const to = url.searchParams.get('to')
      // 実サーバーと同じく `from` / `to` は開始時刻の範囲で、付いたものだけで絞る。
      const inWindow = filtered.filter((r) =>
        (from === null || Date.parse(r.startAt) >= Date.parse(from)) &&
        (to === null || Date.parse(r.startAt) < Date.parse(to)))
      const sorted = [...inWindow].sort((a, b) => Date.parse(b.startAt) - Date.parse(a.startAt))
      return json(sorted.slice(0, limit))
    }
    const upcomingMatch = /^\/api\/recordings\/(\d+)\/upcoming$/.exec(p)
    if (upcomingMatch) return json(Number(upcomingMatch[1]) === 12 ? seriesHubUpcoming : [])
    // 録画単体（`/recordings/$id`、issue #232）。キーボード到達性の判定（⑤）が
    // 詳細ページの `<video>` を見るために引く。ごみ箱の録画は無いのでここでは
    // 常に 200（一覧のフィクスチャから引く）。
    const recMatch = /^\/api\/recordings\/(\d+)$/.exec(p)
    if (recMatch && route.request().method() === 'GET') {
      const id = Number(recMatch[1])
      const rec =
        id === 12 && recordingDetailScenario !== null
          ? recordingDetailScenarios[recordingDetailScenario]
          : recordings.find((r) => r.id === id)
      return rec ? json(rec) : route.fulfill({ status: 404 })
    }
    const chaptersMatch = /^\/api\/recordings\/(\d+)\/chapters$/.exec(p)
    if (chaptersMatch) {
      return json(Number(chaptersMatch[1]) === 12 ? recordingDetailChapters : {
        version: 'fixture-empty',
        detectionPending: false,
        source: 'auto',
        spans: [],
      })
    }
    const dropStatsMatch = /^\/api\/recordings\/(\d+)\/drop-stats$/.exec(p)
    if (dropStatsMatch) {
      return json(
        Number(dropStatsMatch[1]) === 12
          ? [
              {
                pid: 256,
                pidType: 'video',
                packets: 1_500_000,
                drops: 12,
                errors: 0,
                scrambled: 3,
                positions: [{ byteOffset: 123_456, elapsedMs: 42_000 }],
              },
            ]
          : [],
      )
    }
    // サムネイルは 404 に落として実装側のプレースホルダを撮る（画像を作らない）
    if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(p)) return route.fulfill({ status: 404 })
    if (p === `/api/sites/${SITE}/services`) return json(wideLiveFixture ? wideLiveServices : services)
    if (p === `/api/sites/${SITE}/programs`) {
      const startISO = url.searchParams.get('start') ?? iso(nowMs)
      const endISO = url.searchParams.get('end') ?? iso(nowMs + 6 * HOUR)
      const requestedServiceIds = url.searchParams.getAll('serviceId')
      if (
        toastLayout &&
        Date.parse(toastLayoutProgram.startAt) < Date.parse(endISO) &&
        Date.parse(toastLayoutProgram.endAt) > Date.parse(startISO)
      ) {
        return json([toastLayoutProgram])
      }
      return json(
        programsFor(startISO, endISO, requestedServiceIds),
      )
    }
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
    if (/\/intent$/.test(p)) return route.fulfill({ status: 204 })
    if (/\/reservation$/.test(p)) return route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' })
    return json([])
  }
}

