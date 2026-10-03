// 予約詳細のヘッダーとその他メニューが desktop / 360px の両方で使えることを
// 実ブラウザで判定する（issue #1029）。
//
//   cd web && pnpm build && pnpm preview --port 40773 --strictPort &
//   E2E_URL=http://localhost:40773 E2E_SHOT_DIR=/tmp/reservation-detail-shots \
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
let programMode = 'ok'
let programGets = 0
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
  if (p === `/api/sites/default/programs/${reservation.programId}`) {
    programGets += 1
    if (programMode === 'notfound') {
      return route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' })
    }
    if (programMode === 'error') {
      return route.fulfill({ status: 503, contentType: 'application/json', body: '{"error":"epg down"}' })
    }
    return json(program)
  }
  if (p === `/api/sites/default/programs/${reservation.programId}/overlaps`) {
    return json({ count: 0, reservations: [] })
  }
  return json([])
}

const shot = async (page, name) => {
  if (EVIDENCE_DIR) {
    await page.screenshot({ path: path.join(EVIDENCE_DIR, `${name}.png`), fullPage: true, animations: 'disabled' })
  }
}

for (const viewport of [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'smartphone', width: 360, height: 800 },
]) {
  for (const colorScheme of ['light', 'dark']) {
    const tag = `${viewport.name}-${colorScheme}`
    const context = await browser.newContext({
      viewport: { width: viewport.width, height: viewport.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme,
    })
    const page = await context.newPage()
    await installApiStubs(page, apiHandler)
    const open = async (mode) => {
      programMode = mode
      programGets = 0
      await page.goto(`${URL_BASE}/reservations/default/${reservation.programId}`, {
        waitUntil: 'domcontentloaded',
      })
      await page.getByRole('heading', { name: '予約の詳細' }).waitFor({ timeout: 15_000 })
      await page.getByRole('heading', { name: reservation.title }).waitFor({ timeout: 10_000 })
    }

    await open('ok')
    await page.getByText(program.description, { exact: true }).waitFor({ timeout: 2_000 }).catch(() => {
      ng.push(`${tag}: 番組詳細の説明が表示されない`)
    })

    // 題名の塊（題名・局・時刻）の直下に説明が来る。局・時刻の行が題名の直後にあること、
    // 説明がその下にあることを座標で見る。
    const rect = (loc) => loc.evaluate((el) => {
      const r = el.getBoundingClientRect()
      return { top: r.top, bottom: r.bottom }
    })
    const h2 = await rect(page.getByRole('heading', { name: reservation.title }))
    const linkRect = await rect(page.getByTestId('reservation-program-link'))
    const descRect = await rect(page.getByText(program.description, { exact: true }))
    if (linkRect.top - h2.bottom > 12) ng.push(`${tag}: 題名と局・時刻の間が空いている (${linkRect.top - h2.bottom}px)`)
    if (descRect.top < linkRect.bottom) ng.push(`${tag}: 説明が局・時刻の行より上にある`)
    log(`  ${tag}: title->link gap=${Math.round(linkRect.top - h2.bottom)}px, link->desc=${Math.round(descRect.top - linkRect.bottom)}px`)

    const metadata = page.getByTestId('reservation-program-link')
    if ((await metadata.count()) !== 1) {
      ng.push(`${tag}: 局名・開始時刻・尺の導線が番組表へのリンクになっていない`)
    } else {
      const href = await metadata.getAttribute('href')
      const target = new URL(href, URL_BASE)
      if (
        target.pathname !== '/programs' ||
        target.searchParams.get('view') !== 'grid' ||
        target.searchParams.get('at') !== String(Date.parse(startAt))
      ) {
        ng.push(`${tag}: metadata link destination is unexpected (${href})`)
      }
      if ((await page.locator('h2 a').count()) !== 0) ng.push(`${tag}: 番組タイトル自体がリンクになっている`)
      const deco = await metadata.evaluate((el) => getComputedStyle(el).textDecorationLine)
      if (deco !== 'underline') ng.push(`${tag}: 局・時刻のリンクに下線が無い (${deco})`)
    }

    if ((await page.getByRole('button', { name: '予約を取消' }).count()) !== 0) {
      ng.push(`${tag}: メニューを開く前から「予約を取消」ボタンが出ている`)
    }
    await shot(page, `${tag}-normal`)

    const trigger = page.getByRole('button', { name: '予約のその他の操作' })
    if ((await trigger.count()) !== 1) {
      ng.push(`${tag}: ヘッダーの予約操作メニューが表示されない`)
    } else {
      await trigger.click()
      const menu = page.getByRole('menu')
      await menu.waitFor({ state: 'visible', timeout: 2_000 }).catch(() => {
        ng.push(`${tag}: その他メニューを開けない`)
      })
      if ((await page.getByRole('menuitem', { name: '予約を取消' }).count()) !== 1) {
        ng.push(`${tag}: メニュー内に「予約を取消」がない`)
      }
      const menuBox = await menu.boundingBox()
      if (
        !menuBox ||
        menuBox.x < 0 ||
        menuBox.y < 0 ||
        menuBox.x + menuBox.width > viewport.width ||
        menuBox.y + menuBox.height > viewport.height
      ) {
        ng.push(`${tag}: その他メニューが viewport 内に収まらない (${JSON.stringify(menuBox)})`)
      }
      const targets = await page.getByRole('menuitem').evaluateAll((items) =>
        items.map((item) => {
          const box = item.getBoundingClientRect()
          return { text: item.textContent.trim(), width: box.width, height: box.height }
        }),
      )
      for (const t of targets) {
        if (t.width < 24 || t.height < 24) ng.push(`${tag}: メニュー項目「${t.text}」のタップ領域が 24px 未満`)
      }
      log(`  ${tag}: menu=${JSON.stringify(menuBox)}, targets=${JSON.stringify(targets)}`)
      await shot(page, `${tag}-menu`)
    }

    // EPG から消えた番組: retry せず初回の 404 で欄が消える（main.tsx 既定の QueryClient で測る）。
    const t0 = Date.now()
    await open('notfound')
    await page.getByText('詳細を読み込み中…').waitFor({ state: 'detached', timeout: 2_000 }).catch(() => {
      ng.push(`${tag}: 404 で 2 秒経っても「読み込み中」が残る`)
    })
    log(`  ${tag}: 404 hidden after ${Date.now() - t0}ms, program GET=${programGets}`)
    if (programGets !== 1) ng.push(`${tag}: 404 で program GET が ${programGets} 回 (期待 1)`)
    await shot(page, `${tag}-404`)

    // 5xx は retry（1s/2s/4s）を使い切ったあと指定文言を出す。
    await open('error')
    await page.getByText('番組情報の取得に失敗しました').waitFor({ timeout: 12_000 }).catch(() => {
      ng.push(`${tag}: 5xx で「番組情報の取得に失敗しました」が出ない`)
    })
    await shot(page, `${tag}-5xx`)
    await context.close()
  }
}

await finish(ng, browser)
