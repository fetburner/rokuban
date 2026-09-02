// 番組表の site 和集合化を実ブラウザで判定する。
// jsdom はレイアウトを計算しないため、複数 site の列が実際に並び、各列の
// 番組セルが描画されることはここで測る（CLAUDE.md のテスト規律）。
import {
  ListProgramsResponseItem,
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
const FIXED_NOW = new Date('2026-08-12T10:50:00+09:00')
const nowMs = FIXED_NOW.getTime()
const HOUR = 3_600_000
const iso = (ms) => new Date(ms).toISOString()
const sites = ['tokyo', 'takamatsu']

const services = {
  tokyo: {
    id: 401024,
    networkId: 4,
    serviceId: 1024,
    name: '共有BS',
    channelType: 'BS',
    channel: 'BS15_0',
    remoteControlKeyId: 0,
    hasLogoData: false,
    hasPrograms: true,
  },
  takamatsu: {
    id: 401024,
    networkId: 4,
    serviceId: 1024,
    name: '共有BS',
    channelType: 'BS',
    channel: 'BS15_0',
    remoteControlKeyId: 0,
    hasLogoData: false,
    hasPrograms: true,
  },
}

/**
 * grServices は GR（地上波）局。BS の共有フィクスチャ（同一 `Service.id`）とは
 * 別に、**両 site で同じリモコン番号を持つが別放送の局**（実在の東京・高松の
 * NHK 総合・NHK E テレと同じ形）を置く。`orderServices`（`lib/epg-grid.ts`）が
 * リモコン番号を site より先に比べると、この 4 局は 1 列ごとに site が交互する
 * 順になる（レビュー指摘。下記 ① の走の判定が測る）。
 */
const grServices = {
  tokyo: [
    {
      id: 501001,
      networkId: 5,
      serviceId: 1001,
      name: '東京総合',
      channelType: 'GR',
      channel: '27',
      remoteControlKeyId: 1,
      hasLogoData: false,
      hasPrograms: true,
    },
    {
      id: 501002,
      networkId: 5,
      serviceId: 1002,
      name: '東京Eテレ',
      channelType: 'GR',
      channel: '26',
      remoteControlKeyId: 2,
      hasLogoData: false,
      hasPrograms: true,
    },
  ],
  takamatsu: [
    {
      id: 601001,
      networkId: 6,
      serviceId: 1001,
      name: '高松総合',
      channelType: 'GR',
      channel: '27',
      remoteControlKeyId: 1,
      hasLogoData: false,
      hasPrograms: true,
    },
    {
      id: 601002,
      networkId: 6,
      serviceId: 1002,
      name: '高松Eテレ',
      channelType: 'GR',
      channel: '26',
      remoteControlKeyId: 2,
      hasLogoData: false,
      hasPrograms: true,
    },
  ],
}

function programsFor(site) {
  const service = services[site]
  return Array.from({ length: 24 }, (_, index) => {
    const startAt = nowMs + index * HOUR
    return {
      programId: 1_000_000 + index,
      networkId: service.networkId,
      serviceId: service.serviceId,
      eventId: index + 1,
      startAt: iso(startAt),
      endAt: iso(startAt + HOUR),
      durationMs: HOUR,
      name: `共有番組 ${index + 1}`,
      description: '',
      genres: [3],
      isFree: true,
    }
  })
}

/** grProgramsFor は grServices の各局に 24 時間ぶんの番組を割り当てる。 */
function grProgramsFor(site) {
  return grServices[site].flatMap((service) =>
    Array.from({ length: 24 }, (_, index) => {
      const startAt = nowMs + index * HOUR
      return {
        programId: service.networkId * 1_000_000 + service.serviceId * 100 + index,
        networkId: service.networkId,
        serviceId: service.serviceId,
        eventId: index + 1,
        startAt: iso(startAt),
        endAt: iso(startAt + HOUR),
        durationMs: HOUR,
        name: `${service.name} 番組 ${index + 1}`,
        description: '',
        genres: [3],
        isFree: true,
      }
    }),
  )
}

const ng = []

async function apiHandler({ path: pathname, json }) {
  if (pathname === '/api/sites') return json(sites)
  if (pathname === '/api/breakers') return json([])
  if (pathname === '/api/reservations') return json([])
  if (pathname === '/api/capacity/overages') {
    return json(
      sites.map((site) => ({
        site,
        startAt: iso(nowMs + HOUR),
        endAt: iso(nowMs + 2 * HOUR),
        shortfall: site === 'tokyo' ? 1 : 2,
        jammedTypes: ['BS'],
      })),
    )
  }
  if (pathname === '/api/capabilities') return json({ live: false })
  for (const site of sites) {
    if (pathname === `/api/sites/${site}/services`) {
      return json([services[site], ...grServices[site]])
    }
    if (pathname === `/api/sites/${site}/tuners`) return json([])
    if (pathname === `/api/sites/${site}/programs`) {
      return json([...programsFor(site), ...grProgramsFor(site)])
    }
  }
  if (/\/overlaps$/.test(pathname)) return json({ count: 0, reservations: [] })
  if (/\/reservation$/.test(pathname)) return json(null)
  return json([])
}

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()}`)

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ...sites.map((site) => [`${site} service`, ListServicesResponseItem, services[site]]),
    ...sites.flatMap((site) =>
      grServices[site].map((service, index) => [
        `${site} GR service[${index}]`,
        ListServicesResponseItem,
        service,
      ]),
    ),
    ...sites.flatMap((site) =>
      programsFor(site).map((program, index) => [
        `${site} programs[${index}]`,
        ListProgramsResponseItem,
        program,
      ]),
    ),
    ...sites.flatMap((site) =>
      grProgramsFor(site).map((program, index) => [
        `${site} GR programs[${index}]`,
        ListProgramsResponseItem,
        program,
      ]),
    ),
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
const duplicateKeyErrors = []
page.on('console', (message) => {
  if (message.type() === 'error' && /same key|unique "key"/i.test(message.text())) {
    duplicateKeyErrors.push(message.text())
  }
})
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)

await page.goto(`${URL_BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })

