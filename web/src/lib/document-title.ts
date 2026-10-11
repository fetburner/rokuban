/**
 * pageTitle は画面名を `document.title` の形式に合成する（issue #304）。
 *
 * サイドバー/ボトムタブ（`components/app-shell.tsx`）と `index.html` の既定タイトル、
 * 画面名付きタイトルはいずれも製品名 `Rokuban` を使う。
 */
export function pageTitle(screenName: string): string {
  return `${screenName} · Rokuban`
}
