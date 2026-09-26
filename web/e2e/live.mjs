// ライブ視聴（M4-4、issue #92）の受け入れ判定。jsdom では測れないものだけを
// ここで見る（e2e/README.md）。**mirakc もチューナーも不要** --- HLS プレイリスト/
// セグメントは Playwright の page.route でブラウザ側から丸ごと差し替えるため、
// サーバー（rokuban 本体）は「サービス一覧を返す」以外の実仕事をしない。
//
// 合格なら exit 0、1 つでも NG なら exit 1。ffmpeg / Chrome が無い環境では、
// その判定だけを「測れない」として報告し（NG にはしない）、残りは続行する。
//
// 前提:
//   - rokuban サーバーが起動していて、E2E_URL で指定した site に
//     E2E_LIVE_SERVICE_A / E2E_LIVE_SERVICE_B の 2 つの serviceId が
//     epg_services に存在する（DB へ直接 INSERT すれば足りる。mirakc からの
//     実 EPG 同期は不要）
//   - E2E_LIVE_NETWORK_ID がその 2 行の `network_id` と一致している（既定 1）。
//     `?service=<Service.id>` に載せる合成 id を実際の
//     `GET /api/sites/{site}/services` から引く（下記 `resolveServiceId`）ため、
//     食い違うとサービス一覧に見つからずここで落ちる
//   - ffmpeg が PATH にある（固定 HLS フィクスチャの生成に使う。生成は
//     初回だけで、以降は `os.tmpdir()` にキャッシュされる）
//
// 詳しい手順・準備の SQL 例は docs/runbook/live.md §②。使い方だけ：
//   E2E_LIVE_NETWORK_ID=1 E2E_LIVE_SERVICE_A=9001 E2E_LIVE_SERVICE_B=9002 pnpm e2e:live
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const BASE_URL = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = process.env.E2E_LIVE_SITE ?? 'default'
// SERVICE_A / SERVICE_B / NETWORK_ID は SI の値（docs/runbook/live.md §② の
// SQL 投入例と同じ id 空間）のまま持つ --- DB へ投入する行そのものの id なので、
// 準備手順を変えずに済む。
const SERVICE_A = process.env.E2E_LIVE_SERVICE_A ?? '9001'
const SERVICE_B = process.env.E2E_LIVE_SERVICE_B ?? '9002'
const NETWORK_ID = process.env.E2E_LIVE_NETWORK_ID ?? '1'

const ng = []
const skipped = []

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
// resolveServiceId 等より先に見る --- 前提が崩れているとそちらが先に例外で
// 落ち、⓪ に一度も到達しないまま無関係なエラーだけが出てしまう。
await verifyBundleMatchesOrExit(BASE_URL, ng)

/**
 * resolveServiceId は SI の (networkId, serviceId) から `?service=` に載せる
 * `Service.id` を求める。
 *
 * issue #438: `/live` の URL は他画面と同じ `?service=<Service.id>` に統一した。
 * 合成規則（`networkId * 100000 + serviceId`）をここで複製せず、実際に
 * `GET /api/sites/{site}/services` を引いて対応する行の `id` を使う ---
 * mirakc の合成規則をフロント/e2e 側に複製しない（issue #208/#217 と同じ判断）。
 * 一致する行が無ければ、DB への投入内容と env の食い違いを名指しして落とす
 * （`?service=` を組み立てられないまま goto しても、既定の「番組を持つ先頭」に
 * フォールバックしてしまい、以降の判定が別のチャンネルを見てしまう）。
 */
async function resolveServiceId(networkId, serviceId) {
  const url = `${BASE_URL}/api/sites/${SITE}/services`
  // ステータスを先に見る --- 見ないと site 名を間違えたときの 404
  // （`ErrorResponse` オブジェクト）が `services.find is not a function` に化けて
  // 原因を名指ししない。接続不能（`fetch failed`）も URL 込みで言い直す。
  const res = await fetch(url).catch((cause) => {
    throw new Error(`${url} に接続できない（サーバーを起動しているか確認する）`, { cause })
  })
  if (!res.ok) {
    throw new Error(`${url} が ${res.status} を返した（E2E_LIVE_SITE=${SITE} を確認する）`)
  }
  const services = await res.json()
  const match = services.find(
    (s) => String(s.networkId) === String(networkId) && String(s.serviceId) === String(serviceId),
  )
  if (match === undefined) {
    throw new Error(
      `E2E_LIVE_NETWORK_ID=${networkId} / serviceId=${serviceId} が ` +
        `GET /api/sites/${SITE}/services に見つからない` +
        '（docs/runbook/live.md §② の投入例と E2E_LIVE_NETWORK_ID を確認する）',
    )
  }
  return match.id
}

const SERVICE_ID_A = await resolveServiceId(NETWORK_ID, SERVICE_A)
const SERVICE_ID_B = await resolveServiceId(NETWORK_ID, SERVICE_B)

const FIXTURE_DIR = path.join(os.tmpdir(), 'rokuban-e2e-live-fixture')
const SEGMENTS_DIR = path.join(FIXTURE_DIR, 'segments')
const PLAYLIST_PATH = path.join(FIXTURE_DIR, 'playlist.m3u8')

/**
 * liveSegmentsPathOf は serviceId のセグメント要求を照合するためのパス断片を返す。
 *
 * ライブの URL は `/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/...`
 * で、**`{serviceId}` は SI の値そのもの**（一覧 API と同じ id 空間。issue #217）。
 * 以前は mirakc 合成 id（`networkId * 100000 + serviceId`）が載っていたため、
 * ここで一覧 API から networkId を引いて合成し直す必要があった --- その必要が
 * 無くなったので、環境変数の SERVICE_A / _B をそのまま照合に使える。
 */
const MASTER_PATH = path.join(FIXTURE_DIR, 'master.m3u8')

/**
 * ensureCaptionFixture は字幕つき master playlist のフィクスチャを書く（⑩）。
 *
 * **streamer が captions 有効時に配る形を写す。** master は
 * `EXT-X-MEDIA:TYPE=SUBTITLES` のレンディションを持ち、variant playlist と
 * 字幕 playlist はどちらも `.../live/segments/{name}.m3u8` の下に来る
 * （`internal/streamer` の Segment が captions 有効時だけ `.m3u8` / `.vtt` を
 * 受け付けるのはこのため）。
 *
 * **セグメント URI を `segments/` 無しの裸名にするのが要点。** variant / 字幕
 * playlist は `.../live/segments/` の下に置かれるので、ffmpeg が書く
 * `segments/segment_000.ts` をそのまま入れると
 * `.../live/segments/segments/segment_000.ts` に解決されて 404 になる。
 *
 * 毎回上書きする（ffmpeg のフィクスチャと違い生成コストが無い）。
 */
function ensureCaptionFixture() {
  const mediaPlaylist = readFileSync(PLAYLIST_PATH, 'utf8').replaceAll('segments/', '')
  writeFileSync(path.join(SEGMENTS_DIR, 'hd.m3u8'), mediaPlaylist)

  writeFileSync(
    path.join(SEGMENTS_DIR, 'sub.m3u8'),
    [
      '#EXTM3U',
      '#EXT-X-VERSION:3',
      '#EXT-X-TARGETDURATION:40',
      '#EXT-X-MEDIA-SEQUENCE:0',
      '#EXTINF:40.0,',
      'sub_00001.vtt',
      '#EXT-X-ENDLIST',
      '',
    ].join('\n'),
  )
  writeFileSync(
    path.join(SEGMENTS_DIR, 'sub_00001.vtt'),
    ['WEBVTT', '', '00:00:00.000 --> 00:00:40.000', '字幕のフィクスチャ', ''].join('\n'),
  )

  writeFileSync(
    MASTER_PATH,
    [
      '#EXTM3U',
      '#EXT-X-VERSION:3',
      '#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="日本語",LANGUAGE="ja",' +
        'DEFAULT=YES,AUTOSELECT=YES,URI="segments/sub.m3u8"',
      '#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=640x360,SUBTITLES="subs"',
      'segments/hd.m3u8',
      '',
    ].join('\n'),
  )
}

/**
 * livePlaylistWith は「ライブ形」のプレイリストを組み立てる（⑪ 専用）。
 *
 * **⑪ は停滞を作るので、フィクスチャ（VOD 形 = `#EXT-X-ENDLIST` 付き）では
 * 判定にならない。** hls.js は VOD では 30 秒ぶん先読みするので、セグメントの応答を
 * 止めても**バッファを食い切るまで再生が進み続ける**（実測: 25 秒待っても
 * `currentTime` が 20 秒まで進み、停滞と見なされなかった）。
 *
 * ライブ形（`#EXT-X-ENDLIST` を付けず、載せるセグメントを絞る）にすると
 * **先読みできるのは窓のぶんだけ**になり、載せた本数を増やさなければ再生は
 * 数秒で止まる（= 「配信が止まった」）。復旧は本数を増やすことで表せる。
 *
 * **セグメントの長さと URI はフィクスチャのプレイリストから写す。**
 * `#EXTINF` を 2 秒と書いても実体は 10 秒なので、値が食い違うと
 * 「3 本 = 6 秒」のつもりが 30 秒になり、停滞が起きない（実際に踏んだ）。
 * URI に `segments/` を付けるのも必須である --- プレイリスト自身の URL は
 * `.../live/playlist.m3u8` なので、裸名だと `.../live/segment_000.ts` に解決され、
 * 配信側のルート（`.../live/segments/{name}`）と食い違って 404 になる
 * （streamer が `-hls_base_url segments/` を書いている理由そのもの。
 * `internal/streamer/live.go`）。裸名で組んだ版は**1 本もロードされず**
 * `readyState=0` のまま何も再生されなかった。
 *
 * 入力は `[duration, uri]` の配列（フィクスチャの VOD プレイリストから読む）。
 */
function livePlaylistWith(segments, count) {
  const targetDuration = Math.max(...segments.map(([duration]) => duration))
  const lines = [
    '#EXTM3U',
    '#EXT-X-VERSION:6',
    `#EXT-X-TARGETDURATION:${targetDuration}`,
    '#EXT-X-MEDIA-SEQUENCE:0',
    '#EXT-X-INDEPENDENT-SEGMENTS',
  ]
  for (const [duration, uri] of segments.slice(0, count)) {
    lines.push(`#EXTINF:${duration.toFixed(6)},`, uri)
  }
  return lines.join('\n') + '\n'
}

/**
 * profileMasterFor は captions 無効時に streamer が返すプロファイルごとの master を
 * 組み立てる（⑪ 専用）。ffmpeg 9.0 の `-var_stream_map "v:0,agroup:aud a:0,..."`
 * が書く master は video variant の `#EXT-X-STREAM-INF` を 1 行だけ持つ。降格の
 * 判定（`bundlesProfiles`）を分けるのはこの性質なので、そこだけ写す。音声の
 * `#EXT-X-MEDIA` は載せない --- フィクスチャのセグメントは音声を多重化済みで、
 * 別レンディションを指すと hls.js の代替音声の処理が停滞の観測に混ざる。
 */
