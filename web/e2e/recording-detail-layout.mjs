// 録画詳細の面積配分を実ブラウザで測る（issue #1049）。
//
// レイアウト・viewport 内への収まりは jsdom では測れないため、実装前にこの判定を
// 追加して旧画面で落ちることを確かめる。典型例（完了・エンコード済み・チャプターと
// CM 検出あり・シリーズとルールあり・ドロップなし）を 3 種の desktop viewport と
// smartphone で開き、シリーズ無し録画も別に開く。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4188 --strictPort &
//   E2E_URL=http://localhost:4188 E2E_SHOT_DIR=/tmp/recording-detail-shots \
//     corepack pnpm e2e:recording-detail-layout

import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { ListRecordingsResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const EVIDENCE_DIR = process.env.E2E_SHOT_DIR
if (EVIDENCE_DIR) mkdirSync(EVIDENCE_DIR, { recursive: true })
const ng = []

// 判定フィクスチャ。判定側で値を実装から import しない。
const recording = {
  id: 1,
  site: 'default',
  ruleId: 5,
  source: 'rule',
  serviceName: 'ＮＨＫ総合１・岡山',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'ニュース７',
  description: '国内外の主要なニュースを、現場からの中継や解説を交えて詳しくお伝えします。気象情報もあります。キャスターは山田太郎、佐藤花子です。',
  series: 'ニュース７',
  startAt: '2026-09-30T10:00:00.000Z',
  startedAt: '2026-09-30T09:59:30.000Z',
  endedAt: '2026-09-30T10:30:30.000Z',
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'detected', ranges: [] },
  sizeBytes: 3_200_000_000,
  encodedAssets: [
    { profile: 'h264-720p', sizeBytes: 572_000_000 },
    { profile: 'h264-cut', cut: true, sizeBytes: 450_000_000 },
  ],
  encodeProfiles: ['h264-720p', 'h264-cut'],
  createdAt: '2026-09-30T10:35:00.000Z',
}

const nextRecording = {
  ...recording,
  id: 2,
  title: 'ニュース７',
  startAt: '2026-10-01T10:00:00.000Z',
  startedAt: '2026-10-01T10:00:00.000Z',
  endedAt: '2026-10-01T10:30:00.000Z',
  createdAt: '2026-10-01T10:35:00.000Z',
}

const trashRecording = {
  ...recording,
  id: 3,
  title: '録画した番組',
  deletedAt: '2026-10-02T10:40:00.000Z',
}

const inProgressRecording = {
  ...recording,
  id: 4,
  title: '録画中の番組',
  series: undefined,
  status: 'recording',
  startedAt: '2026-10-02T10:00:00.000Z',
  endedAt: undefined,
  sizeBytes: undefined,
  encodedAssets: [],
  encodeProfiles: [],
  ingest: { state: 'pending' },
}

const noSeriesRecording = {
  ...recording,
  id: 5,
  title: 'シリーズに属さない録画',
  series: null,
}

const chapters = {
  source: 'auto',
  version: 'auto:detected:1',
  detectionPending: false,
  spans: [
    { startMs: 300_000, endMs: 330_000, label: 'CM', cut: true },
    { startMs: 900_000, endMs: 930_000, label: 'CM', cut: true },
  ],
}

const rule = {
  id: 5,
  name: 'ニュース７（平日）',
  enabled: true,
  priority: 0,
  keepOriginal: 'always',
  createdAt: '2026-01-01T00:00:00.000Z',
  updatedAt: '2026-01-01T00:00:00.000Z',
}

let purgeCalls = 0

