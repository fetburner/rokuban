// Issue #1224: モバイルでチャンネル選択と「その他」をシートで開く。
// jsdom が測れない viewport への接地と、録画絞り込みシート内での親子遷移を測る。
import { finish, installApiStubs, launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []
const SITE = 'default'
const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')
const MOBILE = { width: 390, height: 844 }

const services = [
  { id: 3273601024, networkId: 32736, serviceId: 1024, name: 'NHK総合', channelType: 'GR', channel: '27', remoteControlKeyId: 1, hasLogoData: false, hasPrograms: true },
  { id: 3273701032, networkId: 32737, serviceId: 1032, name: 'NHKEテレ', channelType: 'GR', channel: '26', remoteControlKeyId: 2, hasLogoData: false, hasPrograms: true },
  { id: 3273801040, networkId: 32738, serviceId: 1040, name: 'テレビ大阪', channelType: 'GR', channel: '18', remoteControlKeyId: 7, hasLogoData: false, hasPrograms: true },
  { id: 400101, networkId: 4, serviceId: 101, name: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', remoteControlKeyId: 0, hasLogoData: false, hasPrograms: true },
]

async function apiHandler({ path, json, route }) {
  if (path === '/api/events') {
    await route.fulfill({ status: 204 })
    return
  }
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === `/api/sites/${SITE}/services`) return json(services)
  return json([])
}

function report(label, action) {
  return action()
    .then(() => log(`  OK: ${label}`))
    .catch((error) => {
      const detail = error instanceof Error ? error.message : String(error)
      ng.push(`${label}: ${detail}`)
      log(`  NG: ${label}: ${detail}`)
    })
}

async function assertSheetTouchesBottom(dialog, page, label) {
  const bounds = await dialog.evaluate((element) => {
    const rect = element.getBoundingClientRect()
    return { left: rect.left, right: rect.right, bottom: rect.bottom, height: rect.height }
  })
  const viewport = page.viewportSize()
  const delta = Math.abs(bounds.bottom - viewport.height)
  if (delta > 1) {
    throw new Error(`${label} bottom=${bounds.bottom.toFixed(1)}px, viewport=${viewport.height}px`)
  }
  if (bounds.left > 1 || bounds.right < viewport.width - 1) {
    throw new Error(`${label} width=${(bounds.right - bounds.left).toFixed(1)}px, viewport=${viewport.width}px`)
  }
  if (label === 'その他シート' && bounds.height > viewport.height * 0.6) {
    throw new Error(`${label} height=${bounds.height.toFixed(1)}px, expected a low sheet under 60% of the viewport`)
  }
}

async function openFilterSheet(page) {
  await page.getByRole('button', { name: '絞り込み' }).click()
  const sheet = page.getByRole('dialog', { name: '絞り込み' })
  await sheet.waitFor({ state: 'visible' })
  return sheet
}

log(`URL: ${URL_BASE}`)
log('\n=== 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const page = await browser.newPage({ viewport: MOBILE })
await page.clock.install({ time: FIXED_NOW })
await installApiStubs(page, apiHandler)

log('\n=== 390px: 番組表のチャンネル選択 ===')
await page.goto(`${URL_BASE}/programs`)
await report('番組表にチャンネル選択が出る', async () => {
  const trigger = page.getByRole('button', { name: 'チャンネル: すべて' })
  await trigger.waitFor({ state: 'visible' })
  await trigger.click()
  const picker = page.getByRole('dialog', { name: 'チャンネル' })
  await picker.waitFor({ state: 'visible' })
  const nhk = picker.getByRole('checkbox', { name: /NHK総合/ })
  await nhk.waitFor({ state: 'visible' })
  if (await nhk.getAttribute('aria-checked') !== 'true') throw new Error('全局状態で NHK 総合が未選択')
})
await report('番組表のチャンネル選択シートが画面下端に接する', async () => {
  await assertSheetTouchesBottom(page.getByRole('dialog', { name: 'チャンネル' }), page, '番組表のチャンネルシート')
})
await page.keyboard.press('Escape')
await page.getByRole('dialog', { name: 'チャンネル' }).waitFor({ state: 'detached' })

log('\n=== 390px: More メニュー ===')
await page.getByRole('button', { name: 'その他' }).click()
const more = page.getByRole('dialog', { name: 'その他のナビゲーション' })
await more.waitFor({ state: 'visible' })
await report('「その他」のシートが画面下端に接する', async () => {
  await assertSheetTouchesBottom(more, page, 'その他シート')
})
await more.getByRole('link', { name: '予約' }).waitFor({ state: 'visible' })
await more.getByRole('button', { name: '完了' }).click()
await more.waitFor({ state: 'detached' })

log('\n=== 390px: 録画一覧の絞り込み内でチャンネルを選ぶ ===')
await page.goto(`${URL_BASE}/recordings`)
await report('録画一覧の絞り込みでチャンネル選択をシート内に進め、戻ると全局へ戻る', async () => {
  const filters = await openFilterSheet(page)
  await filters.getByRole('button', { name: 'チャンネル: すべて' }).click()
  const channels = page.getByRole('dialog', { name: 'チャンネル' })
  await channels.waitFor({ state: 'visible' })
  const dialogCount = await page.getByRole('dialog').count()
  if (dialogCount !== 1) throw new Error(`シート内の画面遷移後に dialog が ${dialogCount} 個ある`)
  if ((await page.getByRole('button', { name: '絞り込みに戻る' }).count()) !== 1) {
    throw new Error('「‹ 絞り込み」へ戻る操作が無い')
  }
  const nhk = channels.getByRole('checkbox', { name: /NHK総合/ })
  await channels.getByRole('checkbox', { name: 'すべて' }).click()
  await nhk.click()
  await nhk.click()
  await channels.getByRole('status').filter({ hasText: '1 つ以上選んでください' }).waitFor({ state: 'visible', timeout: 5000 })
  await page.getByRole('button', { name: '絞り込みに戻る' }).click()
  const returned = page.getByRole('dialog', { name: '絞り込み' })
  await returned.waitFor({ state: 'visible' })
  await returned.getByRole('button', { name: 'チャンネル: すべて' }).waitFor({ state: 'visible' })
  const dialogs = await page.getByRole('dialog').count()
  if (dialogs !== 1) throw new Error(`絞り込みへ戻った後に dialog が ${dialogs} 個ある`)
})

await report('録画一覧の絞り込みで選んだチャンネルは、戻っても「完了」で閉じても残る', async () => {
  await page.goto(`${URL_BASE}/recordings`)
  const filters = await openFilterSheet(page)
  await filters.getByRole('button', { name: 'チャンネル: すべて' }).click()
  const channels = page.getByRole('dialog', { name: 'チャンネル' })
  await channels.waitFor({ state: 'visible' })
  await channels.getByRole('checkbox', { name: 'すべて' }).click()
  await channels.getByRole('checkbox', { name: /NHK総合/ }).click()
  await page.getByRole('button', { name: '絞り込みに戻る' }).click()
  await filters.getByRole('button', { name: 'チャンネル: NHK総合' }).waitFor({ state: 'visible', timeout: 5000 })
  await filters.getByRole('button', { name: '完了' }).first().click()
  await filters.waitFor({ state: 'detached' })
  const reopened = await openFilterSheet(page)
  await reopened.getByRole('button', { name: 'チャンネル: NHK総合' }).waitFor({ state: 'visible', timeout: 5000 })
  await page.keyboard.press('Escape')
})

log('\n=== 390px: シリーズ一覧でも同じ絞り込みを使う ===')
await page.goto(`${URL_BASE}/series`)
await report('シリーズ一覧の絞り込みに同じチャンネル行がある', async () => {
  const filters = await openFilterSheet(page)
  const row = filters.getByRole('button', { name: 'チャンネル: すべて' })
  await row.waitFor({ state: 'visible' })
  await row.click()
  const channels = page.getByRole('dialog', { name: 'チャンネル' })
  await channels.waitFor({ state: 'visible' })
  const dialogCount = await page.getByRole('dialog').count()
  if (dialogCount !== 1) throw new Error(`シリーズのシート内の画面遷移後に dialog が ${dialogCount} 個ある`)
  await channels.getByRole('checkbox', { name: /NHK総合/ }).waitFor({ state: 'visible' })
})
await page.keyboard.press('Escape')

log('\n=== 1280px: 番組表のチャンネル選択はポップオーバー ===')
await page.setViewportSize({ width: 1280, height: 800 })
await page.goto(`${URL_BASE}/programs`)
await report('デスクトップのチャンネル選択は画面下端に接しない', async () => {
  await page.getByRole('button', { name: 'チャンネル: すべて' }).click()
  const picker = page.getByRole('dialog', { name: 'チャンネル' })
  await picker.waitFor({ state: 'visible' })
  const bounds = await picker.evaluate((element) => element.getBoundingClientRect().bottom)
  const viewport = page.viewportSize()
  if (bounds >= viewport.height - 100) {
    throw new Error(`popover bottom=${bounds.toFixed(1)}px, viewport=${viewport.height}px`)
  }
})

await finish(ng, browser)
