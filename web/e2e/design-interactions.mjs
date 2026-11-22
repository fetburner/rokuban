// やること:
//   ④ モバイルの「その他」シート（固定されたボトムバーとは別の Dialog なので、
//      画面下端への接地と高さは jsdom では原理的に測れない。docs/frontend/shell.md）:
//      - ボトムタブが常に 4 個か
//      - 開いたシートの下端が画面に接し、画面の 60% より低い高さか
//      - Tab / Shift+Tab がシート内の操作要素を循環するか
//   ④-A キーボード操作と標的サイズ:
//      - Tab 1 回でスキップリンクが見え、Enter で main にフォーカスが移るか
//      - Chip / 録画タブ / チャンネル候補 / 日付セルの focus-visible リングが
//        Button と同じ --ring の実画素で出るか
//      - Button size="sm" と容量不足バッジの当たり判定が 24px 以上か
//      - 主要 6 画面の button / link / input / select 等を実測し、24px 未満を
//        ポインタ別に列挙して落とすか（標的間の実際の間隔も併記する）
//   ⑤ 録画一覧の行リンクを Enter で開いて詳細（/recordings/$id）へ遷移し、
//      詳細でキーボードの Tab だけで `<video>` に到達できるか（`tabIndex={-1}` を
//      付けると jsdom の focus spy は通り続けるが実ブラウザの Tab 走査から外れる）
//   ⑥ 共通 Button のフォーカスリング / border-color は遷移しない・hover の
//      色と active の押下フィードバックは遷移する（issue #294）
//   ⑦ アニメーション/トランジションが `prefers-reduced-motion: reduce` で
//      縮退し、既定（no-preference）では従来どおり動くこと（両方向。
//      issue #296）。Skeleton の `animate-pulse` / Button の `translate` 遷移を見る

import path from 'node:path'
import { FIXED_NOW, apiHandler, fixturePairs, toastLayoutProgram } from './design-fixtures.mjs'
import {
  URL_BASE,
  checkMissingStrings,
  computedOf,
  computedVar,
  createOpen,
  desktop,
  homeWatchScreen,
  mobile,
  prepareScreenshotDir,
  recordingDetailScreen,
  sameRgb,
  screenOf,
  themes,
} from './design-support.mjs'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const ng = []

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(fixturePairs, ng)

const OUT_DIR = prepareScreenshotDir('design-interactions')

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const open = createOpen(browser, ng)

const INTERACTIVE_TARGET_SELECTOR =
  'button, a[href], [role="button"], [role="switch"], input, select, summary'
const INTERACTIVE_TARGET_MIN_PX = 24
const targetScreenNames = ['programs', 'search', 'reservations', 'recordings', 'series', 'rules', 'live', 'series-hub']
const targetScreens = [...targetScreenNames.map(screenOf), recordingDetailScreen]
const targetPointerProfiles = [
  { name: 'fine', pointer: 'fine', viewport: desktop },
  { name: 'coarse', pointer: 'coarse', viewport: mobile },
]
const TAP_TARGET_SELECTOR =
  'a[href], button, [role="button"], [role="tab"], [role="menuitem"], [role="menuitemcheckbox"], [role="menuitemradio"], [role="option"], [role="switch"], [role="checkbox"], input:not([type="hidden"]), select, summary'
const TAP_TARGET_MIN_PX = 44
const tapTargetWidths = [
  { name: 'mobile-360', width: 360, height: 844 },
  { name: 'mobile-390', width: 390, height: 844 },
]
const tapTargetScreens = [
  homeWatchScreen,
  screenOf('home'),
  screenOf('programs'),
  screenOf('reservations'),
  screenOf('recordings'),
  recordingDetailScreen,
  screenOf('rules'),
  screenOf('series'),
  screenOf('series-hub'),
  screenOf('search'),
  screenOf('live'),
]

/**
 * 実ブラウザで描画された操作標的を列挙する。
 *
 * `getBoundingClientRect()` は element 自身の矩形しか返さないため、祖先の
 * overflow で実際には隠れている操作列をクリップする。容量不足バッジのように
 * 見た目を変えず `::before` だけで当たり判定を広げる実装も、擬似要素の実寸を
 * hit 寸法へ加える。jsdom の DOM 属性や class 名ではなく、ブラウザがレイアウト
 * した値だけを判定へ使う。
 */
async function measureInteractiveTargets(page, label) {
  const result = await page.evaluate(
    ({ selector }) => {
      const clippedRect = (element) => {
        const raw = element.getBoundingClientRect()
        let left = raw.left
        let right = raw.right
        let top = raw.top
        let bottom = raw.bottom

        for (let parent = element.parentElement; parent !== null; parent = parent.parentElement) {
          const style = getComputedStyle(parent)
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowX) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const rect = parent.getBoundingClientRect()
            left = Math.max(left, rect.left)
            right = Math.min(right, rect.right)
          }
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowY) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const rect = parent.getBoundingClientRect()
            top = Math.max(top, rect.top)
            bottom = Math.min(bottom, rect.bottom)
          }
        }

        const width = right - left
        const height = bottom - top
        if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) {
          return null
        }
        return { left, right, top, bottom, width, height }
      }

      const isHidden = (element) => {
        for (let current = element; current !== null; current = current.parentElement) {
          const style = getComputedStyle(current)
          if (
            style.display === 'none' ||
            style.visibility === 'hidden' ||
            style.visibility === 'collapse'
          ) {
            return true
          }
        }
        return false
      }

      const isVisuallyHidden = (element) => {
        const style = getComputedStyle(element)
        return (
          (element.classList.contains('sr-only') && !element.matches(':focus')) ||
          (style.width === '1px' &&
            style.height === '1px' &&
            style.overflow === 'hidden' &&
            style.clip !== 'auto' &&
            style.clip !== 'none')
        )
      }

      const pseudoExtent = (element, pseudo) => {
        const style = getComputedStyle(element, pseudo)
        if (
          style.content === 'none' ||
          style.display === 'none' ||
          style.visibility === 'hidden' ||
          !['absolute', 'fixed'].includes(style.position)
        ) {
          return { width: 0, height: 0 }
        }
        const width = Number.parseFloat(style.width)
        const height = Number.parseFloat(style.height)
        return {
          width: Number.isFinite(width) ? width : 0,
          height: Number.isFinite(height) ? height : 0,
        }
      }

      const accessibleLabel = (element) => {
        const candidates = [
          element.getAttribute('aria-label'),
          element.getAttribute('placeholder'),
          element.getAttribute('title'),
          element.getAttribute('data-testid'),
          element.textContent?.replace(/\s+/g, ' ').trim(),
        ]
        return (candidates.find((value) => value !== null && value !== '') ?? '(無名)').slice(0, 80)
      }

      const targets = Array.from(document.querySelectorAll(selector))
        .filter((element) => {
          if (element instanceof HTMLInputElement && element.type === 'hidden') return false
          // Skip link はキーボードフォーカス時にだけ通常サイズへ戻る。常時
          // sr-only の矩形を標的サイズの失敗にせず、既存の④-Aで Tab 後の
          // 実寸（かつ main への到達）を別途固定する。
          if (isVisuallyHidden(element)) return false
          return !isHidden(element)
        })
        .map((element) => {
          const rect = clippedRect(element)
          if (rect === null) return null
          const before = pseudoExtent(element, '::before')
          const after = pseudoExtent(element, '::after')
          const role = element.getAttribute('role')
          const position = getComputedStyle(element).position
          return {
            tag: element.tagName.toLowerCase(),
            role,
            label: accessibleLabel(element),
            visualWidth: rect.width,
            visualHeight: rect.height,
            hitWidth: Math.max(rect.width, before.width, after.width),
            hitHeight: Math.max(rect.height, before.height, after.height),
            left: rect.left,
            right: rect.right,
            top: rect.top,
            bottom: rect.bottom,
            overlay: position === 'absolute' || position === 'fixed',
          }
        })
        .filter((target) => target !== null)

      const edgeGap = (a, b) => {
        const horizontal = Math.max(a.left - b.right, b.left - a.right, 0)
        const vertical = Math.max(a.top - b.bottom, b.top - a.bottom, 0)
        return Math.hypot(horizontal, vertical)
      }

      let minimumGap = null
      let overlapPairs = 0
      for (let i = 0; i < targets.length; i += 1) {
        for (let j = i + 1; j < targets.length; j += 1) {
          const gap = edgeGap(targets[i], targets[j])
          if (minimumGap === null || gap < minimumGap) minimumGap = gap
          if (gap === 0) overlapPairs += 1
        }
      }

      return { targets, minimumGap, overlapPairs }
    },
    { selector: INTERACTIVE_TARGET_SELECTOR },
  )

  const undersized = result.targets.filter(
    (target) =>
      target.hitWidth < INTERACTIVE_TARGET_MIN_PX || target.hitHeight < INTERACTIVE_TARGET_MIN_PX,
  )
  log(
    `  [${label}] 操作標的=${result.targets.length} 件 ` +
      `最小間隔=${result.minimumGap === null ? '—' : `${result.minimumGap.toFixed(1)}px`} ` +
      `重なり=${result.overlapPairs} 件 ` +
      `判定=${undersized.length === 0 ? 'OK' : 'NG'}`,
  )
  for (const target of result.targets) {
    log(
      `    ${target.tag}${target.role === null ? '' : `[role=${target.role}]`} 「${target.label}」 ` +
        `visual=${target.visualWidth.toFixed(1)}×${target.visualHeight.toFixed(1)}px ` +
        `hit=${target.hitWidth.toFixed(1)}×${target.hitHeight.toFixed(1)}px`,
    )
  }
  for (const target of undersized) {
    ng.push(
      `[${label}] 「${target.label}」の操作標的が ${target.hitWidth.toFixed(1)}×${target.hitHeight.toFixed(1)}px ` +
        `(基準 ${INTERACTIVE_TARGET_MIN_PX}×${INTERACTIVE_TARGET_MIN_PX}px 未満)`,
    )
  }
  if (result.targets.length === 0) {
    ng.push(`[${label}] 操作標的を 1 件も列挙できない`)
  }
  return result
}

/**
 * measureCoarseTapTargets はモバイルの操作標的を実際の hit-test で確認する。
 *
 * 各標的の中心と四辺の近傍を `elementFromPoint` で調べるため、見た目の
 * bounding box が小さくても `::before` / `::after` で広げた当たり判定を測れる。
 * さらに中心に置いた 44×44px の最低限の hit 領域どうしが交差しないかを確認する。
 * DOM の class や擬似要素の宣言だけで通さず、ブラウザが実際にどの要素へ配送するかを使う。
 */