async function apiHandler({ path: apiPath, url, json, route }) {
  const method = route.request().method()
  if (apiPath === '/api/recordings/3/purge' && method === 'POST') {
    purgeCalls += 1
    return route.fulfill({ status: 204 })
  }
  if (apiPath === '/api/sites') return json(['default'])
  if (apiPath === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (apiPath === '/api/breakers') return json([])
  if (apiPath === '/api/encode-profiles') return json([])
  if (apiPath === '/api/rules') return json([rule])
  if (apiPath === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(apiPath)) return json([])
  if (apiPath === '/api/recordings' && method === 'GET') {
    return json(url.searchParams.has('seriesOf') ? [recording, nextRecording] : [recording])
  }
  const recordingMatch = /^\/api\/recordings\/(\d+)$/.exec(apiPath)
  if (recordingMatch && method === 'GET') {
    const item = [recording, nextRecording, trashRecording, inProgressRecording, noSeriesRecording]
      .find((candidate) => candidate.id === Number(recordingMatch[1]))
    return item ? json(item) : json({ error: 'not found' }, 404)
  }
  const chaptersMatch = /^\/api\/recordings\/(\d+)\/chapters$/.exec(apiPath)
  if (chaptersMatch && method === 'GET') return json(chapters)
  if (/^\/api\/recordings\/\d+\/drop-stats$/.test(apiPath) && method === 'GET') return json([])
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(apiPath)) {
    return route.fulfill({
      status: 200,
      contentType: 'image/svg+xml',
      body: '<svg xmlns="http://www.w3.org/2000/svg" width="160" height="90"><rect width="160" height="90" fill="#263650"/><path d="M0 70 60 25l36 32 22-20 42 34v19H0Z" fill="#485d7c"/></svg>',
    })
  }
  if (/^\/api\/media\/recordings\/\d+\/seek-tiles$/.test(apiPath)) return route.fulfill({ status: 404 })
  if (/^\/api\/media\/recordings\/\d+\/file$/.test(apiPath)) {
    return route.fulfill({ status: 404 })
  }
  return json([])
}

