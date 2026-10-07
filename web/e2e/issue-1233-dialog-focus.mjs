// AlertDialog の初期フォーカスと Return / Tab / Escape の実ブラウザ判定（issue #1233）。
// jsdom ではブラウザのフォーカス移動と既定キー操作を測れないため、実際のルール削除
// ダイアログで確認する。API はブラウザ側で差し替え、DB や mirakc は使わない。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/issue-1233-dialog-focus.mjs

import { ListRulesResponseItem } from '../src/api/zod.ts'
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
const rule = {
  id: 1233,
  name: 'フォーカス確認ルール',
  enabled: true,
  priority: 1,
  keepOriginal: 'always',
  createdAt: '2026-10-07T00:00:00Z',
  updatedAt: '2026-10-07T00:00:00Z',
}
const ng = []

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/rules') return json([rule])
  if (path === '/api/reservations' || path === '/api/capacity/overages') return json([])
  return json([])
}

async function openDeleteDialog(page) {
  const trigger = page.getByRole('button', {
    name: `ルール「${rule.name}」のその他の操作`,
  })
  await trigger.click()
  await page.getByRole('menuitem', { name: '削除', exact: true }).click()
  const dialog = page.getByRole('alertdialog', {
    name: `ルール「${rule.name}」を削除しますか？`,
  })
  await dialog.waitFor({ timeout: 15000 })
  // Popup は menu の閉じるアニメーションと並行して開く。実際に alertdialog 内へ
  // フォーカスが移ってから測ることで、初期フォーカスの未適用を空虚に成功させない。
  await page.waitForFunction(
    () => {
      const popup = document.querySelector('[role="alertdialog"]')
      return popup?.contains(document.activeElement) ?? false
    },
    null,
    { timeout: 15000 },
  ).catch(() => {
    ng.push('alertdialog を開いても、フォーカスがダイアログ内へ移らない')
  })
  return { trigger, dialog }
}

async function isFocused(page, locator) {
  return locator.evaluate((element) => element === document.activeElement)
}

async function focusedElementDescription(page) {
  return page.evaluate(() => {
    const element = document.activeElement
    return element instanceof HTMLElement
      ? `${element.tagName.toLowerCase()} role=${element.getAttribute('role') ?? ''} slot=${element.getAttribute('data-slot') ?? ''} text=${(element.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 40)}`
      : '(none)'
  })
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit([['rule', ListRulesResponseItem, rule]], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)
await page.goto(`${URL_BASE}/rules`, { waitUntil: 'domcontentloaded' })
await page.getByRole('button', {
  name: `ルール「${rule.name}」のその他の操作`,
}).waitFor({ timeout: 15000 })

log('\n=== ① 開いた直後はダイアログ本体にフォーカスする ===')
const first = await openDeleteDialog(page)
log(`  active: ${await focusedElementDescription(page)}`)
if (!(await isFocused(page, first.dialog))) {
  ng.push('① 開いた直後のフォーカスが alertdialog 本体にない')
}

log('\n=== ② Tab で最初にキャンセルへ進み、Escape で起動ボタンへ戻る ===')
await page.keyboard.press('Tab')
log(`  after Tab: ${await focusedElementDescription(page)}`)
const cancel = first.dialog.getByRole('button', { name: 'キャンセル', exact: true })
if (!(await isFocused(page, cancel))) {
  ng.push('② alertdialog 本体から Tab した後、キャンセルにフォーカスがない')
}
await page.keyboard.press('Escape')
await first.dialog.waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
  ng.push('② Escape で alertdialog が閉じない')
})
if (!(await isFocused(page, first.trigger))) {
  ng.push('② Escape で閉じた後、起動ボタンにフォーカスが戻らない')
}

log('\n=== ③ 開いた直後の Return ではダイアログを閉じない ===')
const second = await openDeleteDialog(page)
log(`  active: ${await focusedElementDescription(page)}`)
if (!(await isFocused(page, second.dialog))) {
  ng.push('③ Return を押す前のフォーカスが alertdialog 本体にない')
}
await page.keyboard.press('Enter')
await page.waitForTimeout(150)
log(`  after Return: ${await focusedElementDescription(page)} / dialog=${await second.dialog.isVisible().catch(() => false)}`)
if (!(await second.dialog.isVisible().catch(() => false))) {
  ng.push('③ 開いた直後に Return を押すと alertdialog が閉じる')
}
await page.keyboard.press('Escape')

await finish(ng, browser)