async function measureCoarseTapTargets(page, label, scope = page.locator('body'), options = {}) {
  const result = await scope.evaluate(
    async (root, { selector, skipSelectionCheckbox, minTargetPx }) => {
      const hidden = (element) => {
        const summary = element.closest('summary')
        for (let current = element; current; current = current.parentElement) {
          const style = getComputedStyle(current)
          if (
            style.display === 'none' ||
            style.visibility === 'hidden' ||
            style.visibility === 'collapse'
          ) {
            return true
          }
          if (
            current instanceof HTMLDetailsElement &&
            !current.open &&
            summary?.parentElement !== current
          ) {
            return true
          }
        }
        return false
      }

      const visuallyHidden = (element) => {
        const style = getComputedStyle(element)
        return (
          (element.classList.contains('sr-only') && !element.matches(':focus')) ||
          (style.width === '1px' &&
            style.height === '1px' &&
            style.overflow === 'hidden' &&
            style.clip !== 'auto' &&
            style.clip !== 'none')
        )
      }

      const nameOf = (element) => {
        const candidates = [
          element.getAttribute('aria-label'),
          element.getAttribute('placeholder'),
          element.getAttribute('title'),
          element.getAttribute('data-testid'),
          element.textContent?.replace(/\s+/g, ' ').trim(),
        ]
        return (candidates.find((value) => value !== null && value !== '') ?? '(無名)').slice(0, 80)
      }

      const pseudoExtent = (element, pseudo) => {
        const style = getComputedStyle(element, pseudo)
        if (
          style.content === 'none' ||
          style.display === 'none' ||
          style.visibility === 'hidden' ||
          !['absolute', 'fixed'].includes(style.position)
        ) {
          return { width: 0, height: 0 }
        }
        const width = Number.parseFloat(style.width)
        const height = Number.parseFloat(style.height)
        return {
          width: Number.isFinite(width) ? width : 0,
          height: Number.isFinite(height) ? height : 0,
        }
      }

      const targets = []
      let ignoredSortSelects = 0
      let ignoredSelectionCheckboxes = 0
      for (const element of root.querySelectorAll(selector)) {
        if (element instanceof HTMLInputElement && element.type === 'hidden') continue
        if (element.matches(':disabled, [aria-disabled="true"], [data-disabled]')) continue
        if (hidden(element) || visuallyHidden(element)) continue
        // Field / Select の <label> は入力を包み、ラベル文字列を押しても入力へ
        // 操作が届く。ブラウザが label 自体を hit-test するので、入力の外形では
        // なく、その label を実効的な hit surface として測る。
        const surface =
          (element instanceof HTMLInputElement || element instanceof HTMLSelectElement) &&
          element.closest('label')
            ? element.closest('label')
            : element
        // 一覧の下端や長い設定ページの項目も、実際に見える位置へ送って測る。
        // fixed bottom nav の後ろに隠れたまま計ると、スクロールすれば届く操作まで
        // 「押せない」と誤判定する。overflow で閉じた操作列は後段の clipping で除く。
        surface.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' })
        await new Promise(requestAnimationFrame)
        if (!surface.isConnected) continue

        const rect = surface.getBoundingClientRect()
        let left = rect.left
        let right = rect.right
        let top = rect.top
        let bottom = rect.bottom
        for (let parent = surface.parentElement; parent; parent = parent.parentElement) {
          const style = getComputedStyle(parent)
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowX) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const parentRect = parent.getBoundingClientRect()
            left = Math.max(left, parentRect.left)
            right = Math.min(right, parentRect.right)
          }
          if (
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflowY) ||
            ['hidden', 'clip', 'auto', 'scroll'].includes(style.overflow)
          ) {
            const parentRect = parent.getBoundingClientRect()
            top = Math.max(top, parentRect.top)
            bottom = Math.min(bottom, parentRect.bottom)
          }
        }
        const clippedWidth = right - left
        const clippedHeight = bottom - top
        if (clippedWidth <= 0 || clippedHeight <= 0) {
          continue
        }
        const before = pseudoExtent(surface, '::before')
        const after = pseudoExtent(surface, '::after')

        // 録画の並び順 select は #1220 がアイコン化する。ここでは寸法を変えず、
        // その作業へ責務を移す明示例外にする。
        if (element.matches('select[aria-label="並び順"]')) {
          ignoredSortSelects++
          continue
        }

        // 編集モードでは role=option の行全体が click で選択を反転することを
        // 下の browser check で確認する。その場合 checkbox は同じ action の
        // 入力部品なので、行の大きな hit 領域を代表として数える。
        if (
          skipSelectionCheckbox &&
          element instanceof HTMLInputElement &&
          element.type === 'checkbox' &&
          element.closest('[role="option"]')
        ) {
          ignoredSelectionCheckboxes++
          continue
        }

        const rectCenterX = (left + right) / 2
        const rectCenterY = (top + bottom) / 2
        // 端から半 device pixel 内側を測る。いまの実ブラウザ context は DPR=2 なので
        // 21.75px は 44px target の最後の device-pixel center、43.5px target の
        // half-open な境界上になる。±22 の端そのものは外側なので使わない。
        const outerOffset = minTargetPx / 2 - 1 / (2 * devicePixelRatio)
        const rasterEdgeOffset = minTargetPx / 2 - 0.5
        const interiorOffsets = [-14, -7, 0, 7, 14]
        const pointsAt = (x, y) => [
          ...interiorOffsets.flatMap((offsetY) =>
            interiorOffsets.map((offsetX) => [x + offsetX, y + offsetY]),
          ),
          [x - outerOffset, y],
          [x + outerOffset, y],
          [x, y - outerOffset],
          [x, y + outerOffset],
        ]
        const missesAt = (x, y) => {
          return pointsAt(x, y).filter(([px, py]) => {
            if (px < 0 || py < 0 || px >= innerWidth || py >= innerHeight) return true
            const hit = document.elementFromPoint(px, py)
            if (hit !== null && (hit === surface || surface.contains(hit))) return false

            // elementFromPoint は CSSOM の整数座標へ丸める。±21.75 は最後の
            // half-device-pixel center でも丸め後に矩形端へ落ちるブラウザがある。
            // その場合だけ、最後に安定して表現できる pixel center (±21.5) でも
            // 同じ標的に届くことと、実寸が44px以上であることを併せて確認する。
            const dx = px - x
            const dy = py - y
            const isNearEdge =
              (Math.abs(Math.abs(dx) - outerOffset) < 0.001 && Math.abs(dy) < 0.001) ||
              (Math.abs(Math.abs(dy) - outerOffset) < 0.001 && Math.abs(dx) < 0.001)
            if (
              isNearEdge &&
              Math.max(clippedWidth, before.width, after.width) >= minTargetPx &&
              Math.max(clippedHeight, before.height, after.height) >= minTargetPx
            ) {
              const fallbackX = x + (dx === 0 ? 0 : Math.sign(dx) * rasterEdgeOffset)
              const fallbackY = y + (dy === 0 ? 0 : Math.sign(dy) * rasterEdgeOffset)
              const fallbackHit = document.elementFromPoint(fallbackX, fallbackY)
              return fallbackHit === null || (fallbackHit !== surface && !surface.contains(fallbackHit))
            }
            return true
          })
        }
        let centerX = rectCenterX
        let centerY = rectCenterY
        let misses = missesAt(centerX, centerY)

        // 行全面リンクは別リンクやボタンを意図的に前面へ重ねる。そのため矩形中央の
        // 一部だけ別宛先になることがある。44×44 全点がその行リンクへ届く位置が
        // 同じ矩形内にあるかを探し、行の利用可能な領域で合否を決める。
        // 小さな標的には位置ずらしを許さない。擬似要素で中心を広げた実効 hit area
        // を elementFromPoint で測る目的があるため。
        if (clippedWidth >= 44 && clippedHeight >= 44 && misses.length > 0) {
          const maxOffsetX = Math.max(0, Math.floor((clippedWidth - minTargetPx) / 2))
          const maxOffsetY = Math.max(0, Math.floor((clippedHeight - minTargetPx) / 2))
          const offsets = [0]
          for (let offset = 8; offset <= Math.max(maxOffsetX, maxOffsetY); offset += 8) {
            offsets.push(-offset, offset)
          }
          let best = { x: centerX, y: centerY, misses }
          search: for (const dy of offsets) {
            for (const dx of offsets) {
              if (dx === 0 && dy === 0) continue
              if (Math.abs(dx) > maxOffsetX || Math.abs(dy) > maxOffsetY) continue
              const candidate = { x: rectCenterX + dx, y: rectCenterY + dy }
              const candidateMisses = missesAt(candidate.x, candidate.y)
              if (candidateMisses.length < best.misses.length) {
                best = { ...candidate, misses: candidateMisses }
              }
              if (candidateMisses.length === 0) {
                best = { ...candidate, misses: candidateMisses }
                break search
              }
            }
          }
          centerX = best.x
          centerY = best.y
          misses = best.misses
        }
        targets.push({
          tag: element.tagName.toLowerCase(),
          role: element.getAttribute('role'),
          label: nameOf(element),
          centerX,
          centerY,
          documentCenterX: centerX + scrollX,
          documentCenterY: centerY + scrollY,
          href: surface.getAttribute('href'),
          pointerEvents: getComputedStyle(surface).pointerEvents,
          visualWidth: clippedWidth,
          visualHeight: clippedHeight,
          hitWidth: Math.max(clippedWidth, before.width, after.width),
          hitHeight: Math.max(clippedHeight, before.height, after.height),
          missCount: misses.length,
          testedCount: pointsAt(centerX, centerY).length,
          misses: misses.slice(0, 3).map(([x, y]) => {
            const hit = document.elementFromPoint(x, y)
            const describe = (node) => node === null ? '(viewport 外)' : `${node.tagName.toLowerCase()}${node.getAttribute('role') ? `[role=${node.getAttribute('role')}]` : ''}${node.id ? `#${node.id}` : ''}${node.getAttribute('data-testid') ? `[data-testid=${node.getAttribute('data-testid')}]` : ''}${node.className && typeof node.className === 'string' ? `.${node.className.trim().replace(/\s+/g, '.')}` : ''}`
            return {
              x: Number(x.toFixed(1)),
              y: Number(y.toFixed(1)),
              hit: describe(hit),
              hitHref: hit?.closest('a[href]')?.getAttribute('href') ?? null,
            }
          }),
        })
      }

      const overlaps = []
      for (let i = 0; i < targets.length; i++) {
        for (let j = i + 1; j < targets.length; j++) {
          const a = targets[i]
          const b = targets[j]
          if (
            Math.abs(a.documentCenterX - b.documentCenterX) < minTargetPx &&
            Math.abs(a.documentCenterY - b.documentCenterY) < minTargetPx
          ) {
            overlaps.push({ first: a.label, second: b.label })
          }
        }
      }

      return { targets, overlaps, ignoredSortSelects, ignoredSelectionCheckboxes }
    },
    {
      selector: TAP_TARGET_SELECTOR,
      skipSelectionCheckbox: options.skipSelectionCheckbox === true,
      minTargetPx: TAP_TARGET_MIN_PX,
    },
  )

  const misses = result.targets.filter(
    (target) =>
      target.missCount > 0 ||
      target.hitWidth < TAP_TARGET_MIN_PX ||
      target.hitHeight < TAP_TARGET_MIN_PX,
  )
  log(
    `  [${label}] 操作標的=${result.targets.length} 件 ` +
      `44×44 の hit 判定 NG=${misses.length} 件 重なり=${result.overlaps.length} 件`,
  )
  if (result.ignoredSortSelects > 0) {
    log(`    例外: 録画一覧の「並び順」select ${result.ignoredSortSelects} 件（#1220 がアイコン化）`)
  }
  if (result.ignoredSelectionCheckboxes > 0) {
    log(`    例外: 選択行の checkbox ${result.ignoredSelectionCheckboxes} 件（行クリックが同じ切替を担う）`)
  }
  for (const target of misses) {
    ng.push(
      `[${label}] ${target.tag}${target.role === null ? '' : `[role=${target.role}]`} 「${target.label}」の中心 44×44px の hit 領域が要素に届かない ` +
        `(center=${target.centerX.toFixed(1)},${target.centerY.toFixed(1)}; visual=${target.visualWidth.toFixed(1)}×${target.visualHeight.toFixed(1)}px; hit=${target.hitWidth.toFixed(1)}×${target.hitHeight.toFixed(1)}px; miss=${target.missCount}/${target.testedCount}; pointer=${target.pointerEvents}; href=${target.href ?? 'なし'}; ${JSON.stringify(target.misses)})`,
    )
  }
  for (const overlap of result.overlaps) {
    ng.push(`[${label}] 44×44px の hit 領域が重なる: 「${overlap.first}」 / 「${overlap.second}」`)
  }
  if (result.targets.length === 0) {
    ng.push(`[${label}] 操作標的を 1 件も列挙できない`)
  }
  return result
}

/**
 * ルールカードが 360 / 390px で名前を省略せず本文を全幅で縦積みし、補助操作が
 * muted 色 + 方向アイコンのリンク・ボタンとして読めることを測る。幅・実色・
 * アイコン位置は jsdom がレイアウトも CSS 変数の解決も持たないため測れない。
 */
