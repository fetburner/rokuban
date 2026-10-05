/**
 * alert の同じ文言が複数あるとき、テストを失敗させる。
 * setup.ts の後に cleanup を登録するテストからも呼び出す。
 */
export function assertNoDuplicateAlertText(): void {
  const alertCounts = new Map<string, number>()
  for (const alert of document.body.querySelectorAll<HTMLElement>('[role="alert"]')) {
    const text = alert.textContent?.replace(/\s+/g, ' ').trim() ?? ''
    alertCounts.set(text, (alertCounts.get(text) ?? 0) + 1)
  }

  const duplicates = [...alertCounts]
    .filter(([, count]) => count > 1)
    .map(([text]) => JSON.stringify(text))

  if (duplicates.length > 0) {
    throw new Error(`Duplicate role="alert" text: ${duplicates.join(', ')}`)
  }
}
