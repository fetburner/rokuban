// デザイン（色）の受け入れ判定とスクリーンショット。**色は jsdom で測れない**ので、
// ここが視覚的契約の唯一のオラクルになる（e2e/README.md、docs/frontend/design.md）。
//
// やること:
//   ① 主要画面 × ライト / ダーク × デスクトップ / モバイルを撮って
//      e2e/screenshots/ に置く（人が見るための成果物。追跡しない）
//   ② 撮ったページの上で色を実測して合否を出す（機械判定）。**この PR が変えた
//      状態色ぜんぶ**を覆う --- 1 箇所でも判定の外に置くと、そこだけ既定値へ
//      静かに戻っても全部緑のまま通る:
//      - 面（body / ヘッダ / ナビ / 一覧の行）の地が無彩か
//      - サイドバーの現在地の塗りと、2px オフセットしたフォーカスリングがページ地と 3:1 以上か
//      - 共通 Button の focus-visible リングが実画素で --scanline と一致し、ページ地と 3:1 以上か
//      - 録画中バッジがタリーレッドの「塗り」か / 失敗バッジが destructive の淡い地か
//      - チューナー不足・ルールの「条件なし」が琥珀か
//      - 番組リストの時刻に信号色が付いて**いない**か
//      - 現在時刻の線と札がタリーレッドか / 容量超過の帯の罫線が琥珀か
//      - `bg-muted` 系の面（塗り / `/80` の sticky 見出し / `/50` の行 hover /
//        `/30` の詳細パネル）に乗る文字
//      - 上記すべての WCAG コントラスト（文字 4.5 / 面と線 3）
//   ③ 和文が実際に Noto Sans JP で、英数字が実際に Geist で描画されているか
//      （CDP `CSS.getPlatformFontsForNode`）と、和文まじりの文字列でも
//      tabular-nums が実際に等幅を作っているか（DOM の実測幅）
//   ④ モバイルの「その他」ポップオーバー（固定されたボトムバーの上に浮く
//      オーバーレイなので、はみ出し・重なりは jsdom では原理的に測れない。
//      docs/frontend/shell.md）:
//      - ボトムタブが常に 4 個か
//      - 開いたポップオーバーがビューポート内に収まるか
//      - ポップオーバーがトリガーの上端より上に出るか（バーの下に隠れていないか）
//      - Tab / Shift+Tab がポップオーバー内を循環するか
//   ④-A キーボード操作と標的サイズ:
//      - Tab 1 回でスキップリンクが見え、Enter で main にフォーカスが移るか
//      - Chip / 録画タブ / チャンネル候補 / 日付セルの focus-visible リングが
//        Button と同じ --ring の実画素で出るか
//      - Button size="sm" と容量不足バッジの当たり判定が 24px 以上か
//      - 主要 6 画面の button / link / input / select 等を実測し、24px 未満を
//        ポインタ別に列挙して落とすか（標的間の実際の間隔も併記する）
//   ⑤ 録画一覧の行リンクを Enter で開いて詳細（/recordings/$id）へ遷移し、
//      詳細でキーボードの Tab だけで `<video>` に到達できるか（`tabIndex={-1}` を
//      付けると jsdom の focus spy は通り続けるが実ブラウザの Tab 走査から外れる）
//   ⑥ 共通 Button のフォーカスリング / border-color は遷移しない・hover の
//      色と active の押下フィードバックは遷移する（issue #294）
//   ⑦ アニメーション/トランジションが `prefers-reduced-motion: reduce` で
//      縮退し、既定（no-preference）では従来どおり動くこと（両方向。
//      issue #296）。Skeleton の `animate-pulse` / ポップオーバーの
//      `slide-in`・`zoom-in` / Button の `translate` 遷移を見る
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` でブラウザ側から
// 丸ごと差し替える（e2e/live.mjs が HLS でやっているのと同じ手）。サーバーには
// SPA（index.html + dist の資産）を返す仕事しか残らないので、
// `pnpm preview` でも rokuban 本体でもよい。
//
//   pnpm build && pnpm preview --port 4173 &
//   E2E_URL=http://localhost:4173 pnpm e2e:design
//
// 合格なら exit 0、1 つでも NG なら exit 1。判定の詳細は e2e/README.md §デザイン。
import { mkdirSync, rmSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
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
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const OUT_DIR =
  process.env.E2E_SHOT_DIR ??
  path.join(path.dirname(fileURLToPath(import.meta.url)), 'screenshots')

/**
 * 時刻を固定する。番組表・「いま」の線・録画の日時がショットごとに動くと、
 * 前回との差が「実装を変えたから」なのか「時間が経ったから」なのか分からなくなる。
 * `clock.setFixedTime` は Date だけを固定してタイマーは動かす（`pauseAt` と違い、
 * React Query や debounce を止めない）。
 */
const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')

const ng = []

// --- スタブ（API の応答） ---------------------------------------------------

const SITE = 'default'
// 2 サイト運用（`showSite`）の判定専用。`multiSite` オプション付きでしか
// `/api/sites` に出さない --- 既定の全画面ショット/判定を単一サイトのまま保つため。
const SITE2 = 'sub'
const HOUR = 3_600_000

const services = [
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
const iso = (ms) => new Date(ms).toISOString()
/** #779 の多行トースト判定だけで使う専用番組。共有の `titles` 輪番には混ぜない。 */
const toastLayoutProgram = {
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
const encodeQueue = { queued: 2, running: 1 }
const storageRoots = [
  {
    root: 'media',
    path: '/media',
    totalBytes: 1_000_000_000_000,
    usedBytes: 300_000_000_000,
    availableBytes: 700_000_000_000,
    observedAt: iso(nowMs),
  },
  {
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
 * nextHourBoundaryMs は与えられた時刻より後の直近の毎時 0 分（ローカル）を返す。
 * 番組境界は :00 / :30 に落ちることが圧倒的に多く、不足区間の境界も同じ単位
 * （サーバー側の判定）なので、「ちょうど正時に始まる不足区間」フィクスチャを
 * ここで作る（issue #460 レビュー blocker。:34 起点の固定時刻だけでは
 * この最頻ケースを避けてしまっていた）。
 */
function nextHourBoundaryMs(ms) {
  const d = new Date(ms)
  d.setMinutes(0, 0, 0)
  if (d.getTime() <= ms) d.setHours(d.getHours() + 1)
  return d.getTime()
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
  // 同じ y に来る --- avoidTickRow が効いているかをここで機械判定する。
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
  // （直す前の実装ではここで `labelOverlaps` が発火する）。4 本目（CS）は
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
const expectedVisibleLabelTexts = ['BS-1', 'GR-1', 'CS-3', 'GR-2']

// `keepOriginal` は「retention policy を変更できる」機能で応答スキーマが
// 必須化しており（zod に `.optional()`/`.default()` が無い）、これが無いと
// 下の `validateFixturesOrExit` が落ちる。issue #686 とは無関係の既存の穴
// （フィクスチャがそちらの必須化に追従していなかった）で、ここで揃える。
/**
 * シリーズ一覧（`/series`）のフィクスチャ。`/api/recording-shelves` はサーバーが
 * 件数降順で返すので同じ順に並べる。NULL の棚（値なし）は画面に出ない側の確認用、
 * 「NHK高校講座」は分類ルール（下）が勝つ棚＝「手動」の札が付く側。
 */
const seriesShelves = [
  { value: 'NHK高校講座', title: 'NHK高校講座　日本史　第1回', count: 120, playableCount: 100, unwatchedCount: 20, latestStartAt: '2026-08-10T10:00:00Z', representativeId: 11 },
  { value: 'ドラマ', title: 'ドラマ　夜のさざなみ　第3話', count: 40, playableCount: 38, unwatchedCount: 5, latestStartAt: '2026-08-11T13:00:00Z', representativeId: 12 },
  { value: '作品X', title: 'アニメ　作品X　第2話', count: 12, playableCount: 0, unwatchedCount: 0, latestStartAt: '2026-08-12T12:30:00Z', representativeId: 13 },
  { value: '単発', title: '単発の特番', count: 1, playableCount: 1, unwatchedCount: 1, latestStartAt: '2026-08-09T09:00:00Z', representativeId: 14 },
  { title: '【特集】', count: 2, playableCount: 0, unwatchedCount: 2, latestStartAt: '2026-08-12T13:00:00Z', representativeId: 15 },
]
const seriesLabelRules = [
  { id: 1, key: 'series', keyword: '日本史', value: 'NHK高校講座 日本史', priority: 5, valueKey: 'NHK高校講座', createdAt: '2026-08-01T00:00:00Z', updatedAt: '2026-08-01T00:00:00Z' },
  // 番組ハブ側: 起点の録画 12（series '音楽館'）が手動棚になる。
  { id: 11, key: 'series', value: '音楽館', valueKey: '音楽館', keyword: '音楽館', priority: 10, createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
]

const recordings = [
  { id: 11, site: SITE, source: 'rule', serviceName: 'NHK総合', channelType: 'GR', channel: '27', networkId: 32736, serviceId: 1024, eventId: 11, title: 'ニュース７', startAt: iso(nowMs - 600_000), durationMs: 1_800_000, status: 'recording', keepOriginal: 'always', cmDetection: { state: 'disabled' }, createdAt: iso(nowMs - 600_000), startedAt: iso(nowMs - 600_000) },
  // encodedAssets を持たせて詳細ページ（/recordings/$id）で <video> が実ブラウザで
  // 出ることを撮る（キーボード到達性の判定 ⑤）。`encodedProfiles`（非推奨の後方
  // 互換フィールド）だけでは `RecordingPlayer` が <video> を出さない
  // （`encodedAssets` を見るため）ので両方持たせる。
  { id: 12, site: SITE, source: 'manual', serviceName: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', networkId: 4, serviceId: 101, eventId: 12, title: 'クラシック音楽館', description: '番組の内容を補う説明です。画面幅が 360px のときも本文を読みやすい文字サイズで折り返し、詳細欄が横にはみ出さないことを確認するための文章です。', series: '音楽館', seriesKey: 'クラシック音楽館', startAt: iso(nowMs - 26 * HOUR), durationMs: 5_400_000, status: 'finished', keepOriginal: 'always', cmDetection: { state: 'disabled' }, sizeBytes: 8_123_456_789, createdAt: iso(nowMs - 26 * HOUR), dropSummary: { packets: 1_500_000, drops: 12, errors: 0, scrambled: 3 }, encodedAssets: [{ profile: 'hevc-1080p', sizeBytes: 2_345_678_901 }] },
  { id: 13, site: SITE, source: 'rule', serviceName: 'テレビ大阪', channelType: 'GR', channel: '18', networkId: 32738, serviceId: 1040, eventId: 13, title: 'アニメ劇場', startAt: iso(nowMs - 50 * HOUR), durationMs: 1_800_000, status: 'failed', keepOriginal: 'always', cmDetection: { state: 'disabled' }, createdAt: iso(nowMs - 50 * HOUR) },
  // 原本・エンコード資産のない録画。結論の「準備中」バッジのコントラスト測定に使う。
  { id: 14, site: SITE, source: 'rule', serviceName: 'NHKEテレ', channelType: 'GR', channel: '26', networkId: 32737, serviceId: 1032, eventId: 14, title: '連続テレビ小説', startAt: iso(nowMs - 74 * HOUR), durationMs: 900_000, status: 'finished', keepOriginal: 'always', cmDetection: { state: 'disabled' }, sizeBytes: undefined, ingest: { state: 'pending' }, createdAt: iso(nowMs - 74 * HOUR) },
]

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
    cmDetection: { state: 'detected', ranges: [{ startMs: 60_000, endMs: 90_000 }] },
  },
  'short-series': {
    ...recordings[1],
    series: '天気',
    startedAt: iso(nowMs - 26 * HOUR),
    endedAt: iso(nowMs - 24.5 * HOUR),
    cmDetection: { state: 'detected', ranges: [{ startMs: 60_000, endMs: 90_000 }] },
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
    cmDetection: { state: 'detecting' },
  },
  'encode-waiting': {
    ...recordings[1],
    sizeBytes: undefined,
    encodedAssets: [],
    encodeProfiles: ['hevc-1080p'],
    encodeStatus: [{ profile: 'hevc-1080p', state: 'running' }],
    cmDetection: { state: 'detected', ranges: [{ startMs: 60_000, endMs: 90_000 }] },
  },
  trash: {
    ...recordings[1],
    deletedAt: iso(nowMs - 30 * 60_000),
    encodedAssets: [],
    cmDetection: { state: 'disabled' },
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
  keepOriginal: 'always', cmDetection: { state: 'disabled' },
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
  { site: SITE, name: 'ruler_deletes', trippedAt: iso(nowMs - 3 * HOUR), pending: 42, threshold: 20, detail: { total: 42, programs: [{ programId: 9101, title: '大相撲中継' }, { programId: 9102, title: 'ブラタモリ' }] } },
]

// --- 契約検証: フィクスチャが orval 生成の zod スキーマと一致するか ---
//
// 「唯一の視覚オラクル」であるこのスクリプトのフィクスチャが API 契約から
// 遅れていても、これまでは誰も気付かなかった（issue #468。ルールの
// `textMatches` が旧形 `{ field, kind }` のまま `target/mode` に追従して
// おらず、ルール一覧に「undefinedに…を含む」が描かれたまま exit 0 していた）。
// 判定本体は `validateFixturesOrExit`（e2e/lib.mjs）--- `verifyBundleMatchesOrExit`
// と同じ前提条件チェックなので、badge-links.mjs 等の兄弟スクリプトとも共有する。
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
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
    // 実際にブラウザへ配る（:308 参照）ので検証対象に含める。
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
  ],
  ng,
)

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
 * apiHandler は design.mjs の各シナリオに応じた `/api/**` の応答を作る
 * ハンドラを返す（`installApiStubs`（e2e/lib.mjs）の配線に渡す）。
 */
function apiHandler({
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
        ? { queued: 0, running: 0 }
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
      // #1020 のモード比較ショットは mock の警告 3 件（容量超過・ドロップ・失敗）
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

// --- 色の読み取り -----------------------------------------------------------

/**
 * ページの中で色を sRGB のバイト列に落とすスクリプト。
 *
 * **`getComputedStyle()` の戻り値をそのまま正規表現で読んではいけない。**
 * トークンが oklch なので Chromium は計算値も `oklch(0.56 0.215 27)` のまま返す
 * （`rgb(...)` に落ちるという思い込みで書いた最初の版は、全部の判定が
 * 「色が読めない = null」で素通りした）。canvas の `fillStyle` も同じ文字列を
 * 返すので、1px 塗って `getImageData` で実際の画素を採る。
 */
const readColor = (el, input) => {
  const { prop, pseudo = null } = typeof input === 'string' ? { prop: input } : input
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  const toRgba = (v) => {
    ctx.clearRect(0, 0, 1, 1)
    ctx.fillStyle = v
    ctx.fillRect(0, 0, 1, 1)
    const d = ctx.getImageData(0, 0, 1, 1).data
    return [d[0], d[1], d[2], d[3]]
  }

  // backdrop: この要素の**文字や罫線が実際に乗っている面**。自分の背景から
  // 祖先へ遡り、不透明な面に当たるまで重ねて合成する。
  //
  // **要素自身の background-color だけを見てはいけない。** 淡い地を持つのが
  // 外側のバッジで、文字を持つのが内側の span、という組み方は普通にある
  // （容量超過バッジがそう）。内側だけ見ると背景は透明なので、合成が恒等関数に
  // なり「地の上での比」を測ってしまう --- まさにこの判定が防ぐはずだった誤り。
  const layers = []
  let reachedOpaque = false
  for (let node = el; node; node = node.parentElement) {
    const c = toRgba(getComputedStyle(node).backgroundColor)
    if (c[3] === 0) continue
    layers.push(c)
    if (c[3] >= 255) {
      reachedOpaque = true
      break
    }
  }
  // 不透明な面に到達できなければ白を仮定するしかないが、それは**測定ではなく
  // 捏造**。`reachedOpaque` を返して呼び出し側で落とす（遡りが 1 段で止まる
  // regression は、ライトでは白 ≒ 紙白なので比がほとんど変わらず素通りする）
  let backdrop = [255, 255, 255, 255]
  for (let i = layers.length - 1; i >= 0; i--) {
    const a = layers[i][3] / 255
    backdrop = [
      layers[i][0] * a + backdrop[0] * (1 - a),
      layers[i][1] * a + backdrop[1] * (1 - a),
      layers[i][2] * a + backdrop[2] * (1 - a),
      255,
    ]
  }

  const value = getComputedStyle(el, pseudo).getPropertyValue(prop)
  return { value, rgba: toRgba(value), backdrop, reachedOpaque }
}

const chroma = ([r, g, b]) => Math.max(r, g, b) - Math.min(r, g, b)
/**
 * oklchChroma は `getComputedStyle()` が返す `oklch(L C H)` 文字列から C を取り出す。
 *
 * **中間の明度では `chroma()`（RGB のチャンネル差）が過大に出る。** oklch の
 * 色域は明度が両端（白 / 黒）に寄るほど圧縮されるので、同じ oklch chroma でも
 * 中間の明度（`--tone-400` など）は白・墨に近い明度（`--paper` / `--sumi`）より
 * 大きい RGB チャンネル差になる。3 値の無彩性は design-tokens.test.ts と同じ
 * 基準（oklch chroma <= 0.02）で測る --- RGB 側の閾値を緩めると、こちらの
 * 都合で 3 値本体の判定基準まで緩んでしまう
 */
function oklchChroma(value) {
  // L は `0.705` のような比率でも `98.5%` のようなパーセントでも来る
  // （`--scan-lit` はカスタムプロパティ経由で読むため、標準プロパティより
  // 表記ゆれが出やすい）。どちらも通す
  const m = /oklch\(\s*[\d.]+%?\s+([\d.]+)/.exec(value ?? '')
  return m === null ? null : Number(m[1])
}
/** 赤が支配的か（タリー / destructive の判定）。 */
const isRed = ([r, g, b]) => r > 100 && r - g > 60 && r - b > 60 && Math.abs(g - b) < 60
/** 琥珀か（赤 > 緑 > 青 の順に落ちる暖色）。 */
const isAmber = ([r, g, b]) => r > g && g > b && r - b > 60 && g - b > 20

/** relLum は sRGB バイト列の相対輝度（WCAG 2.x）。 */
function relLum([r, g, b]) {
  const f = (v) => {
    const c = v / 255
    return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)
  }
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
}

/**
 * contrast は 2 色の WCAG コントラスト比。
 *
 * **前景に半透明が来たら合成してから測る。** 背景側は `readColor` の
 * `backdrop`（祖先まで遡って合成した実効面）を渡すこと。要素自身の
 * `background-color` を渡すと、淡い地が親にある構造で恒等になる。
 */
function contrast(fg, bg) {
  const alpha = fg[3] / 255
  const composited = alpha >= 1 ? fg : fg.slice(0, 3).map((c, i) => c * alpha + bg[i] * (1 - alpha))
  const [hi, lo] = [relLum(composited), relLum(bg)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

/** computedOf は指定要素の指定プロパティを `{ value, rgba }` で返す（無ければ null）。 */
async function computedOf(locator, prop) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readColor, prop)
}

/** computedPseudoOf は指定要素の疑似要素から色を読む（無ければ null）。 */
async function computedPseudoOf(locator, prop, pseudo) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readColor, { prop, pseudo })
}

/**
 * readCustomColor はカスタムプロパティ（`--scan-gap` / `--scan-lit`）の計算値を
 * 読む。`readColor`（標準プロパティ用）と違って祖先を遡る backdrop 合成はしない
 * --- 縞の 2 色それぞれの値そのものを見るためのもので、透過は想定しない。
 *
 * **これが無いと `background-image` に直接書いた縞の色は一切読めない。**
 * `getComputedStyle().backgroundColor` は `background-color`（= 縞の片側）
 * しか返さないので、もう片側（グラデーションの中の色）を測る手段が無いと
 * 「輝線を文字と同じ色にする」変異が判定をすり抜ける（design.md の失敗事例
 * 「判定を足したことと、それが効いていることは別」と同じ形の穴になる）。
 * `index.css` の `.scanlines` / `.tally-scanlines` は縞の両方の色を
 * `--scan-gap` / `--scan-lit` というカスタムプロパティに出し、
 * `background-color` と `background-image` の両方がそれを `var()` で参照する
 * ようにしてあるので、ここから両方読める
 */
const readCustomColor = (el, varName) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  const value = getComputedStyle(el).getPropertyValue(varName).trim()
  ctx.clearRect(0, 0, 1, 1)
  ctx.fillStyle = value
  ctx.fillRect(0, 0, 1, 1)
  const d = ctx.getImageData(0, 0, 1, 1).data
  return { value, rgba: [d[0], d[1], d[2], d[3]] }
}

/** computedVar は指定要素のカスタムプロパティを `{ value, rgba }` で返す（無ければ null）。 */
async function computedVar(locator, varName) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readCustomColor, varName)
}

/** readScreenshotPixels は Chromium が描いた PNG から指定座標の実画素を読む。 */
async function readScreenshotPixels(page, png, points) {
  return page.evaluate(
    async ({ base64, points }) => {
      const image = new Image()
      image.src = `data:image/png;base64,${base64}`
      await image.decode()
      const canvas = document.createElement('canvas')
      canvas.width = image.width
      canvas.height = image.height
      const context = canvas.getContext('2d', { willReadFrequently: true })
      if (context === null) throw new Error('2d canvas context is unavailable')
      context.drawImage(image, 0, 0)
      const pixels = context.getImageData(0, 0, canvas.width, canvas.height).data
      return points.map(([x, y]) => {
        const px = Math.floor(x)
        const py = Math.floor(y)
        if (px < 0 || py < 0 || px >= canvas.width || py >= canvas.height) {
          throw new Error(`screenshot point is outside the image: (${px}, ${py})`)
        }
        const start = (py * canvas.width + px) * 4
        return Array.from(pixels.slice(start, start + 3))
      })
    },
    { base64: png.toString('base64'), points },
  )
}

// --- 実行 -------------------------------------------------------------------

/** 撮る画面。`wait` はその画面で描画完了と見なせる目印。 */
const screens = [
  // ホームの全画面ショットは管理 mode を明示。既定「見る」は専用判定で `/` のまま確認する。
  { name: 'home', path: '/?mode=ops', wait: 'text=明日の終わり' },
  { name: 'programs', path: '/programs', wait: 'li[data-program-id], [data-testid="program-grid-now-line"]' },
  { name: 'reservations', path: '/reservations', wait: 'text=容量不足' },
  { name: 'recordings', path: '/recordings', wait: 'text=録画中' },
  { name: 'rules', path: '/rules', wait: 'text=朝ドラ' },
  { name: 'series', path: '/series', wait: 'text=作品X' },
  // 検索は初期状態で結果を持たないので、常時表示のフォーム見出しを目印にする
  // （詳細条件は初期状態で折りたたまれており、「チャンネル」は待機目印にならない）
  { name: 'search', path: '/search', wait: 'text=テキスト条件' },
  { name: 'live', path: '/live', wait: 'text=NHK総合' },
  { name: 'series-hub', path: '/recordings/12/series', wait: 'text=エピソード' },
]

/**
 * recordingDetailScreen は録画単体ページ（`/recordings/$id`）。`screens` には
 * 足さない --- 足すと① の全画面ショットが 1 画面ぶん増える。判定に必要なのは
 * path と目印だけなので、この 1 つを複数箇所（②の色判定・withBreaker の
 * ショット）から共有する。
 */
const recordingDetailScreen = { name: 'recording-detail', path: '/recordings/12', wait: '[data-testid="recording-detail-tabs"]' }

const viewports = [
  // 一覧の行長上限は広幅で初めて効くので、デスクトップショットは 2560px で撮る。
  { name: 'desktop', width: 2560, height: 1440 },
  { name: 'mobile', width: 360, height: 844 },
]

const themes = ['light', 'dark']

/** screenOf は名前で `screens` を引く（並び順を変えても判定がずれないように）。 */
function screenOf(name) {
  const found = screens.find((s) => s.name === name)
  if (!found) throw new Error(`画面 ${name} が screens に無い`)
  return found
}

/** desktop は「デスクトップでしか出ない要素」を撮る／判定するときの viewport。 */
const desktop = viewports[0]
/** mobile は「モバイルでしか出ない要素」を撮る／判定するときの viewport。 */
const mobile = viewports[1]
/**
 * mobileWide は issue #686 の到達距離判定専用（iPhone 標準幅 390px）。
 * 全画面スクリーンショットのループ（① / ②）には加えない --- 360px との
 * フルショット差分の価値が低く、7 画面 × 2 テーマの 2 周を 1.5 倍に増やす
 * だけになる（レビュー指摘）。
 */
const mobileWide = { name: 'mobile-wide', width: 390, height: 844 }
/** issue #1020 の主役幅（本文幅とのサムネイル比を測る）。 */
const homeDesktop = { name: 'home-desktop', width: 1280, height: 800 }
/** 番組ハブ配置判定専用。受け入れ条件の 400px 幅をそのまま測る。 */
const seriesHubMobile = { name: 'series-hub-400', width: 400, height: 844 }
/** 同じ判定をデスクトップ幅でも回す。 */
const seriesHubDesktop = { ...desktop, name: 'series-hub-desktop' }

const INTERACTIVE_TARGET_SELECTOR =
  'button, a[href], [role="button"], [role="switch"], input, select, summary'
const INTERACTIVE_TARGET_MIN_PX = 24
const targetScreenNames = ['programs', 'search', 'reservations', 'recordings', 'series', 'rules', 'live', 'series-hub']
const targetScreens = [...targetScreenNames.map(screenOf), recordingDetailScreen]
const targetPointerProfiles = [
  { name: 'fine', pointer: 'fine', viewport: desktop },
  { name: 'coarse', pointer: 'coarse', viewport: mobile },
]
const TAP_TARGET_SELECTOR =
  'a[href], button, [role="button"], [role="tab"], [role="menuitem"], [role="menuitemcheckbox"], [role="menuitemradio"], [role="option"], [role="switch"], [role="checkbox"], input:not([type="hidden"]), select, summary'
const TAP_TARGET_MIN_PX = 44
const tapTargetWidths = [
  { name: 'mobile-360', width: 360, height: 844 },
  { name: 'mobile-390', width: 390, height: 844 },
]
const homeWatchScreen = {
  name: 'home-watch',
  path: '/?mode=watch',
  wait: '[data-testid="home-primary-action"]',
}
const tapTargetScreens = [
  homeWatchScreen,
  screenOf('home'),
  screenOf('programs'),
  screenOf('reservations'),
  screenOf('recordings'),
  recordingDetailScreen,
  screenOf('rules'),
  screenOf('series'),
  screenOf('series-hub'),
  screenOf('search'),
  screenOf('live'),
]

rmSync(OUT_DIR, { recursive: true, force: true })
mkdirSync(OUT_DIR, { recursive: true })

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
let checkedColorSchemeChange = false

/** open は 1 ページを開いてスタブ・時刻・テーマを整えるところまでやる。 */
async function open(viewport, theme, screen, opts = {}) {
  const { pointer = 'fine', recordingView = null, reservationGrouping = null, isMobile = pointer === 'coarse', ...apiOpts } = opts
  if (pointer !== 'fine' && pointer !== 'coarse') {
    throw new Error(`未対応のポインタプロファイル: ${pointer}`)
  }
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: theme,
    deviceScaleFactor: 2,
    hasTouch: pointer === 'coarse',
    isMobile,
  })
  if (recordingView !== null) {
    await context.addInitScript((view) => localStorage.setItem('rokuban:recordings:view', view), recordingView)
  }
  if (reservationGrouping !== null) {
    await context.addInitScript((value) => localStorage.setItem('rokuban:reservations:group', value), reservationGrouping)
  }
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, apiHandler(apiOpts))
  await page.goto(URL_BASE + screen.path, { waitUntil: 'domcontentloaded' })
  // ダークは `.dark` クラスで切り替わる（index.css の @custom-variant）。
  // アプリ自身が `prefers-color-scheme` を初回描画前に `html.dark` へ反映する
  // （index.html の inline script）。ここで直接付けず、context の colorScheme が
  // 起こした到達経路そのものを判定に載せる --- inline script を壊すと全ダーク
  // ショットがここで落ちる。
  const hasDark = await page.evaluate(() => document.documentElement.classList.contains('dark'))
  if ((theme === 'dark') !== hasDark) {
    ng.push(
      `${screen.name}/${theme}/${viewport.name}: html.dark が prefers-color-scheme=${theme} に追従していない（到達経路が壊れている）`,
    )
  }
  if (!checkedColorSchemeChange) {
    const opposite = theme === 'dark' ? 'light' : 'dark'
    await page.emulateMedia({ colorScheme: opposite })
    await page
      .waitForFunction(
        (dark) => document.documentElement.classList.contains('dark') === dark,
        opposite === 'dark',
        { timeout: 1500 },
      )
      .catch(() => ng.push(`prefers-color-scheme の ${theme} → ${opposite} 変更に html.dark が追従しない`))
    await page.emulateMedia({ colorScheme: theme })
    await page
      .waitForFunction(
        (dark) => document.documentElement.classList.contains('dark') === dark,
        theme === 'dark',
        { timeout: 1500 },
      )
      .catch(() => ng.push(`prefers-color-scheme の ${opposite} → ${theme} 変更に html.dark が追従しない`))
    checkedColorSchemeChange = true
  }
  if (screen.wait) {
    await page.locator(screen.wait).first().waitFor({ timeout: 15000 }).catch(() => {
      ng.push(`${screen.name}/${theme}/${viewport.name}: 目印「${screen.wait}」が出ない`)
    })
  }
  // フォント（Geist Variable / Noto Sans JP Variable）の適用とレイアウト確定を待つ。
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(400)
  return { context, page }
}

/**
 * 実ブラウザで描画された操作標的を列挙する。
 *
 * `getBoundingClientRect()` は element 自身の矩形しか返さないため、祖先の
 * overflow で実際には隠れている操作列をクリップする。容量不足バッジのように
 * 見た目を変えず `::before` だけで当たり判定を広げる実装も、擬似要素の実寸を
 * hit 寸法へ加える。jsdom の DOM 属性や class 名ではなく、ブラウザがレイアウト
 * した値だけを判定へ使う。
 */
async function measureInteractiveTargets(page, label) {
  const result = await page.evaluate(
    ({ selector }) => {
      const clippedRect = (element) => {
        const raw = element.getBoundingClientRect()
        let left = raw.left
        let right = raw.right
        let top = raw.top
        let bottom = raw.bottom

        for (let parent = element.parentElement; parent !== null; parent = parent.parentElement) {
          const style = getComputedStyle(parent)
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowX) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const rect = parent.getBoundingClientRect()
            left = Math.max(left, rect.left)
            right = Math.min(right, rect.right)
          }
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowY) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const rect = parent.getBoundingClientRect()
            top = Math.max(top, rect.top)
            bottom = Math.min(bottom, rect.bottom)
          }
        }

        const width = right - left
        const height = bottom - top
        if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) {
          return null
        }
        return { left, right, top, bottom, width, height }
      }

      const isHidden = (element) => {
        for (let current = element; current !== null; current = current.parentElement) {
          const style = getComputedStyle(current)
          if (
            style.display === 'none' ||
            style.visibility === 'hidden' ||
            style.visibility === 'collapse'
          ) {
            return true
          }
        }
        return false
      }

      const isVisuallyHidden = (element) => {
        const style = getComputedStyle(element)
        return (
          (element.classList.contains('sr-only') && !element.matches(':focus')) ||
          (style.width === '1px' &&
            style.height === '1px' &&
            style.overflow === 'hidden' &&
            style.clip !== 'auto' &&
            style.clip !== 'none')
        )
      }

      const pseudoExtent = (element, pseudo) => {
        const style = getComputedStyle(element, pseudo)
        if (
          style.content === 'none' ||
          style.display === 'none' ||
          style.visibility === 'hidden' ||
          !['absolute', 'fixed'].includes(style.position)
        ) {
          return { width: 0, height: 0 }
        }
        const width = Number.parseFloat(style.width)
        const height = Number.parseFloat(style.height)
        return {
          width: Number.isFinite(width) ? width : 0,
          height: Number.isFinite(height) ? height : 0,
        }
      }

      const accessibleLabel = (element) => {
        const candidates = [
          element.getAttribute('aria-label'),
          element.getAttribute('placeholder'),
          element.getAttribute('title'),
          element.getAttribute('data-testid'),
          element.textContent?.replace(/\s+/g, ' ').trim(),
        ]
        return (candidates.find((value) => value !== null && value !== '') ?? '(無名)').slice(0, 80)
      }

      const targets = Array.from(document.querySelectorAll(selector))
        .filter((element) => {
          if (element instanceof HTMLInputElement && element.type === 'hidden') return false
          // Skip link はキーボードフォーカス時にだけ通常サイズへ戻る。常時
          // sr-only の矩形を標的サイズの失敗にせず、既存の④-Aで Tab 後の
          // 実寸（かつ main への到達）を別途固定する。
          if (isVisuallyHidden(element)) return false
          return !isHidden(element)
        })
        .map((element) => {
          const rect = clippedRect(element)
          if (rect === null) return null
          const before = pseudoExtent(element, '::before')
          const after = pseudoExtent(element, '::after')
          const role = element.getAttribute('role')
          const position = getComputedStyle(element).position
          return {
            tag: element.tagName.toLowerCase(),
            role,
            label: accessibleLabel(element),
            visualWidth: rect.width,
            visualHeight: rect.height,
            hitWidth: Math.max(rect.width, before.width, after.width),
            hitHeight: Math.max(rect.height, before.height, after.height),
            left: rect.left,
            right: rect.right,
            top: rect.top,
            bottom: rect.bottom,
            overlay: position === 'absolute' || position === 'fixed',
          }
        })
        .filter((target) => target !== null)

      const edgeGap = (a, b) => {
        const horizontal = Math.max(a.left - b.right, b.left - a.right, 0)
        const vertical = Math.max(a.top - b.bottom, b.top - a.bottom, 0)
        return Math.hypot(horizontal, vertical)
      }

      let minimumGap = null
      let overlapPairs = 0
      for (let i = 0; i < targets.length; i += 1) {
        for (let j = i + 1; j < targets.length; j += 1) {
          const gap = edgeGap(targets[i], targets[j])
          if (minimumGap === null || gap < minimumGap) minimumGap = gap
          if (gap === 0) overlapPairs += 1
        }
      }

      return { targets, minimumGap, overlapPairs }
    },
    { selector: INTERACTIVE_TARGET_SELECTOR },
  )

  const undersized = result.targets.filter(
    (target) =>
      target.hitWidth < INTERACTIVE_TARGET_MIN_PX || target.hitHeight < INTERACTIVE_TARGET_MIN_PX,
  )
  log(
    `  [${label}] 操作標的=${result.targets.length} 件 ` +
      `最小間隔=${result.minimumGap === null ? '—' : `${result.minimumGap.toFixed(1)}px`} ` +
      `重なり=${result.overlapPairs} 件 ` +
      `判定=${undersized.length === 0 ? 'OK' : 'NG'}`,
  )
  for (const target of result.targets) {
    log(
      `    ${target.tag}${target.role === null ? '' : `[role=${target.role}]`} 「${target.label}」 ` +
        `visual=${target.visualWidth.toFixed(1)}×${target.visualHeight.toFixed(1)}px ` +
        `hit=${target.hitWidth.toFixed(1)}×${target.hitHeight.toFixed(1)}px`,
    )
  }
  for (const target of undersized) {
    ng.push(
      `[${label}] 「${target.label}」の操作標的が ${target.hitWidth.toFixed(1)}×${target.hitHeight.toFixed(1)}px ` +
        `(基準 ${INTERACTIVE_TARGET_MIN_PX}×${INTERACTIVE_TARGET_MIN_PX}px 未満)`,
    )
  }
  if (result.targets.length === 0) {
    ng.push(`[${label}] 操作標的を 1 件も列挙できない`)
  }
  return result
}