async function runRuleCardLayoutChecks() {
  log('\n=== H-2: ルールカードのモバイル配置と補助導線 ===')

  const conditionText = '番組名に「連続テレビ小説」を含む'
  const checkActionCue = async (locator, { label, viewport, iconClass, placement }) => {
    if ((await locator.count()) !== 1) {
      ng.push(`[${viewport}/actions] 「${label}」が 1 件表示されない`)
      return
    }
    const cue = await locator.evaluate((element, { iconClass: requiredIconClass, placement: iconPlacement }) => {
      const rootStyle = getComputedStyle(element)
      const probe = document.createElement('span')
      probe.style.cssText = 'position:fixed;left:-10000px;color:var(--muted-foreground)'
      document.body.appendChild(probe)
      const mutedTokenColor = getComputedStyle(probe).color
      probe.remove()

      const icons = [...element.querySelectorAll(`svg.${requiredIconClass}`)]
      const icon = icons[0]
      const iconStyle = icon === undefined ? null : getComputedStyle(icon)
      const rootBox = element.getBoundingClientRect()
      const iconBox = icon?.getBoundingClientRect()
      const children = [...element.children]
      const iconIndex = children.findIndex((child) => child.classList.contains(requiredIconClass))
      const textIndex = children.findIndex((child) => !child.matches('svg') && child.textContent.trim() !== '')
      const leftGap = iconBox === undefined ? Number.POSITIVE_INFINITY : iconBox.left - rootBox.left
      const rightGap = iconBox === undefined ? Number.POSITIVE_INFINITY : rootBox.right - iconBox.right
      const visible =
        iconBox !== undefined &&
        iconBox.width >= 12 &&
        iconBox.height >= 12 &&
        iconStyle?.display !== 'none' &&
        iconStyle?.visibility === 'visible' &&
        Number(iconStyle?.opacity) > 0
      const orderCorrect =
        iconIndex >= 0 &&
        textIndex >= 0 &&
        (iconPlacement === 'leading' ? iconIndex < textIndex : iconIndex > textIndex)
      const geometryCorrect =
        iconBox !== undefined &&
        (iconPlacement === 'leading'
          ? leftGap >= 0 && leftGap <= 8 && iconBox.right < rootBox.left + rootBox.width / 2
          : rightGap >= 0 && rightGap <= 8 && iconBox.left > rootBox.left + rootBox.width / 2)
      return {
        actualColor: rootStyle.color,
        mutedTokenColor,
        backgroundColor: rootStyle.backgroundColor,
        borderWidths: [rootStyle.borderTopWidth, rootStyle.borderRightWidth, rootStyle.borderBottomWidth, rootStyle.borderLeftWidth],
        iconCount: icons.length,
        iconVisible: visible,
        iconSize: iconBox === undefined ? 'missing' : `${iconBox.width.toFixed(1)}×${iconBox.height.toFixed(1)}px`,
        orderCorrect,
        geometryCorrect,
      }
    }, { iconClass, placement })

    if (cue.actualColor !== cue.mutedTokenColor) {
      ng.push(
        `[${viewport}/actions] 「${label}」の実色が muted token と一致しない ` +
          `(${cue.actualColor} / ${cue.mutedTokenColor})`,
      )
    }
    if (cue.backgroundColor !== 'rgba(0, 0, 0, 0)' || cue.borderWidths.some((width) => width !== '0px')) {
      ng.push(`[${viewport}/actions] 「${label}」に常時の背景または枠がある`)
    }
    if (cue.iconCount !== 1 || !cue.iconVisible) {
      ng.push(`[${viewport}/actions] 「${label}」の ${iconClass} が見えないか 12px 未満 (${cue.iconSize})`)
    }
    if (!cue.orderCorrect) {
      ng.push(`[${viewport}/actions] 「${label}」で ${placement} アイコンが文字の外側にない`)
    }
    if (!cue.geometryCorrect) {
      ng.push(`[${viewport}/actions] 「${label}」のアイコンが ${placement} 側に収まっていない`)
    }
  }

  for (const viewport of tapTargetWidths) {
    for (const theme of themes) {
      const { context, page } = await open(viewport, theme, screenOf('rules'), { pointer: 'coarse' })
      const name = page.getByRole('link', { name: 'ルール「朝ドラ」を編集' })
      const condition = page.getByText(conditionText, { exact: true })
      const card = condition.locator('xpath=../../../..')
      const conditionArea = condition.locator('xpath=../..')
      const toggle = page.getByRole('switch', { name: 'ルール「朝ドラ」を有効にする' })
      const menu = page.getByRole('button', { name: 'ルール「朝ドラ」のその他の操作' })
      const recordings = card.getByRole('link', { name: 'このルールの録画', exact: true })
      const createLabel = card.getByRole('button', { name: 'このキーワードで分類ルールを作る', exact: true })
      const actionRow = recordings.locator('..')
      const metadata = card.getByText('優先度 10', { exact: true }).locator('xpath=..')
      await condition.waitFor({ timeout: 5000 }).catch(() => {})

      const cardBox = await card.boundingBox()
      const conditionBox = await conditionArea.boundingBox()
      const headerBoxes = await Promise.all([name.boundingBox(), toggle.boundingBox(), menu.boundingBox()])
      if (cardBox === null || conditionBox === null || headerBoxes.some((box) => box === null)) {
        ng.push(`[${viewport.name}/${theme}/rules-layout] ルールカードの見出しまたは条件欄を測れない`)
      } else {
        // カード内幅 = 枠 + 左右 padding を実測値で引く（px-3 + border 1px で 26px）。
        const insets = await card.evaluate((element) => {
          const style = getComputedStyle(element)
          return ['borderLeftWidth', 'borderRightWidth', 'paddingLeft', 'paddingRight'].reduce(
            (sum, key) => sum + Number.parseFloat(style[key]),
            0,
          )
        })
        const contentWidth = cardBox.width - insets
        log(
          `  [${viewport.name}/${theme}/rules-layout] 条件欄=${conditionBox.width.toFixed(1)}px / ` +
            `カード内幅=${contentWidth.toFixed(1)}px`,
        )
        if (conditionBox.width < contentWidth - 4) {
          ng.push(
            `[${viewport.name}/${theme}/rules-layout] 条件欄がカードの全幅でない ` +
              `(${conditionBox.width.toFixed(1)}px < ${(contentWidth - 4).toFixed(1)}px)`,
          )
        }
        // 名前は省略せず全文が出る（390px で「朝ド…」になった退行の再発防止）。
        // 省略するのは Button 内の span.truncate なので、wrapper でなくそれを測る。
        const nameText = name.locator('.truncate')
        const truncation = await nameText.evaluate((element) => ({
          text: element.textContent,
          scrollWidth: element.scrollWidth,
          clientWidth: element.clientWidth,
        }))
        if (truncation.scrollWidth > truncation.clientWidth) {
          ng.push(
            `[${viewport.name}/${theme}/rules-layout] ルール名「${truncation.text}」が省略されている ` +
              `(scrollWidth=${truncation.scrollWidth} > clientWidth=${truncation.clientWidth})`,
          )
        }
        const headerCenters = headerBoxes.map((box) => box.y + box.height / 2)
        if (Math.max(...headerCenters) - Math.min(...headerCenters) > 4) {
          ng.push(`[${viewport.name}/${theme}/rules-layout] 名前・スイッチ・「…」が同じ見出し行に揃っていない`)
        }
        const headerBottom = Math.max(...headerBoxes.map((box) => box.y + box.height))
        if (conditionBox.y < headerBottom - 1) {
          ng.push(`[${viewport.name}/${theme}/rules-layout] 条件欄が見出しより下に配置されていない`)
        }
        const [actionRowBox, metadataBox] = await Promise.all([actionRow.boundingBox(), metadata.boundingBox()])
        if (actionRowBox === null || metadataBox === null) {
          ng.push(`[${viewport.name}/${theme}/rules-layout] 条件の詳細と補助操作の段を測れない`)
        } else {
          if (actionRowBox.width < contentWidth - 4) {
            ng.push(
              `[${viewport.name}/${theme}/rules-layout] 補助操作の段が全幅でない ` +
                `(${actionRowBox.width.toFixed(1)}px < ${(contentWidth - 4).toFixed(1)}px)`,
            )
          }
          if (actionRowBox.y < metadataBox.y + metadataBox.height + 3) {
            ng.push(`[${viewport.name}/${theme}/rules-layout] 補助操作が条件の詳細より下の段にない`)
          }
        }
      }

      // 「録画予定 N 件」の 44px 当たり判定が行の高さを押し広げ、同じ行の文字より
      // 下がったり補助操作の段との間を空けたりしないこと。44px の箱ではなく文字自体を測る。
      const scheduled = card.getByRole('link', { name: /^録画予定 \d+ 件$/ })
      await scheduled.first().waitFor({ timeout: 5000 }).catch(() => {})
      if ((await scheduled.count()) !== 1) {
        ng.push(`[${viewport.name}/${theme}/rules-layout] 「録画予定 N 件」が 1 件表示されない`)
      } else {
        const rows = await metadata.evaluate((row, link) => {
          const textRect = (element) => {
            const range = document.createRange()
            range.selectNodeContents(element)
            return range.getBoundingClientRect()
          }
          const priority = textRect(row.querySelector('span'))
          const linkText = textRect(link)
          return {
            sameLine: linkText.top < priority.bottom,
            priorityCenter: priority.top + priority.height / 2,
            linkCenter: linkText.top + linkText.height / 2,
            linkTextBottom: linkText.bottom,
            rowBottom: row.getBoundingClientRect().bottom,
          }
        }, await scheduled.elementHandle())
        const centerDiff = Math.abs(rows.linkCenter - rows.priorityCenter)
        const bottomGap = rows.rowBottom - rows.linkTextBottom
        log(
          `  [${viewport.name}/${theme}/rules-layout] 録画予定と優先度の文字の縦中心差=${centerDiff.toFixed(1)}px / ` +
            `行下端と録画予定の文字下端の差=${bottomGap.toFixed(1)}px`,
        )
        // 折り返して別の行に落ちた場合は縦中心が離れて当然なので、中心差は同じ行のときだけ見る。
        if ((rows.sameLine && centerDiff > 2) || bottomGap > 8) {
          ng.push(
            `[${viewport.name}/${theme}/rules-layout] 「録画予定 N 件」が行内でずれている ` +
              `(優先度との縦中心差=${centerDiff.toFixed(1)}px > 2px / ` +
              `行下端との差=${bottomGap.toFixed(1)}px > 8px)`,
          )
        }
      }
      await checkActionCue(recordings, {
        label: 'このルールの録画', viewport: `${viewport.name}/${theme}`, iconClass: 'lucide-chevron-right', placement: 'trailing',
      })
      await checkActionCue(createLabel, {
        label: 'このキーワードで分類ルールを作る', viewport: `${viewport.name}/${theme}`, iconClass: 'lucide-plus', placement: 'leading',
      })
      await context.close()
    }
  }

  // desktop はカードの主情報と補助操作を横に分け、スイッチとメニューを右端に残す。
  {
    const viewport = { name: 'desktop-1280', width: 1280, height: 800 }
    const { context, page } = await open(viewport, 'light', screenOf('rules'), { pointer: 'fine' })
    const condition = page.getByText(conditionText, { exact: true })
    const card = condition.locator('xpath=../../../..')
    const conditionArea = condition.locator('xpath=../..')
    const toggle = page.getByRole('switch', { name: 'ルール「朝ドラ」を有効にする' })
    const menu = page.getByRole('button', { name: 'ルール「朝ドラ」のその他の操作' })
    const action = card.getByRole('link', { name: 'このルールの録画', exact: true })
    const [cardBox, conditionBox, toggleBox, menuBox, actionBox] = await Promise.all([
      card.boundingBox(), conditionArea.boundingBox(), toggle.boundingBox(), menu.boundingBox(), action.boundingBox(),
    ])
    if ([cardBox, conditionBox, toggleBox, menuBox, actionBox].some((box) => box === null)) {
      ng.push('[desktop-1280/rules-layout] 横並びのカード要素を測れない')
    } else {
      if (conditionBox.width >= cardBox.width - 100) {
        ng.push('[desktop-1280/rules-layout] 条件欄がカード全幅へ広がり、横並びを保っていない')
      }
      const menuRightGap = cardBox.x + cardBox.width - menuBox.x - menuBox.width
      log(`  [desktop-1280/rules-layout] メニュー右端とカード右端の間隔=${menuRightGap.toFixed(1)}px`)
      if (Math.abs(menuRightGap - 13) > 2) {
        ng.push(`[desktop-1280/rules-layout] メニューのカード右端余白が13pxでない（${menuRightGap.toFixed(1)}px）`)
      }
      const toggleCenter = toggleBox.y + toggleBox.height / 2
      const menuCenter = menuBox.y + menuBox.height / 2
      if (Math.abs(toggleCenter - menuCenter) > 4 || toggleBox.x + toggleBox.width > menuBox.x) {
        ng.push('[desktop-1280/rules-layout] スイッチとメニューが同じ右端の操作行に揃っていない')
      }
      if (actionBox.x < cardBox.x + cardBox.width / 2) {
        ng.push('[desktop-1280/rules-layout] 補助操作がカード右側に配置されていない')
      }
    }
    await context.close()
  }

  // 番組表の展開領域も、移動先を示す ChevronRight と muted の文字色を持つ。
  for (const viewport of tapTargetWidths) {
    for (const theme of themes) {
      const { context, page } = await open(viewport, theme, screenOf('programs'), { pointer: 'coarse' })
      const row = page.locator('li[data-program-id]').first()
      await row.waitFor({ timeout: 5000 }).catch(() => {})
      const expander = row.locator('button[aria-expanded]').first()
      if ((await expander.count()) === 0) {
        ng.push(`[${viewport.name}/${theme}/program-search-action] 番組行の展開ボタンが見つからない`)
        await context.close()
        continue
      }
      await expander.click()
      const link = row.getByRole('link', { name: 'この番組名で検索', exact: true })
      await link.waitFor({ timeout: 5000 }).catch(() => {})
      if ((await link.count()) !== 1) {
        ng.push(`[${viewport.name}/${theme}/program-search-action] 「この番組名で検索」が表示されない`)
      } else {
        await checkActionCue(link, {
          label: 'この番組名で検索',
          viewport: `${viewport.name}/${theme}`,
          iconClass: 'lucide-chevron-right',
          placement: 'trailing',
        })
      }
      await context.close()
    }
  }

  if (process.env.E2E_RULE_LAYOUT_SCREENSHOTS === '1') {
    const viewport = { name: 'mobile-390', width: 390, height: 844 }
    for (const theme of themes) {
      const { context, page } = await open(viewport, theme, screenOf('rules'), { pointer: 'coarse' })
      await page.screenshot({ path: path.join(OUT_DIR, `issue-1223-rules-${theme}-390.png`) })
      await context.close()

      const programs = await open(viewport, theme, screenOf('programs'), { pointer: 'coarse' })
      const row = programs.page.locator('li[data-program-id]').first()
      await row.waitFor({ timeout: 5000 }).catch(() => {})
      const expander = row.locator('button[aria-expanded]').first()
      if (await expander.count()) await expander.click()
      await programs.page.screenshot({ path: path.join(OUT_DIR, `issue-1223-programs-${theme}-390.png`) })
      await programs.context.close()
    }
  }
}