function profileMasterFor(profile) {
  return [
    '#EXTM3U',
    '#EXT-X-VERSION:6',
    '#EXT-X-STREAM-INF:BANDWIDTH=2000000',
    `${profile}.0.m3u8`,
    '',
  ].join('\n')
}

/**
 * readFixtureSegments はフィクスチャの VOD プレイリストから `[duration, uri]` を
 * 読む（長さと URI を写すため。上記参照）。
 */
function readFixtureSegments() {
  const lines = readFileSync(PLAYLIST_PATH, 'utf8').split('\n')
  const segments = []
  for (let i = 0; i < lines.length; i++) {
    const match = /^#EXTINF:([\d.]+),/.exec(lines[i].trim())
    if (match) segments.push([Number(match[1]), lines[i + 1].trim()])
  }
  return segments
}

/**
 * fixtureSegments はフィクスチャのセグメント（長さと URI）を返す。
 *
 * **モジュールの読み込み時に読んではならない。** フィクスチャは `ensureFixture` が
 * ffmpeg で作る（初回は存在しない）ので、読み込み時に読むと **`ENOENT` で
 * スクリプトごと落ちる** --- ffmpeg が無い環境で「未測定としてスキップ」も
 * 自動生成もできなくなる。使うのは ⑪ の中（`hasFixture` の内側）だけなので、
 * そこまで遅らせて 1 回だけ読む。
 */
let cachedFixtureSegments = null
function fixtureSegments() {
  if (cachedFixtureSegments === null) cachedFixtureSegments = readFixtureSegments()
  return cachedFixtureSegments
}

const liveSegmentsPathOf = (serviceId) => `/services/${serviceId}/live/segments/`

/**
 * ensureFixture は固定 HLS フィクスチャ（testsrc + sine を H.264/AAC でエンコードした
 * 12 秒ぶんのセグメント + プレイリスト）を用意する。実 ISDB-T / mirakc は要らない ---
 * ブラウザ側の再生経路（hls.js の attachMedia 以降）だけを検査したいので、
 * 内容が本物の放送である必要はない。
 *
 * `os.tmpdir()` にキャッシュし、既にあれば再生成しない（毎回 12 秒ぶんの
 * エンコードを待たせないため）。`E2E_LIVE_REBUILD_FIXTURE=1` で強制再生成する。
 */
function ensureFixture() {
  if (process.env.E2E_LIVE_REBUILD_FIXTURE === '1') {
    rmSync(FIXTURE_DIR, { recursive: true, force: true })
  }
  if (existsSync(PLAYLIST_PATH)) {
    log(`フィクスチャは既にある（${FIXTURE_DIR}）。再生成するには E2E_LIVE_REBUILD_FIXTURE=1`)
    return true
  }

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return false
  }

  mkdirSync(SEGMENTS_DIR, { recursive: true })
  log(`フィクスチャを生成中... (${FIXTURE_DIR})`)
  execFileSync(
    'ffmpeg',
    [
      '-y',
      '-f',
      'lavfi',
      '-i',
      'testsrc=size=640x360:rate=25',
      '-f',
      'lavfi',
      '-i',
      'sine=frequency=440',
      '-t',
      '40',
      '-c:v',
      'libx264',
      '-profile:v',
      'baseline',
      '-level',
      '3.0',
      '-pix_fmt',
      'yuv420p',
      '-preset',
      'veryfast',
      '-c:a',
      'aac',
      '-b:a',
      '64k',
      '-f',
      'hls',
      '-hls_time',
      '2',
      '-hls_list_size',
      '0',
      '-hls_flags',
      'independent_segments',
      '-hls_base_url',
      'segments/',
      '-hls_segment_filename',
      'segments/segment_%03d.ts',
      'playlist.m3u8',
    ],
    { cwd: FIXTURE_DIR, stdio: 'ignore' },
  )
  return true
}

/** segmentDelayMs はセグメント応答に足す遅延。 */
/**
 * E2E_LIVE_PROFILES は ⑨ が差し替える画質の一覧（issue #869）。
 *
 * **2 件にするのは意図的である。** `pages/live.tsx` は 1 件以下ならセレクタを
 * 出さないので、1 件だとそもそも切替操作ができない。順序はサーバー側の
 * `live.profiles` と同じ「設定順 = 先頭が既定」の意味を持つ。
 */
const E2E_LIVE_PROFILES = [
  { name: 'hd', height: 720 },
  { name: 'sd', height: 480 },
]

const segmentDelayMs = 400

/**
 * mseAttached はブラウザ側で評価する述語（MSE がアタッチされたか）。
 *
 * **`video.src` だけを見てはいけない。** hls.js は `sourceopen` の後に
 * object URL を `revokeObjectURL` するため `src` の `blob:` は短命で、
 * WebKit では 4 秒後には `src` が空・`currentSrc` にだけ `blob:` が残っていた
 * （レビュー #190 の指摘）。実際に読み込まれた資源を指す `currentSrc` を主に見る。
 */
const mseAttached = () => {
  const v = document.querySelector('video')
  if (!v) return false
  return v.currentSrc.startsWith('blob:') || v.src.startsWith('blob:')
}

/**
 * nativeSrcAssigned はブラウザ側で評価する述語（`<video>` に URL が直接
 * 渡されたか = ネイティブ HLS 経路に入ったか）。
 */
const nativeSrcAssigned = () => {
  const v = document.querySelector('video')
  if (!v) return false
  return v.currentSrc.includes('.m3u8') || v.src.includes('.m3u8')
}

/** playerDecided は再生経路が決まった（どちらかの分岐に入った）ことを表す。 */
const playerDecided = () => {
  const v = document.querySelector('video')
  if (!v) return false
  return v.currentSrc !== '' || v.src !== ''
}

/**
 * liveDiagnosticsBecameNumeric はブラウザ側で評価する述語（issue #476）。
 *
 * 遅延・バッファの計器（`[data-testid="live-diagnostics"]`）は、hls.js の
 * ライブ同期点が決まるまで「放送から— / 先読み—」のままなので、実際に
 * 「放送から約 n 秒」「先読み n 秒」の両方が数値になったことを見る。
 *
 * **`0` は弾く（`[1-9]\d*`）。** `hls.latency` は同期点が決まる前も `NaN` では
 * なく `0` を返す（`node_modules/hls.js` 1.6.17 の
 * `LatencyController.get latency()` が `this._latency || 0`）。`\d+` だと
 * 「放送から約0秒」にもマッチしてしまい、同期点が一生決まらない回帰を
 * 見逃す（レビュー指摘）。
 */
const liveDiagnosticsBecameNumeric = () => {
  const el = document.querySelector('[data-testid="live-diagnostics"]')
  const text = el?.textContent ?? ''
  return /放送から約[1-9]\d*秒/.test(text) && /先読み\d+秒/.test(text)
}

/**
 * mockLiveRoutes は `.../live/playlist.m3u8` と `.../live/segments/*` を
 * フィクスチャ（またはテストが指定する応答）で丸ごと差し替える。
 *
 * `mode.playlist` を書き換えれば、以降の `playlist.m3u8` 要求の応答を
 * 動的に変えられる（⑤ の 503 → 復帰の検証で使う）。
 *
 * **セグメント応答に `segmentDelayMs` の遅延を入れる。** フィクスチャはローカル
 * ファイルなので `route.fulfill` は実質即時応答になり、hls.js の先読み
 * バッファ（既定 30 秒分）が数百 ms で満たされてしまう --- 満たされて
 * fetch が止まると、④ の「切替後に旧チャンネルへの要求が無い」が
 * cleanup の有無にかかわらず真になり、判定として成立しない（destroy を
 * 呼ばなくても「たまたまもう何も残っていない」だけになる）。遅延を入れて
 * 「切替時点でまだ取り切っていない」状態を作ることで、cleanup が実際に
 * 止めているのか・単にネタが尽きていたのかを区別できるようにする。
 */
async function mockLiveRoutes(page, mode) {
  // ライブ画面はサーバーの live.enabled に連動する（issue #209）。この判定は
  // 「ライブが有効なデプロイ」を前提にしているが、判定を回すサーバーの config は
  // 既定（live.enabled: false）のことが多い --- 差し替えないと画面が
  // 「この環境ではライブ視聴が無効です」になり、①〜⑦ が全滅する
  await page.route('**/api/capabilities', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ live: true }),
    })
  })

  // 画質の一覧（issue #869）。実サーバーは config の `live.profiles` を返すが、
  // この e2e は「2 件以上あるデプロイ」を前提にする判定（⑨）を含むので固定する。
  await page.route('**/api/live-profiles', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(E2E_LIVE_PROFILES),
    })
  })

  // ⑪ のライブ形 variant（`<profile>.0.m3u8`）。**毎回読むので、`mode.liveSegmentCount`
  // を増やすと次の再取得から新しいセグメントが載る**（= 配信の復旧を表せる）。
  // hls.js が取り直し続けるのは master ではなくこちらである
  await page.route(/\/live\/[A-Za-z0-9_-]+\.0\.m3u8$/, async (route) => {
    if (mode.liveSegmentCount === undefined) {
      await route.fulfill({ status: 404, contentType: 'text/plain', body: 'not found' })
      return
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/vnd.apple.mpegurl',
      body: livePlaylistWith(fixtureSegments(), mode.liveSegmentCount),
    })
  })

  await page.route('**/live/playlist.m3u8*', async (route) => {
    // ⑪ は**実サーバーと同じ形**（プロファイルごとの master。video variant 1 本）
    // を返す。captions 無効時も streamer は音声レンディションの
    // ために master を返すので、media playlist を返すと「master なら降格しない」
    // 判定の回帰を通してしまう（実際に通していた）
    if (mode.liveSegmentCount !== undefined) {
      const profile =
        new URL(route.request().url()).searchParams.get('profile') ?? E2E_LIVE_PROFILES[0].name
      await route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: profileMasterFor(profile),
      })
      return
    }
    if (mode.playlist === 'error') {
      await route.fulfill({
        status: 503,
        contentType: 'text/plain',
        body: mode.playlistErrorBody ?? 'live stream unavailable',
      })
      return
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/vnd.apple.mpegurl',
      // captions モードは字幕レンディションを持つ master を返す（⑩）
      body: readFileSync(mode.playlist === 'captions' ? MASTER_PATH : PLAYLIST_PATH),
    })
  })

  await page.route('**/live/segments/*', async (route) => {
    // mode.segments でメディア層だけを壊せる（⑦。プレイリストは 200 のまま
    // なので probe は通り、失敗はメディア層にしか現れない）
    if (mode.segments === '404') {
      await route.fulfill({ status: 404, contentType: 'text/plain', body: 'not found' })
      return
    }
    if (mode.segments === 'hang') {
      // 応答しない。WebKit は 3 秒データが来ないと `stalled` を出す（HTML 仕様）
      return
    }
    const u = new URL(route.request().url())
    const name = u.pathname.split('/').pop()
    const file = path.join(SEGMENTS_DIR, name)
    if (!existsSync(file)) {
      await route.fulfill({ status: 404, contentType: 'text/plain', body: 'not found' })
      return
    }
    await new Promise((r) => setTimeout(r, segmentDelayMs))
    // **実際に配った本数を数える**（⑪ が「配信が戻った」ことを配信側でも
    // 確かめるため。応答を止める `hang` では数えない）
    mode.servedSegments = (mode.servedSegments ?? 0) + 1
    // この `video/mp2t` は streamer の実装値の写し（`internal/streamer/live.go`）。
    // フロントの再生経路判定（`lib/live.ts` の `supportsNativeHls`）がこの値に
    // 依存しているが、**ここでモックしている以上、この e2e は Go 側が別の
    // Content-Type に変わったことを検出できない**（Go 側にも同じ注意書きがある）
    await route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
  })
}

