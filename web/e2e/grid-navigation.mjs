// 番組表グリッドの空間的なキーボードナビゲーションの受け入れ判定。
//
// jsdom ではフォーカスとスクロール位置、仮想化後の DOM を同時に測れないため、
// 実ブラウザで次の 4 点を見る:
//   ① セルにフォーカスして ArrowRight を押すと、開始時刻を含む隣列のセルへ移る
//   ② セルにフォーカスして Alt+ArrowLeft を押しても、空間移動は起きない
//      （修飾キー付きの矢印はブラウザ標準の挙動 --- 「戻る」等 --- に譲る）。
//      ②は③がスクロールで nearB を仮想化の外へ追い出す前、①の直後に行う
//   ③ 最初は仮想化で DOM に無い同列の遠い番組へ ArrowDown を押すと、スクロール後に
//      そのセルへフォーカスが移る
//   ④ viewport より高い番組（12 時間）へ ArrowDown で移ると、移動先セルの上端が
//      sticky header の裏（画面外だけでなく、header 行の下端より上）へ隠れない
//   ⑤ 選択ダイアログ（main #753）が開いている間は矢印キーがグリッドへ届かず、
//      閉じるとフォーカスが元のセルへ戻り、矢印移動を再開できる
//   ⑥ 既定でない縮尺（main #758/#724 のズーム。480px/時）でも矢印移動と
//      viewport より高い番組への追従が壊れない
//
// 直す前はそれぞれ次のとおり落ちる（実測。実測していない挙動は書かない）:
//   ①③ 矢印キーのハンドラ自体が無かった実装（本 PR で新規に足した機能）では、
//      ArrowRight を押してもフォーカスは 726001（nearA）のまま動かない。ArrowDown も
//      同様にフォーカスは動かず、scrollTop だけ 0px → 40px とわずかに進む
//      （フォーカス中の領域 `role="region"` へのネイティブなキー操作によるスクロール
//      で、矢印移動ではない）
//   ② 修飾キーのガード（`event.ctrlKey || ... || event.shiftKey` の早期 return）を
//      外すと、nearB（726002）にフォーカスした状態で Alt+ArrowLeft を押すだけで
//      フォーカスが隣列の nearA（726001）へ移る
//   ④ clamp（`Math.min(programBottomPx - clientHeight, timeToPx(axis, startMs))`）を
//      外し旧式の `Math.max(0, programBottomPx - clientHeight)` に戻すと、移動先
//      セル（tallA）の rect.top が -540px まで画面の上へ出る
//
// 各判定は `.catch()` で失敗を `ng` に積むだけで、素の `waitFor` / `waitForFunction`
// の timeout で throw させない --- 直前の判定が失敗してもブラウザを閉じて
// `=== 結果 ===` を出すところまでは必ず到達する（web/e2e/badge-links.mjs 等と同じ規約）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:grid-navigation

import { ListProgramsResponseItem, ListServicesResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const FIXED_NOW = new Date('2026-08-12T09:00:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()
const ng = []

const serviceA = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: '左チャンネル',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

const serviceB = {
  id: 3273601032,
  networkId: 32736,
  serviceId: 1032,
  name: '右チャンネル',
  channelType: 'GR',
  channel: '26',
  remoteControlKeyId: 2,
  hasLogoData: false,
  hasPrograms: true,
}

const makeProgram = (programId, service, startMs, durationMs, name) => ({
  programId,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: programId,
  startAt: iso(startMs),
  endAt: iso(startMs + durationMs),
  durationMs,
  name,
  description: '',
  genres: [0],
  isFree: true,
})

const nearA = makeProgram(726001, serviceA, nowMs + 15 * 60_000, 30 * 60_000, '近くの左番組')
// nearA の開始時刻（09:15）を含むので、ArrowRight の優先規則でこれを選ぶ。
const nearB = makeProgram(726002, serviceB, nowMs, 60 * 60_000, '近くの右番組')
// 初期の可視窓から意図的に外す。ArrowDown はここまでスクロールしてからフォーカスする。
const farA = makeProgram(726003, serviceA, nowMs + 10 * 60 * 60_000, 30 * 60_000, '遠くの左番組')
// farA（19:00-19:30）の次に同列で始まる、viewport より高い番組（12 時間 = 1440px。
// pxPerHour は lib/programs-grid-scale-storage.ts の defaultGridPxPerHour = 120。
// この e2e は新しい browser context を毎回作るので localStorage は常に空
// --- `loadProgramsGridPxPerHour() ?? defaultGridPxPerHour` が既定縮尺に落ちる
// ことを前提にしている）。④は farA から ArrowDown でこれへ移り、移動先セルの
// 上端が隠れないことを見る。
const tallA = makeProgram(726004, serviceA, nowMs + 11 * 60 * 60_000, 12 * 60 * 60_000, '長時間の左番組')
const programs = [nearA, nearB, farA, tallA]

async function apiHandler({ path: p, json }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false })
  if (p === '/api/reservations') return json([])
  if (p === '/api/capacity/overages') return json([])
  if (p === '/api/encode-profiles') return json([])
  if (p === `/api/sites/${SITE}/services`) return json([serviceA, serviceB])
  if (p === `/api/sites/${SITE}/programs`) return json(programs)
  if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
  return json([])
}

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['serviceA', ListServicesResponseItem, serviceA],
    ['serviceB', ListServicesResponseItem, serviceB],
    ...programs.map((program) => [`program ${program.programId}`, ListProgramsResponseItem, program]),
  ],
  ng,
)

