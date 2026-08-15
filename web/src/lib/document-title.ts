/**
 * pageTitle は画面名を `document.title` の形式に合成する（issue #304）。
 *
 * サイドバー/ボトムタブのブランド表記（`components/app-shell.tsx`）と
 * `index.html` の既定タイトルはどちらも「録番」（日本語）を使っている。
 * `package.json` 等の英語表記「Rokuban」ではなくこちらに揃える —
 * ユーザーが実際に UI で見る名前と、タブに出る名前を一致させるため。
 */
export function pageTitle(screenName: string): string {
  return `${screenName} · 録番`
}