log('\n=== ① 複数 site の番組表列 ===')
const columns = page.getByTestId('program-grid-column')
await columns.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('① 番組表の列が 1 本も描画されない')
})

// 列数は site（2）x（共有 BS 1 局 + GR 2 局）= 6。
const expectedColumnCount = sites.length * (1 + 2)
const columnCount = await columns.count()
log(`  描画された列: ${columnCount}`)
if (columnCount !== expectedColumnCount) {
  ng.push(`① 番組表の列が ${expectedColumnCount} 本ではない（${columnCount} 本）`)
}

if (columnCount >= 2) {
  const boxes = await columns.evaluateAll((nodes) =>
    nodes.map((node) => {
      const rect = node.getBoundingClientRect()
      return {
        site: node.getAttribute('data-site'),
        programCount: node.querySelectorAll('[data-testid="program-grid-cell"]').length,
        left: rect.left,
        width: rect.width,
      }
    }),
  )
  log(`  列の実測: ${JSON.stringify(boxes)}`)
  if (new Set(boxes.map((box) => box.site)).size !== 2) {
    ng.push('① 番組表の列が site ごとに識別されていない')
  }
  if (boxes.some((box) => box.programCount === 0 || box.width <= 0)) {
    ng.push('① site 列の一部に番組セルが無い、または幅が 0')
  }

  // レビュー指摘: GR で両 site の remoteControlKeyId が一致する局があると、
  // `orderServices` がリモコン番号を site より先に比べる実装だと 1 列ごとに
  // site が交互してしまう。左端からの並びで「同じ site が連続する走」を数え、
  // 各 site の走が種別の本数（GR 1 本 + BS 1 本 = 2 本）に収まっている
  // （= 1 列ごとに交互していない）ことを確認する。
  const sortedByLeft = [...boxes].sort((a, b) => a.left - b.left)
  const runs = []
  for (const box of sortedByLeft) {
    const last = runs.at(-1)
    if (last && last.site === box.site) last.count++
    else runs.push({ site: box.site, count: 1 })
  }
  log(`  site の走: ${JSON.stringify(runs)}`)
  const runCountBySite = new Map()
  for (const run of runs) {
    runCountBySite.set(run.site, (runCountBySite.get(run.site) ?? 0) + 1)
  }
  for (const site of sites) {
    const runCount = runCountBySite.get(site) ?? 0
    if (runCount !== 2) {
      ng.push(
        `① ${site} の列が連続していない（走が ${runCount} 本。GR + BS で 2 本になるはず。1 列ごとに site が交互している可能性）`,
      )
    }
  }
}

const headerSites = await page.getByTestId('program-grid-header-site').allTextContents()
log(`  可視の site 名: ${JSON.stringify(headerSites)}`)
if (headerSites.length !== columnCount) {
  ng.push(`① 可視の site 名が列数と一致しない（${headerSites.length} / ${columnCount}）`)
}
const uniqueHeaderSites = new Set(headerSites)
if (!sites.every((site) => uniqueHeaderSites.has(site)) || uniqueHeaderSites.size !== sites.length) {
  ng.push('① 同名の共有 BS 列に可視の site 名が無い')
}