/**
 * measureCoarseTapTargets はモバイルの操作標的を実際の hit-test で確認する。
 *
 * 各標的の中心と四辺の近傍を `elementFromPoint` で調べるため、見た目の
 * bounding box が小さくても `::before` / `::after` で広げた当たり判定を測れる。
 * さらに中心に置いた 44×44px の最低限の hit 領域どうしが交差しないかを確認する。
 * DOM の class や擬似要素の宣言だけで通さず、ブラウザが実際にどの要素へ配送するかを使う。
 */
async function measureCoarseTapTargets(page, label, scope = page.locator('body'), options = {}) {
  const result = await scope.evaluate(
    async (root, { selector, skipSelectionCheckbox, minTargetPx }) => {
      const hidden = (element) => {
        const summary = element.closest('summary')
        for (let current = element; current; current = current.parentElement) {
          const style = getComputedStyle(current)
          if (
            style.display === 'none' ||
            style.visibility === 'hidden' ||
            style.visibility === 'collapse'
          ) {
            return true
          }
          if (
            current instanceof HTMLDetailsElement &&
            !current.open &&
            summary?.parentElement !== current
          ) {
            return true
          }
        }
        return false
      }

      const visuallyHidden = (element) => {
        const style = getComputedStyle(element)
        return (
          (element.classList.contains('sr-only') && !element.matches(':focus')) ||
          (style.width === '1px' &&
            style.height === '1px' &&
            style.overflow === 'hidden' &&
            style.clip !== 'auto' &&
            style.clip !== 'none')
        )
      }

      const nameOf = (element) => {
        const candidates = [
          element.getAttribute('aria-label'),
          element.getAttribute('placeholder'),
          element.getAttribute('title'),
          element.getAttribute('data-testid'),
          element.textContent?.replace(/\s+/g, ' ').trim(),
        ]
        return (candidates.find((value) => value !== null && value !== '') ?? '(無名)').slice(0, 80)
      }

      const pseudoExtent = (element, pseudo) => {
        const style = getComputedStyle(element, pseudo)
        if (
          style.content === 'none' ||
          style.display === 'none' ||
          style.visibility === 'hidden' ||
          !['absolute', 'fixed'].includes(style.position)
        ) {
          return { width: 0, height: 0 }
        }
        const width = Number.parseFloat(style.width)
        const height = Number.parseFloat(style.height)
        return {
          width: Number.isFinite(width) ? width : 0,
          height: Number.isFinite(height) ? height : 0,
        }
      }

      const targets = []
      let ignoredSortSelects = 0
      let ignoredChaseLiveEdges = 0
      let ignoredSelectionCheckboxes = 0
      for (const element of root.querySelectorAll(selector)) {
        if (element instanceof HTMLInputElement && element.type === 'hidden') continue
        if (element.matches(':disabled, [aria-disabled="true"], [data-disabled]')) continue
        if (hidden(element) || visuallyHidden(element)) continue
        // Field / Select の <label> は入力を包み、ラベル文字列を押しても入力へ
        // 操作が届く。ブラウザが label 自体を hit-test するので、入力の外形では
        // なく、その label を実効的な hit surface として測る。
        const surface =
          (element instanceof HTMLInputElement || element instanceof HTMLSelectElement) &&
          element.closest('label')
            ? element.closest('label')
            : element
        // 一覧の下端や長い設定ページの項目も、実際に見える位置へ送って測る。
        // fixed bottom nav の後ろに隠れたまま計ると、スクロールすれば届く操作まで
        // 「押せない」と誤判定する。overflow で閉じた操作列は後段の clipping で除く。
        surface.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' })
        await new Promise(requestAnimationFrame)
        if (!surface.isConnected) continue

        const rect = surface.getBoundingClientRect()
        let left = rect.left
        let right = rect.right
        let top = rect.top
        let bottom = rect.bottom
        for (let parent = surface.parentElement; parent; parent = parent.parentElement) {
          const style = getComputedStyle(parent)
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowX) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const parentRect = parent.getBoundingClientRect()
            left = Math.max(left, parentRect.left)
            right = Math.min(right, parentRect.right)
          }
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowY) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const parentRect = parent.getBoundingClientRect()
            top = Math.max(top, parentRect.top)
            bottom = Math.min(bottom, parentRect.bottom)
          }
        }
        const clippedWidth = right - left
        const clippedHeight = bottom - top
        if (clippedWidth <= 0 || clippedHeight <= 0) {
          continue
        }
        const before = pseudoExtent(surface, '::before')
        const after = pseudoExtent(surface, '::after')

        // 録画の並び順 select は #1220 がアイコン化する。ここでは寸法を変えず、
        // その作業へ責務を移す明示例外にする。
        if (element.matches('select[aria-label="並び順"]')) {
          ignoredSortSelects++
          continue
        }

        // 追っかけ再生の先端ボタンはシークバー上の重ね要素（z-[3]）で、広げると
        // 先端の左右でシークのドラッグを奪う。24px のまま置く明示例外。
        if (element.matches('[data-testid="chase-live-edge"]')) {
          ignoredChaseLiveEdges++
          continue
        }

        // 編集モードでは role=option の行全体が click で選択を反転することを
        // 下の browser check で確認する。その場合 checkbox は同じ action の
        // 入力部品なので、行の大きな hit 領域を代表として数える。
        if (
          skipSelectionCheckbox &&
          element instanceof HTMLInputElement &&
          element.type === 'checkbox' &&
          element.closest('[role="option"]')
        ) {
          ignoredSelectionCheckboxes++
          continue
        }

        const rectCenterX = (left + right) / 2
        const rectCenterY = (top + bottom) / 2
        // 端から半 device pixel 内側を測る。いまの実ブラウザ context は DPR=2 なので
        // 21.75px は 44px target の最後の device-pixel center、43.5px target の
        // half-open な境界上になる。±22 の端そのものは外側なので使わない。
        const outerOffset = minTargetPx / 2 - 1 / (2 * devicePixelRatio)
        const rasterEdgeOffset = minTargetPx / 2 - 0.5
        const interiorOffsets = [-14, -7, 0, 7, 14]
        const pointsAt = (x, y) => [
          ...interiorOffsets.flatMap((offsetY) =>
            interiorOffsets.map((offsetX) => [x + offsetX, y + offsetY]),
          ),
          [x - outerOffset, y],
          [x + outerOffset, y],
          [x, y - outerOffset],
          [x, y + outerOffset],
        ]
        const missesAt = (x, y) => {
          return pointsAt(x, y).filter(([px, py]) => {
            if (px < 0 || py < 0 || px >= innerWidth || py >= innerHeight) return true
            const hit = document.elementFromPoint(px, py)
            if (hit !== null && (hit === surface || surface.contains(hit))) return false

            // elementFromPoint は CSSOM の整数座標へ丸める。±21.75 は最後の
            // half-device-pixel center でも丸め後に矩形端へ落ちるブラウザがある。
            // その場合だけ、最後に安定して表現できる pixel center (±21.5) でも
            // 同じ標的に届くことと、実寸が44px以上であることを併せて確認する。
            const dx = px - x
            const dy = py - y
            const isNearEdge =
              (Math.abs(Math.abs(dx) - outerOffset) < 0.001 && Math.abs(dy) < 0.001) ||
              (Math.abs(Math.abs(dy) - outerOffset) < 0.001 && Math.abs(dx) < 0.001)
            if (
              isNearEdge &&
              Math.max(clippedWidth, before.width, after.width) >= minTargetPx &&
              Math.max(clippedHeight, before.height, after.height) >= minTargetPx
            ) {
              const fallbackX = x + (dx === 0 ? 0 : Math.sign(dx) * rasterEdgeOffset)
              const fallbackY = y + (dy === 0 ? 0 : Math.sign(dy) * rasterEdgeOffset)
              const fallbackHit = document.elementFromPoint(fallbackX, fallbackY)
              return fallbackHit === null || (fallbackHit !== surface && !surface.contains(fallbackHit))
            }
            return true
          })
        }
        let centerX = rectCenterX
        let centerY = rectCenterY
        let misses = missesAt(centerX, centerY)

        // 行全面リンクは別リンクやボタンを意図的に前面へ重ねる。そのため矩形中央の
        // 一部だけ別宛先になることがある。44×44 全点がその行リンクへ届く位置が
        // 同じ矩形内にあるかを探し、行の利用可能な領域で合否を決める。
        // 小さな標的には位置ずらしを許さない。擬似要素で中心を広げた実効 hit area
        // を elementFromPoint で測る目的があるため。
        if (clippedWidth >= 44 && clippedHeight >= 44 && misses.length > 0) {
          const maxOffsetX = Math.max(0, Math.floor((clippedWidth - minTargetPx) / 2))
          const maxOffsetY = Math.max(0, Math.floor((clippedHeight - minTargetPx) / 2))
          const offsets = [0]
          for (let offset = 8; offset <= Math.max(maxOffsetX, maxOffsetY); offset += 8) {
            offsets.push(-offset, offset)
          }
          let best = { x: centerX, y: centerY, misses }
          search: for (const dy of offsets) {
            for (const dx of offsets) {
              if (dx === 0 && dy === 0) continue
              if (Math.abs(dx) > maxOffsetX || Math.abs(dy) > maxOffsetY) continue
              const candidate = { x: rectCenterX + dx, y: rectCenterY + dy }
              const candidateMisses = missesAt(candidate.x, candidate.y)
              if (candidateMisses.length < best.misses.length) {
                best = { ...candidate, misses: candidateMisses }
              }
              if (candidateMisses.length === 0) {
                best = { ...candidate, misses: candidateMisses }
                break search
              }
            }
          }
          centerX = best.x
          centerY = best.y
          misses = best.misses
        }
        targets.push({
          tag: element.tagName.toLowerCase(),
          role: element.getAttribute('role'),
          label: nameOf(element),
          centerX,
          centerY,
          documentCenterX: centerX + scrollX,
          documentCenterY: centerY + scrollY,
          href: surface.getAttribute('href'),
          pointerEvents: getComputedStyle(surface).pointerEvents,
          visualWidth: clippedWidth,
          visualHeight: clippedHeight,
          hitWidth: Math.max(clippedWidth, before.width, after.width),
          hitHeight: Math.max(clippedHeight, before.height, after.height),
          missCount: misses.length,
          testedCount: pointsAt(centerX, centerY).length,
          misses: misses.slice(0, 3).map(([x, y]) => {
            const hit = document.elementFromPoint(x, y)
            const describe = (node) => node === null ? '(viewport 外)' : `${node.tagName.toLowerCase()}${node.getAttribute('role') ? `[role=${node.getAttribute('role')}]` : ''}${node.id ? `#${node.id}` : ''}${node.getAttribute('data-testid') ? `[data-testid=${node.getAttribute('data-testid')}]` : ''}${node.className && typeof node.className === 'string' ? `.${node.className.trim().replace(/\s+/g, '.')}` : ''}`
            return {
              x: Number(x.toFixed(1)),
              y: Number(y.toFixed(1)),
              hit: describe(hit),
              hitHref: hit?.closest('a[href]')?.getAttribute('href') ?? null,
            }
          }),
        })
      }

      const overlaps = []
      for (let i = 0; i < targets.length; i++) {
        for (let j = i + 1; j < targets.length; j++) {
          const a = targets[i]
          const b = targets[j]
          if (
            Math.abs(a.documentCenterX - b.documentCenterX) < minTargetPx &&
            Math.abs(a.documentCenterY - b.documentCenterY) < minTargetPx
          ) {
            overlaps.push({ first: a.label, second: b.label })
          }
        }
      }

      return { targets, overlaps, ignoredSortSelects, ignoredChaseLiveEdges, ignoredSelectionCheckboxes }
    },
    {
      selector: TAP_TARGET_SELECTOR,
      skipSelectionCheckbox: options.skipSelectionCheckbox === true,
      minTargetPx: TAP_TARGET_MIN_PX,
    },
  )

  const misses = result.targets.filter(
    (target) =>
      target.missCount > 0 ||
      target.hitWidth < TAP_TARGET_MIN_PX ||
      target.hitHeight < TAP_TARGET_MIN_PX,
  )
  log(
    `  [${label}] 操作標的=${result.targets.length} 件 ` +
      `44×44 の hit 判定 NG=${misses.length} 件 重なり=${result.overlaps.length} 件`,
  )
  if (result.ignoredSortSelects > 0) {
    log(`    例外: 録画一覧の「並び順」select ${result.ignoredSortSelects} 件（#1220 がアイコン化）`)
  }
  if (result.ignoredChaseLiveEdges > 0) {
    log(`    例外: 追っかけの先端ボタン ${result.ignoredChaseLiveEdges} 件（シークバー上の重ね要素。広げるとシークのドラッグを奪う）`)
  }
  if (result.ignoredSelectionCheckboxes > 0) {
    log(`    例外: 選択行の checkbox ${result.ignoredSelectionCheckboxes} 件（行クリックが同じ切替を担う）`)
  }
  for (const target of misses) {
    ng.push(
      `[${label}] ${target.tag}${target.role === null ? '' : `[role=${target.role}]`} 「${target.label}」の中心 44×44px の hit 領域が要素に届かない ` +
        `(center=${target.centerX.toFixed(1)},${target.centerY.toFixed(1)}; visual=${target.visualWidth.toFixed(1)}×${target.visualHeight.toFixed(1)}px; hit=${target.hitWidth.toFixed(1)}×${target.hitHeight.toFixed(1)}px; miss=${target.missCount}/${target.testedCount}; pointer=${target.pointerEvents}; href=${target.href ?? 'なし'}; ${JSON.stringify(target.misses)})`,
    )
  }
  for (const overlap of result.overlaps) {
    ng.push(`[${label}] 44×44px の hit 領域が重なる: 「${overlap.first}」 / 「${overlap.second}」`)
  }
  if (result.targets.length === 0) {
    ng.push(`[${label}] 操作標的を 1 件も列挙できない`)
  }
  return result
}

/**
 * MISSING_STRING_PATTERN は「唯一の視覚オラクル」が欠損データのまま撮れて
 * いないかを見る（issue #468）。`undefined` / `NaN` はレンダーの欠損値が
 * そのまま文字列化されたときに出る典型で、`[object` はオブジェクトを
 * 文字列テンプレートに直接埋め込んだときに出る（`[object Object]` 等）。
 *
 * **`null` は対象にしない。** 番組名・ルール名に偶然「null」を含む文字列が
 * 来ても単語境界だけでは区別できず、実際に偽陽性になりうる（README §デザイン
 * 「判定を足すときの規律」参照）。`undefined` / `NaN` は単語境界
 * （`\b`）で、`[object` は `[` の前が単語文字になり得ない（直前は空白か
 * 文字列先頭）ため前方一致で見る。
 */
const MISSING_STRING_PATTERN = /\b(undefined|NaN)\b|\[object\b/

/**
 * checkMissingStrings は `page.textContent('body')` に欠損文字列が
 * 混ざっていないかを見る。安い判定なので全画面に掛ける
 * （ルールの `textMatches` から `target` が抜けると
 * `rule-condition-summary.ts` の `textTargetSummaryLabels[m.target]` が
 * `undefined` を返し、「undefinedに「連続テレビ小説」を含む」がそのまま
 * ルール一覧に描かれる --- これが issue #468 で実際に見逃されていた壊れ方）。
 */
async function checkMissingStrings(page, label) {
  const text = await page.textContent('body').catch(() => null)
  if (text === null) {
    ng.push(`${label}: body のテキストが取得できず欠損文字列を判定できていない`)
    return
  }
  const found = MISSING_STRING_PATTERN.exec(text)
  if (found) {
    ng.push(`${label}: 画面に欠損文字列「${found[0]}」が混ざっている`)
  }
}

/**
 * ルールカードが 360 / 390px で名前を省略せず本文を全幅で縦積みし、補助操作が
 * muted 色 + 方向アイコンのリンク・ボタンとして読めることを測る。幅・実色・
 * アイコン位置は jsdom がレイアウトも CSS 変数の解決も持たないため測れない。
 */
async function runRuleCardLayoutChecks() {
  log('\n=== H-2: ルールカードのモバイル配置と補助導線 ===')

  const conditionText = '番組名に「連続テレビ小説」を含む'
  const checkActionCue = async (locator, { label, viewport, iconClass, placement }) => {
    if ((await locator.count()) !== 1) {
      ng.push(`[${viewport}/actions] 「${label}」が 1 件表示されない`)
      return
    }
    const cue = await locator.evaluate((element, { iconClass: requiredIconClass, placement: iconPlacement }) => {
      const rootStyle = getComputedStyle(element)
      const probe = document.createElement('span')
      probe.style.cssText = 'position:fixed;left:-10000px;color:var(--muted-foreground)'
      document.body.appendChild(probe)
      const mutedTokenColor = getComputedStyle(probe).color
      probe.remove()

      const icons = [...element.querySelectorAll(`svg.${requiredIconClass}`)]
      const icon = icons[0]
      const iconStyle = icon === undefined ? null : getComputedStyle(icon)
      const rootBox = element.getBoundingClientRect()
      const iconBox = icon?.getBoundingClientRect()
      const children = [...element.children]
      const iconIndex = children.findIndex((child) => child.classList.contains(requiredIconClass))
      const textIndex = children.findIndex((child) => !child.matches('svg') && child.textContent.trim() !== '')
      const leftGap = iconBox === undefined ? Number.POSITIVE_INFINITY : iconBox.left - rootBox.left
      const rightGap = iconBox === undefined ? Number.POSITIVE_INFINITY : rootBox.right - iconBox.right
      const visible =
        iconBox !== undefined &&
        iconBox.width >= 12 &&
        iconBox.height >= 12 &&
        iconStyle?.display !== 'none' &&
        iconStyle?.visibility === 'visible' &&
        Number(iconStyle?.opacity) > 0
      const orderCorrect =
        iconIndex >= 0 &&
        textIndex >= 0 &&
        (iconPlacement === 'leading' ? iconIndex < textIndex : iconIndex > textIndex)
      const geometryCorrect =
        iconBox !== undefined &&
        (iconPlacement === 'leading'
          ? leftGap >= 0 && leftGap <= 8 && iconBox.right < rootBox.left + rootBox.width / 2
          : rightGap >= 0 && rightGap <= 8 && iconBox.left > rootBox.left + rootBox.width / 2)
      return {
        actualColor: rootStyle.color,
        mutedTokenColor,
        backgroundColor: rootStyle.backgroundColor,
        borderWidths: [rootStyle.borderTopWidth, rootStyle.borderRightWidth, rootStyle.borderBottomWidth, rootStyle.borderLeftWidth],
        iconCount: icons.length,
        iconVisible: visible,
        iconSize: iconBox === undefined ? 'missing' : `${iconBox.width.toFixed(1)}×${iconBox.height.toFixed(1)}px`,
        orderCorrect,
        geometryCorrect,
      }
    }, { iconClass, placement })

    if (cue.actualColor !== cue.mutedTokenColor) {
      ng.push(
        `[${viewport}/actions] 「${label}」の実色が muted token と一致しない ` +
          `(${cue.actualColor} / ${cue.mutedTokenColor})`,
      )
    }
    if (cue.backgroundColor !== 'rgba(0, 0, 0, 0)' || cue.borderWidths.some((width) => width !== '0px')) {
      ng.push(`[${viewport}/actions] 「${label}」に常時の背景または枠がある`)
    }
    if (cue.iconCount !== 1 || !cue.iconVisible) {
      ng.push(`[${viewport}/actions] 「${label}」の ${iconClass} が見えないか 12px 未満 (${cue.iconSize})`)
    }
    if (!cue.orderCorrect) {
      ng.push(`[${viewport}/actions] 「${label}」で ${placement} アイコンが文字の外側にない`)
    }
    if (!cue.geometryCorrect) {
      ng.push(`[${viewport}/actions] 「${label}」のアイコンが ${placement} 側に収まっていない`)
    }
  }

  for (const viewport of tapTargetWidths) {
    for (const theme of themes) {
      const { context, page } = await open(viewport, theme, screenOf('rules'), { pointer: 'coarse' })
      const name = page.getByRole('link', { name: 'ルール「朝ドラ」を編集' })
      const condition = page.getByText(conditionText, { exact: true })
      const card = condition.locator('xpath=../../../..')
      const conditionArea = condition.locator('xpath=../..')
      const toggle = page.getByRole('switch', { name: 'ルール「朝ドラ」を有効にする' })
      const menu = page.getByRole('button', { name: 'ルール「朝ドラ」のその他の操作' })
      const recordings = card.getByRole('link', { name: 'このルールの録画', exact: true })
      const createLabel = card.getByRole('button', { name: 'このキーワードで分類ルールを作る', exact: true })
      const actionRow = recordings.locator('..')
      const metadata = card.getByText('優先度 10', { exact: true }).locator('xpath=..')
      await condition.waitFor({ timeout: 5000 }).catch(() => {})

      const cardBox = await card.boundingBox()
      const conditionBox = await conditionArea.boundingBox()
      const headerBoxes = await Promise.all([name.boundingBox(), toggle.boundingBox(), menu.boundingBox()])
      if (cardBox === null || conditionBox === null || headerBoxes.some((box) => box === null)) {
        ng.push(`[${viewport.name}/${theme}/rules-layout] ルールカードの見出しまたは条件欄を測れない`)
      } else {
        // カード内幅 = 枠 + 左右 padding を実測値で引く（px-3 + border 1px で 26px）。
        const insets = await card.evaluate((element) => {
          const style = getComputedStyle(element)
          return ['borderLeftWidth', 'borderRightWidth', 'paddingLeft', 'paddingRight'].reduce(
            (sum, key) => sum + Number.parseFloat(style[key]),
            0,
          )
        })
        const contentWidth = cardBox.width - insets
        log(
          `  [${viewport.name}/${theme}/rules-layout] 条件欄=${conditionBox.width.toFixed(1)}px / ` +
            `カード内幅=${contentWidth.toFixed(1)}px`,
        )
        if (conditionBox.width < contentWidth - 4) {
          ng.push(
            `[${viewport.name}/${theme}/rules-layout] 条件欄がカードの全幅でない ` +
              `(${conditionBox.width.toFixed(1)}px < ${(contentWidth - 4).toFixed(1)}px)`,
          )
        }
        // 名前は省略せず全文が出る（390px で「朝ド…」になった退行の再発防止）。
        // 省略するのは Button 内の span.truncate なので、wrapper でなくそれを測る。
        const nameText = name.locator('.truncate')
        const truncation = await nameText.evaluate((element) => ({
          text: element.textContent,
          scrollWidth: element.scrollWidth,
          clientWidth: element.clientWidth,
        }))
        if (truncation.scrollWidth > truncation.clientWidth) {
          ng.push(
            `[${viewport.name}/${theme}/rules-layout] ルール名「${truncation.text}」が省略されている ` +
              `(scrollWidth=${truncation.scrollWidth} > clientWidth=${truncation.clientWidth})`,
          )
        }
        const headerCenters = headerBoxes.map((box) => box.y + box.height / 2)
        if (Math.max(...headerCenters) - Math.min(...headerCenters) > 4) {
          ng.push(`[${viewport.name}/${theme}/rules-layout] 名前・スイッチ・「…」が同じ見出し行に揃っていない`)
        }
        const headerBottom = Math.max(...headerBoxes.map((box) => box.y + box.height))
        if (conditionBox.y < headerBottom - 1) {
          ng.push(`[${viewport.name}/${theme}/rules-layout] 条件欄が見出しより下に配置されていない`)
        }
        const [actionRowBox, metadataBox] = await Promise.all([actionRow.boundingBox(), metadata.boundingBox()])
        if (actionRowBox === null || metadataBox === null) {
          ng.push(`[${viewport.name}/${theme}/rules-layout] 条件の詳細と補助操作の段を測れない`)
        } else {
          if (actionRowBox.width < contentWidth - 4) {
            ng.push(
              `[${viewport.name}/${theme}/rules-layout] 補助操作の段が全幅でない ` +
                `(${actionRowBox.width.toFixed(1)}px < ${(contentWidth - 4).toFixed(1)}px)`,
            )
          }
          if (actionRowBox.y < metadataBox.y + metadataBox.height + 3) {
            ng.push(`[${viewport.name}/${theme}/rules-layout] 補助操作が条件の詳細より下の段にない`)
          }
        }
      }

      // 「録画予定 N 件」の 44px 当たり判定が行の高さを押し広げ、同じ行の文字より
      // 下がったり補助操作の段との間を空けたりしないこと。44px の箱ではなく文字自体を測る。
      const scheduled = card.getByRole('link', { name: /^録画予定 \d+ 件$/ })
      await scheduled.first().waitFor({ timeout: 5000 }).catch(() => {})
      if ((await scheduled.count()) !== 1) {
        ng.push(`[${viewport.name}/${theme}/rules-layout] 「録画予定 N 件」が 1 件表示されない`)
      } else {
        const rows = await metadata.evaluate((row, link) => {
          const textRect = (element) => {
            const range = document.createRange()
            range.selectNodeContents(element)
            return range.getBoundingClientRect()
          }
          const priority = textRect(row.querySelector('span'))
          const linkText = textRect(link)
          return {
            sameLine: linkText.top < priority.bottom,
            priorityCenter: priority.top + priority.height / 2,
            linkCenter: linkText.top + linkText.height / 2,
            linkTextBottom: linkText.bottom,
            rowBottom: row.getBoundingClientRect().bottom,
          }
        }, await scheduled.elementHandle())
        const centerDiff = Math.abs(rows.linkCenter - rows.priorityCenter)
        const bottomGap = rows.rowBottom - rows.linkTextBottom
        log(
          `  [${viewport.name}/${theme}/rules-layout] 録画予定と優先度の文字の縦中心差=${centerDiff.toFixed(1)}px / ` +
            `行下端と録画予定の文字下端の差=${bottomGap.toFixed(1)}px`,
        )
        // 折り返して別の行に落ちた場合は縦中心が離れて当然なので、中心差は同じ行のときだけ見る。
        if ((rows.sameLine && centerDiff > 2) || bottomGap > 8) {
          ng.push(
            `[${viewport.name}/${theme}/rules-layout] 「録画予定 N 件」が行内でずれている ` +
              `(優先度との縦中心差=${centerDiff.toFixed(1)}px > 2px / ` +
              `行下端との差=${bottomGap.toFixed(1)}px > 8px)`,
          )
        }
      }
      await checkActionCue(recordings, {
        label: 'このルールの録画', viewport: `${viewport.name}/${theme}`, iconClass: 'lucide-chevron-right', placement: 'trailing',
      })
      await checkActionCue(createLabel, {
        label: 'このキーワードで分類ルールを作る', viewport: `${viewport.name}/${theme}`, iconClass: 'lucide-plus', placement: 'leading',
      })
      await context.close()
    }
  }

  // desktop はカードの主情報と補助操作を横に分け、スイッチとメニューを右端に残す。
  {
    const viewport = { name: 'desktop-1280', width: 1280, height: 800 }
    const { context, page } = await open(viewport, 'light', screenOf('rules'), { pointer: 'fine' })
    const condition = page.getByText(conditionText, { exact: true })
    const card = condition.locator('xpath=../../../..')
    const conditionArea = condition.locator('xpath=../..')
    const toggle = page.getByRole('switch', { name: 'ルール「朝ドラ」を有効にする' })
    const menu = page.getByRole('button', { name: 'ルール「朝ドラ」のその他の操作' })
    const action = card.getByRole('link', { name: 'このルールの録画', exact: true })
    const [cardBox, conditionBox, toggleBox, menuBox, actionBox] = await Promise.all([
      card.boundingBox(), conditionArea.boundingBox(), toggle.boundingBox(), menu.boundingBox(), action.boundingBox(),
    ])
    if ([cardBox, conditionBox, toggleBox, menuBox, actionBox].some((box) => box === null)) {
      ng.push('[desktop-1280/rules-layout] 横並びのカード要素を測れない')
    } else {
      if (conditionBox.width >= cardBox.width - 100) {
        ng.push('[desktop-1280/rules-layout] 条件欄がカード全幅へ広がり、横並びを保っていない')
      }
      const menuRightGap = cardBox.x + cardBox.width - menuBox.x - menuBox.width
      log(`  [desktop-1280/rules-layout] メニュー右端とカード右端の間隔=${menuRightGap.toFixed(1)}px`)
      if (Math.abs(menuRightGap - 13) > 2) {
        ng.push(`[desktop-1280/rules-layout] メニューのカード右端余白が13pxでない（${menuRightGap.toFixed(1)}px）`)
      }
      const toggleCenter = toggleBox.y + toggleBox.height / 2
      const menuCenter = menuBox.y + menuBox.height / 2
      if (Math.abs(toggleCenter - menuCenter) > 4 || toggleBox.x + toggleBox.width > menuBox.x) {
        ng.push('[desktop-1280/rules-layout] スイッチとメニューが同じ右端の操作行に揃っていない')
      }
      if (actionBox.x < cardBox.x + cardBox.width / 2) {
        ng.push('[desktop-1280/rules-layout] 補助操作がカード右側に配置されていない')
      }
    }
    await context.close()
  }

  // 番組表の展開領域も、移動先を示す ChevronRight と muted の文字色を持つ。
  for (const viewport of tapTargetWidths) {
    for (const theme of themes) {
      const { context, page } = await open(viewport, theme, screenOf('programs'), { pointer: 'coarse' })
      const row = page.locator('li[data-program-id]').first()
      await row.waitFor({ timeout: 5000 }).catch(() => {})
      const expander = row.locator('button[aria-expanded]').first()
      if ((await expander.count()) === 0) {
        ng.push(`[${viewport.name}/${theme}/program-search-action] 番組行の展開ボタンが見つからない`)
        await context.close()
        continue
      }
      await expander.click()
      const link = row.getByRole('link', { name: 'この番組名で検索', exact: true })
      await link.waitFor({ timeout: 5000 }).catch(() => {})
      if ((await link.count()) !== 1) {
        ng.push(`[${viewport.name}/${theme}/program-search-action] 「この番組名で検索」が表示されない`)
      } else {
        await checkActionCue(link, {
          label: 'この番組名で検索',
          viewport: `${viewport.name}/${theme}`,
          iconClass: 'lucide-chevron-right',
          placement: 'trailing',
        })
      }
      await context.close()
    }
  }

  if (process.env.E2E_RULE_LAYOUT_SCREENSHOTS === '1') {
    const viewport = { name: 'mobile-390', width: 390, height: 844 }
    for (const theme of themes) {
      const { context, page } = await open(viewport, theme, screenOf('rules'), { pointer: 'coarse' })
      await page.screenshot({ path: path.join(OUT_DIR, `issue-1223-rules-${theme}-390.png`) })
      await context.close()

      const programs = await open(viewport, theme, screenOf('programs'), { pointer: 'coarse' })
      const row = programs.page.locator('li[data-program-id]').first()
      await row.waitFor({ timeout: 5000 }).catch(() => {})
      const expander = row.locator('button[aria-expanded]').first()
      if (await expander.count()) await expander.click()
      await programs.page.screenshot({ path: path.join(OUT_DIR, `issue-1223-programs-${theme}-390.png`) })
      await programs.context.close()
    }
  }
}

/** 360 / 390px の全主要画面を coarse pointer で開き、44px の hit 領域を測る。 */
async function runCoarseTapTargetChecks() {
  log('\n=== ④-A H-1: coarse pointer の 44×44px hit 領域 ===')
  for (const viewport of tapTargetWidths) {
    for (const screen of tapTargetScreens) {
      const screenOptions =
        screen.name === 'recording-detail'
          ? { pointer: 'coarse', multiSite: true, recordingDetailScenario: 'completed' }
          : { pointer: 'coarse', ...(screen.name === 'home' || screen.name === 'home-watch' ? { homeModeFixture: true } : {}) }
      const { context, page } = await open(viewport, 'light', screen, screenOptions)
      const pointerIsCoarse = await page.evaluate(() => matchMedia('(pointer: coarse)').matches)
      if (!pointerIsCoarse) {
        ng.push(`[${viewport.name}/${screen.name}] ブラウザが pointer: coarse と判定されない`)
      }
      await measureCoarseTapTargets(page, `${viewport.name}/${screen.name}`)
      if (screen.name === 'home') {
        const details = page.locator('details[data-testid="home-timeline-details"]')
        if ((await details.count()) === 0) {
          ng.push(`[${viewport.name}/home-timeline-details] 詳細行が表示されない`)
        } else {
          await details.locator('summary').click()
          const isOpen = await details.evaluate((element) => element.open)
          if (!isOpen) {
            ng.push(`[${viewport.name}/home-timeline-details] summary の操作で詳細が開かない`)
          } else {
            await measureCoarseTapTargets(page, `${viewport.name}/home-timeline-details`)
          }
        }
      }
      await context.close()
    }
  }

  // 展開行でだけ見える番組名検索リンクと encode profile checkbox label も測る。
  for (const viewport of tapTargetWidths) {
    const { context, page } = await open(viewport, 'light', screenOf('programs'), { pointer: 'coarse' })
    const row = page.locator('li[data-program-id]').first()
    const rowVisible = await row.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!rowVisible) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 番組行が表示されない`)
      await context.close()
      continue
    }
    const expander = row.locator('button[aria-expanded]').first()
    if ((await expander.count()) === 0) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 展開ボタンが見つからない`)
      await context.close()
      continue
    }
    await expander.click()
    if ((await expander.getAttribute('aria-expanded')) !== 'true') {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 行が展開されない`)
      await context.close()
      continue
    }
    const body = row.locator('[id^="program-row-detail-"]')
    const nameSearch = body.getByRole('link', { name: 'この番組名で検索', exact: true })
    const profileGroup = body.getByRole('group', { name: 'エンコードプロファイル' })
    const profileCheckbox = profileGroup.getByRole('checkbox').first()
    const searchVisible = await nameSearch.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    const profileVisible = await profileCheckbox.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!searchVisible) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 番組名検索リンクが表示されない`)
    }
    if (!profileVisible) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] encode profile checkbox が表示されない`)
    }
    if (searchVisible && profileVisible) {
      await measureCoarseTapTargets(page, `${viewport.name}/programs-expanded-first-row`, body)
    }
    await context.close()
  }

  // 短いシリーズ名はシリーズリンクの横幅が本文より狭い状態を作る。
  for (const viewport of tapTargetWidths) {
    const { context, page } = await open(viewport, 'light', recordingDetailScreen, {
      pointer: 'coarse',
      multiSite: true,
      recordingDetailScenario: 'short-series',
    })
    const seriesLink = page.getByRole('link', { name: 'このシリーズへ: 天気' })
    const visible = await seriesLink.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!visible) {
      ng.push(`[${viewport.name}/recording-detail-short-series] 短いシリーズリンクが表示されない`)
    } else {
      await measureCoarseTapTargets(
        page,
        `${viewport.name}/recording-detail-short-series`,
        page.locator('[data-testid="recording-series-links"]'),
      )
    }
    await context.close()
  }

  // 録画中は結論バッジが記録タブへの button として現れる。
  for (const viewport of tapTargetWidths) {
    const { context, page } = await open(viewport, 'light', recordingDetailScreen, {
      pointer: 'coarse',
      multiSite: true,
      recordingDetailScenario: 'recording',
    })
    const verdictButton = page.getByRole('button', { name: '録画状態を記録タブで見る' })
    const visible = await verdictButton.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!visible) {
      ng.push(`[${viewport.name}/recording-detail-recording] 録画状態ボタンが表示されない`)
    } else {
      await measureCoarseTapTargets(
        page,
        `${viewport.name}/recording-detail-recording`,
        page.locator('[data-testid="recording-title-row"]'),
      )
    }
    await context.close()
  }

  // ruleId filter は通常の予約一覧に出ない「解除」ボタンと条件編集リンクも持つ。
  for (const viewport of tapTargetWidths) {
      const screen = {
        ...screenOf('reservations'),
        name: 'reservations-active-filter',
        path: '/reservations?ruleId=1',
        wait: 'button[aria-label*="絞り込みを解除"]',
      }
    const { context, page } = await open(viewport, 'light', screen, { pointer: 'coarse' })
    const removeFilter = page.getByRole('button', { name: 'ルール「朝ドラ」の絞り込みを解除' })
    const editRule = page.getByRole('link', { name: 'ルールの条件を直す', exact: true })
    if ((await removeFilter.count()) === 0) {
      ng.push(`[${viewport.name}/reservations-active-filter] ルール絞り込み解除ボタンが表示されない`)
    }
    if ((await editRule.count()) === 0) {
      ng.push(`[${viewport.name}/reservations-active-filter] ルール条件リンクが表示されない`)
    }
    if ((await removeFilter.count()) > 0 && (await editRule.count()) > 0) {
      await measureCoarseTapTargets(page, `${viewport.name}/reservations-active-filter`)
    }
    await context.close()
  }

  // まず行の外側をタップして選択が反転するか実測する。checkbox 自体を 44px に
  // 広げると別の標的と重なりうるため、行が操作面なら role=option を標的とする。
  {
    const viewport = tapTargetWidths[0]
    const { context, page } = await open(viewport, 'light', screenOf('recordings'), { pointer: 'coarse' })
    const selectionButton = page.getByRole('button', { name: '選択', exact: true })
    if ((await selectionButton.count()) === 0) {
      ng.push('[mobile-360/recordings-selection] 選択モードへ入るボタンが見つからない')
    } else {
      await selectionButton.click()
      const checkbox = page.locator('input[type="checkbox"][aria-label$="を選択"]').first()
      await checkbox.waitFor({ timeout: 5000 }).catch(() => {})
      const row = page.locator('[role="option"]').first()
      const box = await row.boundingBox()
      if (box === null || (await checkbox.count()) === 0) {
        ng.push('[mobile-360/recordings-selection] 行または checkbox が見つからない')
      } else {
        const position = { x: Math.min(100, box.width - 20), y: box.height / 2 }
        let rowToggles = true
        const rowHit = await row.evaluate((element, point) => {
          const rect = element.getBoundingClientRect()
          const hit = document.elementFromPoint(rect.left + point.x, rect.top + point.y)
          return hit === element.querySelector('input[type="checkbox"]')
        }, position)
        if (rowHit) {
          ng.push('[mobile-360/recordings-selection] 行の確認タップが checkbox 自体に当たった')
        }
        for (const expected of [true, false]) {
          await row.click({ position })
          const changed = await page.waitForFunction(
            ({ accessibleName, checked }) =>
              [...document.querySelectorAll('input[type="checkbox"]')].some(
                (input) => input.getAttribute('aria-label') === accessibleName && input.checked === checked,
              ),
            { accessibleName: await checkbox.getAttribute('aria-label'), checked: expected },
            { timeout: 3000 },
          ).then(() => true).catch(() => false)
          if (!changed) {
            ng.push(`[mobile-360/recordings-selection] checkbox 外の行タップで checked=${expected} にならない`)
            rowToggles = false
            break
          }
        }
        if (await checkbox.isChecked()) {
          ng.push('[mobile-360/recordings-selection] 2 回目の行タップ後も選択が解除されない')
          rowToggles = false
        }
        await measureCoarseTapTargets(
          page,
          'mobile-360/recordings-selection',
          page.locator('body'),
          { skipSelectionCheckbox: rowToggles },
        )
      }
    }
    await context.close()
  }

  // MoreMenu は他画面の上に開くため、背後のページ標的と重ねずポップオーバー内だけを測る。
  {
    const { context, page } = await open(tapTargetWidths[0], 'light', screenOf('programs'), { pointer: 'coarse' })
    const trigger = page.getByRole('button', { name: 'その他' })
    if ((await trigger.count()) === 0) {
      ng.push('[mobile-360/more-menu] 「その他」のトリガーが見つからない')
    } else {
      await trigger.click()
      const menu = page.getByRole('dialog', { name: 'その他のナビゲーション' })
      await menu.waitFor({ timeout: 5000 }).catch(() => {
        ng.push('[mobile-360/more-menu] ポップオーバーが開かない')
      })
      if (await menu.count()) {
        await page.waitForTimeout(300)
        await measureCoarseTapTargets(page, 'mobile-360/more-menu', menu)
      }
    }
    await context.close()
  }

  // 共通 DropdownMenuItem の標準寸法を、破壊的操作を持つルール行で実ブラウザ測定する。
  {
    const { context, page } = await open(tapTargetWidths[0], 'light', screenOf('rules'), { pointer: 'coarse' })
    const trigger = page.locator('[aria-haspopup="menu"]').first()
    if ((await trigger.count()) === 0) {
      ng.push('[mobile-360/rule-menu] ルールの操作メニューが見つからない')
    } else {
      await trigger.click()
      const menu = page.locator('[data-slot="dropdown-menu-content"]').last()
      await menu.waitFor({ timeout: 5000 }).catch(() => {
        ng.push('[mobile-360/rule-menu] DropdownMenuContent が開かない')
      })
      if (await menu.count()) {
        await page.waitForTimeout(300)
        await measureCoarseTapTargets(page, 'mobile-360/rule-menu', menu)
      }
    }
    await context.close()
  }

  // pointer: fine / 1280px は既定 Button の高さを 32px のままにする。
  {
    const viewport = { name: 'desktop-1280', width: 1280, height: 800 }
    const { context, page } = await open(viewport, 'light', screenOf('series-hub'), { pointer: 'fine' })
    const button = page.locator('[data-slot="button"].h-8').first()
    if ((await button.count()) === 0) {
      ng.push('[desktop-1280/fine] 既定サイズ Button の実例が見つからない')
    } else {
      const height = await button.evaluate((element) => Number.parseFloat(getComputedStyle(element).height))
      log(`  [desktop-1280/fine] 既定 Button の高さ=${height}px`)
      if (height !== 32) ng.push(`[desktop-1280/fine] 既定 Button が 32px ではない（${height}px）`)
    }
    const pointerIsFine = await page.evaluate(() => matchMedia('(pointer: fine)').matches)
    if (!pointerIsFine) ng.push('[desktop-1280/fine] ブラウザが pointer: fine と判定されない')
    await context.close()
  }
}

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// CI / red-green でこの追加判定だけを実行する入口。通常の e2e:design でも
// 同じ関数を ④-A'' として実行し、色・到達距離の既存判定と一緒に守る。
if (process.env.E2E_TAP_TARGETS_ONLY === '1') {
  await runCoarseTapTargetChecks()
  await finish(ng, browser)
}

if (process.env.E2E_RULE_CARD_ONLY === '1') {
  await runRuleCardLayoutChecks()
  await finish(ng, browser)
}

// --- ① スクリーンショット ---
log('\n=== ① スクリーンショット ===')
for (const viewport of viewports) {
  for (const theme of themes) {
    for (const screen of screens) {
      const screenOptions = screen.name === 'home' ? { homeModeFixture: true } : {}
      const { context, page } = await open(viewport, theme, screen, screenOptions)
      const file = path.join(OUT_DIR, `${screen.name}-${theme}-${viewport.name}.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(page, `${screen.name}/${theme}/${viewport.name}`)
      await context.close()
    }
  }
}