/**
 * clickPlay は選択画面（issue #234 M7-1）の「再生」ボタンを押す。
 *
 * `pages/live.tsx` はチャンネルを選んだだけでは `LivePlayer` をマウントしない
 * （probe もセッションも起こさない。⓪ が直接見る）。①〜⑦は「再生」を押した
 * 後の挙動を見るものなので、`page.goto` の直後にこれを呼んで初めて
 * `LivePlayer` が現れる。
 */
async function clickPlay(page) {
  await page.getByRole('button', { name: /再生/ }).click()
}

/**
 * runConsentCheck は⓪（選択と視聴開始の分離。issue #234 M7-1）を検証する。
 * 併せて `?service=<Service.id>`（issue #438）で直開きし、**要求した id が
 * 実際に選ばれたこと**を選択中の印（`aria-current="page"`）で確認する ---
 * ⓪ の残りは要求件数しか見ないので、この assert が無いと `SERVICE_ID_A` の
 * 解決先とフィクスチャの実際の選択が食い違ったまま緑になりうる。
 * `E2E_LIVE_NETWORK_ID` がフィクスチャの `network_id` と食い違う場合は、
 * この assert より前に `resolveServiceId`（宣言部）がサービス一覧に見つからず
 * 落ちる。
 *
 * この判定が本来見たいのは「チャンネルを選ぶだけではプレイリスト/セグメント要求が飛ばない」
 * こと自体であり、実データ（H.264/AAC）や実再生は要らない --- ①〜⑦と違って
 * ffmpeg フィクスチャに依存せず、bundled Chromium だけで常に測れる。
 *
 * `web/e2e/README.md`「判定を足すときの規律」に沿って、要求件数の assert
 * （0 件であること）は、この判定を足す前の実装（チャンネルをタップした瞬間に
 * probe する版）で落ちることを確認済み。選択中の印の assert は、実サーバー +
 * 実 chromium に対して `pickInitialService` を `s.serviceId === requestedId` に
 * 変異させると落ちる（印が 2 件になり href も要求先と違う。exit 1）。変異を
 * 戻すと ⓪〜⑧ すべて緑（exit 0）。
 *
 * **当たり判定の広さ（`previewBox.width < 600`）の assert は未実行。**
 * issue #725 でこの assert を足した PR では e2e スクリプト自体を一度も
 * 実行していない（サーバーもテストデータも無い）ため、実サーバー + 実
 * ブラウザに対して実際に落ちる/通ることは未確認。ただし「角を押す判定が
 * 旧実装（面全体が div で、中央の小さい <button> だけが aria-label を持つ）
 * でも通ってしまうこと」と「幅で見れば旧実装 51px / 新実装 768px と大きく
 * 割れること」は Chromium（viewport 960x640）で実測済み --- 判定手段として
 * この閾値を選んだ根拠はその実測にある。
 */
async function runConsentCheck() {
  const browser = await launchBrowser()
  try {
    const page = await browser.newPage({ viewport: { width: 960, height: 640 } })
    const requestLog = []
    page.on('request', (req) => requestLog.push(req.url()))

    await page.route('**/api/capabilities', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ live: true }),
      }),
    )
    // 実データは要らない --- 要求そのものの有無だけを見る（decode まではしない）
    await page.route('**/live/playlist.m3u8*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsegments/segment_000.ts\n#EXT-X-ENDLIST\n',
      }),
    )
    await page.route('**/live/segments/*', (route) =>
      route.fulfill({ status: 200, contentType: 'video/mp2t', body: Buffer.from([0x47]) }),
    )

    // 同じ serviceId を持つ別 network が居る構成でも `Service.id`（合成 id）
    // なら正しい方が選ばれること（issue #291 の根。#438 で `?service=` に統一）を
    // 実ブラウザで踏む。
    //
    // **開くのは B。** 既定のフォールバック先（`pickInitialService` の「番組を
    // 持つ先頭」）と要求先が同じチャンネルだと、下の「選択中の印」の assert は
    // 「要求した id が効いた」と「一致に失敗して既定に落ちた」を区別できない ---
    // runbook の投入例（A: remoteControlKeyId 1 / B: 2）では `orderServices` の
    // 先頭が必ず A になるので、A で判定していたときは `pickInitialService` を
    // `s.serviceId === requestedId` に変異させても ⓪ が緑のまま通った（実測）。
    await page.goto(`${BASE_URL}/live?service=${SERVICE_ID_B}`, {
      waitUntil: 'networkidle',
    })

    const requestsAfterOpen = requestLog.filter(
      (u) => u.includes('/live/playlist.m3u8') || u.includes('/live/segments/'),
    )
    log(`  直開き直後のプレイリスト/セグメント要求数: ${requestsAfterOpen.length}`)
    if (requestsAfterOpen.length > 0) {
      ng.push(
        `⓪ 直開きだけでプレイリスト/セグメント要求が ${requestsAfterOpen.length} 件飛んだ` +
          '（選択は再生ボタンで開始する契約に反する）',
      )
    }

    // **要求した `Service.id` が実際に選ばれたことを assert する。**
    // 一覧が描画されるのを待ってから数える --- 描画前に数えると「まだ描かれて
    // いない」を「印が付いていない」と取り違える（非同期の空虚な成功の逆）
    try {
      await page.waitForSelector('nav[aria-label="チャンネル一覧"] a', { timeout: 10000 })
    } catch {
      ng.push(
        '⓪ チャンネル一覧が描画されない（E2E_LIVE_SERVICE_A / _B が epg_services に' +
          '無い可能性。docs/runbook/live.md §② の投入例）',
      )
      return
    }
    const currentLinks = page.locator('nav[aria-label="チャンネル一覧"] a[aria-current="page"]')
    const currentCount = await currentLinks.count()
    const currentHref =
      currentCount > 0 ? ((await currentLinks.first().getAttribute('href')) ?? '') : ''
    log(`  選択中の印（aria-current="page"）: ${currentCount} 件 href=${currentHref || '（無し）'}`)
    if (currentCount !== 1) {
      // 2 件付くのは `serviceId` 単独で同定していたときの壊れ方そのもの
      // （同じ serviceId を持つ別 network の行にも印が付く。issue #291）
      ng.push(`⓪ 選択中の印が ${currentCount} 件ある（1 件でなければ複合キーでの同定が効いていない）`)
    }
    if (!currentHref.includes(`service=${SERVICE_ID_B}`)) {
      ng.push(
        `⓪ 選択されたチャンネルが要求（service=${SERVICE_ID_B}）と違う` +
          `（選択中リンクの href: ${currentHref || '（無し）'}）。` +
          '`resolveServiceId` は id を引けているので、env とフィクスチャの' +
          '食い違いではなく `pickInitialService`（web/src/lib/live.ts）の一致条件を疑う',
      )
    }

    const preview = page.getByRole('button', { name: /再生/ })
    if ((await preview.count()) === 0) {
      ng.push('⓪ 選択プレビューが見つからない')
      return
    }

    requestLog.length = 0
    // **当たり判定の広さを見る。** `getByRole('button', { name: /再生/ })` は
    // 「role と名前を持つ要素」に解決するので、旧実装（面全体が div で、中央の
    // 小さい <button> だけが aria-label を持つ）でもここには小ボタンがヒットし、
    // 角クリックが失敗しても気付けない --- 実測: 旧実装 51x36 / 新実装
    // 768x432（Chromium、viewport 960x640）。閾値 600 はこの 2 値を十分に割る。
    const previewBox = await preview.boundingBox()
    if (previewBox === null) {
      ng.push('⓪ 選択プレビューの位置を取得できない')
      return
    }
    log(`  選択プレビューの当たり判定: ${previewBox.width}x${previewBox.height}`)
    if (previewBox.width < 600) {
      ng.push(
        `⓪ 選択プレビューの当たり判定が幅 ${previewBox.width}px しかない` +
          '（面全体ではなく中央の小さいボタンだけが aria-label を持っている疑い）',
      )
    }
    // **`page.mouse.click` ではなく `locator.click({ position })` を使う。**
    // `mouse.click` はスクロールも actionability 待ちもしないので、viewport
    // 960x640 でプレビューがフォールドの下に出ると、例外も出ないまま何にも
    // 当たらず偽陰性になる（実測）。`locator.click` はスクロールしてから押す。
    await preview.click({ position: { x: 4, y: 4 } })
    let fired = false
    try {
      await page.waitForFunction(
        () =>
          window.performance
            .getEntriesByType('resource')
            .some((r) => r.name.includes('/live/playlist.m3u8')),
        undefined,
        { timeout: 10000 },
      )
      fired = true
    } catch {
      fired = false
    }
    log(`  選択プレビューの角を押した後にプレイリスト要求が飛んだ: ${fired ? 'YES' : 'NO'}`)
    if (!fired) ng.push('⓪ 選択プレビューの角を押してもプレイリスト要求が飛ばない')

    // --- ⓪' 再生中に別チャンネルへ切り替えても、押していない方の playlist/
    // segment 要求が飛ばない ---
    //
    // レビューで発見された穴（PR #259）: 再生状態のリセットを `useEffect` で
    // 行うと、`selectedServiceId` が A→B に変わった直後の 1 コミットだけ古い
    // 再生中フラグが残っていて `LivePlayer` が B の serviceId で透過的に
    // マウントされ、その 1 回の probe が実際に飛ぶ（`AbortController.abort()`
    // では取り消せない --- `internal/streamer/live.go` のセッションは
    // `context.WithCancel(context.Background())` で回る）。「A を再生 → B の
    // チャンネルリンクをクリック（再生は押さない）→ B 向け要求が 0 件」を
    // 確認することで、この透過マウント自体が起きないことを実ブラウザで固定する
    if (fired) {
      requestLog.length = 0
      await page
        .locator(`nav[aria-label="チャンネル一覧"] a[href*="service=${SERVICE_ID_A}"]`)
        .click()
      // 「再生」ボタンが A 向けに再表示される（= 選択状態に戻った）のを待つことで、
      // 一連の再レンダー（切替の 1 回目のコミット・再生状態のリセット）が
      // 落ち着いたことを確認する。ここで待たずに数えると「まだ再レンダーが
      // 済んでいないだけ」を「透過マウントが起きなかった」と誤って合格にする
      await page.getByRole('button', { name: /再生/ }).waitFor({ timeout: 10000 })
      const switchedRequestsWithoutPlay = requestLog.filter((u) =>
        u.includes(`/services/${SERVICE_A}/live/`),
      )
      log(
        `  A のリンクを押しただけ（再生は押さない）での A 向け要求数: ` +
          `${switchedRequestsWithoutPlay.length}`,
      )
      if (switchedRequestsWithoutPlay.length > 0) {
        ng.push(
          `⓪' 再生中に別チャンネルへ切り替えると、押していない A へ` +
            `${switchedRequestsWithoutPlay.length} 件の要求が飛んだ（選択と視聴開始の分離が` +
            '切替の瞬間には成立していない）',
        )
      }
    }
  } finally {
    await browser.close()
  }
}

