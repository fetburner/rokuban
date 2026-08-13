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
//   - ffmpeg が PATH にある（固定 HLS フィクスチャの生成に使う。生成は
//     初回だけで、以降は `os.tmpdir()` にキャッシュされる）
//
// 詳しい手順・準備の SQL 例は docs/runbook/live.md §②。使い方だけ：
//   E2E_LIVE_SERVICE_A=9001 E2E_LIVE_SERVICE_B=9002 pnpm e2e:live
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { chromium, webkit } from 'playwright'

const BASE_URL = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = process.env.E2E_LIVE_SITE ?? 'default'
const SERVICE_A = process.env.E2E_LIVE_SERVICE_A ?? '9001'
const SERVICE_B = process.env.E2E_LIVE_SERVICE_B ?? '9002'

const FIXTURE_DIR = path.join(os.tmpdir(), 'rokuban-e2e-live-fixture')
const SEGMENTS_DIR = path.join(FIXTURE_DIR, 'segments')
const PLAYLIST_PATH = path.join(FIXTURE_DIR, 'playlist.m3u8')

const ng = []
const skipped = []
const log = (...a) => console.log(...a)

/**
 * composedServiceId は SI の serviceId を mirakc 合成 id
 * （`networkId * 100000 + serviceId`。`web/src/lib/live.ts` の
 * `mirakcServiceId` と同じ式）に変換する。プレイリスト / セグメントの URL に
 * 載るのはこちらなので、要求の照合にはこの値が要る（issue #208）。
 *
 * `networkId` は `GET /api/sites/{site}/services` から引く --- 環境変数を
 * 増やすと、SI の id と合成 id のどちらを渡すのかを実行者が判断することになり、
 * 間違えても「タイムアウトした」としか見えない。
 */