// --- ホーム M8-25: モードと「次に見る 1 本」の実寸 -------------------------
// jsdom では測れないサムネイル比・固定ナビとの重なり・バッジのはみ出しを、
// 実ブラウザで 1280 / 360 / 390px において測る。各モードの desktop/phone は
// ライト・ダーク両方を撮り、issue のモックと並べて確認する。
for (const mode of ['watch', 'ops']) {
  for (const theme of themes) {
    for (const viewport of [homeDesktop, mobile, mobileWide]) {
      const screen = { name: `home-${mode}`, path: `/?mode=${mode}` }
      const { context, page } = await open(viewport, theme, screen, { homeModeFixture: true })
      await page.locator('main > header').waitFor({ timeout: 5000 }).catch(() => {})

      const toggle = page.getByTestId('home-mode-toggle')
      if ((await toggle.count()) === 0) {
        ng.push(`[${mode}/${theme}/${viewport.width}px] ホームのモード切替が見つからない`)
      }
      const badge = page.getByTestId('home-warning-count')
      if ((await badge.count()) === 0) {
        ng.push(`[${mode}/${theme}/${viewport.width}px] 警告件数バッジが見つからない`)
      } else if ((await badge.innerText()).trim() !== '3') {
        ng.push(`[${mode}/${theme}/${viewport.width}px] 警告件数が3でない（${(await badge.innerText()).trim()}）`)
      }

      const geometry = await page.evaluate(() => {
        const rect = (selector) => {
          const element = document.querySelector(selector)
          if (!element) return null
          const { x, y, width, height, bottom, right } = element.getBoundingClientRect()
          return { x, y, width, height, bottom, right }
        }
        const badgeElement = document.querySelector('[data-testid="home-warning-count"]')
        const badgeStyle = badgeElement ? getComputedStyle(badgeElement) : null
        return {
          content: rect('[data-testid="page-content"], [data-testid="bounded-page-content"]'),
          thumbnail: rect('[data-testid="home-next-watch-thumbnail"]'),
          primary: rect('[data-testid="home-primary-action"]'),
          header: rect('main > header'),
          nav: rect('[data-testid="bottom-nav"]'),
          toggle: rect('[data-testid="home-mode-toggle"]'),
          badge: rect('[data-testid="home-warning-count"]'),
          badgeWhiteSpace: badgeStyle?.whiteSpace ?? null,
          badgeScrollWidth: badgeElement?.scrollWidth ?? null,
          badgeClientWidth: badgeElement?.clientWidth ?? null,
        }
      })

      if (geometry.toggle !== null && geometry.badge !== null) {
        const { toggle: frame, badge: count } = geometry
        if (
          count.x < frame.x ||
          count.y < frame.y ||
          count.right > frame.right ||
          count.bottom > frame.bottom
        ) {
          ng.push(`[${mode}/${theme}/${viewport.width}px] 管理バッジがトグル枠からはみ出す`)
        }
        if (
          geometry.badgeWhiteSpace !== 'nowrap' ||
          geometry.badgeScrollWidth > geometry.badgeClientWidth
        ) {
          ng.push(`[${mode}/${theme}/${viewport.width}px] 管理バッジが折り返す`)
        }
      }

      if (mode === 'watch' && viewport.width === homeDesktop.width) {
        if (geometry.content === null || geometry.thumbnail === null) {
          ng.push(`[watch/${theme}/1280px] 「次に見る 1 本」か本文の幅を測れない`)
        } else {
          const ratio = geometry.thumbnail.width / geometry.content.width
          log(`  [watch/${theme}/1280px] 主役サムネイル / 本文幅=${ratio.toFixed(3)}`)
          if (ratio < 0.55) {
            ng.push(`[watch/${theme}/1280px] 主役サムネイルが本文幅の55%未満（${ratio.toFixed(3)}）`)
          }
        }
      }

      if (mode === 'watch' && (viewport.width === mobile.width || viewport.width === mobileWide.width)) {
        if (geometry.primary === null || geometry.header === null || geometry.nav === null) {
          ng.push(`[watch/${theme}/${viewport.width}px] 主ボタン・固定ヘッダー・ボトムナビを測れない`)
        } else if (
          geometry.primary.y < geometry.header.bottom ||
          geometry.primary.bottom > viewport.height ||
          geometry.primary.bottom > geometry.nav.y
        ) {
          ng.push(`[watch/${theme}/${viewport.width}px] 主ボタンが初期画面から隠れる`)
        }
      }

      if (viewport.width === homeDesktop.width || viewport.width === mobile.width) {
        const file = path.join(OUT_DIR, `home-${mode}-${theme}-${viewport.name}.png`)
        await page.screenshot({ path: file })
        log(`  ${path.basename(file)}`)
        await checkMissingStrings(page, `home-${mode}/${theme}/${viewport.name}`)
      }
      await context.close()
    }
  }
}

// ホーム「見る」: 2560x1440 でも主役と新着 1 行目が初期 viewport に入り、
// 新着件数は 176px カードで入る列数に追従する。低い desktop viewport でも主役を保つ。
{
  const wideViewport = { name: 'home-wide', width: 2560, height: 1440 }
  const shortViewport = { name: 'home-short', width: 1366, height: 400 }
  const arrivalCounts = new Map()
  for (const viewport of [homeDesktop, wideViewport, shortViewport]) {
    const { context, page } = await open(
      viewport,
      'light',
      { name: 'home-watch', path: '/?mode=watch' },
      { homeModeFixture: true, homeWideFixture: true },
    )
    const grid = page.getByTestId('home-new-arrivals-grid')
    const ready = await grid.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
    if (!ready) {
      ng.push(`home/watch/${viewport.width}: 新着カードが表示されない`)
      await context.close()
      continue
    }
    const metrics = await page.evaluate(() => {
      const rect = (element) => {
        if (!(element instanceof HTMLElement)) return null
        const { x, y, right, bottom, width, height } = element.getBoundingClientRect()
        return { x, y, right, bottom, width, height }
      }
      const gridElement = document.querySelector('[data-testid="home-new-arrivals-grid"]')
      const hero = document.querySelector('[data-testid="home-next-watch-thumbnail"]')
      const firstCard = gridElement?.querySelector('li')
      return {
        columns: Number(gridElement?.getAttribute('data-column-count') ?? 0),
        cards: gridElement?.children.length ?? 0,
        cardRows: gridElement
          ? [...gridElement.children].map((card) => Math.round(card.getBoundingClientRect().y))
          : [],
        hero: rect(hero),
        firstCard: rect(firstCard),
        header: rect(document.querySelector('main > header')),
      }
    })
    arrivalCounts.set(viewport.width, metrics.cards)
    log(`  home/watch/${viewport.width}: columns=${metrics.columns}, cards=${metrics.cards}`)
    if (metrics.cards !== metrics.columns) {
      ng.push(`home/watch/${viewport.width}: 新着 ${metrics.cards} 件が 1 行の ${metrics.columns} 列と一致しない`)
    }
    if (new Set(metrics.cardRows).size > 1) {
      ng.push(`home/watch/${viewport.width}: 新着カードが 2 行以上に折り返されている`)
    }
    if (viewport.name === shortViewport.name) {
      if (metrics.hero === null || metrics.hero.width < 320 || metrics.hero.height < 180) {
        ng.push(`home/watch/1366x400: 主役サムネイルが表示可能な大きさでない（${metrics.hero?.width ?? 0}×${metrics.hero?.height ?? 0}px）`)
      }
      await page.screenshot({ path: path.join(OUT_DIR, 'home-watch-light-short.png') })
    }
    if (viewport.width === wideViewport.width) {
      if (metrics.cards <= 6) ng.push('home/watch/2560: 新着が 6 件以下のまま')
      if (metrics.hero === null || metrics.firstCard === null || metrics.header === null) {
        ng.push('home/watch/2560: 主役または先頭カードの位置を測れない')
      } else {
        if (metrics.hero.width > 1024.5 || metrics.hero.height > 576.5) {
          ng.push(`home/watch/2560: 主役サムネイルの上限を超えている（${metrics.hero.width}×${metrics.hero.height}px）`)
        }
        if (metrics.hero.bottom > viewport.height || metrics.firstCard.bottom > viewport.height) {
          ng.push('home/watch/2560: 主役または新着 1 枚目が初期画面に入らない')
        }
      }
      const file = path.join(OUT_DIR, 'home-watch-light-desktop-wide.png')
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
    }
    await context.close()
  }
  const narrowCount = arrivalCounts.get(homeDesktop.width)
  const wideCount = arrivalCounts.get(2560)
  if (narrowCount !== undefined && wideCount !== undefined && narrowCount >= wideCount) {
    ng.push(`home/watch: 1280px の新着件数 ${narrowCount} が 2560px の ${wideCount} 件より減っていない`)
  }
}

// --- ホーム管理モード M8-26: 時間軸の実寸 -------------------------------
// 3 幅でページ自体は固定し、時間軸の枠だけが横スクロールすることを測る。
// 時間軸は機械配置の推定ではなく既存 API の観測値を時刻に置く表示なので、
// この実ブラウザ検査で初期位置・30 分ブロック・容量ラベルの衝突を確認する。
log('\n=== ホーム管理モード M8-26: 時間軸の実寸 ===')
async function waitForHomeTimelineLayout(page) {
  return page.waitForFunction(
    () => {
      const timeline = document.querySelector('[data-testid="home-ops-timeline"]')
      const frame = timeline?.querySelector('[data-testid="home-ops-timeline-frame"]')
      const content = timeline?.querySelector('[data-testid="home-ops-timeline-content"]')
      const marker = timeline?.querySelector('[data-testid="home-timeline-now"]')
      if (!frame || !content || !marker) return false
      // #1021 の固定 fixture は今日 0:00 から翌々日 0:00 までの 48 時間。
      // スマホは 30px/h、desktop は 64px/h。React Query 完了後に frame が現れ、
      // ResizeObserver と初期スクロールが反映された実寸まで待つ。
      const hourPx = window.innerWidth <= 480 ? 30 : 64
      const expectedContentWidth = 48 * hourPx + 64
      const frameRect = frame.getBoundingClientRect()
      const markerRect = marker.getBoundingClientRect()
      return Math.abs(content.getBoundingClientRect().width - expectedContentWidth) < 1 &&
        frame.scrollLeft > 0 &&
        markerRect.x >= frameRect.x &&
        markerRect.right <= frameRect.right
    },
    null,
    { timeout: 5000 },
  ).then(() => true).catch(() => false)
}
for (const theme of themes) {
  for (const viewport of [homeDesktop, mobile, mobileWide]) {
    const { context, page } = await open(viewport, theme, {
      name: 'home-ops-timeline',
      path: '/?mode=ops',
    }, { homeModeFixture: true, homeOpsFixture: true })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    const layoutReady = await waitForHomeTimelineLayout(page)
    if (!layoutReady) {
      ng.push(`[ops-timeline/${theme}/${viewport.width}px] 画面幅に合う時間軸幅・初期スクロールが安定しない`)
    }
    const geometry = await page.evaluate(() => {
      const rect = (element) => {
        if (!element) return null
        const { x, y, width, height, right, bottom } = element.getBoundingClientRect()
        return { x, y, width, height, right, bottom }
      }
      const timeline = document.querySelector('[data-testid="home-ops-timeline"]')
      const frame = timeline?.querySelector('[data-testid="home-ops-timeline-frame"]') ?? null
      const content = timeline?.querySelector('[data-testid="home-ops-timeline-content"]') ?? null
      const marker = timeline?.querySelector('[data-testid="home-timeline-now"]') ?? null
      const thirtyMinuteBlock = timeline?.querySelector(
        '[data-testid="home-timeline-block"][data-duration-ms="1800000"]',
      ) ?? null
      const rowLabels = [...(timeline?.querySelectorAll('[data-testid="home-timeline-row-label"]') ?? [])]
      const overageLabels = [...(timeline?.querySelectorAll('[data-testid="home-overage-label"]') ?? [])]
      const ticks = [...(timeline?.querySelectorAll('[data-testid="home-timeline-tick"]') ?? [])]
      const home = document.querySelector('[data-testid="page-content"], [data-testid="bounded-page-content"]')
      const controls = [...(home?.querySelectorAll('a, button, input, select, summary, [role="button"], [tabindex="0"]') ?? [])]
        .filter((element) => element.getClientRects().length > 0)
      return {
        timeline: rect(timeline),
        frame: rect(frame),
        marker: rect(marker),
        block: rect(thirtyMinuteBlock),
        documentScrollWidth: document.documentElement.scrollWidth,
        bodyScrollWidth: document.body.scrollWidth,
        viewportWidth: window.innerWidth,
        frameClientWidth: frame?.clientWidth ?? null,
        frameScrollWidth: frame?.scrollWidth ?? null,
        frameScrollLeft: frame?.scrollLeft ?? null,
        contentWidth: content?.getBoundingClientRect().width ?? null,
        hourPx: content === null ? null : content.getBoundingClientRect().width <= 48 * 30 + 64 + 1 ? 30 : 64,
        tickRects: ticks.map((element) => ({ rect: rect(element), visibility: getComputedStyle(element).visibility })),
        rowLabelRects: rowLabels.map(rect),
        overageLabelRects: overageLabels.map(rect),
        controls: controls.map((element) => ({ rect: rect(element), tag: element.tagName })),
      }
    })

    if (geometry.timeline === null || geometry.frame === null) {
      ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸またはスクロール枠が無い`)
    } else {
      if (geometry.documentScrollWidth > geometry.viewportWidth || geometry.bodyScrollWidth > geometry.viewportWidth) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] ページ本体が横にはみ出す`)
      }
      if (!(geometry.frameScrollWidth > geometry.frameClientWidth)) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸の枠内だけの横スクロールが無い`)
      }
      const expectedContentWidth = 48 * (viewport.width <= 480 ? 30 : 64) + 64
      if (geometry.contentWidth === null || Math.abs(geometry.contentWidth - expectedContentWidth) >= 1) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸の縮尺が想定と違う (${geometry.contentWidth}px; expected ${expectedContentWidth}px)`)
      }
      if (geometry.marker === null) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 現在時刻の線が無い`)
      } else if (
        geometry.marker.x < geometry.frame.x ||
        geometry.marker.right > geometry.frame.right ||
        geometry.marker.x < geometry.frame.x - 1
      ) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 現在時刻の線が初期表示範囲に無い`)
      }
      if (viewport.width === mobile.width && (geometry.block === null || geometry.block.width < 12)) {
        ng.push(`[ops-timeline/${theme}/360px] 30 分番組のブロックが 12px 未満`)
      }
      for (const control of geometry.controls) {
        if (control.rect === null || control.rect.width < 24 || control.rect.height < 24) {
          ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸の操作標的が 24×24px 未満 (${control.tag})`)
        }
      }
      if (geometry.overageLabelRects.length !== 2) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 全 jammedTypes の容量超過ラベルが 2 件でない（${geometry.overageLabelRects.length}）`)
      }
      const visibleTicks = geometry.tickRects
        .filter((tick) => tick.visibility !== 'hidden' && tick.rect && tick.rect.right > geometry.frame.x && tick.rect.x < geometry.frame.right)
      if (visibleTicks.some(({ rect: tick }) => tick.x < geometry.frame.x - 0.5 || tick.right > geometry.frame.right + 0.5)) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 初期表示範囲で時刻目盛りラベルがクリップする`)
      }
      for (const label of geometry.overageLabelRects) {
        if (
          label.x < geometry.frame.x ||
          label.right > geometry.frame.right ||
          geometry.rowLabelRects.some((rowLabel) => rowLabel && label.x < rowLabel.right && label.right > rowLabel.x)
        ) {
          ng.push(`[ops-timeline/${theme}/${viewport.width}px] 容量超過ラベルが枠外か行見出しと重なる`)
        }
      }
      log(`  [ops-timeline/${theme}/${viewport.width}px] frame=${geometry.frameClientWidth}px content=${geometry.contentWidth}px (${geometry.hourPx}px/h) scrollLeft=${geometry.frameScrollLeft}px now=${(geometry.marker?.x ?? 0) - geometry.frame.x}px visibleTicks=${visibleTicks.length} hiddenEdgeTicks=${geometry.tickRects.filter((tick) => tick.visibility === 'hidden').length}`)
    }

    {
      const text = ((await page.getByTestId('home-ops-timeline').innerText({ timeout: 3000 }).catch(() => '')) ?? '').replaceAll(/\s+/g, ' ')
      for (const expected of ['今日 0 時 → 明日の終わり', '地デジ / BS ごと', 'チューナー不足の区間', '枠の中を横にスクロールできます']) {
        if (!text.includes(expected)) ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸に「${expected}」が無い`)
      }
    }
    const file = path.join(OUT_DIR, `home-ops-timeline-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)} (${viewport.width}x${viewport.height}, single-site)`)
    await checkMissingStrings(page, `home-ops-timeline/${theme}/${viewport.name}`)
    await context.close()
  }
}

// 警告の順序と overage の説明は、個別の予約を敗者として示さない。
{
  const { context, page } = await open(homeDesktop, 'light', {
    name: 'home-ops-actions',
    path: '/?mode=ops',
  }, { withBreaker: true, homeModeFixture: true, homeOpsFixture: true })
  await page.locator('section[aria-labelledby="home-action-required"]').waitFor({ timeout: 5000 }).catch(() => {})
  const warningRows = page.locator('section[aria-labelledby="home-action-required"] li[data-warning-kind]')
  const warningKinds = await warningRows.evaluateAll((elements) => elements.map((element) => element.dataset.warningKind))
  if (warningKinds.join(',') !== 'breaker,failed,overage,drop') {
    ng.push(`ホーム: 要対応の順序が breaker→failed→overage→drop でない（${warningKinds.join(',')}）`)
  }
  const overageRow = page.locator('li[data-warning-kind="overage"]')
  // 要対応の行が無い実装（M8-25）でも TimeoutError で結果が消えないよう、取れなければ NG に積む。
  const overageText = (await overageRow.innerText({ timeout: 3000 }).catch(() => null))?.replaceAll(/\s+/g, ' ') ?? ''
  if (overageText === '') ng.push('ホーム: チューナー不足の要対応行が無い（時間軸 + 要対応の一覧になっていない）')
  if (!overageText.includes('この時間帯の予約: 大相撲中継')) {
    ng.push(`ホーム: overage の時間帯 subtitle が予約を示していない（${overageText}）`)
  }
  const homeText = (await page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]').innerText()).replaceAll(/\s+/g, ' ')
  if (/(?:チューナー\s*#?\s*\d|tuner\s*#?\s*\d|容量(?:に)?(?:は)?(?:十分|余裕がある)|予約は容量に収まる)/i.test(homeText)) {
    ng.push('ホーム: tuner の個別割当または容量の確約を示す文言がある')
  }
  if (/(?:大相撲中継).{0,20}(?:録画失敗|録れない|失敗する|除外)|(?:録画失敗|録れない|失敗する|除外).{0,20}(?:大相撲中継)/.test(overageText)) {
    ng.push(`ホーム: overage が特定予約を敗者として示している（${overageText}）`)
  }
  await context.close()
}

// 複数サイトは不足区間の site をまたがず、行を site × 種別で分けて描く。
for (const theme of themes) {
  for (const viewport of [homeDesktop, mobile, mobileWide]) {
    const { context, page } = await open(viewport, theme, {
      name: 'home-ops-timeline-multisite',
      path: '/?mode=ops',
    }, { homeModeFixture: true, homeOpsFixture: true, multiSite: true })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    if (!(await waitForHomeTimelineLayout(page))) {
      ng.push(`[ops-timeline-multisite/${theme}/${viewport.width}px] 画面幅に合う時間軸幅・初期スクロールが安定しない`)
    }
    const siteRows = await page.locator('[data-testid="home-timeline-row-label"]').allTextContents()
    if (!siteRows.some((label) => label.includes('default · 地デジ')) ||
        !siteRows.some((label) => label.includes('sub · 地デジ'))) {
      ng.push(`[ops-timeline-multisite/${theme}/${viewport.width}px] site × channelType の行が分離されていない`)
    }
    const file = path.join(OUT_DIR, `home-ops-timeline-multisite-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)} (${viewport.width}x${viewport.height}, multi-site)`)
    await context.close()
  }
}

// 横スクロールバーが常に見える（macOS の自動非表示でも枠の中を横に動かせると分かる）。
// Playwright の既定は --hide-scrollbars でバーの高さが常に 0 になるので、この判定だけ
// そのフラグを外した専用のブラウザで測る。
log('\n=== ホーム管理モード: 横スクロールバーの可視 ===')
{
  const sbBrowser = await launchBrowser('chromium', { ignoreDefaultArgs: ['--hide-scrollbars'] })
  for (const viewport of [homeDesktop, mobile]) {
    const context = await sbBrowser.newContext({
      viewport: { width: viewport.width, height: viewport.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: 'light',
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ homeModeFixture: true, homeOpsFixture: true }))
    await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    const height = await page.getByTestId('home-ops-timeline-frame').evaluate((el) => el.offsetHeight - el.clientHeight).catch(() => 0)
    log(`  [ops-scrollbar/${viewport.width}px] バーの高さ ${height}px`)
    if (height < 4) ng.push(`[ops-scrollbar/${viewport.width}px] 横スクロールバーが見えない（高さ ${height}px）`)
    await context.close()
  }
  await sbBrowser.close()
}

// 窓幅を連続して変えたときに縮尺（30 / 64px/h）が往復しない。切替が時間軸の枠の幅
// （= 切替で変わるラベル幅に依存）で決まっていた版は、単一 site で 592–598px、複数
// site で 660–676px で 2 つのモードを往復した（レビュー実測）。
log('\n=== ホーム管理モード: 窓幅での縮尺の安定 ===')
for (const multiSite of [false, true]) {
  const { context, page } = await open({ name: 'home-ops-resize', width: 700, height: 900 }, 'light', {
    name: 'home-ops-resize',
    path: '/?mode=ops',
  }, { homeModeFixture: true, homeOpsFixture: true, multiSite })
  await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
  const widths = multiSite ? [656, 660, 664, 668, 672, 676, 680] : [588, 592, 594, 596, 598, 600, 604]
  const seen = []
  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 })
    await page.waitForTimeout(150)
    const samples = new Set()
    for (let i = 0; i < 10; i += 1) {
      samples.add(await page.evaluate(() => {
        const section = document.querySelector('[data-testid="home-ops-timeline"]')
        const content = document.querySelector('[data-testid="home-ops-timeline-content"]')
        return `${section?.getAttribute('data-hour-px')}/${Math.round(content?.getBoundingClientRect().width ?? -1)}`
      }))
      await page.waitForTimeout(50)
    }
    seen.push(`${width}px=${[...samples].join('|')}`)
    if ([...samples].some((sample) => sample.startsWith('undefined'))) {
      ng.push(`[ops-resize/${multiSite ? 'multi' : 'single'}/${width}px] 時間軸が描かれていない（縮尺を測れない）`)
    } else if (samples.size !== 1) {
      ng.push(`[ops-resize/${multiSite ? 'multi' : 'single'}/${width}px] 縮尺が往復する（${[...samples].join(' ⇄ ')}）`)
    }
  }
  log(`  [ops-resize/${multiSite ? 'multi' : 'single'}] ${seen.join(' ')}`)
  await context.close()
}

/** visibleTickLabels は時間軸の枠の中に見えている目盛りラベル（文言と中心の x）を返す。 */
async function visibleTickLabels(page) {
  return page.evaluate(() => {
    const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')?.getBoundingClientRect()
    if (!frame) return []
    return [...document.querySelectorAll('[data-testid="home-timeline-tick"]')]
      .filter((tick) => getComputedStyle(tick).visibility !== 'hidden')
      .map((tick) => ({ text: tick.textContent, rect: tick.getBoundingClientRect() }))
      .filter(({ rect }) => rect.x >= frame.x - 0.5 && rect.right <= frame.right + 0.5)
      .map(({ text, rect }) => ({ text, center: rect.x + rect.width / 2 }))
  })
}

// 時刻（午前を含む）とスクロール位置を変えても、見える範囲の境界で目盛りが半端に
// 切れず、「いま」の線が窓の中の正しい位置（現在時刻 × 縮尺）にある。
// 窓の始点が今日 12 時固定だと、午前では「いま」が窓の左端に張り付く。
log('\n=== ホーム管理モード: 午前の「いま」と目盛りの切れ ===')
for (const viewport of [homeDesktop, mobile]) {
  const hourPx = viewport.width <= 480 ? 30 : 64
  const { context, page } = await open(viewport, 'light', {
    name: 'home-ops-ticks',
    path: '/?mode=ops',
  }, { homeModeFixture: true, homeOpsFixture: true })
  for (const clock of ['09:00', '14:17', '20:42', '23:50']) {
    const now = new Date(`2026-08-12T${clock}:00+09:00`)
    await page.clock.setFixedTime(now)
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    await page.waitForFunction(
      (expected) => Math.abs((document.querySelector('[data-testid="home-ops-timeline-content"]')?.getBoundingClientRect().width ?? 0) - expected) < 1,
      48 * hourPx + 64,
      { timeout: 5000 },
    ).catch(() => ng.push(`[ops-ticks/${viewport.width}px/${clock}] 窓が 48 時間でない`))
    const expectedNowX = (now.getHours() + now.getMinutes() / 60) * hourPx
    const measured = await page.evaluate(() => {
      const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
      const content = document.querySelector('[data-testid="home-ops-timeline-content"]')
      const marker = document.querySelector('[data-testid="home-timeline-now"]')
      if (!frame || !content || !marker) return null
      return {
        nowInContent: marker.getBoundingClientRect().x - content.getBoundingClientRect().x,
        nowInFrame: marker.getBoundingClientRect().x - frame.getBoundingClientRect().x,
      }
    })
    if (measured === null || Math.abs(measured.nowInContent - expectedNowX) > 1.5) {
      ng.push(`[ops-ticks/${viewport.width}px/${clock}] 「いま」の線が現在時刻の位置にない（${JSON.stringify(measured)}、期待 ${expectedNowX}px）`)
    }
    // 初期表示（現在 − 3 時間から）で見えている目盛り。ラフ（20:42 / 1280px）は
    // 過去側に「18時」が見える。端から 20px 以内の目盛りを固定箱で隠すと消えていた。
    const initialTicks = await visibleTickLabels(page)
    log(`  [ops-ticks/${viewport.width}px/${clock}] 初期表示の目盛り: ${initialTicks.map((tick) => tick.text).join(' ') || '（なし）'}`)
    if (clock === '20:42') {
      const markerX = await page.getByTestId('home-timeline-now').first().evaluate((el) => el.getBoundingClientRect().x).catch(() => null)
      if (markerX === null || !initialTicks.some((tick) => tick.center < markerX)) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}] 初期表示の過去側に目盛りが 1 本も無い（${initialTicks.map((tick) => tick.text).join(',')}）`)
      }
      if (viewport.width === homeDesktop.width && !initialTicks.some((tick) => tick.text === '18時')) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}] 初期表示に「18時」が見えない（ラフでは見える）`)
      }
    }
    for (const scrollLeft of [0, 37, 101, 333, 100000]) {
      await page.evaluate((x) => {
        const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
        if (frame) frame.scrollLeft = x
      }, scrollLeft)
      await page.waitForTimeout(100)
      const cut = await page.evaluate(() => {
        const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
        if (!frameElement) return ['時間軸の枠が無い']
        const frame = frameElement.getBoundingClientRect()
        return [...document.querySelectorAll('[data-testid="home-timeline-tick"]')]
          .filter((tick) => getComputedStyle(tick).visibility !== 'hidden')
          .map((tick) => ({ text: tick.textContent, rect: tick.getBoundingClientRect() }))
          .filter(({ rect }) => rect.right > frame.x && rect.x < frame.right)
          .filter(({ rect }) => rect.x < frame.x - 0.5 || rect.right > frame.right + 0.5)
          .map(({ text }) => text)
      })
      if (cut.length > 0) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}/scrollLeft=${scrollLeft}] 見える範囲の境界で目盛りが切れて見える（${cut.join(',')}）`)
      }
      // 「いま」の pill（z-20）の下に隠れる目盛りは出さない。
      const underPill = await page.evaluate(() => {
        const pill = document.querySelector('[data-testid="home-timeline-now-label"]')?.getBoundingClientRect()
        if (!pill) return ['pill が無い']
        return [...document.querySelectorAll('[data-testid="home-timeline-tick"]')]
          .filter((tick) => getComputedStyle(tick).visibility !== 'hidden')
          .map((tick) => ({ text: tick.textContent, rect: tick.getBoundingClientRect() }))
          .filter(({ rect }) => rect.right > pill.x && rect.x < pill.right)
          .map(({ text }) => text)
      })
      if (underPill.length > 0) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}/scrollLeft=${scrollLeft}] 目盛りが「いま」の pill の下に隠れる（${underPill.join(',')}）`)
      }
    }
    log(`  [ops-ticks/${viewport.width}px/${clock}] now@content=${measured?.nowInContent?.toFixed(1)}px (期待 ${expectedNowX}px)`)
  }
  await context.close()
}

