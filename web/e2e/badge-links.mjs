// 参照バッジの導線（issue #233 M6-5）の受け入れ判定。jsdom では測れないものだけを
// ここで見る（e2e/README.md）。
//
// 見るのは 2 点:
//   ① 予約一覧の容量不足バッジをクリックすると、行本体の詳細リンク（宛先
//      `/reservations/$site/$programId`）ではなく番組表（`/?at=...`）へ飛ぶこと
//      --- バッジが行本体の `<a>` の中に入れ子の `<a>` として置かれていると、
//      クリックの宛先が不定になり、多くのブラウザ実装では外側（詳細ページ）が
//      勝つ。この構造上の欠陥は jsdom の DOM 構造チェック（`querySelectorAll('a a')`）
//      で拾えるが、**実際にクリックしたときの遷移先**は実ブラウザでしか確認できない
//      （jsdom は Link のクリックで実際のブラウザナビゲーションを起こさない）
//   ② 遷移後、`lg` 以上の画面幅ではグリッド表示に自動で切り替わり、かつ
//      不足区間の帯（`[data-testid="capacity-band"]`）が実際に画面内（スクロール
//      済みの可視範囲）に入っていること --- スクロール位置は jsdom が原理的に
//      測れない領域（`getBoundingClientRect()` が常に 0 を返す）ので、これが
//      唯一の判定手段になる
//
// **mirakc も実チューナーも DB も要らない。** `/api/**` を `page.route` で
// ブラウザ側から丸ごと差し替える（e2e/design.mjs と同じ手）。時刻も
// `page.clock.setFixedTime` で固定する。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:badge-links
//
// **ポートが別プロセスに使われていないか確認すること。** `--strictPort` は
// 使用中なら起動自体が失敗するはずだが、複数の worktree を並行して触っている
// と別の worktree の preview（同じ 4173 等）が先に居座っていることがあり、
// その場合は自分の起動が黙って失敗し、`E2E_URL` は無関係な別ビルドを指したまま
// 判定が進んでしまう（実際にこの罠を 1 度踏んだ。別 worktree の dist を測って
// 「直る前の実装で落ちる」が常に成立するだけの壊れた判定になっていた）。
// 空いているポートを使うか、揃っているかを毎回確認する:
//
//   curl -s http://localhost:4173/ | grep -o 'assets/index-[^"]*\.js'
//   ls dist/assets/ | grep -o 'index-.*\.js'
//   # 上 2 行のファイル名が一致しなければ、そのポートは自分のビルドを配っていない
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { chromium } from 'playwright'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'

const ng = []
const log = (...a) => console.log(...a)

const SITE = 'default'
const HOUR = 3_600_000

/**
 * 時刻を固定する。`lg` 未満の画面幅では日付ジャンプの判定（DayStrip の
 * ハイライト）が「今日から何日先か」で決まるため、実行時刻に依存させない。
 */
const FIXED_NOW = new Date('2026-08-12T10:00:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

const service = {
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

// 不足区間は「今」から 30 時間後（次の次の日）に置く。ProgramGrid の既定の
// 初期スクロール位置は「今」なので、`at` を渡さずに開けば不足区間は画面外
// のままになる --- この距離が①②の判定を空虚にしない（`at` が効いていなければ
// 帯は可視範囲に入らない）。
const overageStart = nowMs + 30 * HOUR
const overageEnd = overageStart + HOUR
const overage = {
  site: SITE,
  startAt: iso(overageStart),
  endAt: iso(overageEnd),
  shortfall: 1,
  jammedTypes: ['BS'],
}

// 予約は不足区間に収まる（バッジが出る条件）。
const reservation = {
  id: 1,
  site: SITE,
  programId: 9001,
  source: 'manual',
  state: 'active',
  title: '容量バッジ導線の確認用予約',
  startAt: iso(overageStart),
  durationMs: HOUR,
  createdAt: iso(nowMs),
  updatedAt: iso(nowMs),
  skip: false,
}

/**
 * programsFor は要求された窓を 1 時間番組で埋める。空だと `ProgramGridView` が
 * `programs.length === 0` で `EmptyState` に落ち、容量帯（`CapacityBands`）を
 * 含むオーバーレイ自体が描かれない（`pages/programs.tsx` 参照）--- 番組が
 * 無いと判定②が成立しないので、窓を必ず埋める。
 */
function programsFor(startISO, endISO) {
  const start = Date.parse(startISO)
  const end = Date.parse(endISO)
  const out = []
  let t = Math.floor(start / HOUR) * HOUR
  let i = 0
  while (t < end) {
    out.push({
      programId: Math.floor(t / 1000),
      networkId: service.networkId,
      serviceId: service.serviceId,
      eventId: i + 1,
      startAt: iso(t),
      endAt: iso(t + HOUR),
      durationMs: HOUR,
      name: `番組 ${i}`,
      description: '',
      genres: [3],
      isFree: true,
    })
    t += HOUR
    i++
  }
  return out
}

/** installApiStubs は `/api/**` をすべてブラウザ側で差し替える（design.mjs と同じ手）。 */
async function installApiStubs(page) {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const p = url.pathname
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/reservations') return json([reservation])
    if (p === '/api/capacity/overages') return json([overage])
    if (p === `/api/sites/${SITE}/services`) return json([service])
    if (p === `/api/sites/${SITE}/programs`) {
      return json(
        programsFor(
          url.searchParams.get('start') ?? iso(nowMs),
          url.searchParams.get('end') ?? iso(nowMs + 24 * HOUR),
        ),
      )
    }
    if (/\/reservation$/.test(p)) {
      return route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' })
    }
    return json([])
  })
}