log('\n=== ⓪ 選択と視聴開始の分離（issue #234 M7-1。ffmpeg 不要） ===')
try {
  await runConsentCheck()
} catch (err) {
  ng.push(`⓪ の検証中に例外が発生した: ${err.message}`)
}

const hasFixture = ensureFixture()
if (hasFixture) ensureCaptionFixture()
if (!hasFixture) {
  log(
    'ffmpeg が見つからないため、フィクスチャを生成できない。' +
      '①②③④⑤⑧（フィクスチャを使う判定）③⑥⑦⑨⑩⑪ をすべて測れないとして報告する',
  )
  // **未測定を「すべて期待どおり」に混ぜない。** フィクスチャが無いと ⑦⑨⑩⑪ も
  // 一度も走らないので、数え漏らすと何も測っていない緑になる
  skipped.push('ffmpeg が無いため ①②③④⑤⑧（と ⑥⑦⑨⑩⑪）すべて未測定')
}

if (hasFixture) {
  const browser = await launchBrowser()
  // ①②④⑤⑧ の途中で何が例外を投げても NG として報告し、後続の ③（別ブラウザ）を
  // 続行する（クラッシュさせない）。実際に「壊してみる」検証で、native HLS
  // 判定を誤らせると `waitForFunction` が例外で落ちることを確認した経緯があるため
  try {
    await runChromiumChecks(browser)
  } catch (err) {
    ng.push(`①②④⑤⑧ の検証中に例外が発生した: ${err.message}`)
  } finally {
    await browser.close()
  }
}