// 0 時をまたぐ録画は 0:00 から終わるまで時間軸に残る。API の `from` は開始時刻で
// 絞るので、窓の始点を `from` に渡すと前日 23:40 開始の録画中が消えていた。
log('\n=== ホーム管理モード: 0 時をまたぐ録画 ===')
{
  const midnight = new Date('2026-08-13T00:20:00+09:00')
  const crossingStart = new Date('2026-08-12T23:40:00+09:00').getTime()
  const crossing = [
    { ...recordings[1], id: 91, title: '日またぎの映画', status: 'recording', startAt: iso(crossingStart), startedAt: iso(crossingStart), durationMs: 140 * 60_000, createdAt: iso(crossingStart) },
    { ...recordings[1], id: 92, title: '日またぎの完了', status: 'finished', startAt: iso(crossingStart - 10 * 60_000), durationMs: 40 * 60_000, createdAt: iso(crossingStart), dropSummary: undefined },
  ]
  for (const viewport of [homeDesktop, mobile]) {
    const hourPx = viewport.width <= 480 ? 30 : 64
    const { context, page } = await open(viewport, 'light', {
      name: 'home-ops-midnight',
      path: '/?mode=ops',
    }, { homeModeFixture: true, homeOpsFixture: true, extraOpsRecordings: crossing })
    await page.clock.setFixedTime(midnight)
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    await page.evaluate(() => document.fonts.ready)
    await page.waitForTimeout(300)
    const blocks = await page.evaluate(() => {
      const content = document.querySelector('[data-testid="home-ops-timeline-content"]')?.getBoundingClientRect()
      return [...document.querySelectorAll('[data-testid="home-timeline-block"]')].map((block) => {
        const rect = block.getBoundingClientRect()
        return { kind: block.dataset.kind, title: block.getAttribute('title'), left: rect.x - (content?.x ?? 0), width: rect.width }
      })
    })
    for (const [title, kind, visibleMinutes] of [['日またぎの映画', 'recording', 120], ['日またぎの完了', 'finished', 10]]) {
      const block = blocks.find((b) => b.title === title)
      if (block === undefined || block.kind !== kind) {
        ng.push(`[ops-midnight/${viewport.width}px] 0 時をまたぐ「${title}」（${kind}）が 00:20 の時間軸に無い（${blocks.map((b) => `${b.kind}:${b.title}`).join(', ')}）`)
      } else if (Math.abs(block.left) > 0.5 || Math.abs(block.width - (visibleMinutes / 60) * hourPx) > 1) {
        ng.push(`[ops-midnight/${viewport.width}px] 「${title}」が窓の左端で切られていない（left=${block.left}px width=${block.width}px、期待 0px / ${(visibleMinutes / 60) * hourPx}px）`)
      }
    }
    log(`  [ops-midnight/${viewport.width}px] ${blocks.map((b) => `${b.kind}:${b.title}@${b.left.toFixed(1)}+${b.width.toFixed(1)}`).join(' ')}`)
    await context.close()
  }
}

// 管理モードのホームから完了録画へ: 「録画・予約の詳細」を開く → 行を選ぶ の 2 操作。
// 「直近の完了へ」アンカーは置かない（docs/frontend/home.md）。
log('\n=== ホーム管理モード: 完了録画への到達 ===')
for (const viewport of [homeDesktop, mobile]) {
  const { context, page } = await open(viewport, 'light', {
    name: 'home-ops-reach',
    path: '/?mode=ops',
  }, { homeModeFixture: true, homeOpsFixture: true })
  const summary = page.getByTestId('home-timeline-details').locator('summary')
  await summary.waitFor({ timeout: 5000 }).catch(() => {})
  let operations = 0
  if (await summary.count() === 1) {
    await summary.click()
    operations += 1
    const row = page.getByTestId('home-timeline-detail-row').filter({ hasText: '録れた' }).first()
    if (await row.count() === 1) {
      await row.locator('a').click()
      operations += 1
      await page.waitForURL(/\/recordings\/\d+$/, { timeout: 5000 }).catch(() => {})
    }
  }
  const pathname = new URL(page.url()).pathname
  if (!/^\/recordings\/\d+$/.test(pathname) || operations !== 2) {
    ng.push(`[ops-reach/${viewport.width}px] 管理モードのホームから 2 操作で完了録画へ着かない（操作=${operations} 到達=${pathname}）`)
  }
  log(`  [ops-reach/${viewport.width}px] 操作=${operations} 到達=${pathname}`)
  await context.close()
}

// 時計更新と recordings SSE による親画面の再描画後も、手動スクロールを奪わず
// 「いま」の線だけが時間経過分だけ進むことを実ブラウザで確認する。
log('\n=== ホーム管理モード: 手動スクロール保持と現在時刻追従 ===')
for (const viewport of [homeDesktop, mobile]) {
  let releaseSse
  let markSseRequested
  const sseGate = new Promise((resolve) => { releaseSse = resolve })
  const sseRequested = new Promise((resolve) => { markSseRequested = resolve })
  const { context, page } = await open(viewport, 'light', {
    name: 'home-ops-follow',
    path: '/?mode=ops',
  }, {
    homeModeFixture: true,
    homeOpsFixture: true,
    homeOpsSseFixture: async () => {
      markSseRequested()
      await sseGate
    },
  })
  const frame = page.getByTestId('home-ops-timeline-frame')
  await frame.waitFor({ timeout: 5000 }).catch(() => {})
  if (!(await waitForHomeTimelineLayout(page))) {
    ng.push(`[ops-follow/${viewport.width}px] 初期の時間軸レイアウトが安定しない`)
    releaseSse()
    await context.close()
    continue
  }
  await Promise.race([
    sseRequested,
    page.waitForTimeout(5000).then(() => { throw new Error('SSE request timeout') }),
  ]).catch(() => ng.push(`[ops-follow/${viewport.width}px] recordings SSE 接続が始まらない`))

  const hourPx = viewport.width <= 480 ? 30 : 64
  const initial = await page.evaluate(() => {
    const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
    const marker = document.querySelector('[data-testid="home-timeline-now"]')
    if (!frameElement || !marker) return null
    return {
      scrollLeft: frameElement.scrollLeft,
      markerX: marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x,
    }
  })
  if (initial === null || Math.abs(initial.markerX - 3 * hourPx) > 1) {
    ng.push(`[ops-follow/${viewport.width}px] 初期表示が現在時刻の約3時間前から始まらない (${JSON.stringify(initial)})`)
  }

  await frame.evaluate((element) => { element.scrollLeft += 48 })
  const manual = await page.evaluate(() => {
    const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
    const marker = document.querySelector('[data-testid="home-timeline-now"]')
    if (!frameElement || !marker) return null
    return {
      scrollLeft: frameElement.scrollLeft,
      markerX: marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x,
    }
  })
  await page.clock.setFixedTime(new Date(FIXED_NOW.getTime() + 60 * 60_000))
  releaseSse()
  const followed = await page.waitForFunction(
    ({ expectedScrollLeft, expectedMarkerX }) => {
      const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
      const marker = document.querySelector('[data-testid="home-timeline-now"]')
      if (!frameElement || !marker) return false
      const markerX = marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x
      return Math.abs(frameElement.scrollLeft - expectedScrollLeft) < 1 &&
        Math.abs(markerX - expectedMarkerX) < 1
    },
    {
      expectedScrollLeft: manual?.scrollLeft ?? Number.NaN,
      expectedMarkerX: (manual?.markerX ?? 0) + hourPx,
    },
    { timeout: 5000 },
  ).then(() => true).catch(() => false)
  if (manual === null || !followed) {
    const actual = await page.evaluate(() => {
      const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
      const marker = document.querySelector('[data-testid="home-timeline-now"]')
      if (!frameElement || !marker) return null
      return {
        scrollLeft: frameElement.scrollLeft,
        markerX: marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x,
      }
    })
    ng.push(`[ops-follow/${viewport.width}px] SSE 後に手動スクロールを保持して現在時刻だけ進まない (manual=${JSON.stringify(manual)}, actual=${JSON.stringify(actual)})`)
  } else {
    log(`  [ops-follow/${viewport.width}px] 初期now=${initial?.markerX}px 手動scrollLeft=${manual.scrollLeft}px → SSE後scrollLeft維持 / now +${hourPx}px`)
  }
  await context.close()
}

// 初回アクセス（URL も localStorage も空）の既定は「見る」。既存の管理側
// シナリオは上の mode=ops で維持し、既定値だけは `/` を直接開いて確認する。
{
  const screen = { name: 'home-default', path: '/', wait: 'text=続きから再生' }
  const { context, page } = await open(homeDesktop, 'light', screen, { homeModeFixture: true })
  const watchLink = page.getByRole('link', { name: '見る', exact: true })
  const opsLink = page.getByRole('link', { name: '管理', exact: false })
  if ((await watchLink.getAttribute('aria-current')) !== 'page') {
    ng.push('home/default: URL・localStorage が空なのに既定が「見る」でない')
  }
  if ((await opsLink.getAttribute('aria-current')) === 'page') {
    ng.push('home/default: URL・localStorage が空なのに「管理」が選択されている')
  }
  await context.close()
}

// ブレーカー発動中の「見る」帯も desktop/phone・ライト/ダークで撮る。
for (const theme of themes) {
  for (const viewport of [homeDesktop, mobile]) {
    const screen = { name: 'home-watch-breaker', path: '/?mode=watch' }
    const { context, page } = await open(viewport, theme, screen, {
      withBreaker: true,
      homeModeFixture: true,
    })
    if ((await page.getByTestId('home-watch-breaker-band').count()) === 0) {
      ng.push(`[home-watch-breaker/${theme}/${viewport.width}px] 見る側のブレーカー帯が無い`)
    }
    if ((await page.getByText('削除が保留されています', { exact: false }).count()) > 0) {
      ng.push(`[home-watch-breaker/${theme}/${viewport.width}px] 共通バナーと見る側の帯が重複する`)
    }
    const file = path.join(OUT_DIR, `home-watch-breaker-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `home-watch-breaker/${theme}/${viewport.name}`)
    await context.close()
  }
}

// --- 番組ハブ: 400px / デスクトップの 3 塊と操作 -------------------------
//
// 全画面ショットは 360px の共通モバイル幅で揃え、ここでは受け入れ条件の 400px 幅と
// デスクトップ幅の両方で同じ判定を回す。API は上の seriesHubRecordings を使うので、最新の
// 失敗回を飛ばす主ボタン、自動キー、次回、分類メニューを実ブラウザで確認できる。
for (const hubViewport of [seriesHubMobile, seriesHubDesktop])
for (const theme of themes) {
  const { context, page } = await open(hubViewport, theme, screenOf('series-hub'))
  const identity = page.getByRole('region', { name: 'シリーズ情報' })
  const actions = page.getByRole('region', { name: 'シリーズの操作' })
  const episodes = page.getByRole('region', { name: 'このシリーズの録画' })
  const blocks = await Promise.all([identity, actions, episodes].map((block) => block.boundingBox()))
  if (blocks.some((box) => box === null)) {
    ng.push(`[${theme}/${hubViewport.name}] 3 つの塊の矩形を取得できない`)
  } else {
    const [identityBox, actionsBox, episodesBox] = blocks
    const identityBottom = identityBox.y + identityBox.height
    const actionsBottom = actionsBox.y + actionsBox.height
    if (!(identityBox.y < actionsBox.y && actionsBox.y < episodesBox.y)) {
      ng.push(`[${theme}/${hubViewport.name}] 3 つの塊が上から識別・行動・エピソードの順でない`)
    }
    if (actionsBox.y - identityBottom < 24 || episodesBox.y - actionsBottom < 24) {
      ng.push(`[${theme}/${hubViewport.name}] 3 つの塊の間隔が 24px 未満`)
    }
  }
  const primary = actions.locator('[class~="bg-primary"]')
  if (await primary.count() !== 1) {
    ng.push(`[${theme}/${hubViewport.name}] 塗りの主ボタンが 1 つでない（${await primary.count()} 件）`)
  }
  const shot = path.join(OUT_DIR, `${hubViewport.name}-${theme}.png`)
  await page.screenshot({ path: shot })
  log(`  ${path.basename(shot)}`)
  await checkMissingStrings(page, `${hubViewport.name}/${theme}`)

  const trigger = page.getByRole('button', { name: 'シリーズのその他の操作' })
  await trigger.click()
  const menu = page.getByRole('menuitem', { name: '分類を直す（割る・指定する）' })
  const menuVisible = await menu.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  if (!menuVisible) {
    ng.push(`[${theme}/${hubViewport.name}] 分類を直すメニューが開かない`)
  } else {
    const menuBox = await menu.boundingBox()
    if (
      menuBox === null ||
      menuBox.x < 0 ||
      menuBox.x + menuBox.width > hubViewport.width ||
      menuBox.y < 0 ||
      menuBox.y + menuBox.height > hubViewport.height
    ) {
      ng.push(`[${theme}/${hubViewport.name}] 分類メニューがビューポートからはみ出す`)
    }
    const menuShot = path.join(OUT_DIR, `series-hub-menu-${hubViewport.name.replace('series-hub-', '')}-${theme}.png`)
    await page.screenshot({ path: menuShot })
    log(`  ${path.basename(menuShot)}`)
  }
  await context.close()

  // 主ボタンの隣に置いた「毎回録画する」が、検索結果とルール作成節を持つ
  // `/search?cond=...` へ実際に着地することも同じ Chromium で確認する。
  const searchContext = await open(hubViewport, theme, screenOf('series-hub'))
  const recurring = searchContext.page.getByRole('link', { name: '毎回録画する' })
  if ((await recurring.count()) === 0) {
    ng.push(`[${theme}/${hubViewport.name}] 「毎回録画する」リンクが見つからない`)
  } else {
    await recurring.click()
    const searchLoaded = await searchContext.page
      .waitForURL('**/search?cond=*', { timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    if (!searchLoaded) {
      ng.push(`[${theme}/${hubViewport.name}] 「毎回録画する」の検索 URL に着地しない`)
    } else {
      const results = searchContext.page.getByRole('region', { name: '検索結果' })
      const createRule = searchContext.page.getByText('この条件でルールを作成').first()
      if ((await results.count()) === 0 || (await createRule.count()) === 0) {
        ng.push(`[${theme}/${hubViewport.name}] 検索結果またはルール作成節が表示されない`)
      }
    }
  }
  await searchContext.context.close()
}

// --- ①-B シリーズ一覧の格子とリスト ---
//
// 格子（2 列 / 4 列）とリストの切替、16:9 のサムネイル、横はみ出しの無さは
// レイアウトの実測でしか分からない（jsdom は測れない）。サムネイルは 404 に
// 落としてあるので、画像が無いときの代替表示（bg-muted。走査線にしない）も測る。
log('\n=== ①-B シリーズ一覧の格子とリスト ===')
for (const viewport of [mobile, desktop]) {
  for (const view of ['card', 'list']) {
    const label = `series/${view}/${viewport.name}`
    const { context, page } = await open(viewport, 'light', screenOf('series'), { recordingView: view })
    await page.screenshot({ path: path.join(OUT_DIR, `series-${view}-${viewport.name}.png`) })
    const m = await page.locator('[data-testid="series-shelf"]').evaluateAll((items) => {
      const rects = items.map((li) => li.getBoundingClientRect())
      const ratios = items.map((li) => {
        const r = li.querySelector('a > span').getBoundingClientRect()
        return r.width / r.height
      })
      const placeholder = items[0].querySelector('a > span > span')
      return {
        n: items.length,
        columns: new Set(rects.map((r) => Math.round(r.left))).size,
        maxRight: Math.max(...rects.map((r) => r.right)),
        ratios,
        docOverflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        placeholderClass: placeholder?.className ?? null,
        placeholderBg: placeholder ? getComputedStyle(placeholder).backgroundColor : null,
        metaSizes: [...document.querySelectorAll('[data-testid="series-shelf-meta"]')].map((el) => getComputedStyle(el).fontSize),
      }
    })
    log(`  ${label}: タイル=${m.n} 列=${m.columns} 右端=${m.maxRight.toFixed(1)} 比=${m.ratios[0]?.toFixed(3)}`)
    if (m.n !== 4) ng.push(`${label}: タイルが 4 件でない（${m.n}。NULL の棚は出さない）`)
    const expectedColumns = view === 'list' ? 1 : viewport === mobile ? 2 : 4
    if (m.columns !== expectedColumns) ng.push(`${label}: 列数が ${expectedColumns} でない（${m.columns}）`)
    if (m.docOverflow || m.maxRight > viewport.width + 0.5) ng.push(`${label}: 横にはみ出している（右端 ${m.maxRight}px）`)
    if (m.ratios.some((r) => Math.abs(r - 16 / 9) > 0.01)) ng.push(`${label}: サムネイルが 16:9 でない（${m.ratios.map((r) => r.toFixed(3)).join(', ')}）`)
    if (m.placeholderClass === null || /scanlines/.test(m.placeholderClass) || m.placeholderBg === 'rgba(0, 0, 0, 0)') {
      ng.push(`${label}: 画像が無いときの代替表示が bg-muted の塗りでない（class=${m.placeholderClass} bg=${m.placeholderBg}）`)
    }
    if (m.metaSizes.some((size) => size !== '14px')) ng.push(`${label}: メタが text-sm でない（${m.metaSizes.join(', ')}）`)
    await context.close()
  }
}

// ストレージ階層は既定で畳むため、展開状態も画面幅・テーマごとに別途撮る。
for (const viewport of viewports) {
  for (const theme of themes) {
    const { context, page } = await open(viewport, theme, screenOf('recordings'))
    await page.locator('main > header details summary').click()
    const file = path.join(OUT_DIR, `recordings-storage-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `recordings-storage/${theme}/${viewport.name}`)
    await context.close()
  }
}
// 番組表グリッド（`lg` 以上でしか出ない。現在時刻線・容量超過の帯・ジャンル淡色が
// 一度に並ぶ画面）とブレーカー発動中（destructive の帯）は別途 1 枚ずつ。
for (const theme of themes) {
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const grid = page.getByRole('button', { name: '番組表' })
    if ((await grid.count()) > 0) {
      await grid.first().click()
      await page.waitForTimeout(1200)
      const file = path.join(OUT_DIR, `programs-grid-${theme}-desktop.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(page, `programs-grid/${theme}`)
    } else {
      log(`  （programs-grid-${theme}: 表示形式の切り替えが出ていないので撮らない）`)
    }
    await context.close()
  }
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'), { withBreaker: true })
    const file = path.join(OUT_DIR, `breaker-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `breaker/${theme}`)
    await context.close()
  }
  {
    // issue #467 の罠: 詳細ページを PageHeader（sticky + `--sticky-banners-height`
    // の top）に乗せたので、ブレーカーバナー表示中のレイアウトが崩れていないかを
    // ここで見る（一覧と違う独自ヘッダを持っていたころは撮っていなかった）。
    const { context, page } = await open(desktop, theme, recordingDetailScreen, { withBreaker: true })
    const file = path.join(OUT_DIR, `breaker-recording-detail-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `breaker-recording-detail/${theme}`)
    await context.close()
  }
  {
    // 読み込み中（Skeleton / ListSkeleton の走査線）を撮る。API が即座に
    // 返る作りだと画面遷移からスクリーンショットの間に必ず解決してしまうので、
    // `/api/recordings` だけ遅延させる。`open()` の `wait` ロケータは
    // 解決後の状態を待つ設計なので、ここは自前でナビゲートする
    const context = await browser.newContext({
      viewport: { width: desktop.width, height: desktop.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ delayPath: '/api/recordings', delayMs: 5000 }))
    await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
    await page
      .locator('.scanlines')
      .first()
      .waitFor({ timeout: 5000 })
      .catch(() => {
        ng.push(`[${theme}] 読み込み中の走査線（.scanlines）が出ない`)
      })
    const file = path.join(OUT_DIR, `loading-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `loading/${theme}`)
    await context.close()
  }
  {
    // 空状態（EmptyState）の文言が実際に読める位置のショット。検索フォームの下に
    // あるため、既定ショットだけではビューポートの下端で文言が切れることがある。
    // 走査線の上の文字が読めるかを判断できるよう、対象までスクロールした 1 組を足す
    const { context, page } = await open(desktop, theme, screenOf('search'))
    const empty = page
      .locator('div.scanlines', { hasText: '条件を指定して検索してください' })
      .first()
    await empty.scrollIntoViewIfNeeded()
    await page.waitForTimeout(100)
    const file = path.join(OUT_DIR, `empty-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `empty/${theme}`)
    await context.close()
  }
  {
    // ホーム（M8-3）の「全セクションが空」= 単一の空状態（EmptyState の走査線）。
    // `home-*-desktop.png`（既定の 4 セクション表示）と対にして人が見比べられる
    // ようにする。
    const context = await browser.newContext({
      viewport: { width: desktop.width, height: desktop.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ emptyHome: true }))
    await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
    await page
      .locator('div.scanlines', { hasText: '表示できる項目がありません' })
      .first()
      .waitFor({ timeout: 5000 })
      .catch(() => {
        ng.push(`[${theme}] ホームの空状態（全セクション空）が出ない`)
      })
    const file = path.join(OUT_DIR, `home-empty-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `home-empty/${theme}`)
    await context.close()
  }
}

// issue #978: レビュー用に録画詳細の4状態をデスクトップ / モバイルで記録する。
for (const scenario of ['completed', 'recording', 'encode-waiting', 'trash']) {
  for (const viewport of viewports) {
    const pointer = viewport === mobile ? 'coarse' : 'fine'
    const { context, page } = await open(viewport, 'light', recordingDetailScreen, {
      pointer,
      multiSite: true,
      recordingDetailScenario: scenario,
    })
    const file = path.join(OUT_DIR, `recording-detail-${scenario}-${viewport.name}.jpg`)
    await page.evaluate(() => {
      for (const element of document.querySelectorAll('*')) {
        element.scrollTop = 0
      }
      document.scrollingElement?.scrollTo({ top: 0, left: 0, behavior: 'instant' })
      window.scrollTo({ top: 0, left: 0, behavior: 'instant' })
    })
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    const scrollPosition = await page.evaluate(() => ({
      window: window.scrollY,
      main: document.querySelector('main')?.scrollTop ?? 0,
    }))
    if (scrollPosition.window !== 0 || scrollPosition.main !== 0) {
      ng.push(
        `recording-detail/${scenario}/${viewport.name}: 撮影前のスクロール位置が先頭でない ` +
          `(window=${scrollPosition.window}, main=${scrollPosition.main})`,
      )
    }
    const pageHeadingTop = await page
      .getByRole('heading', { name: '録画の詳細' })
      .evaluate((element) => element.getBoundingClientRect().top)
      .catch(() => null)
    if (pageHeadingTop === null || pageHeadingTop < 0 || pageHeadingTop > viewport.height) {
      ng.push(
        `recording-detail/${scenario}/${viewport.name}: ページ見出しが撮影範囲にない ` +
          `(top=${pageHeadingTop ?? '取得不能'})`,
      )
    }
    const titleRow = page.locator('[data-testid="recording-title-row"]')
    if ((await titleRow.getByText('完了', { exact: true }).count()) > 0) {
      ng.push(`recording-detail/${scenario}/${viewport.name}: 見出しに完了バッジが残っている`)
    }
    if (scenario === 'encode-waiting' && (await titleRow.getByText('準備中', { exact: true }).count()) === 0) {
      ng.push(`recording-detail/${viewport.name}: エンコード待ちの録画に準備中が出ない`)
    }
    if (scenario === 'trash' && (await titleRow.getByText('再生不可', { exact: true }).count()) > 0) {
      ng.push(`recording-detail/trash/${viewport.name}: ごみ箱の見出しに結論バッジがある`)
    }
    await page.screenshot({
      path: file,
      type: 'jpeg',
      quality: 88,
      fullPage: viewport.name === 'desktop',
    })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(page, `recording-detail/${scenario}/${viewport.name}`)
    await context.close()
  }
}

// Issue #1112: desktop / 360px のリスト・カードと録画詳細で結論の表示を確認する。
log('\n=== 録画結論: desktop / 360px、list / card ===')
for (const viewport of [desktop, mobile]) {
  for (const recordingView of ['list', 'card']) {
    const { context, page } = await open(viewport, 'light', screenOf('recordings'), {
      recordingView,
      pointer: viewport === mobile ? 'coarse' : 'fine',
    })
    const row = page
      .locator('main li')
      .filter({ has: page.getByText('連続テレビ小説', { exact: true }) })
      .first()
    const rowReady = await row.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
    if (!rowReady) {
      ng.push(`recordings/${viewport.name}/${recordingView}: 対象の録画行が無い`)
    } else {
      if ((await row.getByText('準備中', { exact: true }).count()) === 0) {
        ng.push(`recordings/${viewport.name}/${recordingView}: 取り込み中の録画に準備中が出ない`)
      }
      if ((await row.getByText('完了', { exact: true }).count()) > 0) {
        ng.push(`recordings/${viewport.name}/${recordingView}: 視聴状態に完了バッジがある`)
      }
    }
    const documentWidth = await page.evaluate(() => document.documentElement.scrollWidth)
    if (documentWidth > viewport.width) {
      ng.push(`recordings/${viewport.name}/${recordingView}: 横はみ出し ${documentWidth}px`)
    }
    const file = path.join(OUT_DIR, `recording-verdict-${viewport.name}-${recordingView}.png`)
    await page.screenshot({ path: file, fullPage: true })
    log(`  ${path.basename(file)}`)
    await context.close()
  }
}

// --- ①-A' issue #686: 視聴対象への到達距離 -------------------------------
//
// 変更前の基準値は 360/390px が録画詳細リンクの viewport 上端約 317px、
// デスクトップが約 245px だった。ここでは固定時刻・同じ API モックで状態を分け、
// ①リンクの viewport Y、②固定ヘッダー/ボトムナビに隠れないこと、③必要スクロール量を
// 録画一覧（/recordings）で測る。管理モードのホームから完了録画への到達は
// 「ホーム管理モード: 完了録画への到達」が測る。
// 数値は docs/frontend/recordings.md にも結果として記録するが、合否の権威はここ。
log('\n=== ①-A\' issue #686 視聴対象への到達距離 ===')
const layoutScenarios = [
  { name: 'normal', label: '正常' },
  { name: 'capacity', label: '満杯見込み' },
  { name: 'stale', label: '古い観測' },
  { name: 'storage-failure', label: 'ストレージ取得失敗' },
  { name: 'no-observation', label: '観測なし' },
  { name: 'encode-queue', label: 'エンコード待機/実行中' },
  { name: 'many-warnings', label: '警告多数' },
]
const layoutViewports = [desktop, mobile, mobileWide]
const layoutMetrics = new Map()

for (const scenario of layoutScenarios) {
  for (const viewport of layoutViewports) {
    const { context, page } = await open(viewport, 'light', screenOf('recordings'), {
      layoutScenario: scenario.name,
    })
    const target = page.locator('main a[href^="/recordings/"]').first()
    await target.waitFor({ timeout: 5000 }).catch(() => {})
    if ((await target.count()) === 0) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 最初の録画詳細リンクが無い`)
      await context.close()
      continue
    }
    const targetMetrics = await target.evaluate((el) => {
      const targetRect = el.getBoundingClientRect()
      const headerRect = document.querySelector('main > header')?.getBoundingClientRect()
      const bottomRect = document.querySelector('[data-testid="bottom-nav"]')?.getBoundingClientRect()
      const visibleTop = headerRect?.bottom ?? 0
      const visibleBottom = window.innerHeight - (bottomRect?.height ?? 0)
      return {
        y: targetRect.top,
        bottom: targetRect.bottom,
        headerBottom: visibleTop,
        visibleBottom,
        requiredScroll: Math.max(0, targetRect.bottom - visibleBottom, visibleTop - targetRect.top),
      }
    })
    // 管理情報行は常に DOM に存在し、`empty:hidden`（recordings.tsx）が子ノード
    // 0 個のときだけ `display: none` にする。`boundingBox()` は非表示要素に
    // 対して null を返す（Playwright の契約）ので、存在確認を挟まず直接呼べる。
    const managementSummary = page.locator('[data-testid="recordings-management-summary"]')
    const managementBox = await managementSummary.boundingBox()
    const summaryLocator = page.locator('main > header details summary').first()
    const summaryText = (await summaryLocator.count()) > 0
      ? await summaryLocator.textContent()
      : ''
    const metric = {
      ...targetMetrics,
      managementHeight: managementBox?.height ?? null,
      summary: summaryText?.replaceAll(/\s+/g, ' ').trim() ?? '',
    }
    layoutMetrics.set(`${scenario.name}/${viewport.name}`, metric)
    log(
      `  録画一覧/${scenario.label}/${viewport.name}: ` +
        `Y=${metric.y.toFixed(1)}px scroll=${metric.requiredScroll.toFixed(1)}px ` +
        `管理行=${metric.managementHeight?.toFixed(1) ?? '—'}px`,
    )
    if (metric.y < metric.headerBottom - 0.5) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 録画リンクが固定ヘッダーに隠れる`)
    }
    if (metric.bottom > metric.visibleBottom + 0.5) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 録画リンクがボトムナビに隠れる`)
    }
    if (scenario.name === 'normal') {
      const yLimit = viewport.name === 'desktop' ? 230 : 280
      if (metric.y > yLimit) {
        ng.push(
          `録画一覧/正常/${viewport.name}: 正常時の到達距離が短縮されていない` +
            `（Y=${metric.y.toFixed(1)}px、上限 ${yLimit}px）`,
        )
      }
      if (metric.managementHeight === null || metric.managementHeight > 45) {
        ng.push(
          `録画一覧/正常/${viewport.name}: 管理情報が 1 行に収まっていない` +
            `（${metric.managementHeight?.toFixed(1) ?? '—'}px）`,
        )
      }
      if (metric.summary.includes('の見込み') || metric.summary.includes('観測:')) {
        ng.push(`録画一覧/正常/${viewport.name}: 正常時の予測/観測を summary に常置している`)
      }
      // B: 展開時のレイアウト崩れ（`open:basis-full` / 展開グリッドの二重
      // パディング）はスクリーンショット（撮るだけ）にしか触れていなかったので、
      // 実際に開いて機械判定する。デスクトップは幅に余裕がありこの崩れ方が
      // 再現しないため、崩れの実測対象だった 360/390px だけで見る。
      if (viewport.name !== 'desktop') {
        await page.locator('main > header details summary').first().click()
        const opened = await page.evaluate(() => {
          const details = document.querySelector('main > header details')
          const management = document.querySelector('[data-testid="recordings-management-summary"]')
          // StorageRootCapacity のカード内 <dl>（総容量/使用済み/空きの 3 列）。
          // 崩れると `<dd>` の内容（例: 「813.7 GB」）が列幅に収まらず折り返す
          // か、行自体が横に溢れる。
          const dl = details?.querySelector('dl') ?? null
          const dds = dl ? [...dl.querySelectorAll('dd')] : []
          // flex item（details）が実際に占有できる幅は管理情報行の border-box
          // 幅ではなく content-box 幅（`px-4` の左右パディングを除いた分）。
          // border-box の幅同士を比べると、パディング分（32px）を「占有できて
          // いない」と誤検出する。
          const managementStyle = management ? getComputedStyle(management) : null
          const managementContentWidth =
            management && managementStyle
              ? management.clientWidth -
                parseFloat(managementStyle.paddingLeft) -
                parseFloat(managementStyle.paddingRight)
              : null
          // 展開グリッド（details の直下、StorageRootCapacity を並べる div）自身の
          // 左右パディング。二重パディング（外側 recordings-management-summary の
          // px-4 + ここの px-4）は、このフィクスチャの桁数だと 3 列 dl が
          // 折り返す/溢れるところまでは追い込めない（幅に余裕がある）ので、
          // 症状（折り返し・溢れ）だけでなく実装がパディングを持たせていないこと
          // 自体も直接測る。
          const grid = details?.querySelector(':scope > div.grid') ?? null
          const gridStyle = grid ? getComputedStyle(grid) : null
          return {
            detailsWidth: details?.getBoundingClientRect().width ?? null,
            managementContentWidth,
            dlOverflow: dl ? dl.scrollWidth > dl.clientWidth : null,
            // text-xs の 1 行の高さは実測で 16px 程度。2 行に折り返すと
            // 目に見えて超える。
            maxDdHeight: dds.length > 0 ? Math.max(...dds.map((d) => d.getBoundingClientRect().height)) : null,
            gridPaddingLeft: gridStyle ? parseFloat(gridStyle.paddingLeft) : null,
            gridPaddingRight: gridStyle ? parseFloat(gridStyle.paddingRight) : null,
          }
        })
        if (
          opened.detailsWidth === null ||
          opened.managementContentWidth === null ||
          Math.abs(opened.detailsWidth - opened.managementContentWidth) > 1
        ) {
          ng.push(
            `録画一覧/正常/${viewport.name}: 開いた詳細が管理情報行の幅いっぱいを占有していない` +
              `（details=${opened.detailsWidth?.toFixed(1) ?? '—'}px, 行の内容幅=${opened.managementContentWidth?.toFixed(1) ?? '—'}px）`,
          )
        }
        if (opened.dlOverflow !== false) {
          ng.push(`録画一覧/正常/${viewport.name}: 展開したカードの内容が横に溢れている`)
        }
        if (opened.maxDdHeight === null || opened.maxDdHeight > 20) {
          ng.push(
            `録画一覧/正常/${viewport.name}: 展開したカードの値が折り返している` +
              `（${opened.maxDdHeight?.toFixed(1) ?? '—'}px）`,
          )
        }
        if (opened.gridPaddingLeft !== 0 || opened.gridPaddingRight !== 0) {
          ng.push(
            `録画一覧/正常/${viewport.name}: 展開グリッドが compact でも px-4 を持ち、` +
              `外側の管理情報行の px-4 と二重になっている` +
              `（left=${opened.gridPaddingLeft ?? '—'}px, right=${opened.gridPaddingRight ?? '—'}px）`,
          )
        }
      }
    }
    if (scenario.name === 'capacity' && !metric.summary.includes('満杯見込み')) {
      ng.push(`録画一覧/満杯見込み/${viewport.name}: 満杯見込みが summary に常置されていない`)
    }
    if ((scenario.name === 'stale' || scenario.name === 'many-warnings') && !metric.summary.includes('古い可能性')) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 古い観測が summary に常置されていない`)
    }
    if (scenario.name === 'encode-queue') {
      for (const label of ['待機中 2件', '実行中 1件']) {
        if ((await page.getByRole('button', { name: label, exact: true }).count()) === 0) {
          ng.push(`録画一覧/エンコード待機列/${viewport.name}: ${label} が無い`)
        }
      }
    }
    if (scenario.name === 'storage-failure') {
      // ストレージ取得だけが失敗し、エンコード待機列（0/0）は解決するケース。
      // `summaryLocator` が 0 件（= StorageBalance が描かれない）だと否定側の
      // 判定が自明に通ってしまう（管理行が丸ごと消えても緑になる指摘）ので、
      // 肯定側（チップが残っていること）と否定側（欠損した容量情報を出さない
      // こと）の両方を測る。
      if ((await page.getByRole('button', { name: '待機中 0件', exact: true }).count()) === 0) {
        ng.push(`録画一覧/${scenario.label}/${viewport.name}: エンコードチップが残っていない`)
      }
      const headerText = (
        (await page.locator('main > header').textContent()) ?? ''
      ).replaceAll(/\s+/g, ' ')
      if (headerText.includes('空き') || headerText.includes('の見込み')) {
        ng.push(`録画一覧/${scenario.label}/${viewport.name}: 欠損した容量情報を表示している`)
      }
    }
    if (scenario.name === 'no-observation') {
      // エンコード待機列の取得も失敗させ、StorageBalance と両方が何も描かない
      // 組み合わせにしてある（apiHandler 参照）。管理情報の帯（recordings.tsx の
      // `recordings-management-summary`）は `empty:hidden` で DOM には残るが
      // 子ノード 0 個で非表示になる想定なので、DOM の有無ではなく可視性を測る
      // （count() は常に 1 を返すので判定にならない）。
      if (await page.locator('[data-testid="recordings-management-summary"]').isVisible()) {
        ng.push(`録画一覧/${scenario.label}/${viewport.name}: 両方欠損時に空の管理情報帯が残っている`)
      }
    }
    await context.close()
  }
}

