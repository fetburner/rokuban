// 予約詳細のヘッダーとその他メニューが desktop / 360px の両方で使えることを
// 実ブラウザで判定する（issue #1029）。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/reservation-detail-shots \
//     pnpm e2e:reservation-detail-layout

import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { GetProgramReservationResponse, GetProgramResponse } from '../src/api/zod.ts'
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

const startAt = '2026-10-04T03:00:00.000Z'
const reservation = {
  site: 'default',
  programId: 9001,
  source: 'manual',
  state: 'active',
  title: '週末ドキュメンタリー特集',
  serviceName: 'ＮＨＫＢＳプレミアム４Ｋ',
  channelType: 'BS',
  startAt,
  durationMs: 3_600_000,
  createdAt: '2026-10-03T00:00:00.000Z',
  updatedAt: '2026-10-03T00:00:00.000Z',
  skip: false,
}

const program = {
  programId: reservation.programId,
  networkId: 4,
  serviceId: 101,
  eventId: 12,
  startAt,
  endAt: '2026-10-04T04:00:00.000Z',
  durationMs: reservation.durationMs,
  name: reservation.title,
  description: '各地の暮らしと自然を訪ね、地域の人々の営みを丁寧に紹介します。',
  genres: [],
  isFree: true,
  extended: { 出演: '山田花子' },
  video: { resolution: '1920x1080' },
  audios: [],
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit(
  [
    ['reservation', GetProgramReservationResponse, reservation],
    ['program', GetProgramResponse, program],
  ],
  ng,
)

log('\n=== 配信 bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const apiHandler = async ({ path: p, route, json }) => {
  if (p === '/api/events') return sseKeepAlive(route)
  if (p === '/api/sites') return json(['default'])
  if (p === '/api/breakers') return json([])
  if (p === '/api/rules') return json([])
  if (p === '/api/encode-profiles') return json([])
  if (p === '/api/reservations') return json([])
  if (p === '/api/capacity/overages') return json([])
  if (p === `/api/sites/default/programs/${reservation.programId}/reservation`) {
    return json(reservation)
  }
  if (p === `/api/sites/default/programs/${reservation.programId}`) return json(program)
  if (p === `/api/sites/default/programs/${reservation.programId}/overlaps`) {
    return json({ count: 0, reservations: [] })
  }
  return json([])
}

for (const viewport of [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'smartphone', width: 360, height: 800 },
]) {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
  })
  const page = await context.newPage()
  await installApiStubs(page, apiHandler)
  await page.goto(`${URL_BASE}/reservations/default/${reservation.programId}`, {
    waitUntil: 'domcontentloaded',
  })
  await page.getByRole('heading', { name: '予約の詳細' }).waitFor({ timeout: 15_000 })
  await page.getByRole('heading', { name: reservation.title }).waitFor({ timeout: 10_000 })
  await page.getByText(program.description, { exact: true }).waitFor({ timeout: 2_000 }).catch(() => {
    ng.push(`${viewport.name}: 番組詳細の説明が表示されない`)
  })

  const metadata = page.getByTestId('reservation-program-link')
  const metadataCount = await metadata.count()
  if (metadataCount !== 1) {
    ng.push(`${viewport.name}: 局名・開始時刻・尺の導線が番組表へのリンクになっていない`)
  } else {
    const href = await metadata.getAttribute('href')
    const target = new URL(href, URL_BASE)
    if (
      target.pathname !== '/programs' ||
      target.searchParams.get('view') !== 'grid' ||
      target.searchParams.get('at') !== String(Date.parse(startAt))
    ) {
      ng.push(`${viewport.name}: metadata link destination is unexpected (${href})`)
    }
    if ((await page.locator('h2 a').count()) !== 0) {
      ng.push(`${viewport.name}: 番組タイトル自体がリンクになっている`)
    }
  }

  const trigger = page.getByRole('button', { name: '予約のその他の操作' })
  if ((await trigger.count()) !== 1) {
    ng.push(`${viewport.name}: ヘッダーの予約操作メニューが表示されない`)
  } else {
    await trigger.click()
    const menu = page.getByRole('menu')
    await menu.waitFor({ state: 'visible', timeout: 2_000 }).catch(() => {
      ng.push(`${viewport.name}: その他メニューを開けない`)
    })
    const cancelItem = page.getByRole('menuitem', { name: '予約を取消' })
    if ((await cancelItem.count()) !== 1) {
      ng.push(`${viewport.name}: メニュー内に「予約を取消」がない`)
    }

    const menuBox = await menu.boundingBox()
    if (
      !menuBox ||
      menuBox.x < 0 ||
      menuBox.y < 0 ||
      menuBox.x + menuBox.width > viewport.width ||
      menuBox.y + menuBox.height > viewport.height
    ) {
      ng.push(`${viewport.name}: その他メニューが viewport 内に収まらない (${JSON.stringify(menuBox)})`)
    }
    const targets = await page.getByRole('menuitem').evaluateAll((items) =>
      items.map((item) => {
        const box = item.getBoundingClientRect()
        return { text: item.textContent.trim(), width: box.width, height: box.height }
      }),
    )
    for (const target of targets) {
      if (target.width < 24 || target.height < 24) {
        ng.push(`${viewport.name}: メニュー項目「${target.text}」のタップ領域が 24px 未満`)
      }
    }
    log(`  ${viewport.name}: menu=${JSON.stringify(menuBox)}, targets=${JSON.stringify(targets)}`)
    if (EVIDENCE_DIR) {
      await page.screenshot({
        path: path.join(EVIDENCE_DIR, `${viewport.name}.png`),
        fullPage: true,
        animations: 'disabled',
      })
    }
  }
  await context.close()
}

await finish(ng, browser)