/** runChromiumChecks は bundled Chromium で測れる①②④⑤⑧を実行する。 */
async function runChromiumChecks(browser) {
  const page = await browser.newPage({ viewport: { width: 960, height: 640 } })

  const requestLog = []
  page.on('request', (req) => requestLog.push(req.url()))

  // 離脱ヒント（⑧、issue #191）は `navigator.sendBeacon` で飛ぶ。**requestLog とは
  // 別に溜める** --- ④ は観測窓を作るために requestLog を途中でクリアするが、
  // ヒントはまさにそのクリアの直前（チャンネル切り替えの cleanup）で飛ぶので、
  // 同じ配列に入れると必ず消える。sendBeacon が実際にネットワーク要求として出て
  // いるか（jsdom では原理的に測れない）を見るのがこの判定の目的である
  const leaveLog = []
  page.on('request', (req) => {
    if (req.url().includes('/live/leave')) leaveLog.push(`${req.method()} ${req.url()}`)
  })
  /**
   * countLeaves は今までに観測した「その serviceId 向けの離脱ヒント」の件数。
   *
   * **⑧ は累積の有無ではなく「チャンネル切り替えを跨いで増えたか」で判定する。**
   * 累積が 1 件以上あることだけを見ると、切り替えと無関係に飛んだヒント
   * （タブが一瞬 hidden になった等、`visibilitychange` 由来のもの）で合格して
   * しまい、**送信をやめる実装でも緑になりうる**（レビュー指摘）。差分で見れば、
   * その回の切り替えが実際にヒントを出したことしか合格の理由にならない。
   */
  const countLeaves = (serviceId) =>
    leaveLog.filter(
      (entry) =>
        entry.startsWith(`POST ${BASE_URL}/api/sites/${SITE}/networks/`) &&
        entry.endsWith(`/services/${serviceId}/live/leave`),
    ).length

  const mode = { playlist: 'ok' }
  await mockLiveRoutes(page, mode)

  await page.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'networkidle' })
  // 選択と視聴開始の分離（issue #234 M7-1）。「再生」を押すまで <video> は
  // 存在しない --- ⓪ がこの分離自体を検証し、①〜⑤ は押した後の挙動を見る
  await clickPlay(page)
  await page.waitForSelector('video', { timeout: 15000 })

  // --- ① hls.js の動的 import チャンクが実際にリクエストされる ---
  log('\n=== ① hls.js の動的 import ===')
  const loadedHlsChunk = requestLog.some((u) => /\/assets\/hls-.*\.js(\?.*)?$/.test(u))
  log(`  assets/hls-*.js への要求: ${loadedHlsChunk ? 'あり' : 'なし'}`)
  if (!loadedHlsChunk) {
    ng.push(
      '① assets/hls-*.js への要求が無い（hls.js 経路が実際に使われていない。' +
        'supportsNativeHls を強制的に true にする等で壊れると再現する）',
    )
  }

  // --- ② MSE がアタッチされる（video.src が blob: になる） ---
  log('\n=== ② MSE のアタッチ ===')
  let attachedBlob = false
  try {
    await page.waitForFunction(mseAttached, undefined, { timeout: 10000 })
    attachedBlob = true
  } catch {
    attachedBlob = false
  }
  const videoSrc = await page.evaluate(
    () => document.querySelector('video')?.currentSrc ?? '',
  )
  log(`  video.currentSrc = ${videoSrc.slice(0, 40)}...`)
  if (!attachedBlob) {
    ng.push('② video.currentSrc / src のどちらも blob: にならない（MSE がアタッチされていない）')
  }

  // --- ④ チャンネル切り替え後、旧 serviceId へのセグメント要求が 0 件 ---
  log('\n=== ④ チャンネル切り替え時のセグメント要求の停止 ===')
  // **`page.goto` で切り替えてはならない。** フルナビゲーションはドキュメントを
  // 丸ごと破棄するので、それだけで全要求が止まる ---
  // `LivePlayer` の effect cleanup（`AbortController.abort()` / `hls.destroy()`）
  // を一切通らずに「要求が止まる」ことになり、この判定が保証したいもの
  // （cleanup が実際に効くこと）を測れない。チャンネル一覧のリンクをクリックする
  // クライアントサイドのナビゲーションでなければ意味がない
  //
  // 切り替え前に旧チャンネル（A）のセグメントが最低 1 件要求されていることを
  // 確認してから切り替える（そもそも要求していなければ「0 件」の判定が
  // 成立しない）
  const segmentsRequested = ([site, path]) =>
    window.performance
      .getEntriesByType('resource')
      .some((r) => r.name.includes(`/sites/${site}/networks/`) && r.name.includes(path))

  // **セグメントの URL に載るのは SI の serviceId**（issue #217）。#208〜#217 の
  // 間だけは mirakc 合成 id が載っており、ここを SI の id で照合すると
  // `network_id` が 0 でない限り一致せず、この待機が必ずタイムアウトしていた。
  const segmentsA = liveSegmentsPathOf(SERVICE_A)
  const segmentsB = liveSegmentsPathOf(SERVICE_B)

  await page.waitForFunction(segmentsRequested, [SITE, segmentsA], { timeout: 10000 })
  const requestsBeforeSwitchCount = requestLog.filter((u) => u.includes(segmentsA)).length
  log(`  切替前の A 向けセグメント要求数: ${requestsBeforeSwitchCount}`)

  // ⑧ の基準値。**クリックの直前**に取る（切り替えを跨いだ増分だけを見るため）。
  const leavesBeforeSwitch = { a: countLeaves(SERVICE_A), b: countLeaves(SERVICE_B) }

  await page
    .locator(`nav[aria-label="チャンネル一覧"] a[href*="service=${SERVICE_ID_B}"]`)
    .click()
  // 選択と視聴開始の分離（issue #234 M7-1）。チャンネルを切り替えると選択状態
  // （再生ボタン）に戻る --- 同意はチャンネルごとに必要なので、B の
  // LivePlayer を起こすにはここでも「再生」を押す。
  //
  // **`requestLog` のクリアは、この「再生」ボタンが見えるのを待った後にする。**
  // ボタンが見えている = A の `LivePlayer` は（切替時の 1 回目の cleanup と、
  // 再生状態が落ちたことによる 2 回目の cleanup の両方を経て）確実に
  // unmount 済みということなので、ここで初めて「以降 A への要求が無い」の
  // 観測窓を開く。クリアを先にしてクリックを後にすると、クリア直後・クリック
  // 処理が実際に効くまでの数 ms の間に A 自身の自然なセグメント要求（バッファ
  // 継続のための次セグメント取得）が発火してクリア後の配列に載ることがあり、
  // それを cleanup 未実施の「残存要求」と誤認するレースになる（実測: この
  // 順序にする前は毎回ちょうど 1 件、A 向けの要求が「残存」として検出された）
  const playButtonForB = page.getByRole('button', { name: /再生/ })
  await playButtonForB.waitFor()
  requestLog.length = 0
  await playButtonForB.click()
  await page.waitForFunction(segmentsRequested, [SITE, segmentsB], { timeout: 10000 })
  // 切り替え後もしばらく要求が続くかもしれない旧チャンネルの要求を数える余地を
  // 与える（hls.js の非同期な内部タイマーが 1 フレームだけ遅れて発火する
  // ケースを見逃さないため）
  await page.waitForTimeout(1500)
  const staleRequestsAfterSwitch = requestLog.filter((u) => u.includes(segmentsA))
  log(`  切替後の A 向けセグメント要求数: ${staleRequestsAfterSwitch.length}`)
  if (staleRequestsAfterSwitch.length > 0) {
    ng.push(
      `④ チャンネル切替後も旧チャンネル（${SERVICE_A}）へのセグメント要求が` +
        `${staleRequestsAfterSwitch.length} 件続いた（cleanup が破棄していない）`,
    )
  }

  // --- ⑧ 離脱ヒントが実ブラウザから実際に飛ぶ（issue #191） ---
  log('\n=== ⑧ チャンネル切り替え時の離脱ヒント（sendBeacon） ===')
  // **jsdom では原理的に測れない部分がここにある。** `navigator.sendBeacon` は
  // jsdom に存在しないので、ユニットテスト（`live-player.test.tsx`）が見ているのは
  // 「差し替えた sendBeacon が呼ばれたか」という配線だけ。実ブラウザが本当に
  // ネットワーク要求として送出するか（キューに載せて捨てないか）はここでしか出ない。
  // **増分で見る**（累積の有無では、送信をやめる実装でも切り替えと無関係な
  // ヒント 1 件で緑になりうる。`countLeaves` のコメント参照）。
  const gainedA = countLeaves(SERVICE_A) - leavesBeforeSwitch.a
  const gainedB = countLeaves(SERVICE_B) - leavesBeforeSwitch.b
  log(`  切替前の leave 件数: A=${leavesBeforeSwitch.a} B=${leavesBeforeSwitch.b}`)
  log(`  切替で増えた leave 件数: A=${gainedA} B=${gainedB}`)
  log(`  観測した leave 要求: ${leaveLog.length === 0 ? '（なし）' : leaveLog.join(', ')}`)
  if (gainedA < 1) {
    ng.push(
      `⑧ チャンネル切替で旧チャンネル（${SERVICE_A}）への離脱ヒント` +
        `（POST .../live/leave）が増えていない（切替前 ${leavesBeforeSwitch.a} 件、` +
        `切替後 ${countLeaves(SERVICE_A)} 件）`,
    )
  }
  // 新しく見ているチャンネル（B）に送ってはならない --- 送ると自分の視聴の
  // idle 期限を自分で詰めることになる
  if (gainedB > 0) {
    ng.push(`⑧ これから見るチャンネル（${SERVICE_B}）にも離脱ヒントが ${gainedB} 件飛んでいる`)
  }

  // --- ⑤ 503（本文つき）でエラー文言が出る。再読み込みで復帰する ---
  log('\n=== ⑤ 503 エラー表示 → 再読み込みで復帰 ===')
  mode.playlist = 'error'
  mode.playlistErrorBody = 'too many concurrent live sessions on this process'
  await page.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'networkidle' })
  await clickPlay(page)
  let errorShown = false
  try {
    await page.getByText('too many concurrent live sessions on this process').waitFor({
      timeout: 10000,
    })
    errorShown = true
  } catch {
    errorShown = false
  }
  log(`  503 の本文がそのまま表示された: ${errorShown ? 'YES' : 'NO'}`)
  if (!errorShown) ng.push('⑤ 503 の本文がそのまま表示されない')

  mode.playlist = 'ok'
  const retryButton = page.getByRole('button', { name: '再読み込み' })
  if ((await retryButton.count()) === 0) {
    ng.push('⑤ 再読み込みボタンが見つからない')
  } else {
    await retryButton.click()
    let recovered = false
    try {
      await page.waitForFunction(mseAttached, undefined, { timeout: 10000 })
      recovered = true
    } catch {
      recovered = false
    }
    log(`  再読み込みで復帰した: ${recovered ? 'YES' : 'NO'}`)
    if (!recovered) ng.push('⑤ 再読み込みを押しても復帰しない（MSE がアタッチされない）')
  }

  // --- ⑨ 画質（プロファイル）切替（M4-21 / issue #869） ---
  //
  // **jsdom では原理的に測れない 2 点をここで見る。**
  //
  //   1. 一覧が遅れて届いてもプレイリストを取り直さないこと（`LivePlayer` に
  //      導出した既定を渡す実装だと、一覧の到着で `profile` が変わって probe の
  //      effect が再実行され、`<video>` が作り直されて先頭から再生し直しになる）
  //   2. 切替を跨いで音量・ミュートが保たれること（jsdom の `HTMLMediaElement.load`
  //      は no-op なので、ユニットテストでは復元しなくても通ってしまう）
  //
  // あわせて **切替が離脱ヒントを送らない**ことも見る（送れば「セッションを
  // 手放した」ことになり、同じチャンネルの他視聴者の再生を縮める側に倒れる）。
  log('\n=== ⑨ 画質（プロファイル）切替 ===')
  try {
    const profilePage = await browser.newPage({ viewport: { width: 960, height: 640 } })
    const playlistLog = []
    const profileLeaveLog = []
    profilePage.on('request', (req) => {
      if (req.url().includes('/live/playlist.m3u8')) playlistLog.push(req.url())
      if (req.url().includes('/live/leave')) profileLeaveLog.push(req.url())
    })
    // 一覧を**わざと遅らせる**（判定 1 の窓を作る）。`pendingProfiles` が真の間は
    // 応答せず、「再生」を押した後に解放する。
    let releaseProfiles = null
    let holdProfiles = true
    await mockLiveRoutes(profilePage, { playlist: 'ok' })
    await profilePage.unroute('**/api/live-profiles')
    await profilePage.route('**/api/live-profiles', async (route) => {
      if (holdProfiles) {
        await new Promise((resolve) => {
          releaseProfiles = resolve
        })
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(E2E_LIVE_PROFILES),
      })
    })

    // **`networkidle` で待てない。** 一覧をわざと保留しているので、この
    // ページは永久に idle にならない（`waitUntil: 'networkidle'` で実測 30 秒
    // タイムアウトした）。DOM の構築だけを待ち、以降はセレクタで待つ。
    await profilePage.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, {
      waitUntil: 'domcontentloaded',
    })
    await profilePage.getByRole('button', { name: /再生/ }).waitFor({ timeout: 15000 })
    await clickPlay(profilePage)
    await profilePage.waitForFunction(mseAttached, undefined, { timeout: 15000 })
    const probesBeforeRelease = playlistLog.length
    log(`  一覧が未解決のまま再生を開始した（プレイリスト要求 ${probesBeforeRelease} 件）`)

    // 一覧を届かせる。`profile` が変わらない実装なら、ここで要求は増えない
    holdProfiles = false
    releaseProfiles?.()
    await profilePage.waitForSelector('select[aria-label="画質"]', { timeout: 15000 })
    await profilePage.waitForTimeout(500)
    const probesAfterRelease = playlistLog.length
    log(`  一覧の到着後のプレイリスト要求: ${probesAfterRelease} 件（増分 ${probesAfterRelease - probesBeforeRelease}）`)
    if (probesAfterRelease !== probesBeforeRelease) {
      ng.push(
        '⑨ 一覧が遅れて届いただけでプレイリストを取り直している' +
          `（${probesBeforeRelease} → ${probesAfterRelease} 件。再生が先頭からやり直しになる）`,
      )
    }

    // 視聴者が音量とミュートを変える
    await profilePage.evaluate(() => {
      const v = document.querySelector('video')
      v.volume = 0.3
      v.muted = true
    })

    // 再生中の切替
    const leavesBeforeSwitch = profileLeaveLog.length
    await profilePage.selectOption('select[aria-label="画質"]', 'sd')
    const switchDeadline = Date.now() + 15000
    while (
      !playlistLog.some((u) => u.includes('profile=sd')) &&
      Date.now() < switchDeadline
    ) {
      await profilePage.waitForTimeout(100)
    }
    const switched = playlistLog.filter((u) => u.includes('profile=sd')).length
    log(`  切替後に profile=sd で飛んだプレイリスト要求: ${switched} 件`)
    if (switched === 0) {
      ng.push('⑨ 画質を切り替えても ?profile=sd のプレイリスト要求が飛ばない')
    }
    if (profileLeaveLog.length !== leavesBeforeSwitch) {
      ng.push(
        '⑨ 画質の切替で離脱ヒントが飛んだ（セッションを手放す合図であってはならない。' +
          ' 同じチャンネルの他視聴者の再生を縮める側に倒れる）',
      )
    }

    const media = await profilePage.evaluate(() => {
      const v = document.querySelector('video')
      return { volume: v.volume, muted: v.muted }
    })
    log(`  切替後の video.volume = ${media.volume}, video.muted = ${media.muted}`)
    if (Math.abs(media.volume - 0.3) > 0.001) {
      ng.push(`⑨ 画質の切替で音量が失われた（${media.volume}、期待 0.3）`)
    }
    if (media.muted !== true) {
      ng.push('⑨ 画質の切替でミュートが失われた')
    }
  } catch (err) {
    ng.push(`⑨ 画質切替の検証中に例外が発生した: ${err.message}`)
  }

  // --- ⑩ 画質を切り替えても字幕の表示状態が保たれる（M4-21 / issue #869） ---
  //
  // **⑨ と同じ「切替で作り直さない」性質の、字幕側の面である。** hls.js は
  // 新しいマニフェストを読むと字幕トラックの選択を既定に戻す
  // （`SubtitleTrackController.onManifestLoading` が `trackId` を `-1` に戻す。
  // `node_modules/hls.js` 1.7.1 で確認済み）ので、素朴に作り直すと
  // **視聴者がネイティブコントロールで「切り」にした字幕が「入」に戻る**。
  // `LivePlayer` は `MANIFEST_PARSED` で `subtitleDisplay` を設定し直しており、
  // ここはその経路が実ブラウザで効くかを見る唯一の判定である。
  log('\n=== ⑩ 画質切替と字幕の表示状態 ===')
  try {
    const captionPage = await browser.newPage({ viewport: { width: 960, height: 640 } })
    const captionPlaylists = []
    captionPage.on('request', (req) => {
      if (req.url().includes('/live/playlist.m3u8')) captionPlaylists.push(req.url())
    })
    await mockLiveRoutes(captionPage, { playlist: 'captions' })

    await captionPage.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'networkidle' })
    await clickPlay(captionPage)
    await captionPage.waitForFunction(mseAttached, undefined, { timeout: 15000 })

    /** subtitleModes は `<video>` の字幕トラックの mode 一覧。 */
    const subtitleModes = () =>
      captionPage.evaluate(() =>
        Array.from(document.querySelector('video')?.textTracks ?? []).map((t) => t.mode),
      )

    // **トラックが出来るまで待つ（空虚な成功を防ぐ）。** トラックが 1 本も無い
    // 状態で「showing が無い」を見ても、字幕の配線が丸ごと壊れていて通ってしまう。
    let captionTracksAppeared = false
    const appearDeadline = Date.now() + 15000
    while (Date.now() < appearDeadline) {
      if ((await subtitleModes()).length > 0) {
        captionTracksAppeared = true
        break
      }
      await captionPage.waitForTimeout(200)
    }
    const initialModes = await subtitleModes()
    log(`  字幕トラックの数: ${initialModes.length}, mode: ${JSON.stringify(initialModes)}`)
    if (!captionTracksAppeared) {
      ng.push(
        '⑩ 字幕トラックが 1 本も現れない（master playlist の SUBTITLES レンディションを ' +
          'hls.js が読めていない。フィクスチャか master のパスを疑う）',
      )
    } else if (!initialModes.includes('showing')) {
      // 既定は表示（`hls.subtitleDisplay = true`）。ここが既に違うなら、
      // 以降の「切っても入に戻らない」判定が何を測っているか分からなくなる
      ng.push(`⑩ 既定で字幕が表示されていない（mode: ${JSON.stringify(initialModes)}）`)
    }

    // 視聴者がネイティブコントロールの Captions で「切り」にする
    await captionPage.evaluate(() => {
      for (const t of Array.from(document.querySelector('video').textTracks)) t.mode = 'disabled'
    })
    log(`  視聴者が切った直後: ${JSON.stringify(await subtitleModes())}`)

    const before = captionPlaylists.length
    await captionPage.selectOption('select[aria-label="画質"]', 'sd')
    const switchDeadline = Date.now() + 15000
    while (captionPlaylists.length === before && Date.now() < switchDeadline) {
      await captionPage.waitForTimeout(100)
    }

    // **切替後にもう一度トラックが現れるのを待ってから** mode を見る
    // （現れる前に見ると、上の空虚な成功と同じ穴になる）。
    let reappeared = false
    const reappearDeadline = Date.now() + 15000
    while (Date.now() < reappearDeadline) {
      if ((await subtitleModes()).length > 0) {
        reappeared = true
        break
      }
      await captionPage.waitForTimeout(200)
    }
    const afterModes = await subtitleModes()
    log(`  切替後の mode: ${JSON.stringify(afterModes)}`)
    if (!reappeared) {
      ng.push('⑩ 切替後に字幕トラックが現れない（新しいマニフェストのレンディションを読めていない）')
    } else if (afterModes.includes('showing')) {
      ng.push(
        '⑩ 画質の切替で、視聴者が切った字幕が再び表示された' +
          `（mode: ${JSON.stringify(afterModes)}。MANIFEST_PARSED で subtitleDisplay を` +
          ' 設定し直していない）',
      )
    }
  } catch (err) {
    ng.push(`⑩ 字幕の検証中に例外が発生した: ${err.message}`)
  }

  // --- ⑩-WebKit ネイティブ経路（Safari 相当）の字幕 ---
  //
  // **向きが逆である。** WebKit は字幕を**既定で表示しない**（レンディションに
  // `DEFAULT=YES` を書いても、Safari の字幕は利用者が有効にするまで出ない。
  // 実測）。したがってここで見るのは「利用者が入にしてから切り替えても入の
  // まま」の側で、hls.js 経路（⑩）の「切ったまま」と対になる。
  //
  // **`loadedmetadata` で揃えるだけでは足りない**（実測: 入にしてから切り替えると
  // `disabled` に戻った）。WebKit は `loadedmetadata` の時点でまだトラックを
  // 作っていないためで、`LivePlayer` は `TextTrackList` の `addtrack` でも
  // 適用する。ここはその経路が実ブラウザで効くかを見る唯一の判定である。
  log('\n=== ⑩-WebKit 画質切替と字幕（ネイティブ経路） ===')
  let webkitCaptionBrowser = null
  try {
    webkitCaptionBrowser = await launchBrowser('webkit')
  } catch {
    webkitCaptionBrowser = null
  }
  if (!webkitCaptionBrowser) {
    log('  WebKit が無いため測れない')
    skipped.push('⑩-WebKit の字幕は WebKit が無いため未測定')
  } else {
    try {
      const page = await webkitCaptionBrowser.newPage({ viewport: { width: 960, height: 640 } })
      await mockLiveRoutes(page, { playlist: 'captions' })
      await page.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'networkidle' })
      await clickPlay(page)
      await page.waitForFunction(nativeSrcAssigned, undefined, { timeout: 15000 })
      await page.waitForFunction(
        () => (document.querySelector('video')?.textTracks?.length ?? 0) > 0,
        undefined,
        { timeout: 20000 },
      )
      /** subtitleModes は `<video>` の字幕トラックの mode 一覧。 */
      const modes = () =>
        page.evaluate(() =>
          Array.from(document.querySelector('video')?.textTracks ?? []).map((t) => t.mode),
        )
      // 利用者がネイティブコントロールの CC で「入」にする
      await page.evaluate(() => {
        for (const t of Array.from(document.querySelector('video').textTracks)) t.mode = 'showing'
      })
      log(`  入にした直後: ${JSON.stringify(await modes())}`)

      await page.selectOption('select[aria-label="画質"]', 'sd')
      await page.waitForTimeout(3000)
      const after = await modes()
      log(`  切替後の mode: ${JSON.stringify(after)}`)
      if (!after.includes('showing')) {
        ng.push(
          '⑩-WebKit 画質の切替で、利用者が入にした字幕が消えた' +
            `（mode: ${JSON.stringify(after)}。ネイティブ経路は TextTrackList の` +
            ' addtrack で適用していない）',
        )
      }
    } catch (err) {
      ng.push(`⑩-WebKit 字幕の検証中に例外が発生した: ${err.message}`)
    } finally {
      await webkitCaptionBrowser.close()
    }
  }
}