function measureLayout() {
  const rect = (selector) => {
    const element = document.querySelector(selector)
    if (!element) return null
    const { x, y, width, height, bottom } = element.getBoundingClientRect()
    return { x, y, width, height, bottom }
  }
  const detail = rect('[data-testid="recording-detail-body"]')
  const player = rect('[data-testid="recording-player-frame"]')
  const playerColumn = rect('[data-testid="recording-player-column"]')
  const title = rect('[data-testid="recording-title-row"] h2')
  const description = rect('[data-testid="recording-description"]')
  const shelf = rect('[data-testid="recording-series-shelf"]')
  const viewport = { width: innerWidth, height: innerHeight }
  const groups = Object.fromEntries(
    [
      'recording-playback-group',
      'recording-continuation-group',
      'recording-program-group',
      'recording-assets-group',
      'recording-observations',
      'recording-actions-group',
    ].map((name) => {
      const element = document.querySelector(`[data-testid="${name}"]`)
      return [name, element ? Math.round((element.getBoundingClientRect().height / viewport.height) * 1000) / 10 : null]
    }),
  )
  const tabs = rect('[data-testid="recording-detail-tabs"]')
  const tabPanelCount = document.querySelectorAll('[data-testid="recording-detail-tab-panel"]').length
  const outsideProgramTrackCount = document.querySelectorAll('[data-testid="recorded-outside-program-range"]').length
  const outsideProgramSegmentCount = document.querySelectorAll(
    '[data-testid="recorded-before-program"], [data-testid="recorded-after-program"]',
  ).length
  return {
    viewport,
    detail,
    player,
    playerColumn,
    title,
    description,
    shelf,
    playerHeightRatio: player ? Math.round((player.height / viewport.height) * 1000) / 1000 : null,
    playerVisibleInFirstViewport: player !== null && player.y >= 0 && player.bottom <= viewport.height,
    titleVisibleInFirstViewport: title !== null && title.y >= 0 && title.bottom <= viewport.height,
    descriptionVisibleInFirstViewport: description !== null && description.bottom <= viewport.height,
    shelfAlignedWithPlayer: shelf !== null && player !== null && Math.abs(shelf.y - player.y) <= 1,
    shelfRightOfPlayer: shelf !== null && player !== null && shelf.x >= player.x + player.width,
    tabs,
    tabPanelCount,
    outsideProgramTrackCount,
    outsideProgramSegmentCount,
    groups,
  }
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([
  ['recording', ListRecordingsResponseItem, recording],
  ['trashRecording', ListRecordingsResponseItem, trashRecording],
  ['inProgressRecording', ListRecordingsResponseItem, inProgressRecording],
  ['noSeriesRecording', ListRecordingsResponseItem, noSeriesRecording],
], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
for (const viewport of [
  { name: 'desktop-1280x720', desktop: true, width: 1280, height: 720 },
  { name: 'desktop-1280x800', desktop: true, width: 1280, height: 800 },
  { name: 'desktop-1920x1080', desktop: true, width: 1920, height: 1080 },
  { name: 'mobile-400x800', desktop: false, width: 400, height: 800 },
]) {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  await context.clock.setFixedTime(new Date('2026-09-30T11:00:00.000Z'))
  const page = await context.newPage()
  await installApiStubs(page, apiHandler)
  await page.goto(`${URL_BASE}/recordings/1`, { waitUntil: 'domcontentloaded' })
  await page.getByText('ニュース７', { exact: true }).first().waitFor({ timeout: 15000 })
  await page.locator('[data-testid="recording-player-frame"]').waitFor({ timeout: 15000 })

  const measured = await page.evaluate(measureLayout)
  log(`\n=== ${viewport.name} ${viewport.width}×${viewport.height}: 面積実測 ===`)
  log(JSON.stringify(measured, null, 2))
  if (!measured.playerVisibleInFirstViewport) {
    ng.push(`${viewport.name}: 映像全体が最初の画面に収まらない`)
  }
  if (viewport.desktop && !measured.descriptionVisibleInFirstViewport) {
    ng.push(`${viewport.name}: タイトルと説明の先頭2行が最初の画面に収まらない`)
  }
  if (viewport.desktop && !measured.titleVisibleInFirstViewport) {
    ng.push(`${viewport.name}: タイトルが最初の画面に収まらない`)
  }
  if (viewport.desktop && !measured.shelfAlignedWithPlayer) {
    ng.push(`${viewport.name}: シリーズ棚の上端が映像の上端と揃わない`)
  }
  if (viewport.desktop && !measured.shelfRightOfPlayer) {
    ng.push(`${viewport.name}: シリーズ棚が映像の右側にない`)
  }
  if (measured.outsideProgramTrackCount !== 1 || measured.outsideProgramSegmentCount !== 2) {
    ng.push(`${viewport.name}: 番組枠外の前後区間が一本のシークバーに点線表示されない`)
  }

  const tabList = page.locator('[data-testid="recording-detail-tabs"]')
  if ((await tabList.count()) !== 1) {
    ng.push(`${viewport.name}: 番組・版・記録のタブ見出しが1つ表示されない`)
  } else {
    // 層 3・4 は選んだタブの中身だけが DOM にある（閉じた状態で面積を使わない）。パネル数だけでは
    // 常に 1 になって何も言えないので、タブごとにどの塊が DOM にあるかを見る。
    const groupOf = { 番組: 'recording-program-group', 版: 'recording-assets-group', 記録: 'recording-observations' }
    if ((await page.getByRole('menuitem').count()) !== 0) ng.push(`${viewport.name}: 閉じた ⋮ メニューの中身が DOM にある`)
    for (const [tabName, testId] of Object.entries(groupOf)) {
      await page.getByRole('tab', { name: tabName }).click()
      const present = await page.evaluate(
        (ids) => ids.filter((id) => document.querySelector(`[data-testid="${id}"]`) !== null),
        Object.values(groupOf),
      )
      if (present.length !== 1 || present[0] !== testId) {
        ng.push(`${viewport.name}: 「${tabName}」を選んだときに DOM にある塊が ${JSON.stringify(present)}（期待 [${testId}] だけ）`)
      }
    }
    // 初期選択の確認（下）のため、最初のタブへ戻す。
    await page.getByRole('tab', { name: viewport.desktop ? '版' : '番組' }).click()
  }
  const expectedSelectedTab = viewport.desktop ? '版' : '番組'
  if (!(await page.getByRole('tab', { name: expectedSelectedTab }).getAttribute('aria-selected') === 'true')) {
    ng.push(`${viewport.name}: 初期選択タブが「${expectedSelectedTab}」ではない`)
  }
  const shelfVisible = await page.locator('[data-testid="recording-series-shelf"]').isVisible().catch(() => false)
  if (shelfVisible !== viewport.desktop) {
    ng.push(`${viewport.name}: シリーズ棚の表示条件が一致しない`)
  }

  if (EVIDENCE_DIR) {
    await page.screenshot({ path: path.join(EVIDENCE_DIR, `${viewport.name}.png`), fullPage: true, animations: 'disabled' })
  }

  const more = page.getByRole('button', { name: '録画のその他の操作' })
  if ((await more.count()) !== 1) {
    ng.push(`${viewport.name}: ページ見出しのその他メニューが表示されない`)
  } else {
    await more.click()
    try {
      await page.getByRole('menu').waitFor({ state: 'visible', timeout: 2000 })
    } catch {
      ng.push(`${viewport.name}: その他メニューを開けない`)
    }
    if (await page.getByText('今すぐ完全削除', { exact: true }).count() !== 0) {
      ng.push(`${viewport.name}: 通常の録画に今すぐ完全削除が表示される`)
    }
    // 行の高さはラフ（13px × 行間 1.7 + 上下 7px = 36px）を下限にする。詰まると押し分けにくく窮屈に見える。
    const rows = await page.getByRole('menuitem').evaluateAll((items) =>
      // 開くアニメーション（zoom-in-95）の途中で測らないよう、変形の影響を受けない offsetHeight を使う。
      items.map((item) => ({ text: item.textContent.trim(), height: item.offsetHeight })),
    )
    log(`  ${viewport.name} ⋮ メニューの行: ${JSON.stringify(rows)}`)
    for (const row of rows) {
      if (row.height < 36) ng.push(`${viewport.name}: ⋮ メニューの「${row.text}」の行の高さが ${row.height}px（ラフは 36px）`)
    }
    await page.keyboard.press('Escape')
  }
  if (EVIDENCE_DIR && !viewport.desktop) {
    await page.locator('[data-testid="recording-player-frame"]').hover()
    await page.getByRole('button', { name: '再生設定' }).click()
    await page.getByTestId('playback-settings').waitFor({ timeout: 5000 })
    await page.screenshot({ path: path.join(EVIDENCE_DIR, 'mobile-playback-settings-400x800.png'), fullPage: true, animations: 'disabled' })
  }
  await context.close()
}

const trashContext = await browser.newContext({ viewport: { width: 1280, height: 800 }, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
const trashPage = await trashContext.newPage()
await installApiStubs(trashPage, apiHandler)
await trashPage.goto(`${URL_BASE}/recordings/3`, { waitUntil: 'domcontentloaded' })
await trashPage.getByRole('button', { name: '復元' }).waitFor({ timeout: 15000 })
if (await trashPage.getByRole('tab').count() !== 1) ng.push('trash: 番組タブだけが表示されない')
if (await trashPage.locator('[data-testid="recording-series-shelf"]').count() !== 0) {
  ng.push('trash: シリーズ棚が表示される')
}
if (await trashPage.locator('video, img, a[download], a[href*="/file"]').count() !== 0) {
  ng.push('trash: 再生・サムネイル・ダウンロードが表示される')
}
await trashPage.getByRole('button', { name: '録画のその他の操作' }).click()
await trashPage.getByRole('menuitem', { name: '今すぐ完全削除' }).click()
await trashPage.getByRole('alertdialog').waitFor({ timeout: 5000 })
if (purgeCalls !== 0) ng.push('trash: 確認前に purge API を呼び出す')
if (EVIDENCE_DIR) {
  await trashPage.screenshot({ path: path.join(EVIDENCE_DIR, 'trash.png'), fullPage: true, animations: 'disabled' })
}
await trashPage.getByRole('button', { name: 'キャンセル' }).click()
if (purgeCalls !== 0) ng.push('trash: 完全削除のキャンセルで purge API を呼び出す')
await trashPage.getByRole('button', { name: '録画のその他の操作' }).click()
await trashPage.getByRole('menuitem', { name: '今すぐ完全削除' }).click()
await trashPage.getByRole('button', { name: '完全削除を予約する' }).click()
if (purgeCalls !== 1) ng.push('trash: 確認後に purge API を呼び出さない')
await trashContext.close()

const recordingContext = await browser.newContext({ viewport: { width: 1280, height: 800 }, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
const recordingPage = await recordingContext.newPage()
await installApiStubs(recordingPage, apiHandler)
await recordingPage.goto(`${URL_BASE}/recordings/4`, { waitUntil: 'domcontentloaded' })
await recordingPage.getByRole('heading', { name: '録画中の番組' }).waitFor({ timeout: 15000 })
await recordingPage.getByTestId('recording-assets-group').waitFor({ timeout: 5000 })
if (await recordingPage.locator('video').count() !== 0) ng.push('recording: encoded player がないのに video が表示される')
if (await recordingPage.getByRole('tab').count() < 2) ng.push('recording: 版タブの中身があるのにタブがない')
if (await recordingPage.getByTestId('recording-original-row').count() !== 1) {
  ng.push('recording: 取り込み中の原本 TS 行が1つだけ表示されない')
}
if (await recordingPage.getByTestId('recording-version-row').count() !== 0) {
  ng.push('recording: 取り込み中に未完成の変換版が表示される')
}
if (await recordingPage.locator('[data-testid="recording-original-row"] a').count() !== 0) {
  ng.push('recording: 取り込み中の原本 TS に未完成のダウンロードリンクが出る')
}
if (EVIDENCE_DIR) {
  await recordingPage.screenshot({ path: path.join(EVIDENCE_DIR, 'recording.png'), fullPage: true, animations: 'disabled' })
}
await recordingContext.close()

const noSeriesContext = await browser.newContext({ viewport: { width: 1280, height: 800 }, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
const noSeriesPage = await noSeriesContext.newPage()
await installApiStubs(noSeriesPage, apiHandler)
await noSeriesPage.goto(`${URL_BASE}/recordings/5`, { waitUntil: 'domcontentloaded' })
await noSeriesPage.getByRole('heading', { name: 'シリーズに属さない録画' }).waitFor({ timeout: 15000 })
await noSeriesPage.locator('[data-testid="recording-player-frame"]').waitFor({ timeout: 15000 })
const noSeriesLayout = await noSeriesPage.evaluate(measureLayout)
log('\n=== no-series 1280×800: 映像と下段の位置 ===')
log(JSON.stringify(noSeriesLayout, null, 2))
if (noSeriesLayout.player === null || noSeriesLayout.playerColumn === null) {
  ng.push('no-series: 映像または下段カラムを測定できない')
} else if (
  Math.abs(noSeriesLayout.player.x - noSeriesLayout.playerColumn.x) > 1 ||
  Math.abs(noSeriesLayout.player.width - noSeriesLayout.playerColumn.width) > 1
) {
  ng.push('no-series: 映像と下段カラムの左右位置または幅が揃わない')
}
if (await noSeriesPage.locator('[data-testid="recording-series-shelf"]').count() !== 0) {
  ng.push('no-series: シリーズ棚が表示される')
}
if (EVIDENCE_DIR) {
  await noSeriesPage.screenshot({ path: path.join(EVIDENCE_DIR, 'no-series-1280x800.png'), fullPage: true, animations: 'disabled' })
}
await noSeriesContext.close()

await finish(ng, browser)