/** 360 / 390px の全主要画面を coarse pointer で開き、44px の hit 領域を測る。 */
async function runCoarseTapTargetChecks() {
  log('\n=== ④-A H-1: coarse pointer の 44×44px hit 領域 ===')
  for (const viewport of tapTargetWidths) {
    for (const screen of tapTargetScreens) {
      const screenOptions =
        screen.name === 'recording-detail'
          ? { pointer: 'coarse', multiSite: true, recordingDetailScenario: 'completed' }
          : { pointer: 'coarse', ...(screen.name === 'home' || screen.name === 'home-watch' ? { homeModeFixture: true } : {}) }
      const { context, page } = await open(viewport, 'light', screen, screenOptions)
      const pointerIsCoarse = await page.evaluate(() => matchMedia('(pointer: coarse)').matches)
      if (!pointerIsCoarse) {
        ng.push(`[${viewport.name}/${screen.name}] ブラウザが pointer: coarse と判定されない`)
      }
      await measureCoarseTapTargets(page, `${viewport.name}/${screen.name}`)
      if (screen.name === 'home') {
        const details = page.locator('details[data-testid="home-timeline-details"]')
        if ((await details.count()) === 0) {
          ng.push(`[${viewport.name}/home-timeline-details] 詳細行が表示されない`)
        } else {
          await details.locator('summary').click()
          const isOpen = await details.evaluate((element) => element.open)
          if (!isOpen) {
            ng.push(`[${viewport.name}/home-timeline-details] summary の操作で詳細が開かない`)
          } else {
            await measureCoarseTapTargets(page, `${viewport.name}/home-timeline-details`)
          }
        }
      }
      await context.close()
    }
  }

  // 展開行でだけ見える番組名検索リンクと encode profile checkbox label も測る。
  for (const viewport of tapTargetWidths) {
    const { context, page } = await open(viewport, 'light', screenOf('programs'), { pointer: 'coarse' })
    const row = page.locator('li[data-program-id]').first()
    const rowVisible = await row.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!rowVisible) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 番組行が表示されない`)
      await context.close()
      continue
    }
    const expander = row.locator('button[aria-expanded]').first()
    if ((await expander.count()) === 0) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 展開ボタンが見つからない`)
      await context.close()
      continue
    }
    await expander.click()
    if ((await expander.getAttribute('aria-expanded')) !== 'true') {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 行が展開されない`)
      await context.close()
      continue
    }
    const body = row.locator('[id^="program-row-detail-"]')
    const nameSearch = body.getByRole('link', { name: 'この番組名で検索', exact: true })
    const profileGroup = body.getByRole('group', { name: 'エンコードプロファイル' })
    const profileCheckbox = profileGroup.getByRole('checkbox').first()
    const searchVisible = await nameSearch.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    const profileVisible = await profileCheckbox.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!searchVisible) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] 番組名検索リンクが表示されない`)
    }
    if (!profileVisible) {
      ng.push(`[${viewport.name}/programs-expanded-first-row] encode profile checkbox が表示されない`)
    }
    if (searchVisible && profileVisible) {
      await measureCoarseTapTargets(page, `${viewport.name}/programs-expanded-first-row`, body)
    }
    await context.close()
  }

  // 短いシリーズ名はシリーズリンクの横幅が本文より狭い状態を作る。
  for (const viewport of tapTargetWidths) {
    const { context, page } = await open(viewport, 'light', recordingDetailScreen, {
      pointer: 'coarse',
      multiSite: true,
      recordingDetailScenario: 'short-series',
    })
    const seriesLink = page.getByRole('link', { name: 'このシリーズへ: 天気' })
    const visible = await seriesLink.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!visible) {
      ng.push(`[${viewport.name}/recording-detail-short-series] 短いシリーズリンクが表示されない`)
    } else {
      await measureCoarseTapTargets(
        page,
        `${viewport.name}/recording-detail-short-series`,
        page.locator('[data-testid="recording-series-links"]'),
      )
    }
    await context.close()
  }

  // 録画中は結論バッジが記録タブへの button として現れる。
  for (const viewport of tapTargetWidths) {
    const { context, page } = await open(viewport, 'light', recordingDetailScreen, {
      pointer: 'coarse',
      multiSite: true,
      recordingDetailScenario: 'recording',
    })
    const verdictButton = page.getByRole('button', { name: '録画状態を記録タブで見る' })
    const visible = await verdictButton.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
    if (!visible) {
      ng.push(`[${viewport.name}/recording-detail-recording] 録画状態ボタンが表示されない`)
    } else {
      await measureCoarseTapTargets(
        page,
        `${viewport.name}/recording-detail-recording`,
        page.locator('[data-testid="recording-title-row"]'),
      )
    }
    await context.close()
  }

  // ruleId filter は通常の予約一覧に出ない「解除」ボタンと条件編集リンクも持つ。
  for (const viewport of tapTargetWidths) {
      const screen = {
        ...screenOf('reservations'),
        name: 'reservations-active-filter',
        path: '/reservations?ruleId=1',
        wait: 'button[aria-label*="絞り込みを解除"]',
      }
    const { context, page } = await open(viewport, 'light', screen, { pointer: 'coarse' })
    const removeFilter = page.getByRole('button', { name: 'ルール「朝ドラ」の絞り込みを解除' })
    const editRule = page.getByRole('link', { name: 'ルールの条件を直す', exact: true })
    if ((await removeFilter.count()) === 0) {
      ng.push(`[${viewport.name}/reservations-active-filter] ルール絞り込み解除ボタンが表示されない`)
    }
    if ((await editRule.count()) === 0) {
      ng.push(`[${viewport.name}/reservations-active-filter] ルール条件リンクが表示されない`)
    }
    if ((await removeFilter.count()) > 0 && (await editRule.count()) > 0) {
      await measureCoarseTapTargets(page, `${viewport.name}/reservations-active-filter`)
    }
    await context.close()
  }

  // まず行の外側をタップして選択が反転するか実測する。checkbox 自体を 44px に
  // 広げると別の標的と重なりうるため、行が操作面なら role=option を標的とする。
  {
    const viewport = tapTargetWidths[0]
    const { context, page } = await open(viewport, 'light', screenOf('recordings'), { pointer: 'coarse' })
    const selectionButton = page.getByRole('button', { name: '選択', exact: true })
    if ((await selectionButton.count()) === 0) {
      ng.push('[mobile-360/recordings-selection] 選択モードへ入るボタンが見つからない')
    } else {
      await selectionButton.click()
      const checkbox = page.locator('input[type="checkbox"][aria-label$="を選択"]').first()
      await checkbox.waitFor({ timeout: 5000 }).catch(() => {})
      const row = page.locator('[role="option"]').first()
      const box = await row.boundingBox()
      if (box === null || (await checkbox.count()) === 0) {
        ng.push('[mobile-360/recordings-selection] 行または checkbox が見つからない')
      } else {
        const position = { x: Math.min(100, box.width - 20), y: box.height / 2 }
        let rowToggles = true
        const rowHit = await row.evaluate((element, point) => {
          const rect = element.getBoundingClientRect()
          const hit = document.elementFromPoint(rect.left + point.x, rect.top + point.y)
          return hit === element.querySelector('input[type="checkbox"]')
        }, position)
        if (rowHit) {
          ng.push('[mobile-360/recordings-selection] 行の確認タップが checkbox 自体に当たった')
        }
        for (const expected of [true, false]) {
          await row.click({ position })
          const changed = await page.waitForFunction(
            ({ accessibleName, checked }) =>
              [...document.querySelectorAll('input[type="checkbox"]')].some(
                (input) => input.getAttribute('aria-label') === accessibleName && input.checked === checked,
              ),
            { accessibleName: await checkbox.getAttribute('aria-label'), checked: expected },
            { timeout: 3000 },
          ).then(() => true).catch(() => false)
          if (!changed) {
            ng.push(`[mobile-360/recordings-selection] checkbox 外の行タップで checked=${expected} にならない`)
            rowToggles = false
            break
          }
        }
        if (await checkbox.isChecked()) {
          ng.push('[mobile-360/recordings-selection] 2 回目の行タップ後も選択が解除されない')
          rowToggles = false
        }
        await measureCoarseTapTargets(
          page,
          'mobile-360/recordings-selection',
          page.locator('body'),
          { skipSelectionCheckbox: rowToggles },
        )
      }
    }
    await context.close()
  }

  // MoreMenu はシートに開くため、背後のページ標的と重ねずシート内だけを測る。
  {
    const { context, page } = await open(tapTargetWidths[0], 'light', screenOf('programs'), { pointer: 'coarse' })
    const trigger = page.getByRole('button', { name: 'その他' })
    if ((await trigger.count()) === 0) {
      ng.push('[mobile-360/more-menu] 「その他」のトリガーが見つからない')
    } else {
      await trigger.click()
      const menu = page.getByRole('dialog', { name: 'その他' })
      await menu.waitFor({ timeout: 5000 }).catch(() => {
        ng.push('[mobile-360/more-menu] シートが開かない')
      })
      if (await menu.count()) {
        await page.waitForTimeout(300)
        await measureCoarseTapTargets(page, 'mobile-360/more-menu', menu)
      }
    }
    await context.close()
  }

  // 共通 DropdownMenuItem の標準寸法を、破壊的操作を持つルール行で実ブラウザ測定する。
  {
    const { context, page } = await open(tapTargetWidths[0], 'light', screenOf('rules'), { pointer: 'coarse' })
    const trigger = page.locator('[aria-haspopup="menu"]').first()
    if ((await trigger.count()) === 0) {
      ng.push('[mobile-360/rule-menu] ルールの操作メニューが見つからない')
    } else {
      await trigger.click()
      const menu = page.locator('[data-slot="dropdown-menu-content"]').last()
      await menu.waitFor({ timeout: 5000 }).catch(() => {
        ng.push('[mobile-360/rule-menu] DropdownMenuContent が開かない')
      })
      if (await menu.count()) {
        await page.waitForTimeout(300)
        await measureCoarseTapTargets(page, 'mobile-360/rule-menu', menu)
      }
    }
    await context.close()
  }

  // pointer: fine / 1280px は既定 Button の高さを 32px のままにする。
  {
    const viewport = { name: 'desktop-1280', width: 1280, height: 800 }
    const { context, page } = await open(viewport, 'light', screenOf('series-hub'), { pointer: 'fine' })
    const button = page.locator('[data-slot="button"].h-8').first()
    if ((await button.count()) === 0) {
      ng.push('[desktop-1280/fine] 既定サイズ Button の実例が見つからない')
    } else {
      const height = await button.evaluate((element) => Number.parseFloat(getComputedStyle(element).height))
      log(`  [desktop-1280/fine] 既定 Button の高さ=${height}px`)
      if (height !== 32) ng.push(`[desktop-1280/fine] 既定 Button が 32px ではない（${height}px）`)
    }
    const pointerIsFine = await page.evaluate(() => matchMedia('(pointer: fine)').matches)
    if (!pointerIsFine) ng.push('[desktop-1280/fine] ブラウザが pointer: fine と判定されない')
    await context.close()
  }
}

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// CI / red-green でこの追加判定だけを実行する入口。通常の e2e/run.mjs design-interactions でも
// 同じ関数を ④-A'' として実行する。色・到達距離の既存判定と同じく CI で守る。
if (process.env.E2E_TAP_TARGETS_ONLY === '1') {
  await runCoarseTapTargetChecks()
  await finish(ng, browser)
}