if (hasFixture) {
  // --- ③ 実際に再生が進む（実 Chrome のみ。bundled Chromium は H.264/AAC 非対応） ---
  log('\n=== ③ 実再生（video.currentTime が進む） ===')
  let chromeBrowser
  try {
    chromeBrowser = await launchBrowser('chromium', { channel: 'chrome' })
  } catch {
    chromeBrowser = null
  }
  if (!chromeBrowser) {
    log('  ローカルの Google Chrome が見つからないため測れない（bundled Chromium は H.264/AAC 非対応）')
    skipped.push('③ 実再生は Chrome チャンネルが無いため未測定')
  } else {
    // ここから先で何が失敗しても NG として報告する（クラッシュさせない）。
    // 実際に「壊してみる」検証で、native HLS 判定を誤らせると `blob:` に
    // ならず `waitForFunction` が例外で落ちることを確認した経緯があるため
    try {
      const chromePage = await chromeBrowser.newPage({ viewport: { width: 960, height: 640 } })
      await mockLiveRoutes(chromePage, { playlist: 'ok' })
      await chromePage.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'networkidle' })
      await clickPlay(chromePage)
      await chromePage.waitForFunction(mseAttached, undefined, { timeout: 15000 })
      await chromePage.evaluate(() => document.querySelector('video').play())
      const before = await chromePage.evaluate(() => ({
        t: document.querySelector('video').currentTime,
        w: document.querySelector('video').videoWidth,
        r: document.querySelector('video').readyState,
      }))
      await chromePage.waitForTimeout(3000)
      const after = await chromePage.evaluate(() => ({
        t: document.querySelector('video').currentTime,
        w: document.querySelector('video').videoWidth,
        r: document.querySelector('video').readyState,
      }))
      log(`  currentTime: ${before.t.toFixed(2)} → ${after.t.toFixed(2)}`)
      log(`  videoWidth: ${after.w}, readyState: ${after.r}`)
      if (!(after.t > before.t)) ng.push('③ 3 秒待っても currentTime が進まない')
      if (!(after.w > 0)) ng.push('③ videoWidth が 0（映像が実際にデコードされていない）')
      if (!(after.r >= 3)) {
        ng.push(`③ readyState が ${after.r}（3 未満。再生可能な量のデータが届いていない）`)
      }

      // --- 遅延・バッファの計器（issue #476）。hls.js の `latency` /
      // `mainForwardBufferInfo` はライブ同期点が決まるまで値を返さないので、
      // bundled Chromium（H.264/AAC 非対応で実デコードが進まない）では
      // 検証できない --- ここ（実 Chrome）でしか測れない
      try {
        await chromePage.waitForFunction(liveDiagnosticsBecameNumeric, undefined, { timeout: 10000 })
        const diagnosticsText = await chromePage.evaluate(
          () => document.querySelector('[data-testid="live-diagnostics"]')?.textContent ?? '',
        )
        log(`  計器: ${diagnosticsText}`)
        if (diagnosticsText.includes('NaN')) {
          ng.push(`③ 計器に NaN が描画された（${diagnosticsText}）`)
        }
      } catch {
        ng.push(
          '③ 「放送から約 n 秒 / 先読み n 秒」が数値にならない（hls.latency / ' +
            'mainForwardBufferInfo.len を読んでいない可能性がある）',
        )
      }
    } catch (err) {
      ng.push(`③ 実再生の検証中に例外が発生した: ${err.message}`)
    } finally {
      await chromeBrowser.close()
    }
  }
}

if (hasFixture) {
  // --- ⑥ WebKit（Safari 相当）はネイティブ HLS 経路に入る ---
  //
  // **この判定が無かったために、Safari が hls.js 経路へ落ちる変更が e2e 緑のまま
  // 通った**（レビュー #190 の 2 回目の指摘）。①〜⑤ は Chromium 系しか回して
  // いないので、「Safari ではネイティブを使い hls.js を読み込まない」という決定
  // （issue #92 の着手時コメント 1）は一度も機械判定されていなかった。
  //
  // WebKit は `<video>` が MPEG-2 TS を demux できる唯一のエンジンなので、
  // フィクスチャ（H.264/AAC in TS）をそのまま再生できる --- 実再生まで見る。
  log('\n=== ⑥ WebKit（Safari 相当）のネイティブ HLS 経路 ===')
  const webkitBrowser = await launchBrowser('webkit')
  try {
    const page = await webkitBrowser.newPage({ viewport: { width: 960, height: 640 } })
    const requestLog = []
    page.on('request', (req) => requestLog.push(req.url()))
    await mockLiveRoutes(page, { playlist: 'ok' })
    await page.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'networkidle' })
    await clickPlay(page)

    // 経路が決まる（どちらかの分岐が `<video>` に何かを渡す）まで待つ。
    // ここで待たずに数えると「まだ hls.js を要求していないだけ」を
    // 「要求しなかった」と誤って合格にする（非同期の空虚な成功）
    let decided = true
    try {
      await page.waitForFunction(playerDecided, undefined, { timeout: 15000 })
    } catch {
      decided = false
    }

    const src = await page.evaluate(() => {
      const v = document.querySelector('video')
      return { src: v?.src ?? '', currentSrc: v?.currentSrc ?? '' }
    })
    log(`  video.src = ${src.src.slice(0, 60)}`)
    log(`  video.currentSrc = ${src.currentSrc.slice(0, 60)}`)

    if (!decided) {
      ng.push('⑥ WebKit で再生経路が決まらない（video に src も currentSrc も入らない）')
    }

    // 判定 1: hls.js の動的 import チャンクを読み込まない（決定 1 の実体）
    const loadedHlsChunk = requestLog.some((u) => /\/assets\/hls-.*\.js(\?.*)?$/.test(u))
    log(`  assets/hls-*.js への要求: ${loadedHlsChunk ? 'あり' : 'なし'}`)
    if (loadedHlsChunk) {
      ng.push(
        '⑥ WebKit が hls.js のチャンク（約 520 KB）を読み込んだ。' +
          'ネイティブ HLS 分岐に入っていない（issue #92 の決定 1 が成立していない）',
      )
    }

    // 判定 2: `<video>` に m3u8 の URL がそのまま渡っている
    const wentNative = await page.evaluate(nativeSrcAssigned)
    log(`  video に m3u8 の URL が渡っている: ${wentNative ? 'YES' : 'NO'}`)
    if (!wentNative) {
      ng.push('⑥ WebKit で video.src / currentSrc が m3u8 にならない（ネイティブ経路に入っていない）')
    }

    // 判定 3: ネイティブのまま実際に再生が進む（WebKit は TS を demux できる）
    try {
      await page.evaluate(() => document.querySelector('video').play())
      const before = await page.evaluate(() => document.querySelector('video').currentTime)
      await page.waitForTimeout(3000)
      const after = await page.evaluate(() => ({
        t: document.querySelector('video').currentTime,
        w: document.querySelector('video').videoWidth,
        r: document.querySelector('video').readyState,
      }))
      log(`  currentTime: ${before.toFixed(2)} → ${after.t.toFixed(2)}`)
      log(`  videoWidth: ${after.w}, readyState: ${after.r}`)
      if (!(after.t > before)) ng.push('⑥ WebKit で 3 秒待っても currentTime が進まない')
      if (!(after.w > 0)) ng.push('⑥ WebKit で videoWidth が 0（映像がデコードされていない）')

      // **ネイティブ経路のチャンネル切替はここでは判定しない。** 一度足して
      // みたが、どう壊しても落ちなかったので外した（落ちない判定は何も判定して
      // いない。CLAUDE.md「テスト規律」）。理由は測って分かった --- 切替時は
      // 同じ `<video>` に新しい `src` が入るので、それ自体が旧チャンネルの
      // メディア資源を破棄する。cleanup の `removeAttribute('src')` を
      // 無効化しても旧チャンネルへの要求は 0 件のままだった（cleanup が効くのは
      // 画面を離れるときで、それはドキュメントごと消えるので測れない）
    } catch (err) {
      ng.push(`⑥ WebKit の実再生の検証中に例外が発生した: ${err.message}`)
    }
  } catch (err) {
    ng.push(`⑥ の検証中に例外が発生した: ${err.message}`)
  } finally {
    await webkitBrowser.close()
  }
}