async function composedServiceId(serviceId) {
  const res = await fetch(`${BASE_URL}/api/sites/${SITE}/services`)
  if (!res.ok) {
    throw new Error(`GET /api/sites/${SITE}/services が ${res.status}`)
  }
  const services = await res.json()
  const found = services.find((s) => String(s.serviceId) === String(serviceId))
  if (found === undefined) {
    throw new Error(
      `serviceId=${serviceId} が site=${SITE} のサービス一覧に無い` +
        `（E2E_LIVE_SERVICE_A / _B を実在する serviceId にする。docs/runbook/live.md §②）`,
    )
  }
  return found.networkId * 100_000 + found.serviceId
}

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

  await page.route('**/live/playlist.m3u8*', async (route) => {
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
      body: readFileSync(PLAYLIST_PATH),
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
 *
 * この判定が本来見たいのは「タップだけではプレイリスト/セグメント要求が飛ばない」
 * こと自体であり、実データ（H.264/AAC）や実再生は要らない --- ①〜⑦と違って
 * ffmpeg フィクスチャに依存せず、bundled Chromium だけで常に測れる。
 * `web/e2e/README.md`「判定を足すときの規律」どおり、この判定を足す前の実装
 * （チャンネルをタップした瞬間に probe する版）で実際に落ちることを確認済み
 * （PR 本文の変異リスト参照）。
 */
async function runConsentCheck() {
  const browser = await chromium.launch()
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

    await page.goto(`${BASE_URL}/live?serviceId=${SERVICE_A}`, { waitUntil: 'networkidle' })

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

    const playButton = page.getByRole('button', { name: /再生/ })
    if ((await playButton.count()) === 0) {
      ng.push('⓪ 「再生」ボタンが見つからない')
      return
    }

    requestLog.length = 0
    await playButton.click()
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
    log(`  再生ボタン押下後にプレイリスト要求が飛んだ: ${fired ? 'YES' : 'NO'}`)
    if (!fired) ng.push('⓪ 「再生」ボタンを押してもプレイリスト要求が飛ばない')
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
if (!hasFixture) {
  log('ffmpeg が見つからないため、フィクスチャを生成できない。①②③④⑤ をすべて測れないとして報告する')
  skipped.push('ffmpeg が無いため①②③④⑤すべて未測定')
}

if (hasFixture) {
  const browser = await chromium.launch()
  // ①②④⑤ の途中で何が例外を投げても NG として報告し、後続の ③（別ブラウザ）を
  // 続行する（クラッシュさせない）。実際に「壊してみる」検証で、native HLS
  // 判定を誤らせると `waitForFunction` が例外で落ちることを確認した経緯があるため
  try {
    await runChromiumChecks(browser)
  } catch (err) {
    ng.push(`①②④⑤ の検証中に例外が発生した: ${err.message}`)
  } finally {
    await browser.close()
  }
}

/** runChromiumChecks は bundled Chromium で測れる①②④⑤を実行する。 */
async function runChromiumChecks(browser) {
  const page = await browser.newPage({ viewport: { width: 960, height: 640 } })

  const requestLog = []
  page.on('request', (req) => requestLog.push(req.url()))

  const mode = { playlist: 'ok' }
  await mockLiveRoutes(page, mode)

  await page.goto(`${BASE_URL}/live?serviceId=${SERVICE_A}`, { waitUntil: 'networkidle' })
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
  const segmentsRequested = ([site, serviceId]) =>
    window.performance
      .getEntriesByType('resource')
      .some((r) => r.name.includes(`/sites/${site}/services/${serviceId}/live/segments/`))

  // **セグメントの URL に載るのは SI の serviceId ではなく mirakc 合成 id**
  // （`lib/live.ts` の `mirakcServiceId`。issue #208）。ここを SI の id で
  // 照合すると、`network_id` が 0 でない限り一致しない --- 実 EPG（network_id
  // 32200 等）でも runbook の投入例（network_id 1）でも一致せず、この待機が
  // 必ずタイムアウトする（#208 以降ずっとそうなっていた。この修正で解消）
  const composedA = await composedServiceId(SERVICE_A)
  const composedB = await composedServiceId(SERVICE_B)

  await page.waitForFunction(segmentsRequested, [SITE, composedA], { timeout: 10000 })
  const requestsBeforeSwitchCount = requestLog.filter((u) =>
    u.includes(`/services/${composedA}/live/segments/`),
  ).length
  log(`  切替前の A 向けセグメント要求数: ${requestsBeforeSwitchCount}`)

  await page
    .locator(`nav[aria-label="チャンネル一覧"] a[href*="serviceId=${SERVICE_B}"]`)
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
  await page.waitForFunction(segmentsRequested, [SITE, composedB], { timeout: 10000 })
  // 切り替え後もしばらく要求が続くかもしれない旧チャンネルの要求を数える余地を
  // 与える（hls.js の非同期な内部タイマーが 1 フレームだけ遅れて発火する
  // ケースを見逃さないため）
  await page.waitForTimeout(1500)
  const staleRequestsAfterSwitch = requestLog.filter((u) =>
    u.includes(`/services/${composedA}/live/segments/`),
  )
  log(`  切替後の A 向けセグメント要求数: ${staleRequestsAfterSwitch.length}`)
  if (staleRequestsAfterSwitch.length > 0) {
    ng.push(
      `④ チャンネル切替後も旧チャンネル（${SERVICE_A}）へのセグメント要求が` +
        `${staleRequestsAfterSwitch.length} 件続いた（cleanup が破棄していない）`,
    )
  }

  // --- ⑤ 503（本文つき）でエラー文言が出る。再読み込みで復帰する ---
  log('\n=== ⑤ 503 エラー表示 → 再読み込みで復帰 ===')
  mode.playlist = 'error'
  mode.playlistErrorBody = 'too many concurrent live sessions on this process'
  await page.goto(`${BASE_URL}/live?serviceId=${SERVICE_A}`, { waitUntil: 'networkidle' })
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
}

if (hasFixture) {
  // --- ③ 実際に再生が進む（実 Chrome のみ。bundled Chromium は H.264/AAC 非対応） ---
  log('\n=== ③ 実再生（video.currentTime が進む） ===')
  let chromeBrowser
  try {
    chromeBrowser = await chromium.launch({ channel: 'chrome' })
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
      await chromePage.goto(`${BASE_URL}/live?serviceId=${SERVICE_A}`, { waitUntil: 'networkidle' })
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
  const webkitBrowser = await webkit.launch()
  try {
    const page = await webkitBrowser.newPage({ viewport: { width: 960, height: 640 } })
    const requestLog = []
    page.on('request', (req) => requestLog.push(req.url()))
    await mockLiveRoutes(page, { playlist: 'ok' })
    await page.goto(`${BASE_URL}/live?serviceId=${SERVICE_A}`, { waitUntil: 'networkidle' })
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
  log('\n=== ⑦ ネイティブ経路のメディア失敗（WebKit） ===')
  for (const [label, segments, timeout] of [
    ['セグメントが 404', '404', 20000],
    ['セグメントが応答しない', 'hang', 30000],
  ]) {
    const browser = await webkit.launch()
    try {
      const page = await browser.newPage({ viewport: { width: 960, height: 640 } })
      await mockLiveRoutes(page, { playlist: 'ok', segments })
      await page.goto(`${BASE_URL}/live?serviceId=${SERVICE_A}`, { waitUntil: 'domcontentloaded' })
      await clickPlay(page)

      let shown = false
      try {
        await page.getByText('ライブ視聴でエラーが発生しました。').waitFor({ timeout })
        await page.getByRole('button', { name: '再読み込み' }).waitFor({ timeout: 5000 })
        shown = true
      } catch {
        shown = false
      }
      const detail = await page.evaluate(() => {
        const v = document.querySelector('video')
        return {
          text: document.body.innerText.replace(/\s+/g, ' ').slice(0, 160),
          readyState: v?.readyState ?? -1,
          err: v?.error ? v.error.code : null,
        }
      })
      log(`  ${label}: エラー表示 + 再読み込み = ${shown ? 'YES' : 'NO'}`)
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

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))
if (skipped.length > 0) {
  log('  未測定（NG ではない）:')
  skipped.forEach((s) => log('    - ' + s))
}

process.exit(ng.length === 0 ? 0 : 1)
