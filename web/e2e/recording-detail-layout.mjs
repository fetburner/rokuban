// 録画詳細の面積配分を実ブラウザで測る（issue #1018）。
//
// レイアウト・viewport 内への収まりは jsdom では測れないため、実装前にこの判定を
// 追加して旧画面で落ちることを確かめる。典型例（完了・エンコード済み・チャプターと
// CM 検出あり・シリーズとルールあり・ドロップなし）を desktop/mobile の両方で開く。
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
  startedAt: '2026-09-30T10:00:00.000Z',
  endedAt: '2026-09-30T10:30:00.000Z',
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

async function apiHandler({ path: apiPath, url, json, route }) {
  const method = route.request().method()
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
  if (apiPath === '/api/recordings/1' && method === 'GET') return json(recording)
  if (apiPath === '/api/recordings/1/chapters' && method === 'GET') return json(chapters)
  if (apiPath === '/api/recordings/1/drop-stats' && method === 'GET') return json([])
  if (/^\/api\/media\/recordings\/\d+\/(thumbnail|seek-tiles)$/.test(apiPath)) {
    return route.fulfill({ status: 404 })
  }
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
  const tabPanel = rect('[data-testid="recording-detail-tab-panel"]')
  return {
    viewport,
    detail,
    player,
    playerWidthRatio: detail && player ? Math.round((player.width / detail.width) * 1000) / 1000 : null,
    playerVisibleInFirstViewport: player !== null && player.y >= 0 && player.bottom <= viewport.height,
    tabs,
    tabPanel,
    groups,
  }
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
for (const viewport of [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'mobile', width: 400, height: 800 },
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
  if (EVIDENCE_DIR) {
    await page.screenshot({ path: path.join(EVIDENCE_DIR, `${viewport.name}.png`), fullPage: true })
  }

  if (measured.playerWidthRatio === null || measured.playerWidthRatio < 0.95) {
    ng.push(`${viewport.name}: 映像幅がコンテンツ幅の95%未満（比率=${measured.playerWidthRatio ?? '未測定'}）`)
  }
  if (!measured.playerVisibleInFirstViewport) {
    ng.push(`${viewport.name}: 映像全体が最初の画面に収まらない`)
  }

  const tabList = page.locator('[data-testid="recording-detail-tabs"]')
  if ((await tabList.count()) !== 1) {
    ng.push(`${viewport.name}: 番組・版・記録のタブ見出しが1つ表示されない`)
  } else if (measured.tabPanel !== null) {
    ng.push(`${viewport.name}: 初期状態で閉じたタブの内容が面積を使っている`)
  }

  const more = page.getByRole('button', { name: '録画のその他の操作' })
  if ((await more.count()) !== 1) {
    ng.push(`${viewport.name}: ページ見出しのその他メニューが表示されない`)
  } else {
    await more.click()
    if (await page.getByRole('menu').count() !== 1) {
      ng.push(`${viewport.name}: その他メニューを開けない`)
    }
    if (await page.getByText('今すぐ完全削除', { exact: true }).count() !== 0) {
      ng.push(`${viewport.name}: 通常の録画に今すぐ完全削除が表示される`)
    }
    await page.keyboard.press('Escape')
  }
  await context.close()
}

await finish(ng, browser)