if (process.env.E2E_RULE_CARD_ONLY === '1') {
  await runRuleCardLayoutChecks()
  await finish(ng, browser)
}

// --- ④ モバイル: 「その他」シートの判定 ---
//
// ボトムナビと独立した Dialog なので、画面下端への接地と高さは jsdom では
// 原理的に測れない（`app-shell.test.tsx` が固定しているのは DOM の有無と順序だけ）。
//
// タブの本数は ARIA の `listitem` ロールではなく `<li>` を直接数える。
// 実測（このスクリプトが駆動する Chromium）: `nav.getByRole('listitem').count()`
// も CDP の AX ツリー（`Accessibility.getFullAXTree`）も listitem を 4 件返し、
// `list-style-type` を `disc` に戻しても変わらない --- CSS 依存の暗黙ロール抑制は
// 観測されていない。それでも `<li>` を直接数えるのは、ロールの計算をブラウザの
// アクセシビリティ実装に依存させたくないという保険であり、「抑制が起きるから」
// ではない（起きるかどうかは未検証。理由にしない）。
log('\n=== ④ 「その他」シートの判定 ===')
for (const theme of themes) {
  const { context, page } = await open(mobile, theme, screenOf('programs'))

  const nav = page.locator('nav[aria-label="主ナビゲーション"]').last()
  const tabCount = await nav.locator('li').count()
  log(`  [${theme}] ボトムタブの本数 = ${tabCount}`)
  if (tabCount !== 4) {
    ng.push(`[${theme}] ボトムタブが 4 個でない（${tabCount} 個。「その他」への集約が効いていない）`)
  }

  const trigger = nav.getByRole('button', { name: 'その他' })
  if ((await trigger.count()) === 0) {
    ng.push(`[${theme}] 「その他」トリガーが見つからない`)
  } else {
    await trigger.click()
    const menu = page.getByRole('dialog', { name: 'その他' })
    await menu.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] 「その他」を開いてもシートが現れない`)
    })
    if ((await menu.count()) > 0) {
      await page.waitForTimeout(300) // 開くアニメーションの終了を待つ

      const file = path.join(OUT_DIR, `more-menu-open-${theme}-mobile.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(ng, page, `more-menu-open/${theme}`)

      const menuBox = await menu.boundingBox()
      if (menuBox === null) {
        ng.push(`[${theme}] 「その他」シートのバウンディングボックスが取れない`)
      } else {
        if (menuBox.x < 0 || menuBox.x + menuBox.width > mobile.width) {
          ng.push(
            `[${theme}] 「その他」シートが横方向にビューポートをはみ出す` +
              `（x=${menuBox.x.toFixed(1)}, w=${menuBox.width.toFixed(1)}, vw=${mobile.width}）`,
          )
        }
        if (menuBox.y < 0 || menuBox.y + menuBox.height > mobile.height) {
          ng.push(
            `[${theme}] 「その他」シートが縦方向にビューポートをはみ出す` +
              `（y=${menuBox.y.toFixed(1)}, h=${menuBox.height.toFixed(1)}, vh=${mobile.height}）`,
          )
        }
        const bottom = menuBox.y + menuBox.height
        if (Math.abs(bottom - mobile.height) > 1) {
          ng.push(
            `[${theme}] 「その他」シートの下端が画面に接していない` +
              `（bottom=${bottom.toFixed(1)}, viewport=${mobile.height}）`,
          )
        }
        if (menuBox.height >= mobile.height * 0.6) {
          ng.push(
            `[${theme}] 「その他」シートが画面の 60% 以上を占める` +
              `（h=${menuBox.height.toFixed(1)}, vh=${mobile.height}）`,
          )
        }
        log(
          `  [${theme}] シート x=${menuBox.x.toFixed(1)} y=${menuBox.y.toFixed(1)} ` +
            `w=${menuBox.width.toFixed(1)} h=${menuBox.height.toFixed(1)} / bottom=${bottom.toFixed(1)}`,
        )
      }

      // role="dialog" の間は Tab が背後のページへ抜けないこと。完了ボタンと
      // 最後のリンクの間で両方向を実ブラウザ確認する。
      const menuLinks = menu.getByRole('link')
      const closeButton = menu.getByRole('button', { name: '完了', exact: true })
      const menuLinkCount = await menuLinks.count()
      if (menuLinkCount < 2 || (await closeButton.count()) === 0) {
        ng.push(`[${theme}] 「その他」のフォーカストラップを判定できる操作要素が足りない`)
      } else {
        // 待ちが失敗しても、直後に `document.activeElement === el` で実際の状態を
        // 直接読み直す（forwardTrapped/backwardTrapped）ので、待ちの成否を経由せず
        // 本当の結果を測っている。待ちはタイムアウトを早める（1000ms）ためだけの
        // ものなので `.catch(() => {})` で飲んでよい --- 待ちが失敗＝実際に
        // フォーカスが移っていない、という結果自体が下の NG 文言（「Tab が
        // シート外へ抜ける」）と一致し、スタイル回帰の NG とは混ざらない。
        await menuLinks.last().focus()
        await page.keyboard.press('Tab')
        await page
          .waitForFunction(
            () =>
              document.activeElement?.tagName === 'BUTTON' &&
              document.activeElement.closest('[role="dialog"]') !== null,
            undefined,
            { timeout: 1000 },
          )
          .catch(() => {})
        const forwardTrapped = await closeButton
          .evaluate((el) => document.activeElement === el)
        await closeButton.focus()
        await page.keyboard.press('Shift+Tab')
        await page
          .waitForFunction(
            () =>
              document.activeElement?.tagName === 'A' &&
              document.activeElement.closest('[role="dialog"]') !== null,
            undefined,
            { timeout: 1000 },
          )
          .catch(() => {})
        const backwardTrapped = await menuLinks
          .last()
          .evaluate((el) => document.activeElement === el)
        log(`  [${theme}] フォーカストラップ: 前=${forwardTrapped} / 後=${backwardTrapped}`)
        if (!forwardTrapped || !backwardTrapped) {
          ng.push(
            `[${theme}] 「その他」で Tab がシート外へ抜ける` +
              `（前=${forwardTrapped} / 後=${backwardTrapped}）`,
          )
        }
      }
    }
  }

  await context.close()
}

// --- ④-A キーボード操作と標的サイズ ---

/**
 * checkExplicitFocusRing は対象を focus-visible にし、明示リングと --ring の色を測る。
 * box-shadow の存在だけでは透明色でも通るので、内側の border を canvas で画素化して
 * --ring と比較する。outline は none でなければブラウザ既定との二重表示として落とす。
 *
 * **before を測る前の `blur()` を外さないこと。** 呼び出し側がポップオーバー/
 * ダイアログを開いた直後に呼ぶことがあり、その手のコンポーネントは開いた瞬間に
 * 候補へ既定フォーカスを非同期に当てることがある（#521。base-ui の Popover が実例）。
 * `blur()` 無しで before を測ると、その既定フォーカスがまだ来ていないか来た後かで
 * before の値が実行のたびに割れ、before/after の差分判定が偽陽性の NG を出す。
 * 4 呼び出し元で確認済み: 3 つは毎回フレッシュなページ遷移直後の呼び出しで
 * `blur()` は no-op（アプリ側に `focusout`/`blur` ハンドラは無く、base-ui の
 * `useDismiss` も `focusout` を束ねていない）。唯一 before に影響するのが
 * ChannelOption の既定フォーカスで、そちらは元々 60% の確率で偽陽性を出していた
 * （つまり実質的に一度も安定して機能していなかった）ので失う実カバレッジは無く、
 * 常時リング付き（本来の回帰）は before/after が一致するのでむしろ新たに検出できる。
 */
async function checkExplicitFocusRing(page, locator, label, theme) {
  if ((await locator.count()) === 0) {
    ng.push(`[${theme}] ${label} が見つからず focus-visible を判定できない`)
    return
  }

  const target = locator.first()
  // before を測る前に明示的に blur する。呼び出し側（ChannelPicker のポップアップ）が
  // 開いた直後に先頭候補へ既定フォーカスを当てることがあり、それを当てにしたまま
  // before を測ると「既にリング付きの状態」を基準にしてしまい、後段の diff
  // （before と after が同じなら NG）が常に真になる（issue #521）。
  await target.evaluate((el) => el.blur())
  const beforeShadow = await target.evaluate((el) => getComputedStyle(el).boxShadow)
  await target.focus()
  const focusVisible = await target.evaluate((el) => el.matches(':focus-visible'))
  const focusStyle = await target.evaluate((el) => {
    const style = getComputedStyle(el)
    return { boxShadow: style.boxShadow, outlineStyle: style.outlineStyle }
  })
  const border = await computedOf(target, 'border-top-color')
  const ring = await computedVar(page.locator('html'), '--ring')
  log(
    `  [${theme}] ${label}: focus-visible=${focusVisible} border=${border?.rgba} ` +
      `shadow=${focusStyle.boxShadow}`,
  )

  if (!focusVisible) {
    ng.push(`[${theme}] ${label} に :focus-visible が付かない`)
  }
  if (focusStyle.boxShadow === 'none' || focusStyle.boxShadow === beforeShadow) {
    ng.push(`[${theme}] ${label} の focus-visible で明示リングが出ない`)
  }
  if (focusStyle.outlineStyle !== 'none') {
    ng.push(`[${theme}] ${label} に明示リングとブラウザ outline が二重に出る`)
  }
  if (border === null || ring === null || !sameRgb(border.rgba, ring.rgba)) {
    ng.push(
      `[${theme}] ${label} のフォーカス縁が --ring の実画素でない` +
        `（border=${border?.rgba} / ring=${ring?.rgba}）`,
    )
  }
}