log('\n=== ①´ ヘッダーの site 名が列ヘッダーの矩形に収まる（overflow-hidden で切れていない） ===')
// `allTextContents()` は overflow-hidden で視覚的に切れていても文字列自体は
// 取れてしまうため緑になる（レビュー指摘）。site 名を持つ要素の矩形が、
// 固定高（`headerHeightPx`）の列ヘッダーの矩形に収まっているかを実測する。
const headerSiteBoxes = await page.getByTestId('program-grid-header-site').evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    const cell = node.closest('[data-testid="program-grid-header-cell"]')
    const cellRect = cell?.getBoundingClientRect() ?? null
    return {
      top: rect.top,
      bottom: rect.bottom,
      cellTop: cellRect?.top ?? null,
      cellBottom: cellRect?.bottom ?? null,
    }
  }),
)
log(`  ヘッダー site 名の実測: ${JSON.stringify(headerSiteBoxes)}`)
const clipTolerancePx = 0.5
for (const box of headerSiteBoxes) {
  if (box.cellTop === null) {
    ng.push('①´ program-grid-header-cell（列ヘッダーの矩形）が見つからない')
    continue
  }
  if (box.top < box.cellTop - clipTolerancePx || box.bottom > box.cellBottom + clipTolerancePx) {
    ng.push(`①´ ヘッダーの site 名が列ヘッダーの矩形からはみ出している（切れている）: ${JSON.stringify(box)}`)
  }
}

log('\n=== ② 容量帯は site の列内、ラベルは非衝突 ===')
const bands = page.getByTestId('capacity-band')
await bands.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('② 容量帯が描画されない')
})
const bandBoxes = await bands.evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    return { site: node.getAttribute('data-site'), left: rect.left, right: rect.right, width: rect.width }
  }),
)
// GR 局を足したことで、site の列領域は非隣接な複数の走（GR ブロック内 + BS
// ブロック内）に分かれる（① の走の判定と同じ理由）。したがって「同じ site の
// 列内」の判定は単一列の矩形ではなく、`ProgramGrid` が実際に帯をクリップして
// いる `program-grid-site-overlay`（走ごとの領域）と突き合わせる。
const overlayBoxes = await page.getByTestId('program-grid-site-overlay').evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    return { site: node.getAttribute('data-site'), left: rect.left, right: rect.right }
  }),
)
log(`  帯の実測: ${JSON.stringify(bandBoxes)}`)
log(`  site-overlay の実測: ${JSON.stringify(overlayBoxes)}`)
for (const band of bandBoxes) {
  const matchingOverlays = overlayBoxes.filter((overlay) => overlay.site === band.site)
  const insideOwnSite = matchingOverlays.some(
    (overlay) => band.left >= overlay.left - 0.5 && band.right <= overlay.right + 0.5,
  )
  const intersectsOtherSite = overlayBoxes.some(
    (overlay) => overlay.site !== band.site && band.left < overlay.right && band.right > overlay.left,
  )
  if (!insideOwnSite || intersectsOtherSite || band.width <= 0) {
    ng.push(`② ${band.site ?? 'site 不明'} の容量帯が同じ site の列内に限定されていない`)
  }
}

// レビュー指摘: site が種別（GR/BS）ぶんの複数の走を持つと、`ProgramGrid` は
// 走ごとに `siteOverlay` を呼ぶため、対策前は同じ超過区間の読み上げ文
// （sr-only）が走の本数ぶん重複する。走ごとに帯（見た目）は複数出てよいが、
// 読み上げ文は 1 site につき 1 つだけであることを確認する。
const bandAnnouncements = await bands.evaluateAll((nodes) =>
  nodes.map((node) => ({
    site: node.getAttribute('data-site'),
    announceText: node.querySelector('span')?.textContent ?? '',
  })),
)
log(`  帯の sr-only 実測: ${JSON.stringify(bandAnnouncements)}`)
for (const site of sites) {
  const announced = bandAnnouncements.filter((b) => b.site === site && b.announceText !== '')
  if (announced.length !== 1) {
    ng.push(`② ${site} の容量帯の sr-only（読み上げ文）が 1 つではない（${announced.length} 件）`)
  }
}

const labelBoxes = await page.getByTestId('capacity-band-label').evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    return { top: rect.top, bottom: rect.bottom }
  }),
)
if (labelBoxes.length !== 2) ng.push(`② 容量帯ラベルが 2 件ではない（${labelBoxes.length} 件）`)
if (
  labelBoxes.length === 2 &&
  labelBoxes[0].top < labelBoxes[1].bottom &&
  labelBoxes[1].top < labelBoxes[0].bottom
) {
  ng.push('② 別 site の同時刻の容量帯ラベルが重なっている')
}

log('\n=== ③ 一覧行でも site を識別でき、React key が衝突しない ===')
await page.goto(`${URL_BASE}/programs`, { waitUntil: 'domcontentloaded' })
const listRows = page.locator('li[data-program-id="1000000"]')
await listRows.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('③ 共有番組の一覧行が描画されない')
})
if ((await listRows.count()) !== 2) ng.push(`③ 同一 programId の一覧行が 2 件ではない`)
for (const site of sites) {
  const row = listRows.filter({ has: page.getByText(site, { exact: true }) })
  if ((await row.count()) !== 1) ng.push(`③ ${site} の可視名を持つ一覧行が一意でない`)
}
if (duplicateKeyErrors.length > 0) {
  ng.push(`③ React key の重複警告: ${duplicateKeyErrors.join(' / ')}`)
}

await finish(ng, browser)
