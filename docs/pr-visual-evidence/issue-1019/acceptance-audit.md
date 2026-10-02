# Issue #1019 acceptance audit

This audit supplements the passing #1019 E2E, component tests, and visual captures. Each temporary mutant below was applied alone, tested against the named real component or browser path, and reverted. The branch source was clean again after the audit.

## Pre-implementation browser RED

Built parent `5a6934e1735986c156575c8d72c2461c10c9113f` in a detached worktree, copied the #1019 `web/e2e/chapters.mjs` into that worktree, and ran `E2E_URL=http://127.0.0.1:4220 corepack pnpm e2e:chapters`. Playback checks ⓪–③ completed. The run then failed at the chapter-edit entry:

```text
locator.click: Timeout 30000ms exceeded
waiting for getByRole('menuitem', { name: 'チャプターを直す', exact: true })
at web/e2e/chapters.mjs:447
```

The parent has no dedicated chapter-edit entry or filmstrip; the later editor assertions cannot be reached before the implementation.

## Mutation checks

| Contract | Temporary mutation | Check that failed |
|---|---|---|
| Tile timestamp → x placement | Added 2 percentage points to rendered tile `leftPercent` in `RecordingChapterFilmstrip`. | Browser `e2e:chapters` failed the 30-second tile position assertion: actual x `493.109375`, expected x `472` (tolerance 2 px). No other E2E check failed. |
| Select a boundary | Replaced the editor’s filmstrip selection callback with a no-op. | `recording-chapter-editor.test.tsx`, `区間カードから近い境界へ移動…`, failed the selected boundary `aria-pressed` assertion. |
| Nudge by one frame | Made `nudgeSelected` ignore deltas below 40 ms. | The same component test failed because the `10033ms` boundary was absent after +1 frame. |
| Send the adjusted draft in PUT | Subtracted 33 ms from the first span only in the page’s real `chapter-edits` mutation payload. | Browser `e2e:chapters` failed the captured request-body assertion: the submitted start was `30000ms`, expected `30033ms`; the rest of the browser flow completed. |
| Discard a draft | Removed `setDraft(spans)` from `discardDraft`. | `recording-chapter-editor.test.tsx`, `キャンセル命令は dirty draft を捨てる`, failed because `0:00:09` remained instead of restoring `0:00:10`. |
| Guard route navigation | Changed the router blocker predicate to always return false. | `recording-detail.test.tsx`, `未保存の編集から次の回へ移ると確認し…`, failed because `chapter-exit-confirmation` did not appear. |
| Edit without tiles | Disabled the +1-frame control when `tilesAvailable` was false. | `recording-chapter-editor.test.tsx`, `タイルが利用できなくても境界編集は残る`, failed because the control was disabled. |
| Suppress auto-skip while editing | Removed `chapterEditing` from the spans supplied to playback skip handling. | `recording-player.test.tsx`, `編集モードではCM自動スキップと通常目盛りを止め…`, failed: video time was `20` instead of `10.1`. |

## Rough comparison for the PR

The issue comment’s roughs show only the editor surface. These browser captures include the app shell and its reconnect banner. The desktop capture is 1280 × 800 CSS px; the phone capture is 400 × 800 CSS px. The main order matches: desktop player left, chapter cards right, full-width strip below; mobile player, strip and tuning controls above the independently scrolling chapter list. The narrower phone viewport wraps the tuning controls, and the app’s bottom navigation remains visible.

| Desktop rough | Desktop implementation |
|---|---|
| ![Desktop rough](https://raw.githubusercontent.com/fetburner/rokuban/de224e356ad19bd8f458702ad49a5cec4df35cc3/mocks/recording-detail/4-chapter-edit-desktop.png) | ![Desktop implementation](https://raw.githubusercontent.com/fetburner/rokuban/codex/issue-1019-stacked/docs/pr-visual-evidence/issue-1019/desktop.png) |

| Phone rough | Phone implementation |
|---|---|
| ![Phone rough](https://raw.githubusercontent.com/fetburner/rokuban/de224e356ad19bd8f458702ad49a5cec4df35cc3/mocks/recording-detail/5-chapter-edit-phone.png) | ![Phone implementation](https://raw.githubusercontent.com/fetburner/rokuban/codex/issue-1019-stacked/docs/pr-visual-evidence/issue-1019/smartphone.png) |