// --- ①-A 広幅の本文と一覧の文字サイズ ---
//
// jsdom は幅も継承後の実フォントサイズも測れない。2560px の実ブラウザで、一覧本文が
// サイドバーを除く main の幅いっぱいに広がることを全対象画面で見る。
// 題名と副情報は各画面の既定フィクスチャから実要素を掴み、計算済み px 値を測る。
log('\n=== ①-A 広幅の本文と一覧の文字サイズ ===')
const boundedListScreens = [
  { screen: 'recordings', title: 'ニュース７', secondary: 'NHK総合' },
  { screen: 'reservations', title: '連続テレビ小説', secondary: 'NHKEテレ' },
  { screen: 'series', title: '作品X', secondary: 'アニメ　作品X　第2話' },
  { screen: 'rules', title: '朝ドラ', secondary: '番組名に「連続テレビ小説」を含む' },
  { screen: 'programs', title: 'ニュース７', secondary: 'NHK総合' },
]
for (const spec of boundedListScreens) {
  const { context, page } = await open(desktop, 'light', screenOf(spec.screen))
  const content = page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]')
  const contentBox = (await content.count()) === 0 ? null : await content.boundingBox()
  const mainBox = await page.locator('main').boundingBox()
  if (contentBox === null || mainBox === null) {
    ng.push(`${spec.screen}: 本文コンテナが見つからない`)
    await context.close()
    continue
  } else {
    log(`  ${spec.screen}: x=${contentBox.x}, width=${contentBox.width}`)
    if (
      Math.abs(contentBox.x - mainBox.x) > 0.5 ||
      Math.abs(contentBox.width - mainBox.width) > 0.5
    ) {
      ng.push(
        `${spec.screen}: 本文が main 幅いっぱいでない` +
          `（main x=${mainBox.x}, width=${mainBox.width}; 本文 x=${contentBox.x}, width=${contentBox.width}）`,
      )
    }
  }

  const title = content.getByText(spec.title, { exact: true }).first()
  const secondary = content.getByText(spec.secondary, { exact: true }).first()
  // 要素が無い（セレクタが腐った・描画されない）場合を、`.catch(() => null)` で
  // フォントサイズの読み取り失敗に化けさせない。found を待ちの成否そのもので
  // 分岐し、見つからないときは found=false の NG を出して終える（issue #550。
  // 直す前は evaluate() が catch(() => null) を返し、`null !== '16px'` が真になって
  // 「題名が text-base でない（null）」というスタイル回帰と同じ文言の NG になっていた）。
  const [titleFound, secondaryFound] = await Promise.all([
    title
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false),
    secondary
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false),
  ])
  if (!titleFound) ng.push(`${spec.screen}: 一覧の題名（${spec.title}）が見つからない`)
  if (!secondaryFound) ng.push(`${spec.screen}: 一覧の副情報（${spec.secondary}）が見つからない`)
  const titleSize = titleFound ? await title.evaluate((el) => getComputedStyle(el).fontSize) : null
  const secondarySize = secondaryFound
    ? await secondary.evaluate((el) => getComputedStyle(el).fontSize)
    : null
  log(`  ${spec.screen}: 題名=${titleSize ?? '未取得'}, 副情報=${secondarySize ?? '未取得'}`)
  if (titleFound && titleSize !== '16px') {
    ng.push(`${spec.screen}: 一覧の題名が text-base でない（${titleSize}）`)
  }
  if (secondaryFound && secondarySize !== '14px') {
    ng.push(`${spec.screen}: 一覧の副情報が text-sm でない（${secondarySize}）`)
  }
  await context.close()
}

for (const screenName of ['home', 'series-hub']) {
  const { context, page } = await open(
    desktop,
    'light',
    screenOf(screenName),
    screenName === 'home' ? { homeModeFixture: true } : {},
  )
  const content = page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]')
  const contentBox = (await content.count()) === 0 ? null : await content.boundingBox()
  const mainBox = await page.locator('main').boundingBox()
  if (contentBox === null || mainBox === null) {
    ng.push(`${screenName}: 本文コンテナが見つからない`)
  } else if (
    Math.abs(contentBox.x - mainBox.x) > 0.5 ||
    Math.abs(contentBox.width - mainBox.width) > 0.5
  ) {
    ng.push(`${screenName}: 本文が main 幅いっぱいでない（main=${mainBox.width}px, 本文=${contentBox.width}px）`)
  }
  await context.close()
}

// 広い番組リストは説明列を使い、説明の有無にかかわらず左端と行高を揃える。
{
  const viewports = [
    { name: 'desktop-1280', width: 1280, height: 900 },
    desktop,
  ]
  const measured = new Map()
  for (const viewport of viewports) {
    const { context, page } = await open(viewport, 'light', screenOf('programs'))
    const rows = page.locator('li[data-program-id]')
    const rowsReady = await rows.first().waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
    if (!rowsReady) {
      ng.push(`programs/${viewport.name}: 番組行が見つからない`)
      await context.close()
      continue
    }
    const rowMetrics = await rows.evaluateAll((items) =>
      items.slice(0, 8).flatMap((item) => {
        const row = item.querySelector('[data-testid="program-row"]')
        const description = item.querySelector('[data-testid="program-row-description"]')
        if (!row) return []
        const rowRect = row.getBoundingClientRect()
        const descriptionRect = description?.getBoundingClientRect()
        return [{
          id: item.getAttribute('data-program-id'),
          height: rowRect.height,
          descriptionX: descriptionRect?.width ? descriptionRect.x : null,
          descriptionWidth: descriptionRect?.width ?? 0,
        }]
      }),
    )
    measured.set(viewport.width, rowMetrics)
    if (viewport.width === desktop.width) {
      const descriptions = rowMetrics.filter((row) => row.descriptionX !== null)
      if (descriptions.length < 2) {
        ng.push('programs/2560: 番組説明の列が 2 行以上で表示されていない')
      } else {
        const leftEdges = descriptions.map((row) => row.descriptionX)
        if (Math.max(...leftEdges) - Math.min(...leftEdges) > 0.5) {
          ng.push(`programs/2560: 番組説明列の左端が揃っていない（${leftEdges.join(', ')}）`)
        }
      }
    }
    await context.close()
  }

  const normalRows = new Map((measured.get(1280) ?? []).map((row) => [row.id, row]))
  const wideRows = measured.get(2560) ?? []
  for (const row of wideRows) {
    const normal = normalRows.get(row.id)
    if (normal && Math.abs(row.height - normal.height) > 0.5) {
      ng.push(
        `programs/${row.id}: 説明列で番組行の高さが変わった（1280=${normal.height}px, 2560=${row.height}px）`,
      )
    }
  }
}

// カードの幅を上限し、2560px では固定 4 列より多く並べる。
for (const screenName of ['series', 'recordings']) {
  const { context, page } = await open(desktop, 'light', screenOf(screenName))
  const cardToggle = page.getByRole('button', { name: 'カード表示', exact: true })
  if ((await cardToggle.count()) === 0) {
    ng.push(`${screenName}/2560: カード表示への切り替えが無い`)
    await context.close()
    continue
  }
  if ((await cardToggle.getAttribute('aria-pressed')) !== 'true') await cardToggle.click()
  const cards = page.locator(
    screenName === 'series' ? '[data-testid="series-shelf"]' : '[data-testid="recording-card"]',
  )
  const cardsReady = await cards.first().waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
  const boxes = cardsReady ? await cards.evaluateAll((items) => items.map((item) => {
    const { x, y, width, height } = item.getBoundingClientRect()
    return { x, y, width, height }
  })) : []
  if (!cardsReady || boxes.length === 0) {
    ng.push(`${screenName}/2560: カードが見つからない`)
  } else {
    const maxWidth = Math.max(...boxes.map((box) => box.width))
    const rows = new Set(boxes.map((box) => Math.round(box.y)))
    log(`  ${screenName}/2560: cards=${boxes.length}, max-width=${maxWidth}px, rows=${rows.size}`)
    if (maxWidth > 360.5) {
      ng.push(`${screenName}/2560: カード幅 ${maxWidth}px が 360px の上限を超えている`)
    }
    if (boxes.length > 4 && rows.size > 1) {
      ng.push(`${screenName}/2560: 4 列を超えたカードが複数行になっている`)
    }
  }
  await page.screenshot({ path: path.join(OUT_DIR, `${screenName}-cards-light-desktop.png`) })
  await context.close()
}

// ライブ外枠の広幅判定。受け入れ寸法（映像 + 情報 3 行、一覧との隙間なし、
// 複数列、sticky）を実ブラウザで測る。
for (const viewport of [
  { name: 'live-1920', width: 1920, height: 1080 },
  desktop,
]) {
  const liveScreen = {
    ...screenOf('live'),
    path: `/live?service=${services[0].id}&site=${SITE}`,
  }
  const { context, page } = await open(
    viewport,
    'light',
    liveScreen,
    { wideLiveFixture: true },
  )
  const preview = page.getByRole('button', { name: 'NHK総合を再生', exact: true })
  const playerColumn = preview.locator('xpath=..')
  const channelList = page.getByRole('navigation', { name: 'チャンネル一覧' })
  const ready = await preview.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
  if (!ready || (await channelList.count()) === 0) {
    ng.push(`live/${viewport.width}: 映像またはチャンネル一覧が見つからない`)
    await context.close()
    continue
  }
  const station = page.locator('main').getByText('NHK総合', { exact: true }).first()
  const programLine = page.locator('main p.text-xl.font-semibold').first()
  const timeLine = page.locator('main p').filter({ hasText: /予定:/ }).first()
  const [stationFound, programFound, timeFound] = await Promise.all([
    station.waitFor({ timeout: 5000 }).then(() => true).catch(() => false),
    programLine.waitFor({ timeout: 5000 }).then(() => true).catch(() => false),
    timeLine.waitFor({ timeout: 5000 }).then(() => true).catch(() => false),
  ])
  const [videoBox, channelBox, stationBox, programBox, timeBox] = await Promise.all([
    preview.boundingBox(),
    channelList.boundingBox(),
    stationFound ? station.boundingBox() : Promise.resolve(null),
    programFound ? programLine.boundingBox() : Promise.resolve(null),
    timeFound ? timeLine.boundingBox() : Promise.resolve(null),
  ])
  if (videoBox === null || channelBox === null || stationBox === null || programBox === null || timeBox === null) {
    ng.push(`live/${viewport.width}: 映像と局名・番組名・時刻の位置を測れない`)
  } else {
    if (
      videoBox.y + videoBox.height > viewport.height ||
      stationBox.y + stationBox.height > viewport.height ||
      programBox.y + programBox.height > viewport.height ||
      timeBox.y + timeBox.height > viewport.height
    ) {
      ng.push(`live/${viewport.width}: 映像または局名・番組情報が初期 viewport に収まらない`)
    }
    // #1022 のラフの列間隔は 20px。映像の幅上限で一覧が右へ離れる（旧 max-w-3xl）のを弾く
    const gap = channelBox.x - (videoBox.x + videoBox.width)
    if (gap < 0 || gap > 24) {
      ng.push(`live/${viewport.width}: 映像とチャンネル一覧の間隔が 0〜24px に収まらない（${gap}px）`)
    }
    log(`  live/${viewport.width}: video=${videoBox.width}×${videoBox.height}, channel-x=${channelBox.x}`)
  }

  const serviceItems = channelList.locator('ul ul').first().locator(':scope > li')
  if (viewport.width === desktop.width) {
    const columnXs = await serviceItems.evaluateAll((items) =>
      [...new Set(items.map((item) => Math.round(item.getBoundingClientRect().x)))],
    )
    if (columnXs.length < 2) ng.push('live/2560: チャンネル一覧が複数列になっていない')

    const scrollRange = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)
    if (scrollRange < 500) {
      ng.push(`live/2560: sticky を判定するスクロール量が足りない（${scrollRange}px）`)
    } else {
      await page.evaluate(() => window.scrollTo({ top: 600, behavior: 'instant' }))
      await page.waitForTimeout(100)
      const [playerAfterScroll, headerAfterScroll] = await Promise.all([
        playerColumn.boundingBox(),
        page.locator('main > header').boundingBox(),
      ])
      const stickyPosition = await playerColumn.evaluate((element) => getComputedStyle(element).position)
      if (
        playerAfterScroll === null ||
        headerAfterScroll === null ||
        stickyPosition !== 'sticky' ||
        Math.abs(playerAfterScroll.y - headerAfterScroll.y - headerAfterScroll.height) > 1
      ) {
        ng.push('live/2560: スクロール後もプレイヤー列がヘッダー直下に sticky されていない')
      }
      await page.evaluate(() => window.scrollTo(0, 0))
    }
  }
  await page.screenshot({ path: path.join(OUT_DIR, `${viewport.name}-light.png`) })
  await context.close()
}

// 高さが足りない desktop viewport でもライブ映像をゼロ幅にしない。
{
  const viewport = { name: 'live-short', width: 1366, height: 400 }
  const liveScreen = {
    ...screenOf('live'),
    path: `/live?service=${services[0].id}&site=${SITE}`,
  }
  const { context, page } = await open(viewport, 'light', liveScreen, { wideLiveFixture: true })
  const preview = page.getByRole('button', { name: 'NHK総合を再生', exact: true })
  const ready = await preview.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
  const videoBox = ready ? await preview.boundingBox() : null
  if (videoBox === null || videoBox.width < 320 || videoBox.height < 180) {
    ng.push(`live/1366x400: 映像プレビューが表示可能な大きさでない（${videoBox?.width ?? 0}×${videoBox?.height ?? 0}px）`)
  }
  await page.screenshot({ path: path.join(OUT_DIR, 'live-short-light.png') })
  await context.close()
}

// 1280px の一覧ラフ比較用。自動判定だけでなく、1 行目・カード密度・右端の余白を
// 2560px のショットと並べて確認する。
for (const screenName of ['programs', 'reservations', 'recordings', 'rules', 'series', 'series-hub']) {
  const viewport = { name: 'desktop-1280', width: 1280, height: 900 }
  const { context, page } = await open(viewport, 'light', screenOf(screenName))
  const file = path.join(OUT_DIR, `${screenName}-light-desktop-1280.png`)
  await page.screenshot({ path: file })
  log(`  ${path.basename(file)}`)
  await context.close()
}

// #1022 の合意済みライブ画面ラフ（幅約 1180px）との比較対象。
{
  const viewport = { name: 'live-rough', width: 1180, height: 900 }
  const liveScreen = {
    ...screenOf('live'),
    path: `/live?service=${services[0].id}&site=${SITE}`,
  }
  const { context, page } = await open(viewport, 'light', liveScreen)
  const file = path.join(OUT_DIR, 'live-light-rough-1180.png')
  await page.screenshot({ path: file })
  log(`  ${path.basename(file)}`)
  await context.close()
}

