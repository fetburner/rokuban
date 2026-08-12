// 参照バッジの導線（issue #233 M6-5）の受け入れ判定。jsdom では測れないものだけを
// ここで見る（e2e/README.md）。
//
// 見るのは:
//   ⓪ 前提条件 --- 配っている bundle が dist/ の現物と一致するか（他の判定は
//      これが崩れているだけで無意味になるので最初に見る。下記のコメント参照）
//   ① 予約一覧の容量不足バッジをクリックすると、行本体の詳細リンク（宛先
//      `/reservations/$site/$programId`）ではなく番組表（`/?at=...`）へ飛ぶこと
//      --- バッジが行本体の `<a>` の中に入れ子の `<a>` として置かれていると、
//      クリックの宛先が不定になり、多くのブラウザ実装では外側（詳細ページ）が
//      勝つ。この構造上の欠陥は jsdom の DOM 構造チェック（`querySelectorAll('a a')`）
//      で拾えるが、**実際にクリックしたときの遷移先**は実ブラウザでしか確認できない
//      （jsdom は Link のクリックで実際のブラウザナビゲーションを起こさない）
//   ② 遷移後、`lg` 以上の画面幅ではグリッド表示に自動で切り替わり、かつ
//      不足区間の帯（`[data-testid="capacity-band"]`）の上辺が実際に画面内
//      （スクロール済みの可視範囲）に入っていること --- スクロール位置は jsdom が
//      原理的に測れない領域（`getBoundingClientRect()` が常に 0 を返す）ので、
//      これが唯一の判定手段になる
//   ③ `lg` 未満（グリッドが出ずリスト表示のまま）でもクリックが機能し、
//      エラーにならないこと --- ①②は 1280px でしか開いておらず、この経路を
//      一度も通さないと実装のバグがそこに隠れていても検出できない
//
// **mirakc も実チューナーも DB も要らない。** `/api/**` を `page.route` で
// ブラウザ側から丸ごと差し替える（e2e/design.mjs と同じ手）。時刻も
// `page.clock.setFixedTime` で固定する。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:badge-links
//
// **起動したら、配っている bundle が `dist/` の現物と一致するかをこのスクリプト
// 自身が最初に確認する（`verifyBundleMatches` / NG "0" 番）。** `--strictPort` は
// 使用中なら起動自体を失敗させるはずだが、複数の worktree を並行して触っている
// と別の worktree の preview（同じ 4173 等）が先に居座っていることがあり、その
// 場合は自分の起動が黙って失敗し、`E2E_URL` は無関係な別ビルドを指したまま
// ①②の判定だけが進んでしまう（実際にこの罠を 1 度踏み、別 worktree の dist を
// 測って「直る前の実装で落ちる」が常に成立するだけの壊れた判定になっていた）。
// README の手順は変わらないが、**手で確認する代わりにこのスクリプトが毎回
// 自動で確認し、不一致なら他の判定をせず即 exit 1 する**（人がチェックを
// 忘れても事故を再現しない形にする）。
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'

const ng = []
const log = (...a) => console.log(...a)

/**
 * verifyBundleMatches は `URL_BASE` が実際に配っている JS bundle のファイル名
 * （`index-<hash>.js`）と、ローカルの `dist/assets/` にある現物のファイル名を
 * 比較する。
 *
 * `page.route` で `/api/**` を丸ごと差し替えるこの判定は、古い（無関係な）
 * ビルドを配っているサーバーに対しても静かに動いてしまう ---
 * サーバーが何のバイナリ・dist を配っているかはこの判定の関心外だからこそ、
 * 一致確認をどこかで能動的にやる必要がある。ファイル名にコンテンツハッシュが
 * 入っているため、内容が違えばファイル名も必ず違う（vite のデフォルト）。
 *
 * `dist/assets/` はこのスクリプトを `pnpm e2e:badge-links`（`web/` がカレント
 * ディレクトリ）で実行することを前提に相対パスで読む。
 */
