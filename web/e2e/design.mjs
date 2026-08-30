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
  ListCircuitBreakersResponseItem,
  ListProgramsResponseItem,
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
        description: '副調整室の計器盤。地は無彩 3 値、色は信号のみ。',
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

const reservations = [
  { id: 1, site: SITE, programId: 9001, source: 'rule', state: 'active', title: '連続テレビ小説', serviceName: 'NHKEテレ', channelType: 'GR', startAt: iso(nowMs + HOUR), durationMs: 900_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
  { id: 2, site: SITE, programId: 9002, source: 'manual', state: 'active', title: '大相撲中継', serviceName: 'NHK総合', channelType: 'GR', startAt: iso(nowMs + 2 * HOUR), durationMs: 5_400_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
  { id: 3, site: SITE, programId: 9003, source: 'rule', state: 'detached', title: 'クラシック音楽館', serviceName: 'ＮＨＫＢＳ', channelType: 'BS', startAt: iso(nowMs + 5 * HOUR), durationMs: 3_600_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
  { id: 4, site: SITE, programId: 9004, source: 'rule', state: 'orphaned', title: '日曜洋画劇場', serviceName: 'テレビ大阪', channelType: 'GR', startAt: iso(nowMs + 26 * HOUR), durationMs: 7_200_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
]

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
  // 見えるラベルを意図的に持たない（`overagesWithoutVisibleLabel` 参照）。
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
 * 見えるラベルを意図的に持たない帯の本数（issue #460 再レビュー）。正時起点で
 * 9〜18 分の帯は、目盛り回避（avoidTickRow）で押し下げた先が自分の帯の外
 * （= 直後の帯の領域）にはみ出すため、位置の嘘をつくより描かない
 * （`capacity-band.tsx` の `CapacityBandLabel`）。上の `overages` のうち 1 本
 * （10 分の CS 帯）がこれに当たる --- 差し引かずに本数チェックすると、この
 * 意図的な欠落を「消えたバグ」と区別できない。
 */
const overagesWithoutVisibleLabel = 1

const recordings = [
  { id: 11, site: SITE, source: 'rule', serviceName: 'NHK総合', channelType: 'GR', channel: '27', networkId: 32736, serviceId: 1024, eventId: 11, title: 'ニュース７', startAt: iso(nowMs - 600_000), durationMs: 1_800_000, status: 'recording', createdAt: iso(nowMs - 600_000), startedAt: iso(nowMs - 600_000) },
  // encodedAssets を持たせて詳細ページ（/recordings/$id）で <video> が実ブラウザで
  // 出ることを撮る（キーボード到達性の判定 ⑤）。`encodedProfiles`（非推奨の後方
  // 互換フィールド）だけでは `RecordingPlayer` が <video> を出さない
  // （`encodedAssets` を見るため）ので両方持たせる。
  { id: 12, site: SITE, source: 'manual', serviceName: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', networkId: 4, serviceId: 101, eventId: 12, title: 'クラシック音楽館', startAt: iso(nowMs - 26 * HOUR), durationMs: 5_400_000, status: 'finished', sizeBytes: 8_123_456_789, createdAt: iso(nowMs - 26 * HOUR), dropSummary: { packets: 1_500_000, drops: 12, errors: 0, scrambled: 3 }, encodedAssets: [{ profile: 'hevc-1080p', sizeBytes: 2_345_678_901 }] },
  { id: 13, site: SITE, source: 'rule', serviceName: 'テレビ大阪', channelType: 'GR', channel: '18', networkId: 32738, serviceId: 1040, eventId: 13, title: 'アニメ劇場', startAt: iso(nowMs - 50 * HOUR), durationMs: 1_800_000, status: 'failed', createdAt: iso(nowMs - 50 * HOUR) },
  { id: 14, site: SITE, source: 'rule', serviceName: 'NHKEテレ', channelType: 'GR', channel: '26', networkId: 32737, serviceId: 1032, eventId: 14, title: '連続テレビ小説', startAt: iso(nowMs - 74 * HOUR), durationMs: 900_000, status: 'finished', sizeBytes: 1_234_567_890, createdAt: iso(nowMs - 74 * HOUR) },
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
  createdAt: iso(nowMs - 10 * HOUR),
  ingest: {
    state: 'transferring',
    writtenBytes: 600_000_000,
    expectedBytes: 1_000_000_000,
    observedAt: iso(nowMs - 5_000),
  },
}

const rules = [
  { id: 1, name: '朝ドラ', enabled: true, priority: 10, keepOriginal: 'always', textMatches: [{ target: 'name', mode: 'keyword', value: '連続テレビ小説' }], createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
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
    ...programsFor(iso(nowMs), iso(nowMs + 6 * HOUR)).map((p, i) => [`programs[${i}]`, ListProgramsResponseItem, p]),
    ...reservations.map((r, i) => [`reservations[${i}]`, ListReservationsResponseItem, r]),
    // transferringRecording も既定オプション（multiSite + extraRecording）で
    // 実際にブラウザへ配る（:308 参照）ので検証対象に含める。
    ...[...recordings, transferringRecording].map((r) => [`recordings#${r.id}`, ListRecordingsResponseItem, r]),
    ...rules.map((r, i) => [`rules[${i}]`, ListRulesResponseItem, r]),
    ...breakers.map((b, i) => [`breakers[${i}]`, ListCircuitBreakersResponseItem, b]),
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
 */
/**
 * apiHandler は design.mjs の各シナリオに応じた `/api/**` の応答を作る
 * ハンドラを返す（`installApiStubs`（e2e/lib.mjs）の配線に渡す）。
 */
function apiHandler({
  withBreaker = false,
  delayPath = null,
  delayMs = 0,
  emptyHome = false,
  multiSite = false,
  extraRecording = false,
} = {}) {
  return async ({ path: p, url, json, route }) => {
    if (delayPath !== null && p === delayPath) {
      await new Promise((r) => setTimeout(r, delayMs))
    }
    // SSE（/api/events）は明示のスタブを持たず catch-all（200 json []）に落ちる。
    if (p === '/api/sites') return json(multiSite ? [SITE, SITE2] : [SITE])
    // ライブへの導線（主ナビの「ライブ」・/live 画面）はサーバーの live.enabled に
    // 連動する（issue #209）。ここは「有効なデプロイ」の見た目を撮るための判定なので
    // true を返す --- 返さないと主ナビが 5 項目になり、/live はチャンネル一覧ではなく
    // 「無効です」の空状態になる
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json(withBreaker ? breakers : [])
    if (p === '/api/encode-profiles') return json([{ name: 'hevc-1080p', container: 'mp4' }])
    if (p === '/api/rules') return json(rules)
    if (p === '/api/reservations') return json(emptyHome ? [] : reservations)
    if (p === '/api/capacity/overages') return json(emptyHome ? [] : overages)
    if (p === '/api/recordings') {
      // ホーム（M8-3）は `status` / `limit` を実際に付けて 3 本問い合わせる
      // （`いま録画中` = status=recording、完了録画 = status=finished&limit=20 で
      // 「直近の完了」の表示はその先頭 6 件に切られる、失敗録画 =
      // status=failed&limit=20 で警告セクションへの追加項目になる）。
      // フィクスチャの id 13「アニメ劇場」が `status: 'failed'` かつ recency 窓
      // （7 日）の内側なので、ホームの警告には実際にその行が出る。
      // 既定の録画一覧（`pages/recordings.tsx`）は status を付けずに常に
      // limit=50 を送るので、ここでの絞り込みはそちらの見た目に影響しない。
      // 実サーバーの既定（program_start_at 降順）に合わせて並べ替えてから絞る。
      const source = emptyHome
        ? []
        : extraRecording
          ? [...recordings, transferringRecording]
          : recordings
      const status = url.searchParams.get('status')
      const limit = Number(url.searchParams.get('limit') ?? source.length)
      const filtered = status ? source.filter((r) => r.status === status) : source
      const sorted = [...filtered].sort((a, b) => Date.parse(b.startAt) - Date.parse(a.startAt))
      return json(sorted.slice(0, limit))
    }
    // 録画単体（`/recordings/$id`、issue #232）。キーボード到達性の判定（⑤）が
    // 詳細ページの `<video>` を見るために引く。ごみ箱の録画は無いのでここでは
    // 常に 200（一覧のフィクスチャから引く）。
    const recMatch = /^\/api\/recordings\/(\d+)$/.exec(p)
    if (recMatch && route.request().method() === 'GET') {
      const rec = recordings.find((r) => r.id === Number(recMatch[1]))
      return rec ? json(rec) : route.fulfill({ status: 404 })
    }
    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(p)) return json([])
    // サムネイルは 404 に落として実装側のプレースホルダを撮る（画像を作らない）
    if (/^\/api\/recordings\/\d+\/thumbnail$/.test(p)) return route.fulfill({ status: 404 })
    if (p === `/api/sites/${SITE}/services`) return json(services)
    if (p === `/api/sites/${SITE}/programs`) {
      return json(
        programsFor(
          url.searchParams.get('start') ?? iso(nowMs),
          url.searchParams.get('end') ?? iso(nowMs + 6 * HOUR),
          url.searchParams.getAll('serviceId'),
        ),
      )
    }
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
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
const readColor = (el, prop) => {
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

  const value = getComputedStyle(el).getPropertyValue(prop)
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

// --- 実行 -------------------------------------------------------------------

/** 撮る画面。`wait` はその画面で描画完了と見なせる目印。 */
const screens = [
  // ホーム（M8-3, issue #242）は `/` を新設で受け取り、番組表は `/programs` へ
  // 移設した。フィクスチャは「いま録画中」1 件・「今夜〜明日の予約」窓に入る
  // 予約複数件・容量超過 1 件・失敗録画 1 件（id 13「アニメ劇場」、recency 窓の
  // 内側）（いずれも→「警告」）・「直近の完了」複数件を持つので、4 セクション
  // すべてが一度に撮れる。
  { name: 'home', path: '/', wait: 'text=いま録画中' },
  { name: 'programs', path: '/programs', wait: 'li[data-program-id], [data-testid="program-grid-now-line"]' },
  { name: 'reservations', path: '/reservations', wait: 'text=チューナー不足' },
  { name: 'recordings', path: '/recordings', wait: 'text=録画中' },
  { name: 'rules', path: '/rules', wait: 'text=朝ドラ' },
  // 検索は初期状態で結果を持たないので、フォームが立ち上がったことを目印にする
  // （何も待たないと、クエリが解決する前の空のフォームを撮ってしまう）
  { name: 'search', path: '/search', wait: 'text=チャンネル' },
  { name: 'live', path: '/live', wait: 'text=NHK総合' },
]

const viewports = [
  { name: 'desktop', width: 1280, height: 900 },
  { name: 'mobile', width: 390, height: 844 },
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

rmSync(OUT_DIR, { recursive: true, force: true })
mkdirSync(OUT_DIR, { recursive: true })

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

/** open は 1 ページを開いてスタブ・時刻・テーマを整えるところまでやる。 */
async function open(viewport, theme, screen, opts = {}) {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: theme,
    deviceScaleFactor: 2,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, apiHandler(opts))
  await page.goto(URL_BASE + screen.path, { waitUntil: 'domcontentloaded' })
  // ダークは `.dark` クラスで切り替わる（index.css の @custom-variant）。
  // アプリ自身に切り替え手段が無いので、ここで直接付ける（README §デザイン）。
  if (theme === 'dark') await page.evaluate(() => document.documentElement.classList.add('dark'))
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

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// --- ① スクリーンショット ---
log('\n=== ① スクリーンショット ===')
for (const viewport of viewports) {
  for (const theme of themes) {
    for (const screen of screens) {
      const { context, page } = await open(viewport, theme, screen)
      const file = path.join(OUT_DIR, `${screen.name}-${theme}-${viewport.name}.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(page, `${screen.name}/${theme}/${viewport.name}`)
      await context.close()
    }
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
    if (theme === 'dark') await page.evaluate(() => document.documentElement.classList.add('dark'))
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
    // 空状態（EmptyState）の文言が実際に読める位置のショット。デスクトップ
    // 1280×900 では検索フォームが長く、既定のビューポートの下端で切れて
    // 文言まで届かない --- `search-*-desktop.png` は走査線の帯の上端しか
    // 写っていない。この PR の主題（走査線の上の文字が読めるか）を実際に
    // 見て判断できるショットを別途 1 組足す
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
    await page.goto(URL_BASE + '/', { waitUntil: 'domcontentloaded' })
    if (theme === 'dark') await page.evaluate(() => document.documentElement.classList.add('dark'))
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

// --- ①' ホーム（M8-3）: 空セクションが出ない/出るの機械判定 ---
//
// jsdom（`pages/home.test.tsx`）が既に見ている範囲（セクションの出し分けロジック
// そのもの）はここでは繰り返さない。ここで見るのは実ブラウザでしか確認できない
// こと --- 既定フィクスチャで 4 見出しが**実際に画面に出る**こと（レイアウトが
// 崩れて隠れていないか）と、警告セクションのチューナー不足項目が実際にクリック
// できるリンクとして機能すること（href だけでなく実クリックでの遷移）。
log("\n=== ①' ホームの空セクション判定 ===")
{
  const { context, page } = await open(desktop, 'light', screenOf('home'))
  for (const heading of ['いま録画中', '今夜〜明日の予約', '警告', '直近の完了']) {
    const found = await page.getByRole('heading', { name: heading }).count()
    if (found === 0) {
      ng.push(`ホーム: 見出し「${heading}」が既定フィクスチャで出ていない`)
    }
  }
  // チューナー不足の警告項目は番組表（`/programs?at=...`）へのリンクとして機能する
  const shortageLink = page.getByRole('link', { name: /チューナーが不足しています/ })
  if ((await shortageLink.count()) === 0) {
    ng.push('ホーム: 警告セクションにチューナー不足の項目が無い')
  } else {
    await shortageLink.first().click()
    await page.waitForTimeout(400)
    const url = new URL(page.url())
    if (url.pathname !== '/programs') {
      ng.push(`ホーム: チューナー不足をクリックしても番組表へ飛ばない（${url.pathname}）`)
    }
  }
  await context.close()
}
{
  // 両方向: 全セクションが空のときは 4 見出しとも出ず、単一の空状態だけが出る
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
  await page.goto(URL_BASE + '/', { waitUntil: 'domcontentloaded' })
  await page
    .getByText('表示できる項目がありません')
    .waitFor({ timeout: 5000 })
    .catch(() => {
      ng.push('ホーム: 全セクション空でも単一の空状態が出ない')
    })
  for (const heading of ['いま録画中', '今夜〜明日の予約', '警告', '直近の完了']) {
    if ((await page.getByRole('heading', { name: heading }).count()) > 0) {
      ng.push(`ホーム: 全セクション空のはずが見出し「${heading}」が出ている`)
    }
  }
  await context.close()
}

log("\n=== ①'' ホーム: 警告の種別ごとの色（琥珀 vs destructive） ===")
{
  // ブレーカーも同時に出すため withBreaker: true で開く（overage と drop は
  // 既定フィクスチャに既に含まれている）。
  const { context, page } = await open(desktop, 'light', screenOf('home'), { withBreaker: true })

  // 容量超過（チューナー不足）= 琥珀。色はリンク（`<a>`）に付く。
  const overageRow = page.locator('li', { hasText: 'チューナーが不足しています' }).first()
  const overageColor = await computedOf(overageRow.locator('a').first(), 'color')
  if (overageColor === null) {
    ng.push('ホーム: チューナー不足の警告項目の文字色が取得できない')
  } else if (!isAmber(overageColor.rgba)) {
    ng.push(
      `ホーム: チューナー不足の警告項目が琥珀でない（${overageColor.value} = ${overageColor.rgba}）`,
    )
  }

  // サーキットブレーカー = destructive。リンクを持たないので色は <li> 自身に付く。
  const breakerRow = page.locator('li', { hasText: 'ルール評価による予約の削除が停止中' }).first()
  const breakerColor = await computedOf(breakerRow, 'color')
  if (breakerColor === null) {
    ng.push('ホーム: ブレーカーの警告項目の文字色が取得できない')
  } else if (!isRed(breakerColor.rgba)) {
    ng.push(
      `ホーム: ブレーカーの警告項目が destructive でない（${breakerColor.value} = ${breakerColor.rgba}）`,
    )
  }

  // ドロップ = destructive。色はリンクに付く。
  const dropRow = page.locator('li', { hasText: 'クラシック音楽館: ドロップ' }).first()
  const dropColor = await computedOf(dropRow.locator('a').first(), 'color')
  if (dropColor === null) {
    ng.push('ホーム: ドロップの警告項目の文字色が取得できない')
  } else if (!isRed(dropColor.rgba)) {
    ng.push(`ホーム: ドロップの警告項目が destructive でない（${dropColor.value} = ${dropColor.rgba}）`)
  }

  // 失敗録画 = destructive（録画が失われたことは取り返しがつかない）。色はリンク
  // に付く。フィクスチャの id 13「アニメ劇場」が `status: 'failed'`。
  const failedRow = page.locator('li', { hasText: 'アニメ劇場: 録画失敗' }).first()
  const failedColor = await computedOf(failedRow.locator('a').first(), 'color')
  if (failedColor === null) {
    ng.push('ホーム: 失敗録画の警告項目の文字色が取得できない（行が出ていない可能性）')
  } else if (!isRed(failedColor.rgba)) {
    ng.push(
      `ホーム: 失敗録画の警告項目が destructive でない（${failedColor.value} = ${failedColor.rgba}）`,
    )
  }

  log(
    `  チューナー不足=${overageColor?.rgba} / ブレーカー=${breakerColor?.rgba} / ドロップ=${dropColor?.rgba} / 失敗録画=${failedColor?.rgba}`,
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
// docs/frontend/home.md §経緯と失敗事例）。ここだけ時計を止めずに実際の要求回数
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
  await page.goto(URL_BASE + '/', { waitUntil: 'domcontentloaded' })
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

/**
 * 下限を満たさないと分かっていて、いま直さないと決めた組み合わせ。
 *
 * **黙って下限を下げない。** ここに載せたものは合否には数えないが、必ず
 * 「既知の不足」として出力する（CLAUDE.md の「no silent caps」に相当）。
 * 空にできたらこの仕組みごと消す。
 */
const knownGaps = new Map([
  [
    'light/失敗バッジの文字 / destructive の淡い地',
    'destructive は shadcn 既定のまま（この PR の対象外）。' +
      '明度を下げるとタリーレッドと見分けが付かなくなるので、直すなら色相ごと動かす判断が要る' +
      '（実測値は上の表に出る。ここには書かない --- 2 通りの数字を持たないため）',
  ],
  [
    'light/一覧の行の hover 中の副情報の文字 / muted の半透明地',
    'hover 中だけの組み合わせで Lighthouse の監査対象に入らない（常時見える面は下限を満たす）。' +
      '直すには一覧の行の副情報の文字色を 4 画面（録画・予約・ホーム・番組リスト）で' +
      '一斉に上げることになり、常時表示の階層（本文 = foreground / 副情報 = muted）が ' +
      'hover のあいだ崩れる。どちらを取るかは別で決める --- 割っている量は僅かなので、' +
      'ここに載せて見えるようにしたうえで据え置く（実測値は上の表に出る）',
  ],
])

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
  const gap = knownGaps.get(`${theme}/${label}`)
  contrasts.push({ theme, label, ratio, floor, gap })
  if (ratio < floor && gap === undefined) {
    ng.push(`[${theme}] ${label} のコントラストが ${ratio.toFixed(2)}（下限 ${floor}）`)
  }
  return ratio
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
    const failed = page.locator('ul span', { hasText: /^失敗$/ })
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

    // 完了バッジ（issue #308）。`bg-muted` + `text-muted-foreground` だと
    // ライトで 4.5 を割ったため、文字色を `text-foreground` に直した
    // （docs/frontend/design.md 参照）。ここは色の判定（isRed 等）は無く、
    // 「地が塗り（不透明）であること」と「文字とのコントラストが下限を
    // 満たすこと」だけを見る --- `text-muted-foreground` に戻す変異が
    // 入ったらここで落ちる。
    const finished = page.locator('ul span', { hasText: /^完了$/ })
    const finishedBg = await computedOf(finished, 'background-color')
    const finishedFg = await computedOf(finished, 'color')
    if (finishedFg === null || finishedBg === null) {
      ng.push(`[${theme}] 完了バッジが見つからない`)
    } else {
      if (finishedBg.rgba[3] < 200) {
        ng.push(`[${theme}] 完了バッジの地が塗りでない（不透明度 ${finishedBg.rgba[3]}/255。${finishedBg.value}）`)
      }
      checkContrast(theme, '完了バッジの文字 / muted の塗り', finishedFg.rgba, finishedFg, minTextContrast)
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
  // 上のブロックの「完了バッジ」判定は `StatusBadge` の `finished` しか見ておらず、
  // PR 本文はそれを「録画の muted バッジ」全体の判定として書いていたが、実際には
  // site タグ（`showSite` が真になる 2 サイト以上でしか出ない）と `IngestBadge`
  // （`ingest` フィールドを持つ録画がないと出ない）は既定のフィクスチャでは
  // 一度も描画されず、`text-muted-foreground` に戻す変異が入っても緑のまま
  // 通っていた。`multiSite` + `extraRecording` で両方を一度に描画させて測る。
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

  // --- 録画一覧: 行の hover 中の副情報（`hover:bg-muted/50` + `text-muted-foreground`） ---
  //
  // 一覧の行は hover で `bg-muted/50` を敷き、その上に副情報（放送局名・日時・尺）が
  // `text-muted-foreground` のまま乗る。**Lighthouse は hover を測らない**ので
  // 監査には出ないが、`bg-muted` + `text-muted-foreground` と同族の組み合わせで
  // あることは変わらないので、下限を割るかどうかは推測せず実測する（割っている。
  // 直さない判断は `knownGaps` に理由付きで載せてある）。同じ組み方は予約一覧・
  // ホーム・番組リストの行にもあるが、地・文字のトークンと不透明度が同一なので
  // 代表として録画一覧の行で 1 回測る。
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

  // --- 録画詳細: `bg-muted/30` のパネルに乗る muted の文字 ---
  //
  // 詳細（`/recordings/$id`）の本体は `bg-muted/30` の面で、その上の説明文・
  // `<dt>` 群・品質イベントが `text-muted-foreground` のまま乗る（`RecordingDetail`。
  // 一覧はインライン展開を持たないので、この面が出るのは詳細ページだけ）。hover と
  // 違って**常時見えるので Lighthouse の監査対象**に入る。代表として `<dt>`
  // 「チャンネル」を測る（同じパネル・同じトークン対なので説明文・品質イベントも
  // 同値になる）。`screens` には足さない --- 足すと全画面ショットが 1 画面ぶん
  // 増える。判定に必要なのは path と目印だけなので、ここで組んで渡す。
  {
    const detailScreen = { name: 'recording-detail', path: '/recordings/12', wait: 'text=チャンネル' }
    const { context, page } = await open(desktop, theme, detailScreen)
    // `screens`（① のループ）に無い画面なので、明示的に掛けないと欠損文字列
    // 判定から漏れる。
    //
    // **`s.packets.toLocaleString()`（recordings.tsx の DropStatsTable）は
    // ここでは撮れていない** --- それは別エンドポイント
    // （`/api/recordings/{id}/drop-stats`。`ListRecordingDropStatsResponseItem`）
    // が返す per-PID の値で、`dropSummary.packets` とは無関係。design.mjs は
    // このエンドポイントを常に `[]` にスタブしているため（:312）、
    // `DropStatsTable` の行は 1 件も描画されない（未検証の断言をしない。
    // CLAUDE.md「一度も真でなかった記述」）。
    await checkMissingStrings(page, `recording-detail/${theme}`)
    const dt = page.locator('dt', { hasText: /^チャンネル$/ }).first()
    const fg = await computedOf(dt, 'color')
    log(`  [${theme}] 録画詳細のパネルの文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null) {
      ng.push(`[${theme}] 録画詳細の <dt> が見つからない`)
    } else {
      // 面が半透明（`bg-muted/30`）なので、遡って合成できていないと甘い数字が出る。
      // ページの地と一致したら合成が効いていない
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (sameAsGround) {
        ng.push(
          `[${theme}] 録画詳細のパネルの文字が乗る面がページの地と同じ（${fg.backdrop}）` +
            ' --- bg-muted/30 の合成が効いていない',
        )
      }
      checkContrast(
        theme,
        '録画詳細のパネルの文字 / muted の半透明地',
        fg.rgba,
        fg,
        minTextContrast,
      )
    }
    await context.close()
  }

  // --- 予約一覧: チューナー不足 = 琥珀（淡い地の上で読めるか） ---
  {
    const { context, page } = await open(desktop, theme, screenOf('reservations'))
    // **淡い地を持つのは外側のバッジ、文字を持つのは内側の span。**
    // 内側だけを掴むと背景が透明になり、合成が恒等になって「地の上での比」を
    // 測ってしまう。外側から引いて、文字色は子から採る。
    //
    // **外側は `<span>` ではなく `<a>`（`Link`）。** issue #233 M6-5 で
    // バッジ自身が番組表への `Link` になり、淡い地（`bg-warning/10`）を持つ
    // 外側の要素は `<span>` から `<a>` に変わった。この判定はその変更の後も
    // `ul span` のまま据え置かれており、`.filter({ hasText })` が `<a>` の
    // 子である 2 つの `<span>`（sr-only の文・見える側のラベル）しか拾えず
    // `label`（`badge` の子孫を探す）が空になって常に「見つからない」扱いに
    // なっていた（実機で確認: `badge` が実際には見える側のラベル span 自身に
    // 解決し、その子孫に `span[aria-hidden="true"]` は無い）。M8-3 の実装
    // 確認中に発見・修正した（本題（ホーム）とは無関係な既存の不具合）。
    const badge = page.locator('ul a').filter({ hasText: /チューナー不足/ }).first()
    const label = badge.locator('span[aria-hidden="true"]')
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(label, 'color')
    log(`  [${theme}] チューナー不足バッジ 文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null || bg === null) {
      ng.push(`[${theme}] チューナー不足バッジが見つからない`)
    } else if (bg.rgba[3] <= 8) {
      // 外側を掴めていない = 合成が効いていない。素通りさせず落とす
      ng.push(`[${theme}] チューナー不足バッジの地が透明（淡い地を持つ要素を掴めていない）`)
    } else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] チューナー不足バッジが琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      // **`backdrop` の遡りが本当に効いているかをここで検査する。** この文字は
      // 「外側バッジの淡い地」の上に乗っており、遡りが 1 段で止まったり
      // 外側を飛ばしたりすると `backdrop` はページの地と一致してしまう。
      // そのとき比は甘い方へ 0.5〜0.7 動くので、一致 = 判定が壊れている
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (sameAsGround) {
        ng.push(
          `[${theme}] 文字が乗る面がページの地と同じ（${fg.backdrop}）` +
            ' --- 淡い地の合成が効いていない',
        )
      }
      checkContrast(theme, 'チューナー不足の文字 / 琥珀の淡い地', fg.rgba, fg, minTextContrast)
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
      // `overagesWithoutVisibleLabel` 本は意図的にラベルを持たないので差し引く
      // （差し引かないと、この意図的な欠落を「消えたバグ」と区別できない）。
      if (labelBoxes.length < overages.length - overagesWithoutVisibleLabel) {
        ng.push(
          `[${theme}] 帯が ${overages.length} 本あるのに見えるラベルが ${labelBoxes.length} 件しかない` +
            `（意図的に描かない ${overagesWithoutVisibleLabel} 件を差し引いても足りない）`,
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
    if (theme === 'dark') await page.evaluate(() => document.documentElement.classList.add('dark'))
    const skeleton = page.locator('.scanlines').first()
    await skeleton.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] 読み込み中の走査線（.scanlines）が出ない`)
    })
    const gap = await computedOf(skeleton, 'background-color')
    const lit = await computedVar(skeleton, '--scan-lit')
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
const mobile = viewports[1]
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
    }
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
    await trigger.waitFor({ timeout: 10000 }).catch(() => {})
    if ((await trigger.count()) === 0) {
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
log('\n=== 測ったコントラスト ===')
for (const { theme, label, ratio, floor, gap } of contrasts) {
  const mark = ratio >= floor ? ' ' : gap !== undefined ? '△' : '×'
  log(`  ${mark} [${theme}] ${label}: ${ratio.toFixed(2)}（下限 ${floor}）`)
}
const gaps = contrasts.filter((c) => c.ratio < c.floor && c.gap !== undefined)
if (gaps.length > 0) {
  log('\n  既知の不足（合否には数えていない）:')
  for (const { theme, label, gap } of gaps) log(`    △ [${theme}] ${label} --- ${gap}`)
}
// 下限を満たすようになった gap は畳めるので、そのことも言う。
// 言わないと knownGaps が「一度入れたら誰も見ない置き場」になる
const stale = contrasts.filter((c) => c.ratio >= c.floor && c.gap !== undefined)
for (const { theme, label, ratio } of stale) {
  log(`\n  knownGaps に載っているが下限を満たしている（${ratio.toFixed(2)}）: [${theme}] ${label}`)
  log('    → knownGaps から消せる')
}

await finish(ng, browser)