// 同じ /programs でも番組表グリッドは横幅が情報量なので、本文上限を適用しない。
{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  const gridTrigger = page.getByRole('button', { name: '番組表' })
  if ((await gridTrigger.count()) === 0) {
    ng.push('programs-grid: 表示形式の切り替えが無い')
  } else {
    await gridTrigger.click()
    const grid = page.locator('[data-testid="program-grid"]')
    const gridReady = await grid
      .waitFor({ timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    if (!gridReady) {
      // 待ちの失敗をここで飲んで先へ進むと、以下の「1024px 以下に制限されている」
      // という**スタイル回帰の NG と同じ文言**で「そもそも描画されていない」ことが
      // 報告されてしまう（issue #521 と同じ壊れ方）。加えて要素が 0 件のまま
      // `grid.evaluate(...)` へ進むと、そちらは `locator.evaluate` 自身の既定の
      // 30 秒待った末に例外で落ちる（測定: `locator.evaluate threw after 30006 ms:
      // Timeout 30000ms exceeded`。NG リストに積まれず何も判定していない状態で
      // 終わる）。ここで打ち切って区別できる文言を出す。
      ng.push('programs-grid: 番組表グリッドが描画されない（待ちがタイムアウト）')
    } else {
      const gridBox = await grid.boundingBox()
      if ((await page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]').count()) > 0) {
        ng.push('programs-grid: 番組表グリッドに一覧本文の幅上限が適用されている')
      }
      if (gridBox === null || gridBox.width <= 1024.5) {
        ng.push(`programs-grid: 番組表グリッドが 1024px 以下に制限されている（${gridBox?.width ?? '未取得'}px）`)
      }

      const columnMetrics = await grid.evaluate((element) => {
        const columns = [...element.querySelectorAll('[data-testid="program-grid-column"]')]
        const gridRect = element.getBoundingClientRect()
        const firstRect = columns[0]?.getBoundingClientRect()
        return {
          count: columns.length,
          width: firstRect?.width ?? 0,
          availableWidth: firstRect ? element.clientWidth - (firstRect.left - gridRect.left) : 0,
        }
      })
      const expectedColumnWidth = Math.min(
        260,
        Math.max(176, columnMetrics.availableWidth / services.length),
      )
      if (
        columnMetrics.count !== services.length ||
        Math.abs(columnMetrics.width - expectedColumnWidth) > 0.5
      ) {
        ng.push(
          `programs-grid: 4 局の列幅が画面幅に追従していない（実測 ${columnMetrics.width}px、期待 ${expectedColumnWidth}px）`,
        )
      }
    }
  }
  await context.close()
}

// ルール作成はモバイルだけ本文全幅、lg 以上では PageHeader の右端に置く。
{
  const { context, page } = await open(desktop, 'light', screenOf('rules'))
  const create = page.locator('header').getByRole('link', { name: 'ルールを作成', exact: true })
  const createBox = (await create.count()) === 0 ? null : await create.boundingBox()
  if (createBox === null) {
    ng.push('rules/desktop: PageHeader に「ルールを作成」が無い')
  } else if (createBox.width > 200) {
    ng.push(`rules/desktop: 「ルールを作成」が内容幅でない（${createBox.width}px）`)
  }

  // issue #728: 主編集導線はルール名そのもの（`variant="link"` = `text-primary`）
  // で、「編集」という primary 塗りのボタンはもう無い。**背景色ではなく
  // 文字色**が primary トークンと一致することを見る --- `text-primary` /
  // `bg-primary` はどちらも Tailwind の `--color-primary`（= `--primary`）を
  // 参照するので、リンクの文字色と作成ボタンの背景色は同じ rgba になる。
  const editColor = await computedOf(
    page.getByRole('link', { name: /^ルール「.+」を編集$/ }).first(),
    'color',
  )
  const createBg = await computedOf(create, 'background-color')
  if (
    editColor === null ||
    createBg === null ||
    !editColor.rgba.every((value, index) => value === createBg.rgba[index])
  ) {
    ng.push('rules/desktop: ルール名の編集リンクが primary の文字色でない')
  }
  await context.close()
}
{
  const { context, page } = await open(mobile, 'light', screenOf('rules'))
  const headerCreate = page.locator('header').getByRole('link', { name: 'ルールを作成', exact: true })
  if ((await headerCreate.count()) > 0) {
    ng.push('rules/mobile: PageHeader に「ルールを作成」が出ている')
  }
  const mobileContent = page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]')
  const mobileCreate = mobileContent.getByRole('link', { name: 'ルールを作成', exact: true })
  const contentBox =
    (await mobileContent.count()) === 0 ? null : await mobileContent.boundingBox()
  const createBox =
    (await mobileContent.count()) === 0 || (await mobileCreate.count()) === 0
      ? null
      : await mobileCreate.boundingBox()
  const padding =
    (await mobileContent.count()) === 0
      ? null
      : await mobileContent.evaluate((el) => {
          const style = getComputedStyle(el)
          return {
            left: Number.parseFloat(style.paddingLeft),
            right: Number.parseFloat(style.paddingRight),
          }
        })
  if (contentBox === null || createBox === null || padding === null) {
    ng.push('rules/mobile: 「ルールを作成」の全幅を測れない')
  } else {
    const expectedX = contentBox.x + padding.left
    const expectedWidth = contentBox.width - padding.left - padding.right
    if (Math.abs(createBox.x - expectedX) > 0.5 || Math.abs(createBox.width - expectedWidth) > 0.5) {
      ng.push(
        `rules/mobile: 「ルールを作成」が本文の全幅でない` +
          `（x=${createBox.x}, width=${createBox.width}, 期待 x=${expectedX}, width=${expectedWidth}）`,
      )
    }
  }
  await context.close()
}

// --- ホーム管理モード: timeline / action list の実ブラウザ確認 ---
//
// jsdom が見る構造だけでなく、画面に両方の領域が描かれ、容量警告の導線が
// 既存の番組表へ遷移することを実ブラウザで確認する。
log("\n=== ホーム管理モード: timeline / action list ===")
{
  const { context, page } = await open(desktop, 'light', screenOf('home'), { homeModeFixture: true, homeOpsFixture: true })
  for (const heading of ['今日 0 時 → 明日の終わり', '要対応']) {
    const found = await page.getByRole('heading', { name: heading }).count()
    if (found === 0) {
      ng.push(`ホーム: 見出し「${heading}」が #1021 fixture で出ていない`)
    }
  }
  const shortageLink = page.getByRole('link', { name: /地デジ・BSが 1 本不足しています/ })
  if ((await shortageLink.count()) === 0) {
    ng.push('ホーム: 全 jammedTypes の不足警告項目が無い')
  } else {
    await shortageLink.first().click()
    await page.waitForTimeout(400)
    const url = new URL(page.url())
    if (url.pathname !== '/programs') {
      ng.push(`ホーム: 容量不足をクリックしても番組表へ飛ばない（${url.pathname}）`)
    }
  }
  await context.close()
}
{
  // 両方向: 全領域が空のときは timeline/action heading を出さず、単一の空状態だけを出す
  const context = await browser.newContext({
    viewport: { width: desktop.width, height: desktop.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
    deviceScaleFactor: 2,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, apiHandler({ emptyHome: true }))
  await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
  await page
    .getByText('表示できる項目がありません')
    .waitFor({ timeout: 5000 })
    .catch(() => {
      ng.push('ホーム: 全セクション空でも単一の空状態が出ない')
    })
  for (const heading of ['今日 0 時 → 明日の終わり', '要対応']) {
    if ((await page.getByRole('heading', { name: heading }).count()) > 0) {
      ng.push(`ホーム: 全領域空のはずが見出し「${heading}」が出ている`)
    }
  }
  await context.close()
}

log("\n=== ①'' ホーム: 警告の種別ごとの色（琥珀 vs destructive） ===")
{
  // #1021 の各種別と、予約subtitleが実際に重なる fixture を使う。
  const { context, page } = await open(desktop, 'light', screenOf('home'), {
    withBreaker: true,
    homeModeFixture: true,
    homeOpsFixture: true,
  })
  await page.getByRole('heading', { name: '要対応' }).waitFor({ timeout: 5000 }).catch(() => {})

  // 種別チップだけが色を持つ。行の文字色は中立のまま。
  const chipColor = (rowLocator) => computedOf(rowLocator.getByTestId('warning-chip'), 'color')

  // 容量超過 = 琥珀。
  const overageRow = page.locator('li[data-warning-kind="overage"]').first()
  const overageColor = await chipColor(overageRow)
  if (overageColor === null) {
    ng.push('ホーム: 容量不足の警告項目の文字色が取得できない')
  } else if (!isAmber(overageColor.rgba)) {
    ng.push(
      `ホーム: 容量不足の警告項目が琥珀でない（${overageColor.value} = ${overageColor.rgba}）`,
    )
  }

  // サーキットブレーカー = destructive。
  const breakerRow = page.locator('li[data-warning-kind="breaker"]').first()
  const breakerColor = await chipColor(breakerRow)
  if (breakerColor === null) {
    ng.push('ホーム: ブレーカーの警告項目の文字色が取得できない')
  } else if (!isRed(breakerColor.rgba)) {
    ng.push(
      `ホーム: ブレーカーの警告項目が destructive でない（${breakerColor.value} = ${breakerColor.rgba}）`,
    )
  }

  // ドロップ = destructive。
  const dropRow = page.locator('li[data-warning-kind="drop"]').first()
  const dropColor = await chipColor(dropRow)
  if (dropColor === null) {
    ng.push('ホーム: ドロップの警告項目の文字色が取得できない')
  } else if (!isRed(dropColor.rgba)) {
    ng.push(`ホーム: ドロップの警告項目が destructive でない（${dropColor.value} = ${dropColor.rgba}）`)
  }

  // 失敗録画 = destructive（録画が失われたことは取り返しがつかない）。
  const failedRow = page.locator('li[data-warning-kind="failed"]').first()
  const failedColor = await chipColor(failedRow)
  if (failedColor === null) {
    ng.push('ホーム: 失敗録画の警告項目の文字色が取得できない（行が出ていない可能性）')
  } else if (!isRed(failedColor.rgba)) {
    ng.push(
      `ホーム: 失敗録画の警告項目が destructive でない（${failedColor.value} = ${failedColor.rgba}）`,
    )
  }

  log(
    `  容量不足=${overageColor?.rgba} / ブレーカー=${breakerColor?.rgba} / ドロップ=${dropColor?.rgba} / 失敗録画=${failedColor?.rgba}`,
  )
  await context.close()
}

// --- ①''' ホーム: 実時計でのクエリキー安定性（無限再取得の回帰検出） ---
//
// **時計を止めない。** このファイルの他の全判定は `page.clock.setFixedTime` で
// 時計を止めており、それは「時計が動くことに起因する欠陥」（レンダーごとに
// 変わる生の Date.now() をキャッシュキーに載せて無限再取得になる、等）を
// 原理的に検出できない（レビューで発覚。実装 PR で `/api/capacity/overages` の
// `start` に生の Date.now() を渡しており、時計を止めた判定は全部素通りしていた。
// web/e2e/README.md §判定を足すときの規律）。ここだけ時計を止めずに実際の要求回数
// を数える。
//
// **上限だけでなく下限（>= 1）も見る。** 「N 回以下」だけの判定は、クエリを
// 消した・`enabled: false` にした・そもそもページが起動しない、のいずれでも
// 0 回で緑になる（レビュー指摘）。数え始める前にホームの目印を待って画面が
// 実際に立っていることを確かめ、そのうえで回数の下限と上限の両方に掛ける
// （`badge-links.mjs` ⓪ が「配っている bundle が dist の現物と一致するか」を
// 最初に見ているのと同じ思想 --- 前提が崩れていると下流の判定は全部無意味）。
log("\n=== ①''' ホーム: 実時計でのクエリキー安定性（無限再取得の回帰検出） ===")
{
  const context = await browser.newContext({
    viewport: { width: desktop.width, height: desktop.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  const overagesRequests = []
  page.on('request', (req) => {
    const url = new URL(req.url())
    if (url.pathname === '/api/capacity/overages') overagesRequests.push(url.toString())
  })
  await installApiStubs(page, apiHandler())
  await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
  let homeUp = true
  await page
    .locator(screenOf('home').wait)
    .first()
    .waitFor({ timeout: 15000 })
    .catch(() => {
      homeUp = false
      ng.push(
        `ホーム: 実時計（page.clock を使わない）でホームが立たない（目印「${screenOf('home').wait}」が出ない）`,
      )
    })
  await page.waitForTimeout(2500)
  log(`  /api/capacity/overages への実要求回数（実時計 2.5 秒）: ${overagesRequests.length}`)
  if (homeUp && overagesRequests.length < 1) {
    ng.push(
      'ホーム: 実時計で /api/capacity/overages を一度も要求していない' +
        '（クエリが消えている・enabled: false になっている疑い。' +
        'この下限が無いと「0 回」でこの判定は緑になる）',
    )
  }
  if (overagesRequests.length > 3) {
    ng.push(
      `ホーム: 実時計で /api/capacity/overages への要求が ${overagesRequests.length} 回` +
        '（無限再取得の疑い。start に生の Date.now() を渡していないか確認する。' +
        'レビュー実測では 18〜37 回だった）',
    )
  }
  await context.close()
}

// --- ② 色の機械判定 ---
//
// 判定は「この PR が変えた状態色ぜんぶ」を覆う。1 箇所でも判定の外に置くと、
// そこだけ既定値へ静かに戻っても全部緑のまま通る。
log('\n=== ② 色の判定 ===')

/** 小さい文字（バッジ・ラベル）に要求する WCAG 比。 */
const minTextContrast = 4.5
/** 面・線（非テキスト）に要求する WCAG 比。 */
const minUiContrast = 3

/** contrasts は測ったコントラストを表として溜める（合否とは別に、数値を人に見せる）。 */
const contrasts = []
function checkContrast(theme, label, fg, measured, floor) {
  // measured は `readColor` の戻り。背景側は必ずその `backdrop` を使う
  if (measured.reachedOpaque === false) {
    ng.push(`[${theme}] ${label}: 不透明な面まで遡れず、比を測れていない`)
    return null
  }
  const bg = measured.backdrop
  const ratio = contrast(fg, bg)
  contrasts.push({ theme, label, ratio, floor })
  if (ratio < floor) {
    ng.push(`[${theme}] ${label} のコントラストが ${ratio.toFixed(2)}（下限 ${floor}）`)
  }
  return ratio
}

function checkPixelContrast(theme, label, foreground, background, floor) {
  const ratio = contrast([...foreground, 255], [...background, 255])
  contrasts.push({ theme, label, ratio, floor })
  log(`  [${theme}] ${label}: ${ratio.toFixed(2)}:1 (下限 ${floor}:1)`)
  if (ratio < floor) {
    ng.push(`[${theme}] ${label} の実画素コントラストが ${ratio.toFixed(2)}（下限 ${floor}）`)
  }
  return ratio
}

// サイドバーの現在地とフォーカスリングは、DOM の色指定ではなく描画済み PNG の
// 画素で測る。選択行の左端、行の右側にあるページ地、2px オフセットリング上端、
// その外側の地を採る。リングの座標を行の外に固定することで、オフセット無しや
// ブラウザ既定の outline へ戻る退行を拾う。
for (const theme of themes) {
  const { context, page } = await open(homeDesktop, theme, screenOf('programs'))
  const sideNav = page.locator('nav[aria-label="主ナビゲーション"]').first()
  const current = sideNav.locator('a[aria-current="page"]')
  const currentCount = await current.count()
  if (currentCount !== 1) {
    ng.push(`[${theme}] サイドバーの現在地リンクが 1 件でない（${currentCount} 件）`)
    await context.close()
    continue
  }

  const item = current.first()
  await item.focus()
  const focusStyle = await item.evaluate((el) => {
    const style = getComputedStyle(el)
    return {
      focusVisible: el.matches(':focus-visible'),
      boxShadow: style.boxShadow,
      outlineStyle: style.outlineStyle,
      rect: (() => {
        const rect = el.getBoundingClientRect()
        return { x: rect.x, y: rect.y, width: rect.width, height: rect.height }
      })(),
    }
  })
  if (!focusStyle.focusVisible) {
    ng.push(`[${theme}] サイドバーの現在地リンクに :focus-visible が付かない`)
  }
  if (focusStyle.boxShadow === 'none') {
    ng.push(`[${theme}] サイドバーの現在地リンクに明示フォーカスリングが出ない`)
  }
  if (focusStyle.outlineStyle !== 'none') {
    ng.push(`[${theme}] サイドバーの現在地リンクにブラウザ outline が残っている（${focusStyle.outlineStyle}）`)
  }

  const { x, y, width, height } = focusStyle.rect
  const centerY = Math.floor(y + height / 2)
  const centerX = Math.floor(x + width / 2)
  const png = await page.screenshot({ scale: 'css' })
  const [itemPixel, pagePixel, ringPixel, ringBackdropPixel] = await readScreenshotPixels(page, png, [
    [Math.floor(x + 6), centerY],
    [Math.floor(x + width + 4), centerY],
    [centerX, Math.floor(y - 3)],
    [centerX, Math.floor(y - 6)],
  ])
  const scanline = await computedVar(page.locator('html'), '--scanline')
  log(
    `  [${theme}] サイドバー現在地: 行=${itemPixel} / ページ地=${pagePixel} / ` +
      `リング=${ringPixel} / リング外=${ringBackdropPixel}`,
  )
  checkPixelContrast(theme, 'サイドバー現在地 / ページ地', itemPixel, pagePixel, minUiContrast)
  checkPixelContrast(theme, 'サイドバーのフォーカスリング / ページ地', ringPixel, ringBackdropPixel, minUiContrast)
  if (scanline === null || !sameRgb(ringPixel, scanline.rgba)) {
    ng.push(
      `[${theme}] サイドバーのフォーカスリングの実画素が --scanline と一致しない` +
        `（画素=${ringPixel} / --scanline=${scanline?.rgba}）`,
    )
  }
  await context.close()
}

// 共有 Button のリングも、計算済みの border-color ではなく PNG の実画素で測る。
// `ring-ring/50` へ戻ると border は不透明なままなので `checkExplicitFocusRing` の
// border 比較だけでは見逃す。ボタン上端のリングとその外側の面を採り、半透明化で
// 合成された実際のコントラストが 3:1 を割ることを確認する。
for (const theme of themes) {
  const { context, page } = await open(homeDesktop, theme, screenOf('search'))
  const button = page.getByRole('button', { name: '検索', exact: true }).first()
  if ((await button.count()) === 0) {
    ng.push(`[${theme}] 共通 Button が見つからずフォーカスリングを画素で測れない`)
    await context.close()
    continue
  }

  await button.focus()
  const focused = await button.evaluate((el) => {
    const style = getComputedStyle(el)
    const rect = el.getBoundingClientRect()
    return {
      focusVisible: el.matches(':focus-visible'),
      boxShadow: style.boxShadow,
      outlineStyle: style.outlineStyle,
      rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
    }
  })
  if (!focused.focusVisible) {
    ng.push(`[${theme}] 共通 Button に :focus-visible が付かない`)
  }
  if (focused.boxShadow === 'none') {
    ng.push(`[${theme}] 共通 Button の明示フォーカスリングがない`)
  }
  if (focused.outlineStyle !== 'none') {
    ng.push(`[${theme}] 共通 Button にブラウザ outline が残っている（${focused.outlineStyle}）`)
  }

  const ringX = Math.floor(focused.rect.x + focused.rect.width / 2)
  const png = await page.screenshot({ scale: 'css' })
  const [ringPixel, backdropPixel] = await readScreenshotPixels(page, png, [
    [ringX, Math.floor(focused.rect.y - 1)],
    [ringX, Math.floor(focused.rect.y - 4)],
  ])
  const scanline = await computedVar(page.locator('html'), '--scanline')
  log(`  [${theme}] 共通 Button のリング=${ringPixel} / 外側の面=${backdropPixel}`)
  checkPixelContrast(theme, '共通 Button のフォーカスリング / 背景', ringPixel, backdropPixel, minUiContrast)
  if (scanline === null || !sameRgb(ringPixel, scanline.rgba)) {
    ng.push(
      `[${theme}] 共通 Button のフォーカスリングの実画素が --scanline と一致しない` +
        `（画素=${ringPixel} / --scanline=${scanline?.rgba}）`,
    )
  }
  await context.close()
}

for (const theme of themes) {
  // --- 録画一覧: 録画中 = タリーの塗り / 失敗 = destructive の淡い地 / 地は無彩 ---
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    // 一覧の行の中に限る（状態フィルタのチップにも同じ文言があるため）
    const badge = page.locator('ul span', { hasText: /^録画中$/ })
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(badge, 'color')
    log(`  [${theme}] 録画中バッジ 地=${bg?.value} ${bg?.rgba} / 文字=${fg?.value} ${fg?.rgba}`)
    if (bg === null) ng.push(`[${theme}] 録画中バッジが見つからない`)
    else if (bg.rgba[3] < 200) {
      // 淡い地（destructive の流儀 `bg-*/10`）に戻されたらここで落ちる
      ng.push(`[${theme}] 録画中バッジが塗りでない（不透明度 ${bg.rgba[3]}/255。${bg.value}）`)
    } else if (!isRed(bg.rgba)) {
      ng.push(`[${theme}] 録画中バッジの地がタリーレッドでない（${bg.value} = ${bg.rgba}）`)
    }
    if (fg !== null && chroma(fg.rgba) > 30) {
      ng.push(`[${theme}] 録画中バッジの文字に色が付いている（塗り + 無彩の文字であるべき。${fg.value}）`)
    }
    if (bg !== null && fg !== null && bg.rgba[3] >= 200) {
      // 塗りは不透明なので `bg.rgba` でも同値になるが、**背景側は必ず
      // `fg.backdrop` を渡す**。「どこかは自分の背景、どこかは合成後」と
      // 混ざっていると、次に構造が変わったときにどちらが正しいか分からなくなる
      checkContrast(theme, '録画中バッジの文字 / タリーの塗り', fg.rgba, fg, minTextContrast)
    }

    // 失敗バッジは destructive の「文字 + 淡い地」のまま
    const failed = page.locator('ul span', { hasText: /^録画失敗$/ })
    const failedBg = await computedOf(failed, 'background-color')
    const failedFg = await computedOf(failed, 'color')
    if (failedFg === null || failedBg === null) {
      ng.push(`[${theme}] 失敗バッジが見つからない`)
    } else {
      if (failedBg.rgba[3] > 200) {
        ng.push(`[${theme}] 失敗バッジが塗りになっている（destructive は文字 + 淡い地。${failedBg.value}）`)
      }
      if (!isRed(failedFg.rgba)) {
        ng.push(`[${theme}] 失敗バッジの文字が赤でない（${failedFg.value} = ${failedFg.rgba}）`)
      }
      checkContrast(
        theme,
        '失敗バッジの文字 / destructive の淡い地',
        failedFg.rgba,
        failedFg,
        minTextContrast,
      )
    }

    // 「準備中」結論バッジ（issue #1112）。`bg-muted` の文字色は
    // `text-foreground` とし、地が塗り（不透明）であることと文字との
    // コントラストが下限を満たすことを測る。
    const preparing = page.locator('ul span', { hasText: /^準備中$/ })
    const preparingBg = await computedOf(preparing, 'background-color')
    const preparingFg = await computedOf(preparing, 'color')
    if (preparingFg === null || preparingBg === null) {
      ng.push(`[${theme}] 準備中バッジが見つからない`)
    } else {
      if (preparingBg.rgba[3] < 200) {
        ng.push(`[${theme}] 準備中バッジの地が塗りでない（不透明度 ${preparingBg.rgba[3]}/255。${preparingBg.value}）`)
      }
      checkContrast(theme, '準備中バッジの文字 / muted の塗り', preparingFg.rgba, preparingFg, minTextContrast)
    }

    // 地は無彩。body だけを見ると「body に bg-background が当たっているか」しか
    // 言えないので、実際に面を持つ要素を回す。
    //
    // **測れなかったら NG にする。** セレクタが 0 件・面が透明のときに黙って
    // continue すると、「4 面を見ている」と書いてあるのに実際は 2 面しか
    // 見ていない、という状態が緑のまま続く（実際そうなっていた）
    for (const [name, selector] of [
      ['body', 'body'],
      ['ヘッダ', 'header'],
      // ナビは 2 つある（モバイルのボトムタブと md 以上のサイドバー）。
      // `.first()` だと背景を持たない方を掴むので両方回す
      ['ナビ', 'nav[aria-label="主ナビゲーション"]'],
      ['一覧の行', 'li a, li > div'],
    ]) {
      const nodes = await page.locator(selector).all()
      const surfaces = []
      for (const node of nodes) {
        const measured = await node.evaluate(readColor, 'background-color')
        surfaces.push(measured.backdrop)
      }
      if (surfaces.length === 0) {
        ng.push(`[${theme}] ${name}（${selector}）が 0 件で、地を判定できていない`)
        continue
      }
      const worst = surfaces.reduce((a, b) => (chroma(a) >= chroma(b) ? a : b))
      log(`  [${theme}] ${name} の地（${surfaces.length} 件中いちばん彩度が高いもの）= ${worst}`)
      if (chroma(worst) > 8) {
        ng.push(`[${theme}] ${name} の地が無彩でない（チャンネル差 ${chroma(worst)}。${worst}）`)
      }
    }

    // --- 接続断バナー（ConnectionBanner、issue #456）: 地は無彩 ---
    //
    // apiHandler は /api/events に明示のスタブを持たず catch-all（200 json []）
    // に落ちる（apiHandler の doc コメント参照）。Content-Type が
    // text/event-stream でないので EventSource は即座に失敗し、追加のスタブ
    // なしで「切断中」を作れる。disconnectedBannerDelayMs（lib/events.ts と
    // 同じ値をリテラルで書く。10 秒）が経てば帯が出るので、実時間で待ってから
    // 地を測る（`page.clock` はここでは使わない --- `setTimeout` は本物の
    // タイマーのまま動く。open() の `clock.setFixedTime` は Date だけを固定する）。
    //
    // waitFor の失敗だけ try/catch で ng.push に落とす --- 素通しすると帯が出ない
    // 変異で未捕捉例外がスクリプトごと中断し、後続の判定（このテーマの残り・
    // 他のスクリーンショット）と finish() の集計・ブラウザ後始末を丸ごと飛ばす。
    // `computedOf` / `chroma` まで同じ try に入れると、そちらが投げたときの NG が
    // 「帯が出ない」という食い違ったメッセージになるので外に出す。
    {
      const banner = page.locator('[role="status"]', { hasText: '更新通知が止まっています' })
      let bannerAppeared = true
      try {
        await banner.waitFor({ timeout: 10_000 + 5_000 })
      } catch {
        bannerAppeared = false
        ng.push(`[${theme}] 接続断バナーが disconnectedBannerDelayMs + 5 秒待っても出ない`)
      }
      if (bannerAppeared) {
        const bg = await computedOf(banner, 'background-color')
        log(`  [${theme}] 接続断バナーの地 = ${bg?.value} ${bg?.backdrop}`)
        if (bg === null) {
          ng.push(`[${theme}] 接続断バナーが見つからない`)
        } else if (chroma(bg.backdrop) > 8) {
          ng.push(`[${theme}] 接続断バナーの地が無彩でない（チャンネル差 ${chroma(bg.backdrop)}。${bg.backdrop}）`)
        }
      }
    }
    await context.close()
  }

  // --- 録画一覧: site タグ・IngestBadge の合成後コントラスト（issue #308 の
  //     レビューで判明した穴） ---
  //
  // 上のブロックは結論の「準備中」バッジだけを測る。site タグと IngestBadge も
  // muted の地を使う。site タグ（`showSite` が真になる 2 サイト以上でしか出ない）と
  // IngestBadge（`ingest` フィールドを持つ録画がないと出ない）は既定のフィクスチャ
  // では描画されず、`text-muted-foreground` に戻す変異が入っても緑のまま通っていた。
  // `multiSite` + `extraRecording` で両方を描画させて測る。
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'), {
      multiSite: true,
      extraRecording: true,
    })

    const siteTag = page.locator('ul span', { hasText: new RegExp(`^${SITE2}$`) })
    const siteTagBg = await computedOf(siteTag, 'background-color')
    const siteTagFg = await computedOf(siteTag, 'color')
    if (siteTagFg === null || siteTagBg === null) {
      ng.push(`[${theme}] 録画一覧の site タグが見つからない（showSite が効いていない?）`)
    } else {
      if (siteTagBg.rgba[3] < 200) {
        ng.push(`[${theme}] 録画一覧の site タグの地が塗りでない（不透明度 ${siteTagBg.rgba[3]}/255。${siteTagBg.value}）`)
      }
      checkContrast(theme, '録画一覧の site タグの文字 / muted の塗り', siteTagFg.rgba, siteTagFg, minTextContrast)
    }

    // ingest.state = transferring かつ expectedBytes 有りなので文言は「取り込み中 NN%」
    const ingestBadge = page.locator('ul span', { hasText: /^取り込み中/ })
    const ingestBg = await computedOf(ingestBadge, 'background-color')
    const ingestFg = await computedOf(ingestBadge, 'color')
    if (ingestFg === null || ingestBg === null) {
      ng.push(`[${theme}] IngestBadge が見つからない（ingestDisplay が undefined を返している?）`)
    } else {
      if (ingestBg.rgba[3] < 200) {
        ng.push(`[${theme}] IngestBadge の地が塗りでない（不透明度 ${ingestBg.rgba[3]}/255。${ingestBg.value}）`)
      }
      checkContrast(theme, 'IngestBadge の文字 / muted の塗り', ingestFg.rgba, ingestFg, minTextContrast)
    }

    await context.close()
  }

  // --- 録画一覧: 行の hover 中の副情報（`hover:bg-muted/40` + `text-muted-foreground`） ---
  //
  // 一覧の行は hover で `bg-muted/40` を敷き、その上に副情報（放送局名・日時・尺）が
  // `text-muted-foreground` のまま乗る。**Lighthouse は hover を測らない**ので
  // 監査には出ないが、`bg-muted` + `text-muted-foreground` と同族の組み合わせで
  // あることは変わらないので、下限を割るかどうかは推測せず実測する。同じ組み方は
  // 予約一覧・ホーム・番組リストの行にもあるが、地・文字のトークンと不透明度が
  // 同一なので代表として録画一覧の行で 1 回測る。
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    const row = page.locator('li').filter({ hasText: 'クラシック音楽館' }).first()
    // 副情報のうち、明示的な文字色を持たない素の span（バッジは text-foreground を
    // 明示しているので別の組み合わせになる）。
    const sub = row.locator('span', { hasText: /^ＮＨＫＢＳ$/ }).first()
    if ((await sub.count()) === 0) {
      ng.push(`[${theme}] 録画一覧の行の副情報（放送局名）が見つからない`)
    } else {
      const before = await sub.evaluate(readColor, 'color')
      await row.hover()
      // hover の背景は `transition-colors` を持たないので即時に乗るが、
      // 合成後の画素を読む前に 1 フレーム待つ
      await page.waitForTimeout(150)
      const after = await sub.evaluate(readColor, 'color')
      log(`  [${theme}] 一覧の行の副情報 文字=${after.value} / 乗っている面 hover 前=${before.backdrop} → hover 中=${after.backdrop}`)
      // **hover が本当に効いていることをここで検査する。** 効いていなければ
      // 測っているのは通常時の面で、「hover を測った」と言えるのに数字は
      // 通常時のもの、という空虚な成功になる（design.md「判定を足したことと、
      // それが効いていることは別」と同じ形の穴）
      const changed = [0, 1, 2].some((i) => Math.abs(after.backdrop[i] - before.backdrop[i]) >= 1)
      if (!changed) {
        ng.push(
          `[${theme}] 録画一覧の行を hover しても副情報が乗る面が変わらない（${after.backdrop}）` +
            ' --- hover の淡い地が効いていないか、locator が行の外を掴んでいる',
        )
      } else {
        checkContrast(
          theme,
          '一覧の行の hover 中の副情報の文字 / muted の半透明地',
          after.rgba,
          after,
          minTextContrast,
        )
      }
    }
    await context.close()
  }

  // --- 録画一覧: 選択中の行の副情報（`bg-muted/40` + `text-muted-foreground`） ---
  //
  // 選択モードで選んだ行は hover と同じ `bg-muted/40` が乗る（レビュー指摘。
  // 選択中だけ `bg-muted/50` のままだと副情報のコントラストが下限すれすれになる）。
  // hover と違って**常時見えるので Lighthouse の監査対象**に入るため、
  // 下限を割るかどうかは推測せず実測する。
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    const row = page.locator('li').filter({ hasText: 'クラシック音楽館' }).first()
    const sub = row.locator('span', { hasText: /^ＮＨＫＢＳ$/ }).first()
    if ((await sub.count()) === 0) {
      ng.push(`[${theme}] 録画一覧の行の副情報（放送局名）が見つからない（選択中の判定）`)
    } else {
      const before = await sub.evaluate(readColor, 'color')
      await page.getByRole('button', { name: '選択' }).click()
      await page.getByRole('checkbox', { name: 'クラシック音楽館を選択' }).click()
      // hover が乗る位置のままだと測っているものが hover の面になるので、
      // 行からマウスを離してから測る。
      await page.mouse.move(0, 0)
      await page.waitForTimeout(150)
      const after = await sub.evaluate(readColor, 'color')
      log(`  [${theme}] 一覧の行（選択中）の副情報 文字=${after.value} / 乗っている面 選択前=${before.backdrop} → 選択中=${after.backdrop}`)
      // **選択が本当に効いていることをここで検査する。** hover ブロックと同じ
      // 形の穴 --- 効いていなければ測っているのは通常時の面で、数字は
      // 空虚な成功になる（design.md「判定を足したことと、それが効いていることは別」）。
      const changed = [0, 1, 2].some((i) => Math.abs(after.backdrop[i] - before.backdrop[i]) >= 1)
      if (!changed) {
        ng.push(
          `[${theme}] 録画一覧の行を選択しても副情報が乗る面が変わらない（${after.backdrop}）` +
            ' --- 選択中の淡い地が効いていないか、locator が行の外を掴んでいる',
        )
      } else {
        checkContrast(
          theme,
          '一覧の行の選択中の副情報の文字 / muted の半透明地',
          after.rgba,
          after,
          minTextContrast,
        )
      }
    }
    await context.close()
  }

  // --- 録画詳細: muted の文字をページ地の上で測る ---
  // 詳細本体には独立した面を置かないため、`<dt>`「チャンネル」の実効背景が
  // body のページ地と一致することを確かめ、そのページ地に対してコントラストを測る。
  // 代表として `<dt>` を使う。説明文・品質イベントも同じ色トークンを使う。
  {
    const { context, page } = await open(desktop, theme, recordingDetailScreen)
    // `screens`（① のループ）に無い画面なので、明示的に掛けないと欠損文字列
    // 判定から漏れる。
    //
    // **`s.packets.toLocaleString()`（components/drop-stats-table.tsx の
    // DropStatsTable）はここでは撮れていない** --- それは別エンドポイント
    // （`/api/recordings/{id}/drop-stats`。`ListRecordingDropStatsResponseItem`）
    // が返す per-PID の値で、`dropSummary.packets` とは無関係。design.mjs は
    // このエンドポイントを常に `[]` にスタブしているため（:312）、
    // `DropStatsTable` の行は 1 件も描画されない（未検証の断言をしない。
    // CLAUDE.md「一度も真でなかった記述」）。
    await checkMissingStrings(page, `recording-detail/${theme}`)
    // 番組の明細（dt）は「番組」タブの中にある。デスクトップの既定は「版」なので開いてから測る。
    await page.getByRole('tab', { name: '番組' }).click()
    const dt = page.locator('dt', { hasText: /^チャンネル$/ }).first()
    const fg = await computedOf(dt, 'color')
    log(`  [${theme}] 録画詳細の文字=${fg?.value} ${fg?.rgba} / 実効背景=${fg?.backdrop}`)
    if (fg === null) {
      ng.push(`[${theme}] 録画詳細の <dt> が見つからない`)
    } else {
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (ground === null || !sameAsGround) {
        ng.push(
          `[${theme}] 録画詳細の文字がページの地に直接乗っていない` +
            `（文字の実効背景=${fg.backdrop}、ページ地=${ground?.backdrop ?? '取得できない'}）`,
        )
      } else {
        checkContrast(
          theme,
          '録画詳細の muted text / page ground',
          fg.rgba,
          ground,
          minTextContrast,
        )
      }
    }
    await context.close()

    // 360px では詳細欄に余計な面・内側余白を付けず、本文を16pxで折り返す。
    // open() の既定（pointer: 'coarse' なら isMobile: true）をこの呼び出しだけ false で上書きする。
    // isMobile: true では Chromium が layout viewport を内容幅へ広げ、innerWidth が scrollWidth に
    // 追従して横はみ出し判定が空虚に通る（下の本文 40rem の負の対照で documentWidth 656 /
    // innerWidth 656 を light/dark とも実測）。そのため false にする。
    const mobileContext = await open(mobile, theme, recordingDetailScreen, { pointer: 'coarse', isMobile: false })
    const mobilePage = mobileContext.page
    // PID 表は「記録」タブの中にある（異常がある録画だけ）。スマホの既定は「番組」。
    await mobilePage.getByRole('tab', { name: '記録' }).click()
    await mobilePage.getByRole('heading', { name: 'PID 別ドロップ統計' }).waitFor({ state: 'visible' })
    const mobileLayout = await mobilePage.evaluate(() => {
      const body = document.querySelector('[data-testid="recording-detail-body"]')
      const description = document.querySelector('[data-testid="recording-description"]')
      if (!(body instanceof HTMLElement) || !(description instanceof HTMLElement)) return null
      const bodyStyle = getComputedStyle(body)
      const descriptionStyle = getComputedStyle(description)
      const range = document.createRange()
      range.selectNodeContents(description)
      const descriptionLines = range.getClientRects().length
      const bodyRect = body.getBoundingClientRect()
      return {
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        bodyLeft: bodyRect.left,
        bodyRight: bodyRect.right,
        bodyBackground: bodyStyle.backgroundColor,
        paddingLeft: bodyStyle.paddingLeft,
        paddingRight: bodyStyle.paddingRight,
        bodyFontSize: bodyStyle.fontSize,
        descriptionFontSize: descriptionStyle.fontSize,
        descriptionLines,
        pointerCoarse: matchMedia('(pointer: coarse)').matches,
      }
    })
    if (mobileLayout === null) {
      ng.push(`[${theme}] 録画詳細/mobile: 本文か説明文が見つからない`)
    } else {
      if (mobileLayout.documentWidth > mobileLayout.viewportWidth) {
        ng.push(`[${theme}] 録画詳細/mobile: 横スクロールが発生（${mobileLayout.documentWidth}px > ${mobileLayout.viewportWidth}px）`)
      }
      if (mobileLayout.viewportWidth !== mobile.width || !mobileLayout.pointerCoarse) {
        ng.push(`[${theme}] 録画詳細/mobile: 360px/coarse の条件を満たさない（${JSON.stringify(mobileLayout)}）`)
      }
      if (mobileLayout.bodyLeft < 0 || mobileLayout.bodyRight > mobileLayout.viewportWidth) {
        ng.push(`[${theme}] 録画詳細/mobile: 本文が viewport 外にはみ出す（left=${mobileLayout.bodyLeft}, right=${mobileLayout.bodyRight}）`)
      }
      if (mobileLayout.bodyBackground !== 'rgba(0, 0, 0, 0)' || mobileLayout.paddingLeft !== '0px' || mobileLayout.paddingRight !== '0px') {
        ng.push(`[${theme}] 録画詳細/mobile: 本文に背景色または重複した左右余白がある（bg=${mobileLayout.bodyBackground}, padding=${mobileLayout.paddingLeft}/${mobileLayout.paddingRight}）`)
      }
      if (mobileLayout.bodyFontSize !== '14px' || mobileLayout.descriptionFontSize !== '16px') {
        ng.push(`[${theme}] 録画詳細/mobile: 文字サイズが想定外（本文=${mobileLayout.bodyFontSize}, 説明=${mobileLayout.descriptionFontSize}）`)
      }
      if (mobileLayout.descriptionLines < 2) {
        ng.push(`[${theme}] 録画詳細/mobile: 説明文が360px幅で折り返されない（${mobileLayout.descriptionLines}行）`)
      }
      log(`  [${theme}] 録画詳細/mobile 360px: ${JSON.stringify(mobileLayout)}`)

      const pidDetails = mobilePage.getByTestId('drop-stats-details')
      await pidDetails.locator('summary').click()
      const tableViewport = pidDetails.locator('.overflow-x-auto')
      if ((await tableViewport.count()) === 0) {
        ng.push(`[${theme}] 録画詳細/mobile: PID 表の .overflow-x-auto 容器が見つからない`)
      } else {
        const tableOverflow = await tableViewport.evaluate((el) => el.scrollWidth > el.clientWidth)
        if (!tableOverflow) {
          ng.push(`[${theme}] 録画詳細/mobile: はみ出す PID 表が局所スクロール領域に収まっていない`)
        }
        // 負の対照: 容器の overflow を無効化すると、表が自力でページ幅を押し広げることを確かめる。
        await tableViewport.evaluate((el) => {
          el.style.overflowX = 'visible'
        })
        const noGuard = await mobilePage.evaluate(() => document.documentElement.scrollWidth)
        await tableViewport.evaluate((el) => {
          el.style.overflowX = ''
        })
        if (noGuard <= mobileLayout.viewportWidth) {
          ng.push(`[${theme}] 録画詳細/mobile: PID 表容器の overflow 負の対照で横はみ出しを検知できない（${noGuard}px）`)
        } else {
          log(`  [${theme}] 録画詳細/mobile 負の対照 PID 表 overflow 無効: ${noGuard}px > ${mobileLayout.viewportWidth}px`)
        }
      }

      // 負の対照: 本文に 40rem の最小幅を一時設定し、ページ幅判定が実際に
      // 横はみ出しを拾うことを確かめてからスタイルを戻す。
      const detailBody = mobilePage.locator('[data-testid="recording-detail-body"]')
      const originalMinWidth = await detailBody.evaluate((el) => el.style.minWidth)
      await detailBody.evaluate((el) => {
        el.style.minWidth = '40rem'
      })
      const brokenWidth = await mobilePage.evaluate(() => ({
        documentWidth: document.documentElement.scrollWidth,
        viewportWidth: window.innerWidth,
      }))
      await detailBody.evaluate((el, value) => {
        el.style.minWidth = value
      }, originalMinWidth)
      if (brokenWidth.documentWidth <= brokenWidth.viewportWidth) {
        ng.push(`[${theme}] 録画詳細/mobile: 本文の40rem負の対照で横はみ出しを検知できない（${JSON.stringify(brokenWidth)}）`)
      } else {
        log(`  [${theme}] 録画詳細/mobile 負の対照 本文 40rem: ${brokenWidth.documentWidth}px > ${brokenWidth.viewportWidth}px`)
      }
    }
    await mobileContext.context.close()
  }

  // --- 予約一覧: 容量不足 = 琥珀（淡い地の上で読めるか） ---
  // 2 つのバッジを測る。シリーズ行の集約バッジ（件数つき。外側の `<a>` が地と文字を持つ）と、
  // 時間順の行（と展開した各回）が使う `CapacityShortfallBadge`（外側の `<a>` が淡い地、
  // 内側の `span[aria-hidden="true"]` が文字）。後者は集約バッジに判定を入れ替えたとき
  // 一度消えたので、時間順のページを別に開いて測り続ける。
  for (const variant of [
    {
      name: '容量不足バッジ（シリーズの集約）',
      grouping: null,
      wait: 'text=容量不足',
      badge: (page) =>
        page.locator('[data-testid="reservation-series-row"] a').filter({ hasText: /容量不足/ }).first(),
      label: (badge) => badge,
    },
    {
      name: 'チューナー不足バッジ（時間順の行）',
      grouping: 'time',
      wait: 'text=チューナー不足',
      // **淡い地を持つのは外側のバッジ、文字を持つのは内側の span。**
      // 内側だけを掴むと背景が透明になり、合成が恒等になって「地の上での比」を
      // 測ってしまう。外側から引いて、文字色は子から採る。外側は `<span>` ではなく `<a>`（`Link`）。
      badge: (page) => page.locator('ul a').filter({ hasText: /チューナー不足/ }).first(),
      label: (badge) => badge.locator('span[aria-hidden="true"]'),
    },
  ]) {
    const { context, page } = await open(desktop, theme, { ...screenOf('reservations'), wait: variant.wait }, {
      reservationGrouping: variant.grouping,
    })
    const badge = variant.badge(page)
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(variant.label(badge), 'color')
    log(`  [${theme}] ${variant.name} 文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null || bg === null) {
      ng.push(`[${theme}] ${variant.name}が見つからない`)
    } else if (bg.rgba[3] <= 8) {
      // 外側を掴めていない = 合成が効いていない。素通りさせず落とす
      ng.push(`[${theme}] ${variant.name}の地が透明（淡い地を持つ要素を掴めていない）`)
    } else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] ${variant.name}が琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      // `backdrop` の遡りが本当に効いているかを検査する。遡りが 1 段で止まると
      // `backdrop` はページの地と一致し、比は甘い方へ 0.5〜0.7 動く。一致 = 判定が壊れている
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (sameAsGround) {
        ng.push(
          `[${theme}] ${variant.name}の文字が乗る面がページの地と同じ（${fg.backdrop}）` +
            ' --- 淡い地の合成が効いていない',
        )
      }
      checkContrast(theme, `${variant.name}の文字 / 琥珀の淡い地`, fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 番組リスト: 放送中の行は色を使わない（太さだけ） ---
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const airing = page.locator('[data-testid="program-row-time"]')
    const count = await airing.count()
    let colored = 0
    for (let i = 0; i < count; i++) {
      const c = await airing.nth(i).evaluate(readColor, 'color')
      if (chroma(c.rgba) > 30) colored++
    }
    log(`  [${theme}] 番組リストの時刻 ${count} 件中、色付き ${colored} 件`)
    if (count === 0) ng.push(`[${theme}] 番組リストの時刻が見つからない`)
    // リストの ON AIR は希少ではない（チャンネル数ぶん同時に点く）ので、
    // タリーにしてはならない。`text-tally` に戻したらここで落ちる
    if (colored > 0) {
      ng.push(`[${theme}] 番組リストの時刻に信号色が付いている（${colored} 件。太さで示す規律）`)
    }
    await context.close()
  }

  // --- 番組リスト: sticky 日付見出し（issue #308） ---
  //
  // `bg-muted/80` の半透明地の上に文字が乗る（`components/program-list.tsx`）。
  // 半透明なので `computedOf` の `readColor` が祖先まで遡って合成した
  // `backdrop` を使わないと、地の上での比だけを見てしまい甘い数字が出る
  // （「コントラストは毎回測る」参照）。文字色は `text-foreground` に直した
  // ので、`text-muted-foreground` に戻す変異が入ったらここで落ちる。
  //
  // 引くのは `data-testid`（`program-row-time` と同じ流儀）。`h2` の 1 番目で
  // 引くと「/programs の既定ビューが list」「/programs 上の h2 が日付見出しの
  // 1 種類だけ」の 2 つに依存し、将来 PageHeader 等に h2 が入ったときに
  // **別の要素を測ったまま通る**。
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const heading = page.locator('[data-testid="program-list-date-heading"]').first()
    const fg = await computedOf(heading, 'color')
    log(`  [${theme}] 番組リストの日付見出し 文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null) {
      ng.push(`[${theme}] 番組リストの日付見出しが見つからない`)
    } else {
      checkContrast(theme, '番組リストの日付見出しの文字 / muted の半透明地', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 番組表グリッド: 現在時刻の線と札 = タリー / 容量超過の帯 = 琥珀 ---
  // （グリッドは `lg` 以上でしか出ないのでデスクトップのみ）
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const grid = page.getByRole('button', { name: '番組表' })
    if ((await grid.count()) === 0) {
      ng.push(`[${theme}] 表示形式の切り替えが出ていないのでグリッドを判定できない`)
    } else {
      await grid.first().click()
      await page.locator('[data-testid="program-grid-now-line"]').waitFor({ timeout: 10000 })
      await page.waitForTimeout(400)

      const line = await computedOf(page.locator('[data-testid="program-grid-now-line"]'), 'border-top-color')
      log(`  [${theme}] 現在時刻線 = ${line?.value} ${line?.rgba}`)
      if (line === null) ng.push(`[${theme}] 現在時刻線が見つからない`)
      else if (!isRed(line.rgba)) {
        ng.push(`[${theme}] 現在時刻線がタリーレッドでない（${line.value} = ${line.rgba}）`)
      }

      // 現在時刻の札は「塗り」。11px の赤い文字はダークの地で AA に届かないので、
      // タリーは塗りにしか使わない（design.md「タリーは塗り」）
      const chip = page.locator('[data-testid="program-grid-now-label"] span')
      const chipBg = await computedOf(chip, 'background-color')
      const chipFg = await computedOf(chip, 'color')
      log(`  [${theme}] 現在時刻の札 地=${chipBg?.value} ${chipBg?.rgba} / 文字=${chipFg?.rgba}`)
      if (chipBg === null || chipFg === null) ng.push(`[${theme}] 現在時刻の札が見つからない`)
      else {
        if (chipBg.rgba[3] < 200 || !isRed(chipBg.rgba)) {
          ng.push(`[${theme}] 現在時刻の札がタリーの塗りでない（${chipBg.value}）`)
        } else {
          checkContrast(theme, '現在時刻の札の文字 / タリーの塗り', chipFg.rgba, chipFg, minTextContrast)
        }
      }

      // 放送終了セル。ジャンル淡色の上に muted/30 を重ね、foreground を半透明にする。
      // `::before` は通常の backdrop 探索では拾えないので、疑似要素の色をセルの
      // 実効面へ合成してから文字との比を測る。
      const endedCell = page.locator('[data-testid="program-grid-cell"][data-ended="true"]').first()
      const endedFg = await computedOf(endedCell, 'color')
      const endedOverlay = await computedPseudoOf(endedCell, 'background-color', '::before')
      if (endedFg === null || endedOverlay === null) {
        ng.push(`[${theme}] 放送終了セルが見つからずコントラストを判定できない`)
      } else if (endedOverlay.rgba[3] === 0) {
        ng.push(`[${theme}] 放送終了セルの減光面が透明`)
      } else {
        const alpha = endedOverlay.rgba[3] / 255
        const endedBackdrop = endedOverlay.rgba
          .slice(0, 3)
          .map((c, i) => c * alpha + endedOverlay.backdrop[i] * (1 - alpha))
        endedBackdrop.push(255)
        log(
          `  [${theme}] 放送終了セル 文字=${endedFg.value} ${endedFg.rgba}` +
            ` / 減光後の面=${endedBackdrop}`,
        )
        checkContrast(
          theme,
          '放送終了セルの文字 / ジャンル淡色と muted の合成面',
          endedFg.rgba,
          { ...endedFg, backdrop: endedBackdrop },
          minTextContrast,
        )
      }

      // 容量超過の帯。罫線が区間の境界を伝えるので、線が琥珀であることを見る。
      //
      // 帯が重なっているのは番組セル（ジャンル淡色）で、**セルは帯の祖先ではなく
      // 兄弟**なので `backdrop` では拾えない。そこでグリッドのセルを**全件**集めて、
      // 罫線に対していちばん不利な面に対して測る（どのセルと実際に重なっているかは
      // 判定しない --- 全件は安全側の過大集合で、見落とす方向には倒れない）。
      // 帯自身の `backdrop` も候補に入れる: `background-clip` の既定は `border-box`
      // なので、罫線は自分の淡い地の上に描かれる
      const band = page.locator('[data-testid="capacity-band"]')
      const bandBorder = await computedOf(band, 'border-top-color')
      log(`  [${theme}] 容量超過の帯の罫線 = ${bandBorder?.value} ${bandBorder?.rgba}`)
      if (bandBorder === null) ng.push(`[${theme}] 容量超過の帯が見つからない`)
      else {
        if (!isAmber(bandBorder.rgba)) {
          ng.push(`[${theme}] 容量超過の帯の罫線が琥珀でない（${bandBorder.value} = ${bandBorder.rgba}）`)
        }
        const cells = await page.locator('[data-testid="program-grid-cell"]').all()
        const surfaces = [bandBorder.backdrop]
        for (const cell of cells) {
          surfaces.push((await cell.evaluate(readColor, 'background-color')).backdrop)
        }
        // 罫線の色に対していちばんコントラストが低くなる面を選ぶ
        const worst = surfaces.reduce((a, b) =>
          contrast(bandBorder.rgba, a) <= contrast(bandBorder.rgba, b) ? a : b,
        )
        log(`  [${theme}] 帯が重なる面 ${surfaces.length} 種のうち最も不利なもの = ${worst}`)
        checkContrast(theme, '容量超過の帯の罫線 / 最も不利な面', bandBorder.rgba, { backdrop: worst }, minUiContrast)
      }

      // 帯ラベルとセルの時刻文字の重なり（issue #460）。帯の上端がセルの上端に
      // 近いと、見た目のラベル（「BS-1」等）とセルの時刻文字（「23:30」）が
      // 同じ px に描かれてどちらも読めなくなる（ライトの
      // `programs-grid-light-desktop.png` で実際に確認済み）。jsdom はレイアウトを
      // 計算しないのでここでしか測れない --- rect の非交差を機械判定する。
      const labelHandles = await page.locator('[data-testid="capacity-band-label"]').all()
      const labelBoxes = []
      for (const l of labelHandles) {
        const box = await l.boundingBox()
        if (box === null) continue
        // 幅の切れは実際に truncate が効く内側の要素（アイコン分の幅を除いた
        // テキスト span）で測る --- 外側の箱はアイコン込みで常に時間軸列いっぱい
        // に張るので、外側だけを見ると切れを見落とす。
        const overflow = await l.evaluate((el) => {
          const textEl = el.querySelector('[data-testid="capacity-band-label-text"]') ?? el
          return { clientWidth: textEl.clientWidth, scrollWidth: textEl.scrollWidth, text: el.textContent }
        })
        labelBoxes.push({ ...box, ...overflow })
      }
      const cellTimeBoxes = (
        await Promise.all(
          (await page.locator('[data-testid="program-grid-cell-time"]').all()).map((c) =>
            c.boundingBox(),
          ),
        )
      ).filter((b) => b !== null)
      log(`  [${theme}] 帯ラベル ${labelBoxes.length} 件 / セル時刻 ${cellTimeBoxes.length} 件`)
      if (labelBoxes.length === 0) {
        ng.push(`[${theme}] 容量超過の帯ラベルが見つからない`)
      }

      const rectsIntersect = (a, b) =>
        a.x < b.x + b.width &&
        a.x + a.width > b.x &&
        a.y < b.y + b.height &&
        a.y + a.height > b.y

      // レビュー should 1: フィクスチャ（`overages`）に隣接する（重ならない）
      // 帯を複数入れてある。同一サイト内の不足区間はサーバー側で重ならないと
      // 保証されている（`internal/capacity/capacity.go` の `Compute`）ので
      // 「同時刻に重なる帯」はフィクスチャとしても不適切 --- ここで見るのは
      // 「隣接する帯のラベルがそれぞれ独立に見えるか」だけ。
      //
      // 件数ではなく集合（`expectedVisibleLabelTexts`）で照合する。件数だけだと
      // 「隠しているはずの CS 帯が描かれ、同時に別の帯のラベルが消える」変異が
      // 合計件数の一致で素通りする（issue #460 再々レビュー）。
      const actualLabelTexts = [...labelBoxes.map((l) => l.text)].sort()
      const wantLabelTexts = [...expectedVisibleLabelTexts].sort()
      if (JSON.stringify(actualLabelTexts) !== JSON.stringify(wantLabelTexts)) {
        ng.push(
          `[${theme}] 見えるラベルの集合が期待と異なる` +
            `（期待 ${JSON.stringify(wantLabelTexts)} / 実際 ${JSON.stringify(actualLabelTexts)}）`,
        )
      }
      const labelOverlaps = []
      for (let i = 0; i < labelBoxes.length; i++) {
        for (let j = i + 1; j < labelBoxes.length; j++) {
          if (rectsIntersect(labelBoxes[i], labelBoxes[j])) {
            labelOverlaps.push([labelBoxes[i], labelBoxes[j]])
          }
        }
      }
      if (labelOverlaps.length > 0) {
        log(`  [${theme}] ラベル同士の重なり ${JSON.stringify(labelOverlaps[0])}`)
        ng.push(
          `[${theme}] 帯ラベル同士の rect が ${labelOverlaps.length} 件重なっている` +
            '（同時刻の帯が積まれず、片方が隠れている）',
        )
      }

      // レビュー blocker 1: 時間軸列（56px）に収まらず省略記号の外に文字が
      // 切れていないか。scrollWidth が clientWidth を超えていれば、見えている
      // 分の外に切れた文字がある（「BS-1」のような短い形のはずが「チューナ…」
      // まで切れて種別も本数も読めなくなった実例がレビューで見つかった）。
      for (const l of labelBoxes) {
        if (l.scrollWidth > l.clientWidth) {
          ng.push(
            `[${theme}] 帯ラベル「${l.text}」が時間軸列の幅で切れている` +
              `（clientWidth ${l.clientWidth} / scrollWidth ${l.scrollWidth}）`,
          )
        }
      }

      // レビュー blocker 2: ラベルが帯の全高を塗って時間軸の目盛りや現在時刻
      // チップを消していないか。ラベルは自分の内容ぶんの小さい高さしか
      // 持たないはず（旧実装は帯の高さぶん引き伸ばしていた --- 3 時間の帯なら
      // ラベルの高さも 3 時間ぶんの px になっていた）。
      const maxReasonableLabelHeightPx = 24
      for (const l of labelBoxes) {
        if (l.height > maxReasonableLabelHeightPx) {
          ng.push(
            `[${theme}] 帯ラベルの高さが ${l.height}px --- 帯の全高を塗って` +
              `目盛りを消している疑い（上限の目安 ${maxReasonableLabelHeightPx}px）`,
          )
        }
      }
      // 上の高さ判定を回避しても実際に目盛りが隠れていないかは直接見る ---
      // レビューの実測（`coveredTicks: ["00:00"]`）と同じものをここで測る。
      // 判定は rect の交差（`rectsIntersect`）にする --- 「全高を包含する
      // か」だと、ラベル（16px）が目盛り（実測 18.5px）より低い限り算術的に
      // 真になり得ず、判定として永久に発火しない（レビューで指摘）。
      const tickBoxes = (
        await Promise.all(
          (await page.locator('[data-testid="program-grid-tick"]').all()).map((t) =>
            t.boundingBox(),
          ),
        )
      ).filter((b) => b !== null)
      const coveredTicks = []
      for (const t of tickBoxes) {
        for (const l of labelBoxes) {
          if (rectsIntersect(l, t)) coveredTicks.push(t)
        }
      }
      if (coveredTicks.length > 0) {
        log(`  [${theme}] coveredTicks: ${coveredTicks.length} 件`)
        ng.push(`[${theme}] 帯ラベルが時間軸の目盛りと ${coveredTicks.length} 件重なっている`)
      }

      // レビュー should 2: 前提条件そのものを表明する。帯の上端付近にセルの
      // 時刻要素が無ければ、下の非交差判定は「そもそも重なりようがない」
      // だけで通ってしまい、`FIXED_NOW` や `gridPxPerHour` を変えただけで
      // 気付かず空虚に通るようになる。
      const nearPx = 20
      const hasAdjacentCell = labelBoxes.some((l) =>
        cellTimeBoxes.some((c) => Math.abs(c.y - l.y) <= nearPx),
      )
      if (!hasAdjacentCell) {
        ng.push(
          `[${theme}] 前提条件が崩れている: 帯ラベルの上端付近（${nearPx}px 以内）に` +
            'セルの時刻要素が無い。以降の非交差判定はこの回では何も検証していない',
        )
      }

      const overlaps = []
      for (const l of labelBoxes) {
        for (const c of cellTimeBoxes) {
          if (rectsIntersect(l, c)) overlaps.push({ label: l, cell: c })
        }
      }
      if (overlaps.length > 0) {
        log(`  [${theme}] 重なり ${JSON.stringify(overlaps[0])}`)
        ng.push(
          `[${theme}] 帯ラベルとセルの時刻文字の rect が ${overlaps.length} 件重なっている`,
        )
      }
    }
    await context.close()
  }

  // --- ルール一覧: 「条件なし」の警告 = 琥珀 ---
  {
    const { context, page } = await open(desktop, theme, screenOf('rules'))
    const warn = page.locator('span', { hasText: /^条件なし（すべての番組にマッチ）$/ })
    const fg = await computedOf(warn, 'color')
    log(`  [${theme}] ルールの「条件なし」= ${fg?.value} ${fg?.rgba}`)
    if (fg === null) ng.push(`[${theme}] ルールの「条件なし」警告が見つからない`)
    else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] ルールの「条件なし」が琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      checkContrast(theme, '「条件なし」の文字 / 乗っている面', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 空状態: 走査線の上の文字（EmptyState。components/page.tsx） ---
  //
  // 検索は初期状態（未検索）が EmptyState なので、既存の 'search' 画面が
  // そのまま撮れる。**縞の 2 色（`--scan-gap` = background-color /
  // `--scan-lit` = background-image。index.css の `.scanlines` 参照）を
  // 両方測り、両方が文字色との AA を満たすことを見る。**
  //
  // 「間隙側だけが最悪ケード」という前提を置かない --- ライトはたまたま
  // 間隙（明るい）側が字（墨）に対して不利だが、ダークは逆に輝線側が字
  // （紙白）に対して不利になる。片方しか測らないと「輝線を文字と同じ色に
  // する」変異（グリフの半分が地に溶ける）が判定をすり抜ける
  // （design.md の失敗事例と同じ形の穴。かつてここは間隙側だけを見ていた）
  {
    const { context, page } = await open(desktop, theme, screenOf('search'))
    const empty = page
      .locator('div.scanlines', { hasText: '条件を指定して検索してください' })
      .first()
    const gap = await computedOf(empty, 'background-color')
    const lit = await computedVar(empty, '--scan-lit')
    const fg = await computedOf(empty, 'color')
    log(
      `  [${theme}] 空状態の走査線 間隙=${gap?.value} ${gap?.rgba} / ` +
        `輝線=${lit?.value} ${lit?.rgba} / 文字=${fg?.value} ${fg?.rgba}`,
    )
    if (gap === null || lit === null || fg === null) {
      ng.push(`[${theme}] 空状態（EmptyState）の走査線が見つからない`)
    } else {
      if (gap.rgba[3] < 200) {
        ng.push(`[${theme}] 空状態の走査線の間隙が不透明でない（${gap.value}）`)
      }
      if (lit.rgba[3] < 200) {
        ng.push(`[${theme}] 空状態の走査線の輝線が不透明でない（${lit.value}）`)
      }
      for (const [side, measured] of [
        ['間隙', gap],
        ['輝線', lit],
      ]) {
        const c = oklchChroma(measured.value)
        if (c === null || c > 0.02) {
          ng.push(`[${theme}] 空状態の走査線の${side}が無彩でない（oklch chroma ${c}。${measured.value}）`)
        }
      }
      checkContrast(theme, '空状態の文字 / 走査線の間隙', fg.rgba, fg, minTextContrast)
      checkContrast(theme, '空状態の文字 / 走査線の輝線', fg.rgba, { backdrop: lit.rgba }, minTextContrast)
    }
    await context.close()
  }

  // --- 読み込み中: Skeleton / ListSkeleton の走査線（components/page.tsx） ---
  //
  // 文字は乗らないプレースホルダなので AA の対象ではない。ここで見るのは
  // 縞の 2 色（`--scan-gap` / `--scan-lit`）が両方とも不透明・無彩かという
  // 構造の存在確認。API が即座に返る作りだと遷移直後に解決してしまうので、
  // `/api/recordings` だけ遅延させて捕まえる
  {
    const context = await browser.newContext({
      viewport: { width: desktop.width, height: desktop.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ delayPath: '/api/recordings', delayMs: 5000 }))
    await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
    const skeleton = page.locator('.scanlines').first()
    await skeleton.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] 読み込み中の走査線（.scanlines）が出ない`)
    })
    const gap = await computedOf(skeleton, 'background-color')
    const lit = await computedVar(skeleton, '--scan-lit')
    // bgImage の NG（下記）は gap/lit が両方とも非 null（＝要素が存在した）の
    // 分岐でしか出さないので、ここでの null は要素消失ではなくスタイルそのものの
    // 欠如を指す。`.catch(() => null)` で飲んでよい。
    const bgImage = await skeleton
      .evaluate((el) => getComputedStyle(el).backgroundImage)
      .catch(() => null)
    log(
      `  [${theme}] 読み込み中の走査線 間隙=${gap?.value} ${gap?.rgba} / 輝線=${lit?.value} ${lit?.rgba} / ` +
        `background-image=${bgImage && bgImage !== 'none' ? 'あり' : 'なし'}`,
    )
    if (gap === null || lit === null) {
      ng.push(`[${theme}] 読み込み中（Skeleton）の走査線が見つからない`)
    } else {
      if (gap.rgba[3] < 200) {
        ng.push(`[${theme}] 読み込み中の走査線の間隙が不透明でない（${gap.value}）`)
      }
      if (lit.rgba[3] < 200) {
        ng.push(`[${theme}] 読み込み中の走査線の輝線が不透明でない（${lit.value}）`)
      }
      for (const [side, measured] of [
        ['間隙', gap],
        ['輝線', lit],
      ]) {
        const c = oklchChroma(measured.value)
        if (c === null || c > 0.02) {
          ng.push(`[${theme}] 読み込み中の走査線の${side}が無彩でない（oklch chroma ${c}。${measured.value}）`)
        }
      }
      if (bgImage === null || bgImage === 'none') {
        ng.push(`[${theme}] 読み込み中に走査線の輝線（background-image）が無い（${bgImage}）`)
      }
    }
    await context.close()
  }

  // --- ライブ: ON AIR バッジ = タリーの塗り + 走査線（pages/live.tsx OnAirBadge） ---
  //
  // 縞の 2 色（`--scan-gap` = `--tally` そのもの / `--scan-lit` = `--tally` を
  // 明度だけ落とした段。index.css の `.tally-scanlines` 参照）を両方測る。
  // タリーは既定で赤なので両方に `isRed` を掛け、文字とのコントラストも
  // 両方で見る --- 間隙だけでは「輝線を文字と同じ色にする」変異
  // （グリフの半分が塗りに溶ける）を見逃す
  {
    const { context, page } = await open(desktop, theme, screenOf('live'))
    const badge = page.locator('span.tally-scanlines', { hasText: /^ON AIR$/ }).first()
    const gap = await computedOf(badge, 'background-color')
    const lit = await computedVar(badge, '--scan-lit')
    const fg = await computedOf(badge, 'color')
    log(
      `  [${theme}] ON AIR バッジ 間隙=${gap?.value} ${gap?.rgba} / ` +
        `輝線=${lit?.value} ${lit?.rgba} / 文字=${fg?.value} ${fg?.rgba}`,
    )
    if (gap === null || lit === null || fg === null) {
      ng.push(`[${theme}] ON AIR バッジが見つからない（いま放送中の番組が無いスタブになっていないか）`)
    } else {
      if (chroma(fg.rgba) > 30) {
        ng.push(`[${theme}] ON AIR バッジの文字に色が付いている（塗り + 無彩の文字であるべき。${fg.value}）`)
      }
      for (const [side, measured] of [
        ['間隙', gap],
        ['輝線', lit],
      ]) {
        if (measured.rgba[3] < 200) {
          ng.push(`[${theme}] ON AIR バッジの${side}が塗りでない（不透明度 ${measured.rgba[3]}/255。${measured.value}）`)
          continue
        }
        if (!isRed(measured.rgba)) {
          ng.push(`[${theme}] ON AIR バッジの${side}がタリーレッドでない（マゼンタ等への色相ずれの疑い。${measured.value} = ${measured.rgba}）`)
        }
      }
      // **地に対する比だけを見ると甘い数字が出る。** 間隙（`--tally` そのもの。
      // 録画中バッジと同じ値）・輝線（`--tally` を明度だけ落とした段）の
      // 両方を文字色との比で見る。輝線は間隙より暗いので理屈のうえでは
      // 間隙側が不利なはずだが、**「そのはず」を判定の根拠にはしない** ---
      // 両方を実測して両方に下限を掛けることで、想定が外れても気付ける形にする
      if (gap.rgba[3] >= 200) {
        checkContrast(theme, 'ON AIR バッジの文字 / タリー走査線の間隙', fg.rgba, fg, minTextContrast)
      }
      if (lit.rgba[3] >= 200) {
        checkContrast(theme, 'ON AIR バッジの文字 / タリー走査線の輝線', fg.rgba, { backdrop: lit.rgba }, minTextContrast)
      }
      // この分岐は gap/lit/fg が全て非 null（＝バッジが存在した）ときにしか
      // 入らないので、ここでの null は要素消失ではなくスタイルの欠如を指す。
      // `.catch(() => null)` で飲んでよい。
      const bgImage = await badge
        .evaluate((el) => getComputedStyle(el).backgroundImage)
        .catch(() => null)
      if (bgImage === null || bgImage === 'none') {
        ng.push(`[${theme}] ON AIR バッジに走査線の輝線（background-image）が無い（${bgImage}）`)
      }
    }
    await context.close()
  }
}

// --- ③ フォントの実描画判定 ---
//
// **`getComputedStyle().fontFamily` は指定した文字列を返すだけで、ブラウザが
// 実際にどのフォントを選んで描画したかは別**（docs/frontend/stack.md「フォント
// は英数字と和文で 2 書体を使い分ける」）。CDP の `CSS.getPlatformFontsForNode`
// で実際に使われたフォントを見る。フォントファイルが unicode-range で分割
// されていて、かつ Noto Sans JP の import が消えても `--font-sans` の
// フォールバック先（システムフォント）が和文をレンダリングできてしまうため、
// **この判定を外すと「Noto Sans JP を削除して和文がシステムフォントに戻る」
// 事故がスクリーンショット上は気付かれないまま緑で通り続ける**。
log('\n=== ③ フォントの判定 ===')

/**
 * platformFontsOf は CDP 経由で selector に一致するノードの実使用フォントを返す。
 *
 * **`cdp` は呼び出し元が `DOM.enable` / `CSS.enable` を送信済みのセッションで
 * あること。** `CSS.enable` を呼ばずに `CSS.getPlatformFontsForNode` を呼ぶと
 * **空配列ではなく protocol error で throw する**
 * （`Protocol error (CSS.getPlatformFontsForNode): CSS agent was not enabled`。
 * 実測で確認済み）。セッションを作るタイミングはナビゲーションの前後どちらでも
 * 結果は変わらない（両方実測済み。以前このファイルに「ナビゲーション後に
 * セッションを作ると空配列が返る」という誤った記述があったが、それは次の罠を
 * 誤って帰属したものだった）。
 *
 * **本当の罠は selector の選び方。** `main` や `body` のような「直接はテキストを
 * 持たずブロック要素だけを子に持つ」要素を渡すと常に空配列が返る（throw ではない）。
 * `CSS.getPlatformFontsForNode` はノード自身のインラインレイアウト（実際に
 * テキストランを持つ層）に紐付いたフォント使用だけを返し、ブロックの子孫を
 * 再帰集約しない。実際にテキストを直接持つ要素（番組リストの行
 * `li[data-program-id]` 等）を渡す必要がある。
 */
async function platformFontsOf(cdp, selector) {
  const { root } = await cdp.send('DOM.getDocument')
  const { nodeId } = await cdp.send('DOM.querySelector', { nodeId: root.nodeId, selector })
  if (!nodeId) return null
  const { fonts } = await cdp.send('CSS.getPlatformFontsForNode', { nodeId })
  return fonts.map((f) => `${f.familyName} x${f.glyphCount}`)
}

{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  // このブロックでしか CDP を使わないので、ここでセッションを作って有効化する
  // （`open()` は全画面 × テーマ × ビューポートで呼ばれるので、そちらに置くと
  // 使わない呼び出しでも毎回セッションを作ることになる）。
  const cdp = await context.newCDPSession(page)
  await cdp.send('DOM.enable')
  await cdp.send('CSS.enable')

  // 番組リストの行は時刻（Geist が担当）と番組名（Noto Sans JP が担当）を
  // 同じ行に持つので、1 要素で両方の実使用フォントが確認できる
  const fonts = await platformFontsOf(cdp, 'li[data-program-id]')
  log(`  実使用フォント（番組リストの行） = ${fonts?.join(', ') ?? '(取れず)'}`)
  if (fonts === null) {
    ng.push('フォント判定: li[data-program-id] が見つからない')
  } else {
    if (!fonts.some((f) => f.includes('Noto Sans JP'))) {
      ng.push(`和文が Noto Sans JP で描画されていない（実使用: ${fonts.join(', ')}）`)
    }
    if (!fonts.some((f) => f.includes('Geist'))) {
      ng.push(`英数字が Geist で描画されていない（実使用: ${fonts.join(', ')}）`)
    }
    // 「1 グリフだけ Noto Sans JP、残りはシステムフォント」という部分的な退行は
    // 上の `some` だけでは検出できない（1 件でも Noto があれば真になる）。
    // 和文システムフォント（--font-sans のフォールバック候補）が実使用に
    // 一切現れないことも見て、検出力を上げる。
    const systemJpFonts = fonts.filter((f) => /Hiragino|Yu Gothic|Meiryo/.test(f))
    if (systemJpFonts.length > 0) {
      ng.push(`和文の一部がシステムフォントに落ちている（実使用: ${fonts.join(', ')}）`)
    }
  }

  // tabular-nums が和文まじりの文字列でも実際に等幅を作っているか。
  // canvas 2D の `font` ショートハンドには font-variant-numeric を渡せないので、
  // 実要素を DOM に挿して getBoundingClientRect で幅を測る（実描画の幅そのもの）。
  // `normal` 側も測って、判定が「たまたま両方同じ幅」ではなく tabular-nums の
  // 効果そのものを見ていることを確認する。
  const widths = await page.evaluate(() => {
    function width(text, variant) {
      const el = document.createElement('span')
      el.style.position = 'absolute'
      el.style.visibility = 'hidden'
      el.style.whiteSpace = 'pre'
      el.style.fontVariantNumeric = variant
      el.textContent = text
      document.body.appendChild(el)
      const w = el.getBoundingClientRect().width
      el.remove()
      return w
    }
    return {
      tabularA: width('第11話', 'tabular-nums'),
      tabularB: width('第88話', 'tabular-nums'),
      normalA: width('第11話', 'normal'),
      normalB: width('第88話', 'normal'),
    }
  })
  log(
    `  第11話/第88話 幅（tabular-nums） = ${widths.tabularA.toFixed(2)} / ${widths.tabularB.toFixed(2)}`,
  )
  log(
    `  第11話/第88話 幅（normal）       = ${widths.normalA.toFixed(2)} / ${widths.normalB.toFixed(2)}`,
  )
  if (Math.abs(widths.tabularA - widths.tabularB) > 0.5) {
    ng.push(
      `tabular-nums が和文まじりの文字列で等幅を作っていない（${widths.tabularA.toFixed(2)} / ${widths.tabularB.toFixed(2)}）`,
    )
  }
  if (Math.abs(widths.normalA - widths.normalB) < 0.5) {
    ng.push(
      'tabular-nums 無指定でも同じ幅になっている（この判定が tabular-nums の効果を検出できていない）',
    )
  }

  await context.close()
}

// --- ④ モバイル: 「その他」ポップオーバーの判定 ---
//
// 固定されたボトムバーの上に浮くオーバーレイなので、画面端でのはみ出し・
// バーの上に出るか・safe-area との重なりは jsdom では原理的に測れない
// （`app-shell.test.tsx` が固定しているのは DOM の有無と順序だけ）。
//
// タブの本数は ARIA の `listitem` ロールではなく `<li>` を直接数える。
// 実測（このスクリプトが駆動する Chromium）: `nav.getByRole('listitem').count()`
// も CDP の AX ツリー（`Accessibility.getFullAXTree`）も listitem を 4 件返し、
// `list-style-type` を `disc` に戻しても変わらない --- CSS 依存の暗黙ロール抑制は
// 観測されていない。それでも `<li>` を直接数えるのは、ロールの計算をブラウザの
// アクセシビリティ実装に依存させたくないという保険であり、「抑制が起きるから」
// ではない（起きるかどうかは未検証。理由にしない）。
log('\n=== ④ 「その他」ポップオーバーの判定 ===')
for (const theme of themes) {
  const { context, page } = await open(mobile, theme, screenOf('programs'))

  const nav = page.locator('nav[aria-label="主ナビゲーション"]').last()
  const tabCount = await nav.locator('li').count()
  log(`  [${theme}] ボトムタブの本数 = ${tabCount}`)
  if (tabCount !== 4) {
    ng.push(`[${theme}] ボトムタブが 4 個でない（${tabCount} 個。「その他」への集約が効いていない）`)
  }

  const trigger = nav.getByRole('button', { name: 'その他' })
  if ((await trigger.count()) === 0) {
    ng.push(`[${theme}] 「その他」トリガーが見つからない`)
  } else {
    await trigger.click()
    const menu = page.getByRole('dialog', { name: 'その他のナビゲーション' })
    await menu.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] 「その他」を開いてもポップオーバーが現れない`)
    })
    if ((await menu.count()) > 0) {
      await page.waitForTimeout(300) // 開くアニメーションの終了を待つ

      const file = path.join(OUT_DIR, `more-menu-open-${theme}-mobile.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(page, `more-menu-open/${theme}`)

      const triggerBox = await trigger.boundingBox()
      const menuBox = await menu.boundingBox()
      if (triggerBox === null || menuBox === null) {
        ng.push(`[${theme}] 「その他」のバウンディングボックスが取れない`)
      } else {
        if (menuBox.x < 0 || menuBox.x + menuBox.width > mobile.width) {
          ng.push(
            `[${theme}] 「その他」ポップオーバーが横方向にビューポートをはみ出す` +
              `（x=${menuBox.x.toFixed(1)}, w=${menuBox.width.toFixed(1)}, vw=${mobile.width}）`,
          )
        }
        if (menuBox.y < 0 || menuBox.y + menuBox.height > mobile.height) {
          ng.push(
            `[${theme}] 「その他」ポップオーバーが縦方向にビューポートをはみ出す` +
              `（y=${menuBox.y.toFixed(1)}, h=${menuBox.height.toFixed(1)}, vh=${mobile.height}）`,
          )
        }
        // ボトムバーの上に出ること（下端が沈んでバーの後ろに隠れていないか）。
        // トリガーの上端より上にポップオーバーの下端が来ていることを見る
        if (menuBox.y + menuBox.height > triggerBox.y + 1) {
          ng.push(
            `[${theme}] 「その他」ポップオーバーがトリガーの上端より上に出ていない` +
              `（menu bottom=${(menuBox.y + menuBox.height).toFixed(1)}, trigger top=${triggerBox.y.toFixed(1)}）`,
          )
        }
        log(
          `  [${theme}] ポップオーバー x=${menuBox.x.toFixed(1)} y=${menuBox.y.toFixed(1)} ` +
            `w=${menuBox.width.toFixed(1)} h=${menuBox.height.toFixed(1)} / トリガー top=${triggerBox.y.toFixed(1)}`,
        )
      }

      // role="dialog" の間は Tab が背後のページへ抜けないこと。最後→最初と
      // 最初→最後の両方向を実ブラウザで確認する。
      const menuLinks = menu.getByRole('link')
      const closeButton = menu.getByRole('button', { name: 'メニューを閉じる' })
      const menuLinkCount = await menuLinks.count()
      if (menuLinkCount < 2 || (await closeButton.count()) === 0) {
        ng.push(`[${theme}] 「その他」のフォーカストラップを判定できる操作要素が足りない`)
      } else {
        const closeTabIndex = await closeButton.evaluate((el) => el.tabIndex)
        if (closeTabIndex >= 0) {
          ng.push(`[${theme}] 「その他」の見えない閉じるボタンが Tab 順に入っている`)
        }

        // 待ちが失敗しても、直後に `document.activeElement === el` で実際の状態を
        // 直接読み直す（forwardTrapped/backwardTrapped）ので、待ちの成否を経由せず
        // 本当の結果を測っている。待ちはタイムアウトを早める（1000ms）ためだけの
        // ものなので `.catch(() => {})` で飲んでよい --- 待ちが失敗＝実際に
        // フォーカスが移っていない、という結果自体が下の NG 文言（「Tab が
        // ポップオーバー外へ抜ける」）と一致し、スタイル回帰の NG とは混ざらない。
        await menuLinks.last().focus()
        await page.keyboard.press('Tab')
        await page
          .waitForFunction(
            () =>
              document.activeElement?.tagName === 'A' &&
              document.activeElement.closest('[aria-label="その他のナビゲーション"]') !== null,
            undefined,
            { timeout: 1000 },
          )
          .catch(() => {})
        const forwardTrapped = await menuLinks
          .first()
          .evaluate((el) => document.activeElement === el)
        await menuLinks.first().focus()
        await page.keyboard.press('Shift+Tab')
        await page
          .waitForFunction(
            () =>
              document.activeElement?.tagName === 'A' &&
              document.activeElement.closest('[aria-label="その他のナビゲーション"]') !== null,
            undefined,
            { timeout: 1000 },
          )
          .catch(() => {})
        const backwardTrapped = await menuLinks
          .last()
          .evaluate((el) => document.activeElement === el)
        log(`  [${theme}] フォーカストラップ: 前=${forwardTrapped} / 後=${backwardTrapped}`)
        if (!forwardTrapped || !backwardTrapped) {
          ng.push(
            `[${theme}] 「その他」で Tab がポップオーバー外へ抜ける` +
              `（前=${forwardTrapped} / 後=${backwardTrapped}）`,
          )
        }
      }
    }
  }

  await context.close()
}

// --- ④-A キーボード操作と標的サイズ ---

/** sameRgb は alpha を除く実画素 3 値が一致するかを見る。 */
function sameRgb(a, b) {
  return a.slice(0, 3).every((value, index) => value === b[index])
}

/**
 * checkExplicitFocusRing は対象を focus-visible にし、明示リングと --ring の色を測る。
 * box-shadow の存在だけでは透明色でも通るので、内側の border を canvas で画素化して
 * --ring と比較する。outline は none でなければブラウザ既定との二重表示として落とす。
 *
 * **before を測る前の `blur()` を外さないこと。** 呼び出し側がポップオーバー/
 * ダイアログを開いた直後に呼ぶことがあり、その手のコンポーネントは開いた瞬間に
 * 候補へ既定フォーカスを非同期に当てることがある（#521。base-ui の Popover が実例）。
 * `blur()` 無しで before を測ると、その既定フォーカスがまだ来ていないか来た後かで
 * before の値が実行のたびに割れ、before/after の差分判定が偽陽性の NG を出す。
 * 4 呼び出し元で確認済み: 3 つは毎回フレッシュなページ遷移直後の呼び出しで
 * `blur()` は no-op（アプリ側に `focusout`/`blur` ハンドラは無く、base-ui の
 * `useDismiss` も `focusout` を束ねていない）。唯一 before に影響するのが
 * ChannelOption の既定フォーカスで、そちらは元々 60% の確率で偽陽性を出していた
 * （つまり実質的に一度も安定して機能していなかった）ので失う実カバレッジは無く、
 * 常時リング付き（本来の回帰）は before/after が一致するのでむしろ新たに検出できる。
 */
async function checkExplicitFocusRing(page, locator, label, theme) {
  if ((await locator.count()) === 0) {
    ng.push(`[${theme}] ${label} が見つからず focus-visible を判定できない`)
    return
  }

  const target = locator.first()
  // before を測る前に明示的に blur する。呼び出し側（ChannelPicker のポップアップ）が
  // 開いた直後に先頭候補へ既定フォーカスを当てることがあり、それを当てにしたまま
  // before を測ると「既にリング付きの状態」を基準にしてしまい、後段の diff
  // （before と after が同じなら NG）が常に真になる（issue #521）。
  await target.evaluate((el) => el.blur())
  const beforeShadow = await target.evaluate((el) => getComputedStyle(el).boxShadow)
  await target.focus()
  const focusVisible = await target.evaluate((el) => el.matches(':focus-visible'))
  const focusStyle = await target.evaluate((el) => {
    const style = getComputedStyle(el)
    return { boxShadow: style.boxShadow, outlineStyle: style.outlineStyle }
  })
  const border = await computedOf(target, 'border-top-color')
  const ring = await computedVar(page.locator('html'), '--ring')
  log(
    `  [${theme}] ${label}: focus-visible=${focusVisible} border=${border?.rgba} ` +
      `shadow=${focusStyle.boxShadow}`,
  )

  if (!focusVisible) {
    ng.push(`[${theme}] ${label} に :focus-visible が付かない`)
  }
  if (focusStyle.boxShadow === 'none' || focusStyle.boxShadow === beforeShadow) {
    ng.push(`[${theme}] ${label} の focus-visible で明示リングが出ない`)
  }
  if (focusStyle.outlineStyle !== 'none') {
    ng.push(`[${theme}] ${label} に明示リングとブラウザ outline が二重に出る`)
  }
  if (border === null || ring === null || !sameRgb(border.rgba, ring.rgba)) {
    ng.push(
      `[${theme}] ${label} のフォーカス縁が --ring の実画素でない` +
        `（border=${border?.rgba} / ring=${ring?.rgba}）`,
    )
  }
}

// スキップリンクは DOM の先頭にあるだけでなく、Tab 1 回で実際に見える寸法へ戻り、
// Enter 後は URL の fragment だけでなく main 自身へフォーカスが移ることを見る。
{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur())
  await page.keyboard.press('Tab')
  const skip = page.getByRole('link', { name: '本文へ移動' })
  if ((await skip.count()) === 0) {
    ng.push('スキップリンク: 「本文へ移動」が見つからない')
  } else {
    const focused = await skip.evaluate((el) => document.activeElement === el)
    const skipMetrics = await skip.evaluate((el) => {
      const rect = el.getBoundingClientRect()
      const style = getComputedStyle(el)
      return {
        x: rect.x,
        y: rect.y,
        width: rect.width,
        height: rect.height,
        position: style.position,
        zIndex: Number(style.zIndex),
      }
    })
    log(
      `  スキップリンク: focused=${focused} box=${skipMetrics.width}x${skipMetrics.height} ` +
        `position=${skipMetrics.position} z-index=${skipMetrics.zIndex}`,
    )
    if (!focused) ng.push('スキップリンク: Tab 1 回でフォーカスされない')
    if (skipMetrics.width < 24 || skipMetrics.height < 24) {
      ng.push('スキップリンク: フォーカスされても 24px 以上の可視寸法に戻らない')
    }
    if (
      skipMetrics.position !== 'fixed' ||
      skipMetrics.x < 0 ||
      skipMetrics.y < 0 ||
      skipMetrics.zIndex < 50
    ) {
      ng.push('スキップリンク: 固定位置または前面の重なり順になっていない')
    }
    await page.keyboard.press('Enter')
    // 待ちが失敗しても、直後に同じ条件を `mainFocused` として直接読み直す。
    // 待ちはタイムアウトを早めるだけのもので、失敗＝実際に main へ移っていない、
    // という結果自体が下の NG 文言と一致するので `.catch(() => {})` で飲んでよい。
    await page.waitForFunction(() => document.activeElement?.id === 'main').catch(() => {})
    const mainFocused = await page.evaluate(() => document.activeElement?.id === 'main')
    if (!mainFocused) ng.push('スキップリンク: Enter 後に main へフォーカスが移らない')
  }
  await context.close()
}

// 明示リングは DayStrip の濃い選択地を含め、ライト / ダークの両方で測る。
for (const theme of themes) {
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    await checkExplicitFocusRing(
      page,
      page.getByRole('button', { name: 'リスト', exact: true }),
      'Chip',
      theme,
    )
    await checkExplicitFocusRing(
      page,
      page.getByRole('group', { name: '日付' }).getByRole('button').first(),
      'DayStrip',
      theme,
    )

    const picker = page.getByRole('button', { name: /^チャンネル:/ })
    if ((await picker.count()) === 0) {
      ng.push(`[${theme}] チャンネルピッカーが見つからない`)
    } else {
      await picker.focus()
      await page.keyboard.press('Enter')
      const popup = page.getByRole('dialog', { name: 'チャンネル' })
      const popupOpened = await popup
        .waitFor({ timeout: 5000 })
        .then(() => true)
        .catch(() => false)
      if (!popupOpened) {
        // ポップアップが開かなかったときに、スタイル回帰の NG（`checkExplicitFocusRing`
        // が出す「明示リングが出ない」等）と同じ文言を出さないための分岐。
        // なお #521 の実際の原因はこれではない（下記のコメント参照）。
        ng.push(`[${theme}] チャンネルピッカーのポップアップが開かない（待ちがタイムアウト）`)
      } else {
        // #521 の実際の原因: base-ui の Popover は開いた直後、先頭候補（＝この
        // 「すべて」自身）へ既定フォーカスを非同期に当てる（queueMicrotask →
        // requestAnimationFrame 1 回。@base-ui/react の
        // floating-ui-react/utils/enqueueFocus.js）。これを待たずに
        // `checkExplicitFocusRing` が before を測ると、その既定フォーカスが
        // 「まだ来ていない」か「もう来た」かで before の box-shadow が実行のたびに
        // 割れ、before/after の差分判定が偽陽性の NG を出す（実機で確認: 同一コードで
        // alreadyFocused が true/false どちらにもなり、before !== 'none' と NG が
        // 8/8 で一致）。この既定フォーカスの発火は queueMicrotask → rAF 1 回の
        // 一本道で、フレーム数は負荷が増えても変わらない（増えるのは 1 フレームの
        // 長さ）ため、rAF を 2 回挟めば確実に済んでいる。
        await page.evaluate(
          () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))),
        )
        await checkExplicitFocusRing(
          page,
          popup.getByRole('checkbox', { name: 'すべて', exact: true }),
          'ChannelOption',
          theme,
        )
      }
    }
    await context.close()
  }

  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    await checkExplicitFocusRing(
      page,
      page.getByRole('button', { name: 'ライブラリ', exact: true }),
      'ViewTab',
      theme,
    )
    await context.close()
  }
}

// 共通 Button の sm は 32px、見た目を広げない容量不足バッジは ::before だけを
// 24px にする。バッジの z-index も見て、行全面リンクの上で当たり判定が生きることを固定する。
{
  const { context, page } = await open(desktop, 'light', screenOf('rules'))
  // issue #728: sm の実測サンプルはルール名の編集リンク（size="sm" の
  // `min-h-8`）に付け替える。旧「検索しながら編集」ボタンは無くなった。
  const smallButton = page.getByRole('link', { name: /^ルール「.+」を編集$/ }).first()
  const box = (await smallButton.count()) === 0 ? null : await smallButton.boundingBox()
  log(`  Button size=sm: height=${box?.height}`)
  if (box === null || box.height < 32) {
    ng.push(`Button size="sm" の高さが 32px 未満（${box?.height ?? '取得不能'}px）`)
  }
  await context.close()
}

{
  const { context, page } = await open(desktop, 'light', screenOf('reservations'))
  const badge = page.getByRole('link', { name: /チューナーが不足しています/ }).first()
  if ((await badge.count()) === 0) {
    ng.push('容量不足バッジが見つからず当たり判定を測れない')
  } else {
    const target = await badge.evaluate((el) => {
      const rect = el.getBoundingClientRect()
      const before = getComputedStyle(el, '::before')
      const pseudoHeight = Number.parseFloat(before.height)
      return {
        visualHeight: rect.height,
        hitHeight: Number.isFinite(pseudoHeight) ? pseudoHeight : 0,
        position: before.position,
        zIndex: getComputedStyle(el).zIndex,
      }
    })
    log(
      `  容量不足バッジ: visual=${target.visualHeight}px hit=${target.hitHeight}px ` +
        `z-index=${target.zIndex}`,
    )
    if (target.position !== 'absolute' || target.hitHeight < 24) {
      ng.push(`容量不足バッジの当たり判定が 24px 未満（${target.hitHeight}px）`)
    }
    if (target.hitHeight <= target.visualHeight) {
      ng.push('容量不足バッジの見た目ごと拡大され、::before で当たりだけを広げていない')
    }
    if (target.zIndex === 'auto' || Number(target.zIndex) <= 0) {
      ng.push('容量不足バッジが行全面リンクより上の重なり順を持たない')
    }
  }
  await context.close()
}

// --- ④-A' issue #779: 多行トーストの action / close を縦中央へ揃える ---
//
// `titles` の輪番へ長い題名を混ぜると、既存のスクリーンショットや別判定で
// 予約する番組が変わる。そこでこの判定だけ `toastLayout` の専用応答にし、
// 長い番組を 1 件だけ返して予約する。モバイルの `max-w-sm` で本当に 2 行以上へ
// 折り返したことを先に確認してから、メッセージ span と右側の action 群 / close
// の `getBoundingClientRect()` の縦中心を比較する。jsdom ではこの判定はできない。
{
  const { context, page } = await open(mobile, 'light', screenOf('programs'), { toastLayout: true })
  const row = page.locator('li[data-program-id]').filter({ hasText: toastLayoutProgram.name }).first()
  const rowVisible = await row
    .waitFor({ timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  if (!rowVisible) {
    ng.push('[#779] 多行トースト用の専用番組が表示されない')
  } else {
    const expander = row.locator('button[aria-expanded]').first()
    const expanderVisible = await expander
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false)
    if (!expanderVisible) {
      ng.push('[#779] 多行トースト用番組の展開ボタンが見つからない')
    } else {
      await expander.click()
      await page.waitForTimeout(250)
      const reserveAction = row.locator('[data-program-action="reserve"] button').first()
      const reserveVisible = await reserveAction
        .waitFor({ timeout: 5000 })
        .then(() => true)
        .catch(() => false)
      if (!reserveVisible) {
        ng.push('[#779] 多行トースト用番組の予約ボタンが見つからない')
      } else {
        await reserveAction.click()
        const toast = page.locator('[aria-live="polite"] > div').last()
        const action = toast.getByRole('button', { name: '取消', exact: true })
        const toastVisible = await action
          .waitFor({ timeout: 5000 })
          .then(() => true)
          .catch(() => false)
        if (!toastVisible) {
          ng.push('[#779] 予約後の多行トーストが表示されない')
        } else {
          const metrics = await toast.evaluate((el) => {
            const rectOf = (node) => {
              if (!(node instanceof HTMLElement)) return null
              const rect = node.getBoundingClientRect()
              return {
                top: rect.top,
                height: rect.height,
                center: rect.top + rect.height / 2,
              }
            }
            const message = el.querySelector(':scope > span')
            const actions = el.querySelector(':scope > div')
            const cancel = el.querySelector('button:not([aria-label="閉じる"])')
            const close = el.querySelector('button[aria-label="閉じる"]')
            return {
              message: rectOf(message),
              actions: rectOf(actions),
              cancel: rectOf(cancel),
              close: rectOf(close),
            }
          })
          const messageHeight = metrics.message?.height ?? 0
          log(
            `  [#779] 多行トースト message=${metrics.message?.center?.toFixed(1) ?? '—'} ` +
              `actions=${metrics.actions?.center?.toFixed(1) ?? '—'} ` +
              `cancel=${metrics.cancel?.center?.toFixed(1) ?? '—'} ` +
              `close=${metrics.close?.center?.toFixed(1) ?? '—'} ` +
              `messageHeight=${messageHeight.toFixed(1)}px`,
          )
          if (metrics.message === null || metrics.actions === null || metrics.cancel === null || metrics.close === null) {
            ng.push('[#779] 多行トーストの message / action / close の矩形が取得できない')
          } else if (messageHeight <= 24) {
            ng.push(
              `[#779] 多行トーストが 1 行のまま（message height ${messageHeight.toFixed(1)}px）`,
            )
          } else {
            const centerDelta = (a, b) => Math.abs(a.center - b.center)
            const deltas = {
              actions: centerDelta(metrics.message, metrics.actions),
              cancel: centerDelta(metrics.message, metrics.cancel),
              close: centerDelta(metrics.message, metrics.close),
            }
            const maxDelta = Math.max(...Object.values(deltas))
            if (maxDelta > 1) {
              ng.push(
                `[#779] 多行トーストの縦中心が揃っていない（最大差 ${maxDelta.toFixed(1)}px ` +
                  ` / actions ${deltas.actions.toFixed(1)}px / cancel ${deltas.cancel.toFixed(1)}px / ` +
                  `close ${deltas.close.toFixed(1)}px）`,
              )
            }
          }
        }
      }
    }
  }
  await context.close()
}