const browser = await chromium.launch()
// lg（64rem = 1024px）以上の幅で開く --- 判定②はグリッドの自動切替が前提
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page)

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)
log(`不足区間 : ${overage.startAt} 〜 ${overage.endAt}`)

await page.goto(URL_BASE + '/reservations', { waitUntil: 'domcontentloaded' })

// --- ① バッジをクリックすると番組表へ飛ぶ（詳細ページへは飛ばない） ---
log('\n=== ① 容量バッジのクリック先 ===')
const badge = page.getByRole('link', { name: /チューナーが不足しています/ })
await badge.waitFor({ timeout: 15000 }).catch(() => {
  ng.push('① バッジ（リンク）が見つからない')
})

if ((await badge.count()) > 0) {
  // 構造の確認: <a> の中に <a> を作っていないこと（バッジ自身が Link になった
  // ので、行本体の Link の中に置くとクリックの宛先が不定になる）。入れ子だと
  // 外側の行本体リンクの accessible name にも内側の文言が混ざって同じ名前で
  // 2 つ解決することがあるため `.first()` で明示的に絞る --- それ自体も
  // 「入れ子は良くない」ことの追加の証拠になる。
  const nestedAnchors = await page.evaluate(() => document.querySelectorAll('a a').length)
  log(`  <a> の入れ子: ${nestedAnchors} 件`)
  if (nestedAnchors > 0) ng.push(`① <a> の中に <a> が ${nestedAnchors} 件ある`)

  try {
    await badge.first().click()
    await page.waitForTimeout(600)
    const url = new URL(page.url())
    log(`  クリック後の URL: ${url.pathname}${url.search}`)

    if (url.pathname === '/reservations' || /^\/reservations\//.test(url.pathname)) {
      ng.push(
        `① バッジを押しても予約詳細（${url.pathname}）に留まっている（<a> の入れ子で外側が勝った疑い）`,
      )
    } else if (url.pathname !== '/' || !url.searchParams.has('at')) {
      ng.push(`① 番組表（/?at=...）ではなく ${url.pathname}${url.search} へ飛んだ`)
    } else {
      log('  番組表へ遷移した（期待どおり）')
    }
  } catch (err) {
    // 入れ子構造だとクリック先が複数解決してしまい Playwright の strict mode が
    // 例外を投げることがある。これも「入れ子にしてはいけない」ことの実害の 1 つ
    // なので、握り潰さず NG として記録する。
    ng.push(`① バッジのクリックで例外: ${String(err.message).split('\n')[0]}`)
  }
} else {
  ng.push('① バッジが無いため②の判定に進めない')
}

// --- ② lg 以上ではグリッドへ自動で切り替わり、不足区間の帯が可視範囲に入る ---
log('\n=== ② グリッドの自動切替 + 帯のスクロール位置 ===')
const grid = page.locator('[data-testid="program-grid"]')
await grid.waitFor({ timeout: 15000 }).catch(() => {
  ng.push('② lg 以上なのにグリッドへ自動で切り替わらない')
})

if ((await grid.count()) > 0) {
  const band = page.locator(`[data-testid="capacity-band"][data-start-at="${overage.startAt}"]`)
  await band.waitFor({ timeout: 5000 }).catch(() => {
    ng.push('② 不足区間の帯（capacity-band）自体が見つからない')
  })

  if ((await band.count()) > 0) {
    const gridBox = await grid.boundingBox()
    const bandBox = await band.boundingBox()
    log(`  グリッドの可視範囲: top=${Math.round(gridBox.y)} bottom=${Math.round(gridBox.y + gridBox.height)}`)
    log(`  帯の位置          : top=${Math.round(bandBox.y)} bottom=${Math.round(bandBox.y + bandBox.height)}`)

    const bandVisible = bandBox.y < gridBox.y + gridBox.height && bandBox.y + bandBox.height > gridBox.y
    if (!bandVisible) {
      ng.push(
        `② 不足区間の帯がグリッドの可視範囲外（帯 top=${Math.round(bandBox.y)}〜${Math.round(bandBox.y + bandBox.height)}, 可視範囲 top=${Math.round(gridBox.y)}〜${Math.round(gridBox.y + gridBox.height)}）`,
      )
    } else {
      log('  帯が可視範囲に入っている（期待どおり）')
    }
  }
} else {
  log('  （グリッドが出ていないため帯の可視性は判定不能）')
}

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