function verifyBundleMatches(servedHtml) {
  const servedMatch = /assets\/(index-[^"]+\.js)/.exec(servedHtml)
  const served = servedMatch?.[1]
  const distDir = path.join(process.cwd(), 'dist', 'assets')
  let local
  try {
    local = readdirSync(distDir).find((f) => /^index-.*\.js$/.test(f))
  } catch {
    local = undefined
  }
  return { served, local, matches: served !== undefined && served === local }
}

const SITE = 'default'
const HOUR = 3_600_000

/**
 * 時刻を固定する。`lg` 未満の画面幅では日付ジャンプの判定（DayStrip の
 * ハイライト）が「今日から何日先か」で決まるため、実行時刻に依存させない。
 *
 * **時ちょうどにしない。** `dayOrigin(0)` は「今を分・秒で切り捨てた時刻」を
 * day 0 の軸の起点にするため、時ちょうどだと「軸の先頭（top=0）」と「現在時刻
 * の位置」が偶然一致してしまい、②'（「今日」へ戻したときに現在時刻の位置へ
 * 正しくスクロールし直すこと）の判定が「たまたま範囲外にフォールバックして
 * top に落ちた」だけでも通ってしまう（実際にこれで見かけ上パスする回帰を
 * 1 度作ってしまった）。
 */
const FIXED_NOW = new Date('2026-08-12T10:50:00+09:00')
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
    // ProgramRow は未予約の番組に常に ProgramOverlapWarning を出す（展開しなくても
    // 見える位置。issue #24 M2-8）ので、リスト表示（③）ではここが必ず呼ばれる。
    // 形は `{ count, reservations }`（design.mjs と同じ）。catch-all の `[]` を
    // 返すと `overlaps.reservations.map` が `undefined.map` になって
    // 落ちる（実際に踏んだ。実装のバグではなくこのフィクスチャの不足だった ---
    // グリッド（①②が通る経路）はセルの中に ProgramRow を作り込まないため、
    // ①②の間はこの穴が一度も踏まれず気付けなかった。nit 6 の指摘どおり、リスト
    // 表示の経路を通す③を足して初めて表面化した）。
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    return json([])
  })
}

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)
log(`不足区間 : ${overage.startAt} 〜 ${overage.endAt}`)

// --- ⓪ 配っている bundle が dist/ の現物と一致するか（他の判定より先に見る） ---
// `/api/**` は丸ごと差し替えるので、無関係なサーバー（古いビルド・別 worktree の
// preview）が答えていても①②はそれらしく動いてしまう。一致しないなら以降の
// 判定に意味が無いので、ここで打ち切る。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
const rootHtml = await fetch(URL_BASE + '/').then((r) => r.text())
const bundleCheck = verifyBundleMatches(rootHtml)
log(`  配っている bundle: ${bundleCheck.served ?? '(取得できない)'}`)
log(`  dist/assets/     : ${bundleCheck.local ?? '(見つからない。web/ で実行しているか確認)'}`)
if (!bundleCheck.matches) {
  ng.push(
    `⓪ ${URL_BASE} が配っている bundle（${bundleCheck.served ?? '不明'}）が dist/assets/ の現物（${bundleCheck.local ?? '不明'}）と一致しない --- 別プロセス・古いビルドを測っている可能性が高いので、これ以降の判定を打ち切る`,
  )
  log('\n=== 結果 ===')
  ng.forEach((f) => log('  NG: ' + f))
  process.exit(1)
}
log('  一致（このサーバーは自分のビルドを配っている）')

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
    } else if (url.pathname !== '/') {
      ng.push(`① 番組表（/）ではなく ${url.pathname}${url.search} へ飛んだ`)
    } else {
      // `at` は URL から消費・削除しない方針にした（`pages/programs.tsx` の
      // コメント参照。素朴に消す実装は初回スクロールとの競合で退行した）ので、
      // `?at=...` が残ったままなのが正しい。「at が現在地を離れたら効かなく
      // なる」ことは②'（「今日」へ戻すと now へスクロールし直す）で確認する。
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

    // **上辺が可視範囲に入っていること**まで見る（1px でも重なれば合格、では
    // ±数時間ずれても通る余地が残る。スクロール位置が「その時間帯」に本当に
    // 合っているかを主張するには、帯の始点が画面内にあることを要求する）。
    const bandTopVisible = bandBox.y >= gridBox.y && bandBox.y <= gridBox.y + gridBox.height
    if (!bandTopVisible) {
      ng.push(
        `② 不足区間の帯の上辺がグリッドの可視範囲外（帯 top=${Math.round(bandBox.y)}, 可視範囲 top=${Math.round(gridBox.y)}〜${Math.round(gridBox.y + gridBox.height)}）`,
      )
    } else {
      log('  帯の上辺が可視範囲に入っている（期待どおり）')
    }

    // --- ②': 「今日」へ戻すと、古い at ではなく「今」の位置へスクロールし直す ---
    // （レビュー nit 4）。`at` は URL から消さない方式にしたため、「今日」ボタンを
    // 押した後も古い実装のままなら「today の軸には at が範囲外」で単純に先頭
    // （scrollTop=0）へ落ちるだけになる。**可視範囲に「今」が入っているかどうか
    // では見分けられない**（day 0 の軸の起点は「今を時で切り捨てた時刻」なので、
    // 「今」は軸の先頭からたかだか 1 時間ぶんの位置にしかならず、先頭（0）へ
    // フォールバックしても大きな viewport では「今」がついでに視界に入って
    // しまい、正しいスクロールと区別が付かない --- 実際にこれで見かけ上パスする
    // 誤った判定を 1 度書いてしまった）。**scrollTop の実測値**を直接見て、
    // 「先頭に落ちただけ」（0）と「『今』の位置へ実際にスクロールした」
    // （0 より明確に大きい）を区別する。
    log("\n=== ②' 「今日」へ戻すと now の位置へスクロールし直す（at には戻らない） ===")
    const todayButton = page.locator('[role="group"][aria-label="日付"] button').first()
    await todayButton.click()
    await page.waitForTimeout(800)

    const nowLine = page.locator('[data-testid="program-grid-now-line"]')
    await nowLine.waitFor({ timeout: 5000 }).catch(() => {
      ng.push('②\' 「今日」へ戻した後に現在時刻の線が見つからない')
    })
    if ((await nowLine.count()) > 0) {
      const scrollTopAfterToday = await grid.evaluate((el) => el.scrollTop)
      log(`  「今日」後の scrollTop: ${scrollTopAfterToday}px`)
      // FIXED_NOW を時ちょうどからずらしてあるので、正しく「今」へスクロール
      // していれば scrollTop は明確に 0 より大きくなる（「先頭に落ちただけ」
      // なら厳密に 0）。しきい値は誤差を見込んで緩めに取る。
      if (scrollTopAfterToday < 10) {
        ng.push(
          `②' 「今日」を押しても scrollTop が ${scrollTopAfterToday}px のまま（先頭に落ちただけで「今」の位置へスクロールしていない疑い）`,
        )
      } else {
        log('  現在時刻の位置へスクロールし直されている（古い at には戻っていない。期待どおり）')
      }
    }
  }
} else {
  log('  （グリッドが出ていないため帯の可視性は判定不能）')
}