// 主要画面の実装された操作標的を、ポインタの性質ごとに同じ実ブラウザで列挙する。
// ここでは 44px を一律に要求しない。密度を保った管理画面の共通下限は 24px とし、
// 日付・チャンネル候補・行の主操作・ライブチャンネルの 44px と、モバイルナビの
// 幅44px・高さ56pxは下記の個別契約で固定する。
for (const profile of targetPointerProfiles) {
  for (const screen of targetScreens) {
    const { context, page } = await open(
      profile.viewport,
      'light',
      screen,
      screen.name === 'recording-detail'
        ? { pointer: profile.pointer, multiSite: true, recordingDetailScenario: 'completed' }
        : { pointer: profile.pointer },
    )
    const measurement = await measureInteractiveTargets(page, `${profile.name}/${screen.name}`)
    if (
      screen.name === 'recordings' &&
      !measurement.targets.some((target) => target.tag === 'summary')
    ) {
      ng.push(`[${profile.name}/recordings] <summary> を操作標的として列挙できない`)
    }
    await context.close()
  }
}

/** 役割ごとの個別寸法を採用している標的の実寸を固定する。 */
async function checkMinimumTargetSize(locator, label, minimumWidth, minimumHeight = minimumWidth) {
  const count = await locator.count()
  if (count === 0) {
    ng.push(`${label} が見つからず標的サイズを判定できない`)
    return
  }
  for (let index = 0; index < count; index += 1) {
    const box = await locator.nth(index).boundingBox()
    log(
      `  ${label}[${index + 1}/${count}]: ` +
        `${box === null ? '取得不能' : `${box.width.toFixed(1)}×${box.height.toFixed(1)}px`} ` +
        `(基準 ${minimumWidth}×${minimumHeight}px)`,
    )
    if (box === null || box.width < minimumWidth || box.height < minimumHeight) {
      ng.push(
        `${label}[${index + 1}] が ${box === null ? '取得不能' : `${box.width.toFixed(1)}×${box.height.toFixed(1)}px`} ` +
          `(基準 ${minimumWidth}×${minimumHeight}px 未満)`,
      )
    }
  }
}