await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)
await page.goto(`${BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })

const grid = page.getByTestId('program-grid')
await grid.waitFor({ timeout: 15000 }).catch(() => {
  ng.push('番組表グリッドが描画されない')
})

const cellFor = (program) =>
  page.locator(
    `[data-testid="program-grid-cell"][data-site="${SITE}"][data-program-id="${program.programId}"]`,
  )

const nearACell = cellFor(nearA)
const nearBCell = cellFor(nearB)
const farACell = cellFor(farA)
const tallACell = cellFor(tallA)
await nearACell.waitFor({ timeout: 10000 }).catch(() => {
  ng.push('近くの左番組のセルが描画されない')
})
await nearBCell.waitFor({ timeout: 10000 }).catch(() => {
  ng.push('近くの右番組のセルが描画されない')
})

// 遠い番組が最初から DOM にあると、③が仮想化境界を通らないので判定を止める。
if ((await farACell.count()) !== 0) {
  ng.push('③ 遠い番組が初期表示から DOM にあり、仮想化後の移動を検証できない')
}

log('\n=== ① ArrowRight: 開始時刻を含む隣列へ移動 ===')
if ((await nearACell.count()) > 0) {
  await nearACell.focus()
  await nearACell.press('ArrowRight')
  const rightFocus = await page.evaluate(() => {
    const active = document.activeElement
    return {
      programId: active?.getAttribute('data-program-id'),
      site: active?.getAttribute('data-site'),
    }
  })
  log(`  フォーカス先: ${JSON.stringify(rightFocus)}`)
  if (rightFocus.programId !== String(nearB.programId) || rightFocus.site !== SITE) {
    ng.push(`① ArrowRight 後のフォーカスが隣列へ移らない（${JSON.stringify(rightFocus)}）`)
  }
} else {
  ng.push('① 近くの左番組のセルが無いため検証できない')
}

log('\n=== ② Alt+ArrowLeft: 修飾キー付き矢印は空間移動を起こさない ===')
if ((await nearBCell.count()) > 0) {
  await nearBCell.focus()
  await nearBCell.press('Alt+ArrowLeft')
  const altFocus = await page.evaluate(() => document.activeElement?.getAttribute('data-program-id'))
  log(`  フォーカス先の programId: ${altFocus}`)
  // 修飾キー無しの ArrowLeft なら、nearB の開始時刻（09:00）に最も近い隣列の
  // 番組（nearA, 09:15）へ移る。Alt+ArrowLeft でこれが起きればガード漏れ。
  if (altFocus !== String(nearB.programId)) {
    ng.push(`② Alt+ArrowLeft でフォーカスが移動する（${altFocus} へ）`)
  }
} else {
  ng.push('② 近くの右番組のセルが無いため検証できない')
}

log('\n=== ③ ArrowDown: 仮想化された遠い番組へ追従 ===')
if ((await nearACell.count()) > 0) {
  await nearACell.focus()
  // focus 自身がスクロールを起こしうる（`locator.focus()` は `preventScroll` では
  // ない）ため、矢印移動の成果と混ざらないよう scrollBefore は focus の後で読む。
  const scrollBefore = await grid.evaluate((element) => element.scrollTop)
  await nearACell.press('ArrowDown')
  await page
    .waitForFunction(
      (programId) => document.activeElement?.getAttribute('data-program-id') === String(programId),
      farA.programId,
      { timeout: 10000 },
    )
    .catch(() => {
      ng.push('③ ArrowDown 後のフォーカスが遠い同列へ移らない（timeout）')
    })
  const scrollAfter = await grid.evaluate((element) => element.scrollTop)
  const downFocus = await page.evaluate(() => ({
    programId: document.activeElement?.getAttribute('data-program-id'),
    site: document.activeElement?.getAttribute('data-site'),
  }))
  log(`  scrollTop: ${scrollBefore}px → ${scrollAfter}px / フォーカス先: ${JSON.stringify(downFocus)}`)
  if (downFocus.programId !== String(farA.programId) || downFocus.site !== SITE) {
    ng.push(`③ ArrowDown 後のフォーカスが遠い同列へ移らない（${JSON.stringify(downFocus)}）`)
  }
  if (scrollAfter <= scrollBefore) {
    ng.push(`③ 目的セルへ追従してスクロールしない（${scrollBefore}px → ${scrollAfter}px）`)
  }
} else {
  ng.push('③ 近くの左番組のセルが無いため検証できない')
}

log('\n=== ④ ArrowDown: viewport より高い番組へ移っても上端が隠れない ===')
let movedToTallA = false
const farAFocused = await farACell
  .focus({ timeout: 10000 })
  .then(() => true)
  .catch(() => {
    ng.push('④ 長時間番組の前段（遠くの左番組）のセルにフォーカスできない')
    return false
  })
if (farAFocused) {
  await farACell.press('ArrowDown')
  await page
    .waitForFunction(
      (programId) => document.activeElement?.getAttribute('data-program-id') === String(programId),
      tallA.programId,
      { timeout: 10000 },
    )
    .catch(() => {
      ng.push('④ ArrowDown 後のフォーカスが長時間番組へ移らない（timeout）')
    })
  const tallFocus = await page.evaluate(() => ({
    programId: document.activeElement?.getAttribute('data-program-id'),
    site: document.activeElement?.getAttribute('data-site'),
  }))
  log(`  フォーカス先: ${JSON.stringify(tallFocus)}`)
  if (tallFocus.programId !== String(tallA.programId) || tallFocus.site !== SITE) {
    ng.push(`④ ArrowDown 後のフォーカスが長時間番組へ移らない（${JSON.stringify(tallFocus)}）`)
  } else {
    // 「画面外でない」（top >= 0）だけでは、sticky header の裏（0 〜 header 下端）に
    // 隠れる壊れ方を見逃す。sticky なヘッダ行（program-grid-header-cell）自身の
    // rect.bottom を実測し、それを移動先セルの rect.top の下限として使う ---
    // `headerHeightPx`（program-grid.tsx）をこちらにリテラルで写すと権威が割れる。
    const headerBottom = await page
      .getByTestId('program-grid-header-cell')
      .first()
      .evaluate((element) => element.getBoundingClientRect().bottom)
    const rectTop = await tallACell.evaluate((element) => element.getBoundingClientRect().top)
    log(`  header 行の下端: ${headerBottom}px / 移動先セルの rect.top: ${rectTop}px`)
    if (rectTop < headerBottom) {
      ng.push(
        `④ 移動先セルの上端が sticky header の裏に隠れる（header 下端: ${headerBottom}px, セル top: ${rectTop}px）`,
      )
    }
    movedToTallA = true
  }
}

// main の #753（番組表セルの操作をモーダルにする）とこの PR の空間ナビゲーションが
// 同じコンポーネントを触るため、両者の相互作用を測る。Radix/Base UI のダイアログは
// フォーカスをトラップし portal で document.body 直下に出るため、開いている間は
// キー入力がグリッドの DOM 部分木を経由せず、`handleKeyDown` へ届かないはず。
log('\n=== ⑤ ダイアログ表示中は矢印キーがグリッドへ届かない（モーダル化との整合） ===')
if (movedToTallA) {
  await tallACell.click()
  const dialog = page.getByRole('dialog', { name: tallA.name })
  const dialogOpened = await dialog
    .waitFor({ timeout: 10000 })
    .then(() => true)
    .catch(() => {
      ng.push('⑤ セルをクリックしてもダイアログが開かない')
      return false
    })
  if (dialogOpened) {
    await page.keyboard.press('ArrowDown')
    await page.waitForTimeout(100)
    const focusInDialog = await page.evaluate(() => {
      const active = document.activeElement
      return active instanceof HTMLElement && active.closest('[role="dialog"]') !== null
    })
    log(`  ダイアログ表示中に ArrowDown を押した後もフォーカスがダイアログ内: ${focusInDialog}`)
    if (!focusInDialog) {
      ng.push(
        '⑤ ダイアログ表示中に ArrowDown を押すとフォーカスがダイアログの外へ出る（handleKeyDown が発火している）',
      )
    }

    await page.keyboard.press('Escape')
    const dialogClosed = await dialog
      .waitFor({ state: 'detached', timeout: 10000 })
      .then(() => true)
      .catch(() => {
        ng.push('⑤ Escape でダイアログが閉じない')
        return false
      })
    if (dialogClosed) {
      const returnedFocus = await page.evaluate(() =>
        document.activeElement?.getAttribute('data-program-id'),
      )
      log(`  Escape 後のフォーカス先 programId: ${returnedFocus}`)
      if (returnedFocus !== String(tallA.programId)) {
        ng.push(`⑤ ダイアログを閉じてもフォーカスが元のセルへ戻らない（${returnedFocus}）`)
      } else {
        // フォーカス復帰後も矢印移動を再開できることを見る（同列の直前 = farA へ戻る）。
        await page.keyboard.press('ArrowUp')
        const afterFocus = await page.evaluate(() =>
          document.activeElement?.getAttribute('data-program-id'),
        )
        log(`  ダイアログを閉じた後の ArrowUp: ${afterFocus}`)
        if (afterFocus !== String(farA.programId)) {
          ng.push(`⑤ ダイアログを閉じた後、矢印移動を再開できない（${afterFocus}）`)
        }
      }
    }
  }
} else {
  ng.push('⑤ ④の前提（長時間番組へのフォーカス）が崩れているため検証できない')
}

// main の #758/#724（番組表をズーム）で axis.pxPerHour が可変になった。
// `scrollProgramIntoView` は `timeToPx` 経由の計算なので縮尺非依存に見えるが、
// 式を読むだけでは「実際に壊れていない」ことは言えないので、既定でない縮尺
// （480px/時、3 段階のうち最大）でも矢印移動と追従が壊れないことを実ブラウザで測る。
log('\n=== ⑥ 既定でない縮尺（480px/時）でも矢印移動と追従が壊れない ===')
await grid.evaluate((element) => {
  element.scrollTop = 0
  element.scrollLeft = 0
})
const scaleButton = page.getByRole('button', { name: '480 px/時' })
const scaleButtonFound = (await scaleButton.count()) > 0
if (!scaleButtonFound) {
  ng.push('⑥ 縮尺切替ボタン（480 px/時）が見つからない')
} else {
  await scaleButton.click()
  // GridScaleChips の状態更新と再レイアウトを待つ（他の判定と同じ 100ms 予算）。
  await page.waitForTimeout(150)
  const savedScale = await page.evaluate(() => localStorage.getItem('rokuban:programs:grid-scale'))
  log(`  縮尺切替後の localStorage: ${savedScale}`)
  if (savedScale !== '480') ng.push(`⑥ 縮尺が 480 に切り替わらない（${savedScale}）`)

  if ((await nearACell.count()) > 0) {
    await nearACell.focus()
    await nearACell.press('ArrowRight')
    const rightFocus480 = await page.evaluate(() => ({
      programId: document.activeElement?.getAttribute('data-program-id'),
      site: document.activeElement?.getAttribute('data-site'),
    }))
    log(`  480px/時 ArrowRight フォーカス先: ${JSON.stringify(rightFocus480)}`)
    if (rightFocus480.programId !== String(nearB.programId) || rightFocus480.site !== SITE) {
      ng.push(`⑥ 480px/時で ArrowRight 後のフォーカスが隣列へ移らない（${JSON.stringify(rightFocus480)}）`)
    }

    await nearACell.focus()
    await nearACell.press('ArrowDown')
    const reachedFarA480 = await page
      .waitForFunction(
        (programId) => document.activeElement?.getAttribute('data-program-id') === String(programId),
        farA.programId,
        { timeout: 10000 },
      )
      .then(() => true)
      .catch(() => {
        ng.push('⑥ 480px/時で ArrowDown 後のフォーカスが遠い同列へ移らない（timeout）')
        return false
      })
    if (reachedFarA480) {
      await page.keyboard.press('ArrowDown')
      const reachedTallA480 = await page
        .waitForFunction(
          (programId) => document.activeElement?.getAttribute('data-program-id') === String(programId),
          tallA.programId,
          { timeout: 10000 },
        )
        .then(() => true)
        .catch(() => {
          ng.push('⑥ 480px/時で ArrowDown 後のフォーカスが長時間番組へ移らない（timeout）')
          return false
        })
      if (reachedTallA480) {
        const headerBottom480 = await page
          .getByTestId('program-grid-header-cell')
          .first()
          .evaluate((element) => element.getBoundingClientRect().bottom)
        const rectTop480 = await tallACell.evaluate((element) => element.getBoundingClientRect().top)
        log(`  480px/時 header 行の下端: ${headerBottom480}px / 移動先セルの rect.top: ${rectTop480}px`)
        if (rectTop480 < headerBottom480) {
          ng.push(
            `⑥ 480px/時で移動先セルの上端が sticky header の裏に隠れる（header 下端: ${headerBottom480}px, セル top: ${rectTop480}px）`,
          )
        }
      }
    }
  } else {
    ng.push('⑥ 縮尺切替後に近くの左番組のセルが見当たらない')
  }
}

await finish(ng, browser)