// --- ③ lg 未満（グリッドが出ない = リスト表示のまま）でもクリックが機能する ---
// ①②は 1280px（`lg` 以上）でしか開いていない。「①②以外の経路が一度も通って
// いない」状態は、そこに実装のバグが隠れていても検出できない
// （実際に別経路の手動プローブで無関係な例外を見た。実装由来ではないと確認済みだが、
//  以後同じ死角を残さないためにここに機械判定として足す）。
log('\n=== ③ lg 未満（リスト表示のまま）でもクリックが機能する ===')
const narrowContext = await browser.newContext({
  viewport: { width: 375, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const narrowPage = await narrowContext.newPage()
await narrowPage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(narrowPage)
await narrowPage.goto(URL_BASE + '/reservations', { waitUntil: 'domcontentloaded' })

const narrowBadge = narrowPage.getByRole('link', { name: /チューナーが不足しています/ })
await narrowBadge.waitFor({ timeout: 15000 }).catch(() => {
  ng.push('③ 375px でバッジ（リンク）が見つからない')
})

if ((await narrowBadge.count()) > 0) {
  try {
    await narrowBadge.first().click()
    await narrowPage.waitForTimeout(600)
    const narrowUrl = new URL(narrowPage.url())
    log(`  クリック後の URL: ${narrowUrl.pathname}${narrowUrl.search}`)

    const bodyText = await narrowPage.evaluate(() => document.body.textContent ?? '')
    if (/Something went wrong/.test(bodyText)) {
      ng.push('③ 375px で番組表へ飛んだ直後にエラー境界に落ちた')
    } else if (narrowUrl.pathname !== '/') {
      ng.push(`③ 375px でも番組表（/）へ飛ぶはずが ${narrowUrl.pathname}${narrowUrl.search} だった`)
    } else {
      const narrowGrid = narrowPage.locator('[data-testid="program-grid"]')
      if ((await narrowGrid.count()) > 0) {
        ng.push('③ lg 未満なのにグリッドが出ている（表示形式の出し分けが壊れている）')
      } else {
        log('  番組表（リスト表示）へ遷移し、グリッドは出ていない（期待どおり）')

        // グリッドが無くても、at が指す日への日付ジャンプは効いているはず
        // （不足区間は「今」から 30 時間後なので、ハイライトは today = index 0
        // ではないはず）
        const dayButtons = narrowPage.locator('[role="group"][aria-label="日付"] button')
        const currentIndex = await dayButtons
          .evaluateAll((els) => els.findIndex((el) => el.getAttribute('aria-current') === 'date'))
        log(`  ハイライトされている日の添字: ${currentIndex}（0 なら今日のまま）`)
        if (currentIndex <= 0) {
          ng.push('③ リスト表示でも at の日付ジャンプが効くはずが、今日のままだった')
        } else {
          log('  日付ジャンプが効いている（期待どおり）')
        }
      }
    }
  } catch (err) {
    // ① と同じ理由（`className="relative"` を外す等でバッジが押せなくなる
    // 回帰があると、クリック待ちが未捕捉の TimeoutError で落ち、`=== 結果 ===`
    // の要約が一度も印字されないままスタックトレースだけが出る。exit code
    // 自体は 1 のまま変わらないが、診断のしやすさを①と揃える。
    ng.push(`③ バッジのクリックで例外: ${String(err.message).split('\n')[0]}`)
  }
}
await narrowContext.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