// 録画詳細の前後ナビゲーションはモバイルでは映像の中央に置き、タッチ時に44pxを確保する。
{
  const { context, page } = await open(mobile, 'light', recordingDetailScreen, {
    pointer: 'coarse',
    multiSite: true,
    recordingDetailScenario: 'completed',
  })
  const previous = page.getByRole('button', { name: '前のチャプター' })
  const next = page.getByRole('button', { name: '次のチャプター' })
  await checkMinimumTargetSize(previous, '前のチャプター', 44)
  await checkMinimumTargetSize(next, '次のチャプター', 44)
  await page.getByRole('button', { name: '再生設定' }).click()
  const editorEntry = page.getByRole('menuitem', { name: 'チャプターを直す' })
  await checkMinimumTargetSize(editorEntry, '「チャプターを直す」メニュー項目', 44)

  const details = page.locator('[data-testid="chapter-edit-layout"]')
  await editorEntry.click()
  await details.waitFor({ timeout: 10000 })
  await details.evaluate((node) => {
    node.dataset.e2eIdentity = 'chapter-details-before-timeupdate'
  })
  const video = page.locator('video')
  for (let index = 0; index < 4; index += 1) {
    // 再生位置が動かないと currentSeconds が変わらず再描画が起きない。位置ごと動かして投げる。
    await video.evaluate((node, seconds) => {
      node.currentTime = seconds
      node.dispatchEvent(new Event('timeupdate', { bubbles: true }))
    }, index + 1)
    await page.waitForTimeout(250)
  }
  const editorState = await details.evaluate((node) => ({
    same: node.dataset.e2eIdentity === 'chapter-details-before-timeupdate',
  }))
  if (editorState.same !== true) {
    ng.push('4Hzのtimeupdate再描画後にチャプター編集画面が作り直される')
  }
  await context.close()
}

// 高頻度の主操作は、共通下限とは別に 44px 高の配置契約を保つ
// （モバイル主ナビだけは高さ56px）。
{
  const { context, page } = await open(mobile, 'light', screenOf('programs'), { pointer: 'coarse' })
  await checkMinimumTargetSize(
    page.locator('button[aria-label*="月"][aria-label*="("]'),
    '日付セル',
    24,
    44,
  )
  const channelTrigger = page.getByRole('button', { name: /^チャンネル:/ }).first()
  await checkMinimumTargetSize(channelTrigger, 'チャンネルピッカー', 44)
  await channelTrigger.click()
  const channelPopup = page.getByRole('dialog', { name: 'チャンネル' })
  await channelPopup.waitFor({ timeout: 5000 }).catch(() => {})
  await checkMinimumTargetSize(channelPopup.getByRole('checkbox'), 'チャンネル候補', 44)
  await context.close()
}

{
  const { context, page } = await open(mobile, 'light', screenOf('live'), { pointer: 'coarse' })
  const bottomNav = page.getByTestId('bottom-nav')
  const bottomLinks = bottomNav.getByRole('link')
  const bottomItemCount = await bottomNav.locator('li').count()
  log(`  モバイル主ナビの項目本数=${bottomItemCount}`)
  if (bottomItemCount !== 4) ng.push(`モバイル主ナビの項目本数が 4 ではない（${bottomItemCount}）`)
  await checkMinimumTargetSize(bottomLinks, 'モバイル主ナビ', 44, 56)
  await checkMinimumTargetSize(bottomNav.getByRole('button'), 'モバイル「その他」', 44, 56)
  await checkMinimumTargetSize(page.locator('nav[aria-label="チャンネル一覧"] a'), 'ライブチャンネル', 44)
  await context.close()
}

// タッチで展開した番組行の予約 / ライブ操作と、#729 のトースト action / close
// も同じ実寸判定に載せる。予約 API はこのスクリプトのスタブで 204 を返す。
{
  const { context, page } = await open(mobile, 'light', screenOf('programs'), { pointer: 'coarse' })
  const row = page.locator('li[data-program-id]').filter({ hasText: '大相撲中継' }).first()
  const expander = row.locator('button[aria-expanded]').first()
  if ((await expander.count()) === 0) {
    ng.push('番組行の展開ボタンが見つからず行主操作を判定できない')
  } else {
    await expander.click()
    await page.waitForTimeout(250)
    const reserveAction = row.locator('[data-program-action="reserve"] button').first()
    await checkMinimumTargetSize(reserveAction, '番組行の予約操作', 44)
    const liveAction = row.locator('[data-program-action="live"] a, [data-program-action="live"] button')
    if ((await liveAction.count()) > 0) {
      await checkMinimumTargetSize(liveAction.first(), '番組行のライブ操作', 44)
    }
    await reserveAction.click()
    const toast = page.locator('[aria-live="polite"] > div').last()
    await toast.getByRole('button', { name: '取消', exact: true }).waitFor({ timeout: 5000 }).catch(() => {})
    await checkMinimumTargetSize(toast.getByRole('button', { name: '取消', exact: true }), 'トースト action', 32)
    await checkMinimumTargetSize(toast.getByRole('button', { name: '閉じる', exact: true }), 'トースト close', 28)
    await measureInteractiveTargets(page, 'coarse/programs/展開行+トースト')
  }
  await context.close()
}

// --- ⑤ 録画詳細: キーボードだけで <video> へ到達できるか ---
//
// jsdom では測れない領域（web/e2e/README.md §デザイン）。`<video>` に
// `tabIndex={-1}` を付けると、プログラムからの `.focus()` は変わらず効く
// ため jsdom のユニットテスト（focus spy）は通り続けるが、実ブラウザの
// キーボード Tab 走査からは完全に外れる（M5-4 / issue #227 でこの属性を
// 一度入れて実際に壊した退行そのもの）。視聴は詳細ページ（/recordings/$id）に
// 寄せた（issue #311）ので、到達性の判定もそこへ移した --- 一覧は展開も
// プレイヤーも持たない。ページ先頭からの Tab 走査で `<video>` に止まるかを見る
// （行の展開経路が無くなったので、旧判定を一覧に残すと直後に赤のままになる）。
{
  const { context, page } = await open(desktop, 'light', screenOf('recordings'))
  const row = page.locator('li', { hasText: 'クラシック音楽館' })
  const detailLink = row.getByRole('link', { name: 'クラシック音楽館' })
  if ((await detailLink.count()) === 0) {
    ng.push('キーボード到達性: encoded 付き録画の詳細リンクが見つからない')
  } else {
    // 旧実装の常時「再生」列が残っていないことを、実ブラウザでも見る。
    if ((await row.getByRole('button', { name: /再生/ }).count()) > 0) {
      ng.push('録画一覧: 常時の「再生」ボタンが残っている')
    }
    await detailLink.focus()
    await page.keyboard.press('Enter')
    // どちらの待ちが失敗しても、直後に `<video>` の有無を直接数え直して判定する
    // （遷移が終わっていなければ <video> も無いので、この 1 つの NG 文言に
    // 正しく合流する）。待ちはタイムアウトを早めるだけなので飲んでよい。
    await page.waitForURL('**/recordings/12', { timeout: 5000 }).catch(() => {})
    await page.locator('video').first().waitFor({ timeout: 5000 }).catch(() => {})
    if ((await page.locator('video').count()) === 0) {
      ng.push('キーボード到達性: 詳細ページに <video> が出ない')
    } else {
      // ページ先頭から Tab 走査する。「戻る」など先行の Tab stop があるので
      // 上限は緩める --- 見たいのは「<video> がいつか Tab 順に現れる
      // （tabIndex={-1} で外れていない）」ことで、正確な回数ではない。
      await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur())
      const maxPresses = 12
      let reachedAt = null
      for (let i = 1; i <= maxPresses; i++) {
        await page.keyboard.press('Tab')
        const tag = await page.evaluate(() => document.activeElement?.tagName)
        if (tag === 'VIDEO') {
          reachedAt = i
          break
        }
      }
      log(`  キーボード到達性: 詳細ページで Tab ${reachedAt ?? `${maxPresses}+`} 回で video`)
      if (reachedAt === null) {
        ng.push(
          `キーボード到達性: 詳細ページで Tab ${maxPresses} 回以内に <video> へ到達しない` +
            '（<video> に tabIndex={-1} を付けて Tab 順から外していないか確認する。' +
            'M5-4 で一度この属性を付けて実際に壊した退行）',
        )
      }
    }
  }
  await context.close()
}

// --- ⑥ Button: フォーカスリング / border-color は遷移しない・hover の色と
//     active の押下フィードバックは遷移する（issue #294） ---
//
// `transition-all` は `box-shadow`（focus-visible の ring-3）と
// `focus-visible:border-ring`（1px 罫線の border-color）の**両方**を
// 遷移対象に含めてしまい、キーボードフォーカスの瞬間にリングの縁までが
// アニメーションで出現する（WCAG のフォーカス可視・「Focus rings that
// animate in」という tell）。border-color は ring の外側の淡い box-shadow
// より内側にある最も鮮明な縁なので、box-shadow だけ外しても border-color が
// 残れば「リングが遷移していない」は満たせない（レビューで実測: alpha
// 0 → 0.0073 → 0.88 → 1 と 150ms かけてフェードインしていた）。
//
// **jsdom では測れない。** CSS transition が実際に走るかどうかはブラウザの
// transition エンジンでしか観測できない --- `transition-property` という
// 文字列がクラス名に含まれているかを読むテストは「そのユーティリティを
// 書いた」ことしか確認できず、**実際に発火するか**（他のクラスに上書きされて
// いないか・ブラウザが実際に transitionstart を上げるか）までは保証しない。
// ここでは実際の `transitionstart` イベント（発火したプロパティ名つき）を
// ボタン要素自身に張った listener で観測する。
//
// 3 方向を見る: ① focus-visible で box-shadow / outline / border-*-color が
// 遷移**しない**こと、② hover で背景色の遷移が従来どおり**起きる**こと
// （issue の受け入れ基準）、③ active で `translate`（`active:...:translate-y-px`
// の押下フィードバック）が遷移**すること** --- Tailwind v4 は
// `translate-y-px` を `transform` ではなく `translate` プロパティへ
// コンパイルするため、挙げるプロパティを間違えると「押下フィードバックを
// 残すつもりが実際には何も遷移しない」という死んだ意図になり得る
// （レビューで実測して発覚）。①だけでなく③も持たせることで、将来
// `translate` がクラス列から静かに落ちても検出できる。
log('\n=== ⑥ Button: フォーカスリング / border-color / hover / active の遷移（issue #294） ===')
{
  const { context, page } = await open(desktop, 'light', screenOf('search'))
  const button = page.getByRole('button', { name: '検索' })
  if ((await button.count()) === 0) {
    ng.push('フォーカスリング: 検索ボタン（shared Button）が見つからない')
  } else {
    // listener はボタン要素自身に張る（document + capture ではない）。
    // transitionstart はバブルするので、対象要素に直接張れば document まで
    // 遡る理由が無く、他要素の transition と混ざる余地も無くなる。
    await button.first().evaluate((el) => {
      el.__transitioned = []
      el.addEventListener('transitionstart', (e) => el.__transitioned.push(e.propertyName))
    })
    const readTransitioned = () => button.first().evaluate((el) => el.__transitioned)
    const resetTransitioned = () =>
      button.first().evaluate((el) => {
        el.__transitioned = []
      })

    // border-*-color は `border-color`（ショートハンド）ではなく
    // `border-top-color` 等のロングハンドで上がる（実測）。`outline` も
    // `outline-color` / `outline-width` / `outline-style` のロングハンドで
    // 上がりうる（実測: transition-all のもとで `outline-width` が上がった）
    // ので、どちらも前方一致で拾う。box-shadow はロングハンドを持たないので
    // 完全一致でよい。
    const isRingLike = (p) =>
      p === 'box-shadow' || p.startsWith('outline') || (p.startsWith('border-') && p.endsWith('-color'))

    // Playwright の `.focus()` は script からの `element.focus()` で、実際の
    // Tab 走査ではないが、ページ読み込み後まだ一度もポインタ操作をしていない
    // 状態でのプログラム的フォーカスは Chromium で :focus-visible を伴う。
    // それを前提にせず、次の行で実際に :focus-visible が付いたかを確認して
    // いるので、前提が崩れていれば「遷移していない」ではなく専用の NG で落ちる
    // （検証していない前提を持たない --- CLAUDE.md「測っていない挙動を断言しない」）。
    await button.first().focus()
    const isFocusVisible = await button.first().evaluate((el) => el.matches(':focus-visible'))
    if (!isFocusVisible) {
      ng.push(
        'フォーカスリング: 検索ボタンに :focus-visible が付かない（判定の前提が崩れている。' +
          'focus() の呼び方を見直す）',
      )
    } else {
      // transition-all（既定 150ms）や border-color が残っていれば
      // box-shadow / outline* / border-*-color の transitionstart が飛ぶ。
      // 150ms を安全側に見て待つ
      await page.waitForTimeout(400)
      const transitioned = await readTransitioned()
      log(`  :focus-visible で遷移したプロパティ: [${transitioned.join(', ') || '(なし)'}]`)
      for (const prop of transitioned.filter(isRingLike)) {
        ng.push(
          `フォーカスリング: :focus-visible で ${prop} が遷移している` +
            '（box-shadow・outline・border-*-color は遷移対象から外す）',
        )
      }
    }

    // 両方向: hover の色遷移は従来どおり効くこと（issue の受け入れ基準）
    await button.first().evaluate((el) => el.blur())
    await page.mouse.move(0, 0)
    await resetTransitioned()
    await button.first().hover()
    await page.waitForTimeout(400)
    const hoverTransitioned = await readTransitioned()
    log(`  hover で遷移したプロパティ: [${hoverTransitioned.join(', ') || '(なし)'}]`)
    if (!hoverTransitioned.includes('background-color') && !hoverTransitioned.includes('color')) {
      ng.push(
        'フォーカスリング: hover で色（background-color / color）の遷移が起きていない' +
          '（transition-all を外した副作用で色遷移まで消えていないか確認する）',
      )
    }

    // active の押下フィードバック（`translate-y-px`）は遷移**すること**。
    // ここが無いと、将来 `translate` がクラス列から落ちても気付けない
    // （このレビューで実際に `transform`（誤り）のまま気付かれずにいた）。
    await resetTransitioned()
    const box = await button.first().boundingBox()
    if (box === null) {
      ng.push('フォーカスリング: 検索ボタンの座標が取れず active を再現できない')
    } else {
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
      await page.mouse.down()
      await page.waitForTimeout(400)
      const activeTransitioned = await readTransitioned()
      log(`  active で遷移したプロパティ: [${activeTransitioned.join(', ') || '(なし)'}]`)
      if (!activeTransitioned.includes('translate')) {
        ng.push(
          'フォーカスリング: active で translate の遷移が起きていない' +
            '（`active:...:translate-y-px` の押下フィードバックが snap している。' +
            'Tailwind v4 は translate-y-px を transform ではなく translate に' +
            'コンパイルするので、遷移対象に挙げるなら translate で挙げる）',
        )
      }
      await page.mouse.up()
    }
  }
  await context.close()
}

// --- ⑦ アニメーション: prefers-reduced-motion で縮退する（issue #296） ---
//
// jsdom は `prefers-reduced-motion` の matchMedia も CSS の実際の適用も測れない
// （README §デザイン冒頭）。Playwright の `reducedMotion` コンテキストオプションで
// OS の設定をエミュレートし、`getComputedStyle` の実測をオラクルにする。
//
// **両方向を見る。** 縮退側だけを見る判定は、動きを恒久的に殺した実装
// （`no-preference` でも動かない）を通してしまう --- CLAUDE.md テスト規律
// 「分岐を直したら両方向を確認する」と同型の穴。
//
// 対象は Skeleton の `animate-pulse`、モバイル「その他」ポップオーバー
// （`ui/popover.tsx`）の `slide-in-from-*` / `zoom-in-95`、共通 `Button` の
// 押下フィードバック（`active:...:translate-y-px` の `transition`）。
// 予約実行中ボタンの `animate-spin` は #298 で削除した（楽観更新が確定表示を
// 出しているのにスピナーがそれを覆い高速応答時に点滅していた）ので対象外。
log('\n=== ⑦ アニメーション: prefers-reduced-motion の縮退（issue #296） ===')

// 縮退後の継続時間はほぼ 0（`index.css` は 0.01ms）、既定の継続時間は
// どれも 100ms 以上（ポップオーバー 100ms / Button 150ms / pulse 2s）
// なので、50ms を境に両方向をまとめて判定できる。
const REDUCE_THRESHOLD_MS = 50

/** parseCssTime は `"150ms"` / `"0.15s"` / カンマ区切り（複数プロパティ）を ms にする。 */
function parseCssTime(v) {
  const first = (v ?? '').split(',')[0].trim()
  // Chromium は極小の値（0.01ms 相当）を指数表記でシリアライズする
  // （実測: `1e-05s`）。仮数部に `e±N` を許す形にしておかないと、縮退後の
  // 値そのものが「読めない」で null になり、判定が意図と逆に落ちる。
  const m = /^([\d.]+(?:e[-+]?\d+)?)(m?s)$/i.exec(first)
  if (m === null) return null
  return m[2].toLowerCase() === 's' ? Number(m[1]) * 1000 : Number(m[1])
}

/** motionOf は要素の animation-duration / transition-duration / opacity をまとめて読む。 */
async function motionOf(locator) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate((el) => {
    const cs = getComputedStyle(el)
    return {
      animationDuration: cs.animationDuration,
      transitionDuration: cs.transitionDuration,
      opacity: cs.opacity,
    }
  })
}

/** newMotionContext は `open()` と違い `reducedMotion` を指定できる素の context。 */
async function newMotionContext(viewport, reducedMotion) {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
    deviceScaleFactor: 2,
    reducedMotion,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  return { context, page }
}

for (const reducedMotion of ['reduce', 'no-preference']) {
  const isReduced = reducedMotion === 'reduce'

  // --- Skeleton の animate-pulse（読み込み中） ---
  {
    const { context, page } = await newMotionContext(desktop, reducedMotion)
    await installApiStubs(page, apiHandler({ delayPath: '/api/recordings', delayMs: 5000 }))
    await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
    const skeleton = page.locator('.animate-pulse').first()
    await skeleton.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${reducedMotion}] Skeleton の .animate-pulse が見つからない`)
    })
    const m = await motionOf(skeleton)
    if (m === null) {
      ng.push(`[${reducedMotion}] Skeleton（.animate-pulse）が見つからない`)
    } else {
      const ms = parseCssTime(m.animationDuration)
      log(
        `  [${reducedMotion}] Skeleton animation-duration=${m.animationDuration} ` +
          `opacity=${m.opacity}`,
      )
      if (isReduced) {
        if (ms === null || ms > REDUCE_THRESHOLD_MS) {
          ng.push(
            `[reduce] Skeleton の animate-pulse が縮退していない` +
              `（animation-duration=${m.animationDuration}）`,
          )
        }
        // 縮退後に不可視・不読になっていないか（`animation: none` ではなく
        // 継続時間を切り詰める判断の理由そのもの。index.css のコメント参照）。
        if (Number(m.opacity) < 0.5) {
          ng.push(
            `[reduce] Skeleton が縮退後に不透明度 ${m.opacity} まで下がり判読できない`,
          )
        }
      } else if (ms === null || ms < REDUCE_THRESHOLD_MS) {
        ng.push(
          `[no-preference] Skeleton の animate-pulse が既定（2s 周期）のまま動いていない` +
            `（animation-duration=${m.animationDuration}）`,
        )
      }
    }
    await context.close()
  }

  // --- モバイル「その他」ポップオーバーの slide-in-from-* / zoom-in-95 ---
  {
    const { context, page } = await newMotionContext(mobile, reducedMotion)
    await installApiStubs(page, apiHandler())
    await page.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
    const nav = page.locator('nav[aria-label="主ナビゲーション"]').last()
    const trigger = nav.getByRole('button', { name: 'その他' })
    // `trigger.count()` は DOM に存在するかしか見ず、可視かは見ない。待ちが
    // 失敗したのに count() だけで先へ進むと、非表示のまま `.click()` して
    // Playwright 自身のアクショナビリティ待ちで長時間ハングした末に無関係な
    // 例外で落ちる（NG として報告されない）おそれがある。待ちの成否そのもので
    // 分岐する。
    const triggerVisible = await trigger
      .waitFor({ timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    if (!triggerVisible) {
      ng.push(`[${reducedMotion}] 「その他」トリガーが見つからない（ポップオーバー判定）`)
    } else {
      await trigger.click()
      const menu = page.getByRole('dialog', { name: 'その他のナビゲーション' })
      await menu.waitFor({ timeout: 5000 }).catch(() => {
        ng.push(`[${reducedMotion}] 「その他」ポップオーバーが開かない`)
      })
      const m = await motionOf(menu)
      if (m === null) {
        ng.push(`[${reducedMotion}] ポップオーバー要素が見つからない`)
      } else {
        const ms = parseCssTime(m.animationDuration)
        log(`  [${reducedMotion}] ポップオーバー animation-duration=${m.animationDuration}`)
        if (isReduced) {
          if (ms === null || ms > REDUCE_THRESHOLD_MS) {
            ng.push(
              `[reduce] ポップオーバーの slide-in/zoom-in が縮退していない` +
                `（animation-duration=${m.animationDuration}）`,
            )
          }
        } else if (ms === null || ms < REDUCE_THRESHOLD_MS) {
          ng.push(
            `[no-preference] ポップオーバーの slide-in/zoom-in が既定のまま動いていない` +
              `（animation-duration=${m.animationDuration}）`,
          )
        }
      }
    }
    await context.close()
  }

  // --- 共通 Button の押下フィードバック（translate）の transition ---
  {
    const { context, page } = await newMotionContext(desktop, reducedMotion)
    await installApiStubs(page, apiHandler())
    await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
    const button = page.getByRole('button', { name: '検索' })
    // 待ちが失敗しても `motionOf` が `locator.count()` で見つからなさを直接
    // 見分けて distinct な NG を出す（`.evaluate` 自体は可視性を要求しないので
    // ハングもしない）。待ちはタイムアウトを早めるだけなので飲んでよい。
    await button.waitFor({ timeout: 10000 }).catch(() => {})
    const m = await motionOf(button)
    if (m === null) {
      ng.push(`[${reducedMotion}] 検索ボタンが見つからない（Button 遷移判定）`)
    } else {
      const ms = parseCssTime(m.transitionDuration)
      log(`  [${reducedMotion}] Button transition-duration=${m.transitionDuration}`)
      if (isReduced) {
        if (ms === null || ms > REDUCE_THRESHOLD_MS) {
          ng.push(
            `[reduce] Button の transition-duration が縮退していない` +
              `（${m.transitionDuration}）`,
          )
        }
      } else if (ms === null || ms < REDUCE_THRESHOLD_MS) {
        ng.push(
          `[no-preference] Button の transition-duration が既定（150ms）のまま動いていない` +
            `（${m.transitionDuration}）`,
        )
      }
    }
    await context.close()
  }
}

// 数値は docs に転記しない（転記した瞬間に二重管理になる）。docs は
// 「ここで測る」とだけ言い、実際の数値はこの出力が権威。
// 通常の design.mjs と browser-e2e の E2E_RULE_CARD_ONLY 入口で同じ H-2 判定を実行する。
await runRuleCardLayoutChecks()
await runCoarseTapTargetChecks()

log('\n=== 測ったコントラスト ===')
for (const { theme, label, ratio, floor } of contrasts) {
  const mark = ratio >= floor ? ' ' : '×'
  log(`  ${mark} [${theme}] ${label}: ${ratio.toFixed(2)}（下限 ${floor}）`)
}

await finish(ng, browser)