// スキップリンクは DOM の先頭にあるだけでなく、Tab 1 回で実際に見える寸法へ戻り、
// Enter 後は URL の fragment だけでなく main 自身へフォーカスが移ることを見る。
{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur())
  await page.keyboard.press('Tab')
  const skip = page.getByRole('link', { name: '本文へ移動' })
  if ((await skip.count()) === 0) {
    ng.push('スキップリンク: 「本文へ移動」が見つからない')
  } else {
    const focused = await skip.evaluate((el) => document.activeElement === el)
    const skipMetrics = await skip.evaluate((el) => {
      const rect = el.getBoundingClientRect()
      const style = getComputedStyle(el)
      return {
        x: rect.x,
        y: rect.y,
        width: rect.width,
        height: rect.height,
        position: style.position,
        zIndex: Number(style.zIndex),
      }
    })
    log(
      `  スキップリンク: focused=${focused} box=${skipMetrics.width}x${skipMetrics.height} ` +
        `position=${skipMetrics.position} z-index=${skipMetrics.zIndex}`,
    )
    if (!focused) ng.push('スキップリンク: Tab 1 回でフォーカスされない')
    if (skipMetrics.width < 24 || skipMetrics.height < 24) {
      ng.push('スキップリンク: フォーカスされても 24px 以上の可視寸法に戻らない')
    }
    if (
      skipMetrics.position !== 'fixed' ||
      skipMetrics.x < 0 ||
      skipMetrics.y < 0 ||
      skipMetrics.zIndex < 50
    ) {
      ng.push('スキップリンク: 固定位置または前面の重なり順になっていない')
    }
    await page.keyboard.press('Enter')
    // 待ちが失敗しても、直後に同じ条件を `mainFocused` として直接読み直す。
    // 待ちはタイムアウトを早めるだけのもので、失敗＝実際に main へ移っていない、
    // という結果自体が下の NG 文言と一致するので `.catch(() => {})` で飲んでよい。
    await page.waitForFunction(() => document.activeElement?.id === 'main').catch(() => {})
    const mainFocused = await page.evaluate(() => document.activeElement?.id === 'main')
    if (!mainFocused) ng.push('スキップリンク: Enter 後に main へフォーカスが移らない')
  }
  await context.close()
}

// 明示リングは DayStrip の濃い選択地を含め、ライト / ダークの両方で測る。
for (const theme of themes) {
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    await checkExplicitFocusRing(
      page,
      page.getByRole('button', { name: 'リスト', exact: true }),
      'Chip',
      theme,
    )
    await checkExplicitFocusRing(
      page,
      page.getByRole('group', { name: '日付' }).getByRole('button').first(),
      'DayStrip',
      theme,
    )

    const picker = page.getByRole('button', { name: /^チャンネル:/ })
    if ((await picker.count()) === 0) {
      ng.push(`[${theme}] チャンネルピッカーが見つからない`)
    } else {
      await picker.focus()
      await page.keyboard.press('Enter')
      const popup = page.getByRole('dialog', { name: 'チャンネル' })
      const popupOpened = await popup
        .waitFor({ timeout: 5000 })
        .then(() => true)
        .catch(() => false)
      if (!popupOpened) {
        // ポップアップが開かなかったときに、スタイル回帰の NG（`checkExplicitFocusRing`
        // が出す「明示リングが出ない」等）と同じ文言を出さないための分岐。
        // なお #521 の実際の原因はこれではない（下記のコメント参照）。
        ng.push(`[${theme}] チャンネルピッカーのポップアップが開かない（待ちがタイムアウト）`)
      } else {
        // #521 の実際の原因: base-ui の Popover は開いた直後、先頭候補（＝この
        // 「すべて」自身）へ既定フォーカスを非同期に当てる（queueMicrotask →
        // requestAnimationFrame 1 回。@base-ui/react の
        // floating-ui-react/utils/enqueueFocus.js）。これを待たずに
        // `checkExplicitFocusRing` が before を測ると、その既定フォーカスが
        // 「まだ来ていない」か「もう来た」かで before の box-shadow が実行のたびに
        // 割れ、before/after の差分判定が偽陽性の NG を出す（実機で確認: 同一コードで
        // alreadyFocused が true/false どちらにもなり、before !== 'none' と NG が
        // 8/8 で一致）。この既定フォーカスの発火は queueMicrotask → rAF 1 回の
        // 一本道で、フレーム数は負荷が増えても変わらない（増えるのは 1 フレームの
        // 長さ）ため、rAF を 2 回挟めば確実に済んでいる。
        await page.evaluate(
          () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))),
        )
        await checkExplicitFocusRing(
          page,
          popup.getByRole('checkbox', { name: 'すべて', exact: true }),
          'ChannelOption',
          theme,
        )
      }
    }
    await context.close()
  }

  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    await checkExplicitFocusRing(
      page,
      page.getByRole('button', { name: 'ライブラリ', exact: true }),
      'ViewTab',
      theme,
    )
    await context.close()
  }
}

// 共通 Button の sm は 32px、見た目を広げない容量不足バッジは ::before だけを
// 24px にする。バッジの z-index も見て、行全面リンクの上で当たり判定が生きることを固定する。
{
  const { context, page } = await open(desktop, 'light', screenOf('rules'))
  // issue #728: sm の実測サンプルはルール名の編集リンク（size="sm" の
  // `min-h-8`）に付け替える。旧「検索しながら編集」ボタンは無くなった。
  const smallButton = page.getByRole('link', { name: /^ルール「.+」を編集$/ }).first()
  const box = (await smallButton.count()) === 0 ? null : await smallButton.boundingBox()
  log(`  Button size=sm: height=${box?.height}`)
  if (box === null || box.height < 32) {
    ng.push(`Button size="sm" の高さが 32px 未満（${box?.height ?? '取得不能'}px）`)
  }
  await context.close()
}

{
  const { context, page } = await open(desktop, 'light', screenOf('reservations'))
  const badge = page.getByRole('link', { name: /チューナーが不足しています/ }).first()
  if ((await badge.count()) === 0) {
    ng.push('容量不足バッジが見つからず当たり判定を測れない')
  } else {
    const target = await badge.evaluate((el) => {
      const rect = el.getBoundingClientRect()
      const before = getComputedStyle(el, '::before')
      const pseudoHeight = Number.parseFloat(before.height)
      return {
        visualHeight: rect.height,
        hitHeight: Number.isFinite(pseudoHeight) ? pseudoHeight : 0,
        position: before.position,
        zIndex: getComputedStyle(el).zIndex,
      }
    })
    log(
      `  容量不足バッジ: visual=${target.visualHeight}px hit=${target.hitHeight}px ` +
        `z-index=${target.zIndex}`,
    )
    if (target.position !== 'absolute' || target.hitHeight < 24) {
      ng.push(`容量不足バッジの当たり判定が 24px 未満（${target.hitHeight}px）`)
    }
    if (target.hitHeight <= target.visualHeight) {
      ng.push('容量不足バッジの見た目ごと拡大され、::before で当たりだけを広げていない')
    }
    if (target.zIndex === 'auto' || Number(target.zIndex) <= 0) {
      ng.push('容量不足バッジが行全面リンクより上の重なり順を持たない')
    }
  }
  await context.close()
}