if (hasFixture) {
  // --- ⑦ ネイティブ経路で「probe は 200 だがメディアが死んでいる」ときの失敗表面 ---
  //
  // probe（`fetch` によるプレイリストの事前取得）は HTTP 層しか見ないので、
  // プレイリストが 200 でセグメントが壊れている状況は素通りする。ここを
  // `<video>` のイベントで拾えていないと、**永久に止まった黒いプレイヤー**に
  // なる（文言も読み込み表示も再読み込みボタンも出ない）。レビュー #190 の
  // 3 回目の指摘で実測された症状そのものを判定にする。
  //
  // 2 通り試すのは、実測で**壊れ方によって出るイベントが違った**ため:
  //   404  → `error` が出る（`video.error` は code 3）
  //   応答なし → `error` は出ず `stalled` だけが出る（3.6 秒後）
  // 片方だけ見ると、もう片方を落とす実装変更を通してしまう
  //
  // **応答なしの窓は 30 秒では足りない（実測 31.5 秒）。** 自動降格（issue #871）が
  // 猶予の満了で 1 回挟まるためで、そこから更に 1 窓ぶん待って初めてエラー表示に
  // 落ちる（降格先が無ければそこでエラーになる）。降格の導入前は 30 秒で
  // 足りていた。**窓そのものが「黒いままにならない」の上限である** --- 判定は
  // 「窓の中でエラー表示と再読み込みが出るか」しか見ないので、窓を広げすぎると
  // 遅延の退行（例: 閾値を 25 秒にすると約 57 秒）を黙って通す。実測 31.5 秒に
  // 対して余裕を残した 40 秒にする（この e2e のフィクスチャはプロファイル 2 件
  // なので降格は高々 1 回。3 件以上ある実運用では、段が尽きるまで降格を試すので
  // この待ち時間はプロファイル数に比例して伸びる）
  log('\n=== ⑦ ネイティブ経路のメディア失敗（WebKit） ===')
  for (const [label, segments, timeout] of [
    ['セグメントが 404', '404', 20000],
    ['セグメントが応答しない', 'hang', 40000],
  ]) {
    const browser = await launchBrowser('webkit')
    try {
      const page = await browser.newPage({ viewport: { width: 960, height: 640 } })
      await mockLiveRoutes(page, { playlist: 'ok', segments })
      await page.goto(`${BASE_URL}/live?service=${SERVICE_ID_A}`, { waitUntil: 'domcontentloaded' })
      await clickPlay(page)

      let shown = false
      const shownStartedAt = Date.now()
      try {
        await page.getByText('ライブ視聴でエラーが発生しました。').waitFor({ timeout })
        await page.getByRole('button', { name: '再読み込み' }).waitFor({ timeout: 5000 })
        shown = true
      } catch {
        shown = false
      }
      const shownAfterMs = Date.now() - shownStartedAt
      const detail = await page.evaluate(() => {
        const v = document.querySelector('video')
        return {
          text: document.body.innerText.replace(/\s+/g, ' ').slice(0, 160),
          readyState: v?.readyState ?? -1,
          err: v?.error ? v.error.code : null,
        }
      })
      log(`  ${label}: エラー表示 + 再読み込み = ${shown ? 'YES' : 'NO'}（${shownAfterMs} ms）`)
      log(`    readyState=${detail.readyState} video.error=${detail.err}`)
      if (!shown) {
        ng.push(
          `⑦ ${label}のとき、エラー表示も再読み込みボタンも出ない` +
            `（永久に止まった黒いプレイヤーになる）。画面のテキスト: ${detail.text}`,
        )
      }
    } catch (err) {
      ng.push(`⑦ ${label}の検証中に例外が発生した: ${err.message}`)
    } finally {
      await browser.close()
    }
  }
}