// --- ④-A' issue #779: 多行トーストの action / close を縦中央へ揃える ---
//
// `titles` の輪番へ長い題名を混ぜると、既存のスクリーンショットや別判定で
// 予約する番組が変わる。そこでこの判定だけ `toastLayout` の専用応答にし、
// 長い番組を 1 件だけ返して予約する。モバイルの `max-w-sm` で本当に 2 行以上へ
// 折り返したことを先に確認してから、メッセージ span と右側の action 群 / close
// の `getBoundingClientRect()` の縦中心を比較する。jsdom ではこの判定はできない。
{
  const { context, page } = await open(mobile, 'light', screenOf('programs'), { toastLayout: true })
  const row = page.locator('li[data-program-id]').filter({ hasText: toastLayoutProgram.name }).first()
  const rowVisible = await row
    .waitFor({ timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  if (!rowVisible) {
    ng.push('[#779] 多行トースト用の専用番組が表示されない')
  } else {
    const expander = row.locator('button[aria-expanded]').first()
    const expanderVisible = await expander
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false)
    if (!expanderVisible) {
      ng.push('[#779] 多行トースト用番組の展開ボタンが見つからない')
    } else {
      await expander.click()
      await page.waitForTimeout(250)
      const reserveAction = row.locator('[data-program-action="reserve"] button').first()
      const reserveVisible = await reserveAction
        .waitFor({ timeout: 5000 })
        .then(() => true)
        .catch(() => false)
      if (!reserveVisible) {
        ng.push('[#779] 多行トースト用番組の予約ボタンが見つからない')
      } else {
        await reserveAction.click()
        const toast = page.locator('[aria-live="polite"] > div').last()
        const action = toast.getByRole('button', { name: '取消', exact: true })
        const toastVisible = await action
          .waitFor({ timeout: 5000 })
          .then(() => true)
          .catch(() => false)
        if (!toastVisible) {
          ng.push('[#779] 予約後の多行トーストが表示されない')
        } else {
          const metrics = await toast.evaluate((el) => {
            const rectOf = (node) => {
              if (!(node instanceof HTMLElement)) return null
              const rect = node.getBoundingClientRect()
              return {
                top: rect.top,
                height: rect.height,
                center: rect.top + rect.height / 2,
              }
            }
            const message = el.querySelector(':scope > span')
            const actions = el.querySelector(':scope > div')
            const cancel = el.querySelector('button:not([aria-label="閉じる"])')
            const close = el.querySelector('button[aria-label="閉じる"]')
            return {
              message: rectOf(message),
              actions: rectOf(actions),
              cancel: rectOf(cancel),
              close: rectOf(close),
            }
          })
          const messageHeight = metrics.message?.height ?? 0
          log(
            `  [#779] 多行トースト message=${metrics.message?.center?.toFixed(1) ?? '—'} ` +
              `actions=${metrics.actions?.center?.toFixed(1) ?? '—'} ` +
              `cancel=${metrics.cancel?.center?.toFixed(1) ?? '—'} ` +
              `close=${metrics.close?.center?.toFixed(1) ?? '—'} ` +
              `messageHeight=${messageHeight.toFixed(1)}px`,
          )
          if (metrics.message === null || metrics.actions === null || metrics.cancel === null || metrics.close === null) {
            ng.push('[#779] 多行トーストの message / action / close の矩形が取得できない')
          } else if (messageHeight <= 24) {
            ng.push(
              `[#779] 多行トーストが 1 行のまま（message height ${messageHeight.toFixed(1)}px）`,
            )
          } else {
            const centerDelta = (a, b) => Math.abs(a.center - b.center)
            const deltas = {
              actions: centerDelta(metrics.message, metrics.actions),
              cancel: centerDelta(metrics.message, metrics.cancel),
              close: centerDelta(metrics.message, metrics.close),
            }
            const maxDelta = Math.max(...Object.values(deltas))
            if (maxDelta > 1) {
              ng.push(
                `[#779] 多行トーストの縦中心が揃っていない（最大差 ${maxDelta.toFixed(1)}px ` +
                  ` / actions ${deltas.actions.toFixed(1)}px / cancel ${deltas.cancel.toFixed(1)}px / ` +
                  `close ${deltas.close.toFixed(1)}px）`,
              )
            }
          }
        }
      }
    }
  }
  await context.close()
}

// 主要画面の実装された操作標的を、ポインタの性質ごとに同じ実ブラウザで列挙する。
// ここでは 44px を一律に要求しない。密度を保った管理画面の共通下限は 24px とし、
// 日付・チャンネル候補・行の主操作・ライブチャンネルの 44px と、モバイルナビの
// 幅44px・高さ56pxは下記の個別契約で固定する。
for (const profile of targetPointerProfiles) {
  for (const screen of targetScreens) {
    const { context, page } = await open(
      profile.viewport,
      'light',
      screen,
      screen.name === 'recording-detail'
        ? { pointer: profile.pointer, multiSite: true, recordingDetailScenario: 'completed' }
        : { pointer: profile.pointer },
    )
    const measurement = await measureInteractiveTargets(page, `${profile.name}/${screen.name}`)
    if (
      screen.name === 'recordings' &&
      !measurement.targets.some((target) => target.tag === 'summary')
    ) {
      ng.push(`[${profile.name}/recordings] <summary> を操作標的として列挙できない`)
    }
    await context.close()
  }
}

/** 役割ごとの個別寸法を採用している標的の実寸を固定する。 */
async function checkMinimumTargetSize(locator, label, minimumWidth, minimumHeight = minimumWidth) {
  const count = await locator.count()
  if (count === 0) {
    ng.push(`${label} が見つからず標的サイズを判定できない`)
    return
  }
  for (let index = 0; index < count; index += 1) {
    const box = await locator.nth(index).boundingBox()
    log(
      `  ${label}[${index + 1}/${count}]: ` +
        `${box === null ? '取得不能' : `${box.width.toFixed(1)}×${box.height.toFixed(1)}px`} ` +
        `(基準 ${minimumWidth}×${minimumHeight}px)`,
    )
    if (box === null || box.width < minimumWidth || box.height < minimumHeight) {
      ng.push(
        `${label}[${index + 1}] が ${box === null ? '取得不能' : `${box.width.toFixed(1)}×${box.height.toFixed(1)}px`} ` +
          `(基準 ${minimumWidth}×${minimumHeight}px 未満)`,
      )
    }
  }
}

// 録画詳細の前後ナビゲーションはモバイルでは映像の中央に置き、タッチ時に44pxを確保する。
{
  const { context, page } = await open(mobile, 'light', recordingDetailScreen, {
    pointer: 'coarse',
    multiSite: true,
    recordingDetailScenario: 'completed',
  })
  const previous = page.getByRole('button', { name: '前のチャプター' })
  const next = page.getByRole('button', { name: '次のチャプター' })
  await checkMinimumTargetSize(previous, '前のチャプター', 44)
  await checkMinimumTargetSize(next, '次のチャプター', 44)
  await page.getByRole('button', { name: '再生設定' }).click()
  const editorEntry = page.getByRole('menuitem', { name: 'チャプターを直す' })
  await checkMinimumTargetSize(editorEntry, '「チャプターを直す」メニュー項目', 44)

  const details = page.locator('[data-testid="chapter-edit-layout"]')
  await editorEntry.click()
  await details.waitFor({ timeout: 10000 })
  await details.evaluate((node) => {
    node.dataset.e2eIdentity = 'chapter-details-before-timeupdate'
  })
  const video = page.locator('video')
  for (let index = 0; index < 4; index += 1) {
    // 再生位置が動かないと currentSeconds が変わらず再描画が起きない。位置ごと動かして投げる。
    await video.evaluate((node, seconds) => {
      node.currentTime = seconds
      node.dispatchEvent(new Event('timeupdate', { bubbles: true }))
    }, index + 1)
    await page.waitForTimeout(250)
  }
  const editorState = await details.evaluate((node) => ({
    same: node.dataset.e2eIdentity === 'chapter-details-before-timeupdate',
  }))
  if (editorState.same !== true) {
    ng.push('4Hzのtimeupdate再描画後にチャプター編集画面が作り直される')
  }
  await context.close()
}

// 高頻度の主操作は、共通下限とは別に 44px 高の配置契約を保つ
// （モバイル主ナビだけは高さ56px）。
{
  const { context, page } = await open(mobile, 'light', screenOf('programs'), { pointer: 'coarse' })
  await checkMinimumTargetSize(
    page.locator('button[aria-label*="月"][aria-label*="("]'),
    '日付セル',
    24,
    44,
  )
  const channelTrigger = page.getByRole('button', { name: /^チャンネル:/ }).first()
  await checkMinimumTargetSize(channelTrigger, 'チャンネルピッカー', 44)
  await channelTrigger.click()
  const channelPopup = page.getByRole('dialog', { name: 'チャンネル' })
  await channelPopup.waitFor({ timeout: 5000 }).catch(() => {})
  await checkMinimumTargetSize(channelPopup.getByRole('checkbox'), 'チャンネル候補', 44)
  await context.close()
}

{
  const { context, page } = await open(mobile, 'light', screenOf('live'), { pointer: 'coarse' })
  const bottomNav = page.getByTestId('bottom-nav')
  const bottomLinks = bottomNav.getByRole('link')
  const bottomItemCount = await bottomNav.locator('li').count()
  log(`  モバイル主ナビの項目本数=${bottomItemCount}`)
  if (bottomItemCount !== 4) ng.push(`モバイル主ナビの項目本数が 4 ではない（${bottomItemCount}）`)
  await checkMinimumTargetSize(bottomLinks, 'モバイル主ナビ', 44, 56)
  await checkMinimumTargetSize(bottomNav.getByRole('button'), 'モバイル「その他」', 44, 56)
  await checkMinimumTargetSize(page.locator('nav[aria-label="チャンネル一覧"] a'), 'ライブチャンネル', 44)
  await context.close()
}

// タッチで展開した番組行の予約 / ライブ操作と、#729 のトースト action / close
// も同じ実寸判定に載せる。予約 API はこのスクリプトのスタブで 204 を返す。
{
  const { context, page } = await open(mobile, 'light', screenOf('programs'), { pointer: 'coarse' })
  const row = page.locator('li[data-program-id]').filter({ hasText: '大相撲中継' }).first()
  const expander = row.locator('button[aria-expanded]').first()
  if ((await expander.count()) === 0) {
    ng.push('番組行の展開ボタンが見つからず行主操作を判定できない')
  } else {
    await expander.click()
    await page.waitForTimeout(250)
    const reserveAction = row.locator('[data-program-action="reserve"] button').first()
    await checkMinimumTargetSize(reserveAction, '番組行の予約操作', 44)
    const liveAction = row.locator('[data-program-action="live"] a, [data-program-action="live"] button')
    if ((await liveAction.count()) > 0) {
      await checkMinimumTargetSize(liveAction.first(), '番組行のライブ操作', 44)
    }
    await reserveAction.click()
    const toast = page.locator('[aria-live="polite"] > div').last()
    await toast.getByRole('button', { name: '取消', exact: true }).waitFor({ timeout: 5000 }).catch(() => {})
    await checkMinimumTargetSize(toast.getByRole('button', { name: '取消', exact: true }), 'トースト action', 32)
    await checkMinimumTargetSize(toast.getByRole('button', { name: '閉じる', exact: true }), 'トースト close', 28)
    await measureInteractiveTargets(page, 'coarse/programs/展開行+トースト')
  }
  await context.close()
}

// --- ⑤ 録画詳細: キーボードだけで <video> へ到達できるか ---
//
// jsdom では測れない領域（web/e2e/README.md §デザイン）。`<video>` に
// `tabIndex={-1}` を付けると、プログラムからの `.focus()` は変わらず効く
// ため jsdom のユニットテスト（focus spy）は通り続けるが、実ブラウザの
// キーボード Tab 走査からは完全に外れる（M5-4 / issue #227 でこの属性を
// 一度入れて実際に壊した退行そのもの）。視聴は詳細ページ（/recordings/$id）に
// 寄せた（issue #311）ので、到達性の判定もそこへ移した --- 一覧は展開も
// プレイヤーも持たない。ページ先頭からの Tab 走査で `<video>` に止まるかを見る
// （行の展開経路が無くなったので、旧判定を一覧に残すと直後に赤のままになる）。
{
  const { context, page } = await open(desktop, 'light', screenOf('recordings'))
  const row = page.locator('li', { hasText: 'クラシック音楽館' })
  const detailLink = row.getByRole('link', { name: 'クラシック音楽館' })
  if ((await detailLink.count()) === 0) {
    ng.push('キーボード到達性: encoded 付き録画の詳細リンクが見つからない')
  } else {
    // 旧実装の常時「再生」列が残っていないことを、実ブラウザでも見る。
    if ((await row.getByRole('button', { name: /再生/ }).count()) > 0) {
      ng.push('録画一覧: 常時の「再生」ボタンが残っている')
    }
    await detailLink.focus()
    await page.keyboard.press('Enter')
    // どちらの待ちが失敗しても、直後に `<video>` の有無を直接数え直して判定する
    // （遷移が終わっていなければ <video> も無いので、この 1 つの NG 文言に
    // 正しく合流する）。待ちはタイムアウトを早めるだけなので飲んでよい。
    await page.waitForURL('**/recordings/12', { timeout: 5000 }).catch(() => {})
    await page.locator('video').first().waitFor({ timeout: 5000 }).catch(() => {})
    if ((await page.locator('video').count()) === 0) {
      ng.push('キーボード到達性: 詳細ページに <video> が出ない')
    } else {
      // ページ先頭から Tab 走査する。「戻る」など先行の Tab stop があるので
      // 上限は緩める --- 見たいのは「<video> がいつか Tab 順に現れる
      // （tabIndex={-1} で外れていない）」ことで、正確な回数ではない。
      await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur())
      const maxPresses = 12
      let reachedAt = null
      for (let i = 1; i <= maxPresses; i++) {
        await page.keyboard.press('Tab')
        const tag = await page.evaluate(() => document.activeElement?.tagName)
        if (tag === 'VIDEO') {
          reachedAt = i
          break
        }
      }
      log(`  キーボード到達性: 詳細ページで Tab ${reachedAt ?? `${maxPresses}+`} 回で video`)
      if (reachedAt === null) {
        ng.push(
          `キーボード到達性: 詳細ページで Tab ${maxPresses} 回以内に <video> へ到達しない` +
            '（<video> に tabIndex={-1} を付けて Tab 順から外していないか確認する。' +
            'M5-4 で一度この属性を付けて実際に壊した退行）',
        )
      }
    }
  }
  await context.close()
}