if (hasFixture) {
  // --- ⑪ 停滞したときに画質が 1 段下がる（M4-23 / issue #871） ---
  //
  // **jsdom では原理的に作れない状況である。** 実際に映像が止まる（`currentTime` が
  // 進まない）状態は、実ブラウザでセグメントの応答を止めないと現れない。判定の
  // 純関数（`lib/live.ts`）と配線（`live-player.test.tsx`）はユニットテストが
  // 見ているが、「実ブラウザで停滞させると実際に 1 段下がる」ことはここでしか
  // 出ない。
  //
  // **両方向を見る。** 自動（`?profile=` なし）では下がり、利用者が明示的に
  // 選んだ画質（`?profile=hd`）では下がらない。後者を「下がらない」だけで
  // 確かめると空虚な成功になる（そもそも判定が一度も走っていなくても動く）ので、
  // **同じ壊し方・同じ待ち時間で自動側が実際に下がることを前者が示している**
  // ことを前提に、同じ窓だけ待って下がらないことを見る。
  //
  // **どの assert がどの回帰を捕まえるか**（実測で確認した。再現するときは
  // 実装を一時的に壊し、`cd web && npm run build` してからこのスクリプトを
  // 走らせる。**壊すのはコンパイルが通る形にすること** --- 型エラーでビルドが
  // 落ちると、前のバンドルを測ったまま「緑」になる）:
  //
  // | 壊し方 | 落ちる assert |
  // |---|---|
  // | 降格を止める（`onStalled` を渡さない） | 自動の要求・通知・セレクタの 3 件 |
  // | 抑止を「master かどうか」で判定する（`bundlesProfiles` を `#EXT-X-STREAM-INF` の有無に） | 同じ 3 件 |
  // | 再開を止める（`preserved.playing` を常に false に固定） | 自動の「復旧後に進んだ」 |
  // | 実再生の前提を壊す（`play()` を `pause()` に） | 前提 + 上記 4 件 |
  // | 復旧させない（プレイリストを伸ばさない） | 自動の再開と、明示側の対照 |
  //
  // 離脱ヒント 0 件の判定は、**現在の配線では原理的に落ちない**（ヒントの effect
  // の依存に `profile` が無い）。依存に足す・`key` で作り直す回帰が来たときの
  // ための位置づけである。
  //
  // **停滞は「ライブの窓が伸びない」ことで作る。** 窓に載るセグメントを 1 本に
  // 絞って配り（`livePlaylistWith`）、再取得しても本数を増やさないと、hls.js は
  // そのぶんを再生し切ったところで進まなくなる。**セグメントの応答を止める
  // （`hang`）のは効かない** --- 窓を絞ると停滞の窓の中では新しいセグメントを
  // 取りに行かないので、止めても止めなくても降格は同じ時刻（実測 22,184 ms /
  // 22,188 ms）で起きる（フィクスチャ全体は 4 本 × 10 秒 = 40 秒。`-hls_time 2`
  // は `-g` を指定していないので効いておらず、`#EXT-X-TARGETDURATION:10` である）。
  log('\n=== ⑪ 停滞したときの画質の自動降格（issue #871） ===')
  const downgraded = E2E_LIVE_PROFILES[1]
  // 停滞の閾値（`liveStallTimeoutMs` = 12 秒）+ 窓 1 本ぶん（セグメント長 10 秒）
  // + hls.js のポーリング 1 秒 + probe / アタッチの時間。**判定の窓と同じ長さを
  // 両方向に使う**（片方だけ長くすると「下がらない」の側が空虚になる）。
  // 実測の降格は 22,184〜23,263 ms（5 回）なので、30 秒で 7〜8 秒の余裕を取る
  // （25 秒だと余裕が 1.7〜2.8 秒しかなく、実行間の振れが 1.1 秒あった）
  const stallWaitMs = 30_000
  // 復旧を待つ窓。**hls.js のプレイリスト再取得間隔は target duration（10 秒）
  // なので、位相が最悪だと復旧の反映に 10 秒以上かかる**（実測の復帰は 4 回とも
  // 500 ms 以内だったが、それは再取得がたまたま直後に来たため）
  const recoverWaitMs = 2 * Math.max(...fixtureSegments().map(([duration]) => duration)) * 1000 + 5000

  /** readCurrentTime は `<video>` の現在位置（秒。無ければ null）。 */
  async function readCurrentTime(page) {
    return page.evaluate(() => {
      const v = document.querySelector('video')
      return v ? v.currentTime : null
    })
  }

  /**
   * waitForProgress は配信の復旧後に `currentTime` が進むのを待つ。
   *
   * **`paused` も返す** --- 「進まなかった」の原因が一時停止なのか、データが
   * 来ていないのかを報告で区別できるようにする。
   *
   * **限界**: フィクスチャのセグメントが 10 秒なので、「1 本だけ配って止まる」
   * 状態までは区別できない（0.2 秒進んだところで合格する）。そこは配った
   * セグメント数（呼び出し側が `mode.servedSegments` で数える）と組み合わせて
   * 見る。厳密に見るなら 2 秒セグメントのフィクスチャが要る（`ensureFixture` の
   * `-g` を指定していないため今は 10 秒になっている）。
   */
  async function waitForProgress(page, timeoutMs = 12000) {
    let previous = await readCurrentTime(page)
    const deadline = Date.now() + timeoutMs
    while (Date.now() < deadline) {
      await page.waitForTimeout(500)
      const sample = await page.evaluate(() => {
        const v = document.querySelector('video')
        return { currentTime: v?.currentTime ?? 0, paused: v?.paused ?? true }
      })
      // **`paused` も要求する。** `load()` の直後は一時停止したままでも
      // `currentTime` が少し進むことがあり、「進んだ」だけでは
      // 「利用者がもう一度再生を押す必要がある」状態を見逃す（実測: 再開の
      // コードを外した版で `advanced=true, paused=true` になった）
      if (sample.currentTime > (previous ?? 0) + 0.2 && !sample.paused) {
        return { advanced: true, currentTime: sample.currentTime, paused: sample.paused }
      }
      previous = Math.max(previous ?? 0, sample.currentTime)
    }
    const last = await page.evaluate(() => {
      const v = document.querySelector('video')
      return { currentTime: v?.currentTime ?? 0, paused: v?.paused ?? true }
    })
    return { advanced: false, currentTime: last.currentTime, paused: last.paused }
  }

  /**
   * stallCheck は 1 方向ぶんを実行し、観測したものを返す。
   *
   * **手順が要点である。** まずセグメントを配って**実再生させる**（`currentTime` が
   * 進むことを確かめる）。そのうえで配信を止め、停滞の判定を待つ。最後に配信を
   * 復旧させ、**再生が続くか**を見る。
   *
   * 最初に実再生させないと 2 つの意味が壊れる。(1) 「停滞」が回線の停滞ではなく
   * 「そもそも一度も再生できなかった」ことになる。(2) 切替の cleanup が読む
   * `paused` が true のままになり、利用者が再生していた場合の経路（切替の後に
   * 再生を再開する）を一度も通らない。実際、実再生を挟まない版では
   * `preserved.playing` が false になり、降格後も `paused=true` のままで
   * 何も検証できていなかった。
   *
   * **復旧のあとに再生が進むことは両方向で見る。** 切替を挟まない側（明示選択）
   * が自動で復帰するので、切替を挟んだ側が復帰しなければ差分は切替に帰せる。
   */
  async function stallCheck(browser, path) {
    const page = await browser.newPage({ viewport: { width: 960, height: 640 } })
    const playlists = []
    const leaves = []
    page.on('request', (req) => {
      if (req.url().includes('/live/playlist.m3u8')) playlists.push(req.url())
      if (req.url().includes('/live/leave')) leaves.push(req.url())
    })
    /**
     * mode は書き換えると以降の応答が変わる（`mockLiveRoutes` が毎回読む）。
     * **ライブ形で始める**（先読みを窓のぶんに限る。`livePlaylistWith` の説明）。
     */
    const mode = { playlist: 'ok', segments: 'ok', liveSegmentCount: 1 }
    await mockLiveRoutes(page, mode)
    await page.goto(`${BASE_URL}${path}`, { waitUntil: 'domcontentloaded' })
    await clickPlay(page)
    await page.waitForFunction(mseAttached, undefined, { timeout: 15000 })
    // **再生を押す（`paused` を false にする）。** `<video>` に `autoPlay` は
    // 無く、`paused` の間は `currentTime` が進まないのが正常なので、押さないと
    // 判定は一度も走らない
    await page.evaluate(() => {
      document.querySelector('video').play()?.catch(() => {})
    })
    // 実際に映像が進んだことを確かめてから止める（上のコメント参照）
    let playedTo = null
    try {
      await page.waitForFunction(
        () => (document.querySelector('video')?.currentTime ?? 0) > 0.2,
        undefined,
        { timeout: 15000 },
      )
      playedTo = await readCurrentTime(page)
    } catch {
      playedTo = null
    }
    // 配信を止める（プレイリストを伸ばさない = 新しいセグメントを配らない）

    const startedAt = Date.now()
    const wanted = `profile=${downgraded.name}`
    while (Date.now() - startedAt < stallWaitMs) {
      if (playlists.some((u) => u.includes(wanted))) break
      await page.waitForTimeout(200)
    }
    const elapsedMs = Date.now() - startedAt
    const notice = await page
      .getByTestId('live-quality-downgraded')
      .textContent({ timeout: 2000 })
      .catch(() => null)
    const selected = await page.evaluate(() => {
      const el = document.querySelector('select[aria-label="画質"]')
      return el instanceof HTMLSelectElement ? el.value : null
    })
    const crossed = playlists.some((u) => u.includes(wanted))

    // 配信を復旧させる（プレイリストを伸ばす = 新しいセグメントを配る）
    mode.liveSegmentCount = segmentCount
    const servedBefore = mode.servedSegments
    const resumed = await waitForProgress(page, recoverWaitMs)
    // **「復旧した」ことを配信側でも確かめる。** `currentTime` が進んだだけでは
    // 「切替で 0 秒から読み直して、その 1 本を再生した」場合と区別できない
    // （新しいデータが届いていなければ、そもそも resume の `canplay` も来ない）
    const served = (mode.servedSegments ?? 0) - servedBefore
    log(
      `  ${path} → 実再生 t=${playedTo === null ? 'なし' : playedTo.toFixed(2)}` +
        ` / ${elapsedMs} ms で ${wanted} の要求: ${crossed ? 'あり' : 'なし'}` +
        ` / 通知: ${notice === null ? 'なし' : JSON.stringify(notice)}` +
        ` / 画質セレクタ: ${JSON.stringify(selected)}` +
        ` / 離脱ヒント: ${leaves.length} 件` +
        ` / 復旧後: 進んだ=${resumed.advanced}（t=${resumed.currentTime?.toFixed(2)},` +
        ` paused=${resumed.paused}, 配ったセグメント=${served} 本）`,
    )
    await page.close()
    return { playlists, leaves, notice, selected, crossed, elapsedMs, playedTo, resumed, served }
  }

  /**
   * segmentCount はフィクスチャに実在するセグメントの本数。
   *
   * **窓の大きさをここから導く。** 実在しない本数を載せたプレイリストを配ると
   * そのセグメントは 404 になり、hls.js は致命的なエラーで停止して
   * `readyState=0` に戻る（実際に 10 本を載せた版で踏んだ）。
   *
   * フィクスチャは 4 本 × 10 秒 = 40 秒である（`-t 40` / `-hls_time 2` だが
   * `-g` を指定していないのでキーフレームは 10 秒間隔になり、セグメントも
   * 10 秒になる。`#EXT-X-TARGETDURATION:10`）。
   */
  const segmentCount = existsSync(SEGMENTS_DIR)
    ? readdirSync(SEGMENTS_DIR).filter((f) => /^segment_\d+\.ts$/.test(f)).length
    : 0
  const segmentDurationMs = fixtureSegments()[0][0] * 1000
  log(`  フィクスチャのセグメント: ${segmentCount} 本 × ${segmentDurationMs / 1000} 秒`)

  const stallBrowser = await launchBrowser()
  try {
    // 方向 1: 自動（`?profile=` がない）→ 1 段下がり、そのまま再生が続く
    const auto = await stallCheck(stallBrowser, `/live?service=${SERVICE_ID_A}`)
    // 停滞を作る前提（実再生していたこと）が崩れていないかを見る。崩れていると
    // 以下の「再開しない」の判定が別の理由（元から止まっていた）で通ってしまう
    if (segmentCount < 4) {
      ng.push(
        `⑪ フィクスチャのセグメントが足りない（${segmentCount} 本）。` +
          ' `E2E_LIVE_REBUILD_FIXTURE=1` で作り直す（判定の前提）',
      )
    }
    if (auto.playedTo === null) {
      ng.push(
        '⑪ 停滞を作る前に実再生できていない（currentTime が 0.2 を超えない。' +
          ' フィクスチャか経路を疑う。判定の前提が崩れている）',
      )
    }
    if (auto.playlists[0]?.includes('profile=')) {
      ng.push(
        `⑪ 自動の判定になっていない（最初の要求に ?profile= が載っている: ` +
          `${JSON.stringify(auto.playlists[0] ?? null)}）`,
      )
    }
    if (!auto.crossed) {
      ng.push(
        `⑪ 停滞させても ${downgraded.name} のプレイリストを取りに行かない` +
          `（${stallWaitMs} ms 待った。最初の要求: ${JSON.stringify(auto.playlists[0] ?? null)}）`,
      )
    }
    if (auto.notice === null || !auto.notice.includes(downgraded.name)) {
      ng.push(
        '⑪ 自動で画質を下げたことが画面に出ない' +
          `（黙って画質が落ちると「汚くなった」と読める。通知: ${JSON.stringify(auto.notice)}）`,
      )
    }
    if (auto.selected !== downgraded.name) {
      ng.push(
        `⑪ 下げた後も画質セレクタの表示が下げた先と違う（${JSON.stringify(auto.selected)}、` +
          `期待 ${downgraded.name}）`,
      )
    }
    // **降格の目的は「見え続けられること」である。** セグメントを復旧させた後に
    // 映像が進んでいなければ、画質だけ下げて黒いままという状態になる
    if (auto.crossed && !auto.resumed.advanced) {
      ng.push(
        '⑪ 画質を下げた後に再生が続かない' +
          `（配信を復旧させても currentTime が ${auto.resumed.currentTime?.toFixed(2)} で` +
          ` 止まったまま。paused=${auto.resumed.paused}。切替の cleanup が load() で` +
          ' paused に戻し、誰も再生を再開していない）',
      )
    }
    // **配信側でも確かめる。** `currentTime` が 0.2 秒進んだだけでは「切替で 0 秒から
    // 読み直して 1 本を再生した」場合と区別できない（再開の `canplay` 自体は
    // 新しいデータが届かないと来ないので、両方見る）
    if (auto.resumed.advanced && auto.served === 0) {
      ng.push(
        '⑪ 画質を下げた後に再生は進んだが、配信側は 1 本も新しいセグメントを' +
          ' 配っていない（切替前のバッファを再生しただけの可能性がある）',
      )
    }
    // 離脱ヒント（= セッションを手放す合図）は送らない。画質の切替は同じ
    // セッションの別プレイリストを取るだけである（docs/frontend/live.md）。
    // **現在の配線では原理的に落ちない**（ヒントの effect の依存に `profile` が
    // 無いので、降格では再実行されない）が、依存に足す・`key` で作り直すといった
    // 回帰が来たら落ちる位置に置いておく
    if (auto.leaves.length > 0) {
      ng.push(`⑪ 画質の自動降格で離脱ヒントが飛んだ（${auto.leaves.length} 件）`)
    }

    // 方向 2: 明示選択（`?profile=hd`）→ 下げない。**要求が 1 件のままであること
    // まで見る**（`profile=sd` を探すだけでは、別の再取得が起きたことを見逃す）
    const explicit = await stallCheck(stallBrowser, `/live?service=${SERVICE_ID_A}&profile=hd`)
    if (!explicit.playlists.some((u) => u.includes('profile=hd'))) {
      ng.push(
        `⑪ 明示選択の判定になっていない（最初の要求に profile=hd が無い: ` +
          `${JSON.stringify(explicit.playlists[0] ?? null)}）`,
      )
    }
    if (explicit.crossed) {
      ng.push(
        `⑪ 利用者が明示的に選んだ画質を自動が上書きした（${downgraded.name} の` +
          ' プレイリストを取りに行った）',
      )
    }
    // **同じ URL の再取得は数えない**（hls.js は target duration ごとにプレイリストを
    // 取り直す。実測 6〜7 件）。見るのは「**他の**プロファイルを取りに行かないこと」
    if (explicit.playlists.some((u) => !u.includes('profile=hd'))) {
      ng.push(
        `⑪ 明示選択のときに別プロファイルのプレイリストを要求した（` +
          `${JSON.stringify(explicit.playlists)}）`,
      )
    }
    if (explicit.notice !== null) {
      ng.push('⑪ 明示選択のときに自動降格の通知が出た')
    }
    // 対照: 切替を挟まない側は、配信が戻れば自動で再生が続く。これが成立して
    // いなければ「復旧後の再生」の判定自体が成立していない（自動側の NG を
    // 切替のせいにできない）
    if (!explicit.resumed.advanced || explicit.served === 0) {
      ng.push(
        '⑪ 明示選択の側でも配信の復旧後に再生が続かない' +
          `（currentTime ${explicit.resumed.currentTime?.toFixed(2)}、` +
          `paused=${explicit.resumed.paused}。切替を挟まないので、これは降格とは` +
          ' 別の要因。⑪ の「復旧後の再生」の判定が成立していない）',
      )
    }
  } catch (err) {
    ng.push(`⑪ 自動降格の検証中に例外が発生した: ${err.message}`)
  } finally {
    await stallBrowser.close()
  }
}

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))
if (skipped.length > 0) {
  log('  未測定（NG ではない）:')
  skipped.forEach((s) => log('    - ' + s))
}

process.exit(ng.length === 0 ? 0 : 1)