// --- ⑥ Button: フォーカスリング / border-color は遷移しない・hover の色と
//     active の押下フィードバックは遷移する（issue #294） ---
//
// `transition-all` は `box-shadow`（focus-visible の ring-3）と
// `focus-visible:border-ring`（1px 罫線の border-color）の**両方**を
// 遷移対象に含めてしまい、キーボードフォーカスの瞬間にリングの縁までが
// アニメーションで出現する（WCAG のフォーカス可視・「Focus rings that
// animate in」という tell）。border-color は ring の外側の淡い box-shadow
// より内側にある最も鮮明な縁なので、box-shadow だけ外しても border-color が
// 残れば「リングが遷移していない」は満たせない（レビューで実測: alpha
// 0 → 0.0073 → 0.88 → 1 と 150ms かけてフェードインしていた）。
//
// **jsdom では測れない。** CSS transition が実際に走るかどうかはブラウザの
// transition エンジンでしか観測できない --- `transition-property` という
// 文字列がクラス名に含まれているかを読むテストは「そのユーティリティを
// 書いた」ことしか確認できず、**実際に発火するか**（他のクラスに上書きされて
// いないか・ブラウザが実際に transitionstart を上げるか）までは保証しない。
// ここでは実際の `transitionstart` イベント（発火したプロパティ名つき）を
// ボタン要素自身に張った listener で観測する。
//
// 3 方向を見る: ① focus-visible で box-shadow / outline / border-*-color が
// 遷移**しない**こと、② hover で背景色の遷移が従来どおり**起きる**こと
// （issue の受け入れ基準）、③ active で `translate`（`active:...:translate-y-px`
// の押下フィードバック）が遷移**すること** --- Tailwind v4 は
// `translate-y-px` を `transform` ではなく `translate` プロパティへ
// コンパイルするため、挙げるプロパティを間違えると「押下フィードバックを
// 残すつもりが実際には何も遷移しない」という死んだ意図になり得る
// （レビューで実測して発覚）。①だけでなく③も持たせることで、将来
// `translate` がクラス列から静かに落ちても検出できる。
log('\n=== ⑥ Button: フォーカスリング / border-color / hover / active の遷移（issue #294） ===')
{
  const { context, page } = await open(desktop, 'light', screenOf('search'))
  const button = page.getByRole('button', { name: '検索' })
  if ((await button.count()) === 0) {
    ng.push('フォーカスリング: 検索ボタン（shared Button）が見つからない')
  } else {
    // listener はボタン要素自身に張る（document + capture ではない）。
    // transitionstart はバブルするので、対象要素に直接張れば document まで
    // 遡る理由が無く、他要素の transition と混ざる余地も無くなる。
    await button.first().evaluate((el) => {
      el.__transitioned = []
      el.addEventListener('transitionstart', (e) => el.__transitioned.push(e.propertyName))
    })
    const readTransitioned = () => button.first().evaluate((el) => el.__transitioned)
    const resetTransitioned = () =>
      button.first().evaluate((el) => {
        el.__transitioned = []
      })

    // border-*-color は `border-color`（ショートハンド）ではなく
    // `border-top-color` 等のロングハンドで上がる（実測）。`outline` も
    // `outline-color` / `outline-width` / `outline-style` のロングハンドで
    // 上がりうる（実測: transition-all のもとで `outline-width` が上がった）
    // ので、どちらも前方一致で拾う。box-shadow はロングハンドを持たないので
    // 完全一致でよい。
    const isRingLike = (p) =>
      p === 'box-shadow' || p.startsWith('outline') || (p.startsWith('border-') && p.endsWith('-color'))

    // Playwright の `.focus()` は script からの `element.focus()` で、実際の
    // Tab 走査ではないが、ページ読み込み後まだ一度もポインタ操作をしていない
    // 状態でのプログラム的フォーカスは Chromium で :focus-visible を伴う。
    // それを前提にせず、次の行で実際に :focus-visible が付いたかを確認して
    // いるので、前提が崩れていれば「遷移していない」ではなく専用の NG で落ちる
    // （検証していない前提を持たない --- CLAUDE.md「測っていない挙動を断言しない」）。
    await button.first().focus()
    const isFocusVisible = await button.first().evaluate((el) => el.matches(':focus-visible'))
    if (!isFocusVisible) {
      ng.push(
        'フォーカスリング: 検索ボタンに :focus-visible が付かない（判定の前提が崩れている。' +
          'focus() の呼び方を見直す）',
      )
    } else {
      // transition-all（既定 150ms）や border-color が残っていれば
      // box-shadow / outline* / border-*-color の transitionstart が飛ぶ。
      // 150ms を安全側に見て待つ
      await page.waitForTimeout(400)
      const transitioned = await readTransitioned()
      log(`  :focus-visible で遷移したプロパティ: [${transitioned.join(', ') || '(なし)'}]`)
      for (const prop of transitioned.filter(isRingLike)) {
        ng.push(
          `フォーカスリング: :focus-visible で ${prop} が遷移している` +
            '（box-shadow・outline・border-*-color は遷移対象から外す）',
        )
      }
    }

    // 両方向: hover の色遷移は従来どおり効くこと（issue の受け入れ基準）
    await button.first().evaluate((el) => el.blur())
    await page.mouse.move(0, 0)
    await resetTransitioned()
    await button.first().hover()
    await page.waitForTimeout(400)
    const hoverTransitioned = await readTransitioned()
    log(`  hover で遷移したプロパティ: [${hoverTransitioned.join(', ') || '(なし)'}]`)
    if (!hoverTransitioned.includes('background-color') && !hoverTransitioned.includes('color')) {
      ng.push(
        'フォーカスリング: hover で色（background-color / color）の遷移が起きていない' +
          '（transition-all を外した副作用で色遷移まで消えていないか確認する）',
      )
    }

    // active の押下フィードバック（`translate-y-px`）は遷移**すること**。
    // ここが無いと、将来 `translate` がクラス列から落ちても気付けない
    // （このレビューで実際に `transform`（誤り）のまま気付かれずにいた）。
    await resetTransitioned()
    const box = await button.first().boundingBox()
    if (box === null) {
      ng.push('フォーカスリング: 検索ボタンの座標が取れず active を再現できない')
    } else {
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
      await page.mouse.down()
      await page.waitForTimeout(400)
      const activeTransitioned = await readTransitioned()
      log(`  active で遷移したプロパティ: [${activeTransitioned.join(', ') || '(なし)'}]`)
      if (!activeTransitioned.includes('translate')) {
        ng.push(
          'フォーカスリング: active で translate の遷移が起きていない' +
            '（`active:...:translate-y-px` の押下フィードバックが snap している。' +
            'Tailwind v4 は translate-y-px を transform ではなく translate に' +
            'コンパイルするので、遷移対象に挙げるなら translate で挙げる）',
        )
      }
      await page.mouse.up()
    }
  }
  await context.close()
}

// --- ⑦ アニメーション: prefers-reduced-motion で縮退する（issue #296） ---
//
// jsdom は `prefers-reduced-motion` の matchMedia も CSS の実際の適用も測れない
// （README §デザイン冒頭）。Playwright の `reducedMotion` コンテキストオプションで
// OS の設定をエミュレートし、`getComputedStyle` の実測をオラクルにする。
//
// **両方向を見る。** 縮退側だけを見る判定は、動きを恒久的に殺した実装
// （`no-preference` でも動かない）を通してしまう --- CLAUDE.md テスト規律
// 「分岐を直したら両方向を確認する」と同型の穴。
//
// 対象は Skeleton の `animate-pulse`、共通 Button の押下フィードバック
// （`active:...:translate-y-px` の `transition`）。
// 予約実行中ボタンの `animate-spin` は #298 で削除した（楽観更新が確定表示を
// 出しているのにスピナーがそれを覆い高速応答時に点滅していた）ので対象外。
log('\n=== ⑦ アニメーション: prefers-reduced-motion の縮退（issue #296） ===')

// 縮退後の継続時間はほぼ 0（`index.css` は 0.01ms）、既定の継続時間は
// 100ms 以上（Button 150ms / pulse 2s）なので、50ms を境に両方向をまとめて判定できる。
const REDUCE_THRESHOLD_MS = 50

/** parseCssTime は `"150ms"` / `"0.15s"` / カンマ区切り（複数プロパティ）を ms にする。 */
function parseCssTime(v) {
  const first = (v ?? '').split(',')[0].trim()
  // Chromium は極小の値（0.01ms 相当）を指数表記でシリアライズする
  // （実測: `1e-05s`）。仮数部に `e±N` を許す形にしておかないと、縮退後の
  // 値そのものが「読めない」で null になり、判定が意図と逆に落ちる。
  const m = /^([\d.]+(?:e[-+]?\d+)?)(m?s)$/i.exec(first)
  if (m === null) return null
  return m[2].toLowerCase() === 's' ? Number(m[1]) * 1000 : Number(m[1])
}

/** motionOf は要素の animation-duration / transition-duration / opacity をまとめて読む。 */
async function motionOf(locator) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate((el) => {
    const cs = getComputedStyle(el)
    return {
      animationDuration: cs.animationDuration,
      transitionDuration: cs.transitionDuration,
      opacity: cs.opacity,
    }
  })
}

/** newMotionContext は `open()` と違い `reducedMotion` を指定できる素の context。 */
async function newMotionContext(viewport, reducedMotion) {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
    deviceScaleFactor: 2,
    reducedMotion,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  return { context, page }
}

for (const reducedMotion of ['reduce', 'no-preference']) {
  const isReduced = reducedMotion === 'reduce'

  // --- Skeleton の animate-pulse（読み込み中） ---
  {
    const { context, page } = await newMotionContext(desktop, reducedMotion)
    await installApiStubs(page, apiHandler({ delayPath: '/api/recordings', delayMs: 5000 }))
    await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
    const skeleton = page.locator('.animate-pulse').first()
    await skeleton.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${reducedMotion}] Skeleton の .animate-pulse が見つからない`)
    })
    const m = await motionOf(skeleton)
    if (m === null) {
      ng.push(`[${reducedMotion}] Skeleton（.animate-pulse）が見つからない`)
    } else {
      const ms = parseCssTime(m.animationDuration)
      log(
        `  [${reducedMotion}] Skeleton animation-duration=${m.animationDuration} ` +
          `opacity=${m.opacity}`,
      )
      if (isReduced) {
        if (ms === null || ms > REDUCE_THRESHOLD_MS) {
          ng.push(
            `[reduce] Skeleton の animate-pulse が縮退していない` +
              `（animation-duration=${m.animationDuration}）`,
          )
        }
        // 縮退後に不可視・不読になっていないか（`animation: none` ではなく
        // 継続時間を切り詰める判断の理由そのもの。index.css のコメント参照）。
        if (Number(m.opacity) < 0.5) {
          ng.push(
            `[reduce] Skeleton が縮退後に不透明度 ${m.opacity} まで下がり判読できない`,
          )
        }
      } else if (ms === null || ms < REDUCE_THRESHOLD_MS) {
        ng.push(
          `[no-preference] Skeleton の animate-pulse が既定（2s 周期）のまま動いていない` +
            `（animation-duration=${m.animationDuration}）`,
        )
      }
    }
    await context.close()
  }

  // --- 共通 Button の押下フィードバック（translate）の transition ---
  {
    const { context, page } = await newMotionContext(desktop, reducedMotion)
    await installApiStubs(page, apiHandler())
    await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
    const button = page.getByRole('button', { name: '検索' })
    // 待ちが失敗しても `motionOf` が `locator.count()` で見つからなさを直接
    // 見分けて distinct な NG を出す（`.evaluate` 自体は可視性を要求しないので
    // ハングもしない）。待ちはタイムアウトを早めるだけなので飲んでよい。
    await button.waitFor({ timeout: 10000 }).catch(() => {})
    const m = await motionOf(button)
    if (m === null) {
      ng.push(`[${reducedMotion}] 検索ボタンが見つからない（Button 遷移判定）`)
    } else {
      const ms = parseCssTime(m.transitionDuration)
      log(`  [${reducedMotion}] Button transition-duration=${m.transitionDuration}`)
      if (isReduced) {
        if (ms === null || ms > REDUCE_THRESHOLD_MS) {
          ng.push(
            `[reduce] Button の transition-duration が縮退していない` +
              `（${m.transitionDuration}）`,
          )
        }
      } else if (ms === null || ms < REDUCE_THRESHOLD_MS) {
        ng.push(
          `[no-preference] Button の transition-duration が既定（150ms）のまま動いていない` +
            `（${m.transitionDuration}）`,
        )
      }
    }
    await context.close()
  }
}

// 通常の design-interactions.mjs と browser-e2e の E2E_RULE_CARD_ONLY 入口で同じ H-2 判定を実行する。
await runRuleCardLayoutChecks()
await runCoarseTapTargetChecks()

await finish(ng, browser)
